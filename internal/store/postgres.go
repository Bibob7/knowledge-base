package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore keeps the knowledge base in Postgres with pgvector (HNSW index, tsvector full text).
type pgStore struct {
	pool  *pgxpool.Pool
	dim   int
	model string
}

// openPostgresReadOnly connects without running migrations (fast, for hooks).
func openPostgresReadOnly(ctx context.Context, url string, dim int) (*pgStore, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("Postgres not reachable (is it running? see /knowledge-base:setup): %w", err)
	}
	return &pgStore{pool: pool, dim: dim}, nil
}

// openPostgres connects, runs the migrations and verifies that the database was embedded
// with the configured model and dimension.
func openPostgres(ctx context.Context, url string, dim int, model string) (*pgStore, error) {
	s, err := openPostgresReadOnly(ctx, url, dim)
	if err != nil {
		return nil, err
	}
	s.model = normalizeModel(model)
	pool := s.pool
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *pgStore) Close() { s.pool.Close() }

func (s *pgStore) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`CREATE TABLE IF NOT EXISTS kb_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS documents (
			id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			title      TEXT        NOT NULL,
			source     TEXT,
			tags       TEXT[]      NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS project TEXT`,
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'document'`,
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active'`,
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS superseded_by BIGINT REFERENCES documents(id) ON DELETE SET NULL`,
		`DROP INDEX IF EXISTS documents_source_key`,
		`CREATE UNIQUE INDEX IF NOT EXISTS documents_active_source_key ON documents (source) WHERE source IS NOT NULL AND status = 'active'`,
		`CREATE INDEX IF NOT EXISTS documents_project_idx ON documents (project)`,
		`CREATE INDEX IF NOT EXISTS documents_tags_idx ON documents USING gin (tags)`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS chunks (
			id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			document_id BIGINT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
			idx         INT    NOT NULL,
			content     TEXT   NOT NULL,
			embedding   vector(%d) NOT NULL,
			tsv         tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED,
			UNIQUE (document_id, idx)
		)`, s.dim),
		`CREATE INDEX IF NOT EXISTS chunks_embedding_idx ON chunks USING hnsw (embedding vector_cosine_ops)`,
		`CREATE INDEX IF NOT EXISTS chunks_tsv_idx ON chunks USING gin (tsv)`,
		pgUsageTable,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO kb_meta (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`,
		usageSinceKey, time.Now().UTC().Format(timeLayout)); err != nil {
		return fmt.Errorf("migration: %w", err)
	}
	// The dimension of the existing table must match the configuration.
	var typmod int
	err := s.pool.QueryRow(ctx, `SELECT atttypmod FROM pg_attribute
		WHERE attrelid = 'chunks'::regclass AND attname = 'embedding'`).Scan(&typmod)
	if err != nil {
		return fmt.Errorf("checking dimension: %w", err)
	}
	if typmod != s.dim {
		return fmt.Errorf("EMBEDDING_DIM=%d, but the database was created with dimension %d "+
			"(switching models requires re-embedding: run 'knowledge-base-mcp reindex')", s.dim, typmod)
	}
	// The model must match too: vectors of different models are not comparable even
	// if the dimension is the same.
	var stored string
	err = s.pool.QueryRow(ctx, `SELECT value FROM kb_meta WHERE key = 'embedding_model'`).Scan(&stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Databases from before this check: assume the configured model.
		_, err = s.pool.Exec(ctx, `INSERT INTO kb_meta (key, value) VALUES ('embedding_model', $1)
			ON CONFLICT (key) DO NOTHING`, s.model)
		return err
	case err != nil:
		return fmt.Errorf("checking embedding model: %w", err)
	case normalizeModel(stored) != s.model:
		return fmt.Errorf("the database was embedded with model %q but %q is configured "+
			"(run 'knowledge-base-mcp reindex' to re-embed everything with the configured model)", stored, s.model)
	}
	return nil
}

// AddDocument stores a document. If an active one with the same source already
// exists, it is replaced (re-ingest). Entries in Supersedes are marked as
// outdated (and kept as history).
func (s *pgStore) AddDocument(ctx context.Context, in DocumentInput) (AddResult, error) {
	var res AddResult
	if in.Tags == nil {
		in.Tags = []string{}
	}
	if in.Kind == "" {
		in.Kind = "document"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := s.checkEmbedding(ctx, tx); err != nil {
		return res, err
	}

	var src, proj *string
	var predecessors []int64 // entries the replaced version had superseded
	if in.Source != "" {
		src = &in.Source
		rows, err := tx.Query(ctx, `SELECT id FROM documents WHERE superseded_by IN
			(SELECT id FROM documents WHERE source = $1 AND status = 'active')`, in.Source)
		if err != nil {
			return res, err
		}
		if predecessors, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
			return res, err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM documents WHERE source = $1 AND status = 'active'`, in.Source)
		if err != nil {
			return res, err
		}
		res.Replaced = tag.RowsAffected() > 0
	}
	if in.Project != "" {
		proj = &in.Project
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO documents (title, source, project, kind, tags) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		in.Title, src, proj, in.Kind, in.Tags).Scan(&res.ID); err != nil {
		return res, err
	}
	if len(predecessors) > 0 {
		// The new version takes over the history of the replaced one.
		if _, err := tx.Exec(ctx, `UPDATE documents SET superseded_by = $1 WHERE id = ANY($2)`, res.ID, predecessors); err != nil {
			return res, err
		}
	}
	batch := &pgx.Batch{}
	for i, c := range in.Chunks {
		batch.Queue(`INSERT INTO chunks (document_id, idx, content, embedding) VALUES ($1, $2, $3, $4::vector)`,
			res.ID, i, c.Content, vectorLiteral(c.Embedding))
	}
	br := tx.SendBatch(ctx, batch)
	for range in.Chunks {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return res, err
		}
	}
	if err := br.Close(); err != nil {
		return res, err
	}
	if len(in.Supersedes) > 0 {
		res.Superseded, err = markSuperseded(ctx, tx, in.Supersedes, res.ID)
		if err != nil {
			return res, err
		}
	}
	return res, tx.Commit(ctx)
}

// checkEmbedding makes sure, inside the write transaction, that the database is still
// embedded with the model and dimension of this process (see the SQLite variant).
func (s *pgStore) checkEmbedding(ctx context.Context, tx pgx.Tx) error {
	if s.model == "" {
		return nil
	}
	var typmod int
	var model string
	err := tx.QueryRow(ctx, `SELECT
		(SELECT atttypmod FROM pg_attribute WHERE attrelid = 'chunks'::regclass AND attname = 'embedding'),
		COALESCE((SELECT value FROM kb_meta WHERE key = 'embedding_model'), '')`).Scan(&typmod, &model)
	if err != nil {
		return err
	}
	if typmod != s.dim || normalizeModel(model) != s.model {
		return errEmbeddingChanged(strconv.Itoa(typmod), model, s.dim, s.model)
	}
	return nil
}

func markSuperseded(ctx context.Context, tx pgx.Tx, oldIDs []int64, newID int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `
UPDATE documents SET status = 'superseded', superseded_by = $2, updated_at = now()
WHERE id = ANY($1) AND id <> $2 AND status = 'active'
RETURNING id`, oldIDs, newID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Supersede marks oldIDs as replaced by newID. newID must be active.
func (s *pgStore) Supersede(ctx context.Context, oldIDs []int64, newID int64) ([]int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM documents WHERE id = $1`, newID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("new_id %d: %w", newID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, fmt.Errorf("new_id %d is itself not active (%s)", newID, status)
	}
	ids, err := markSuperseded(ctx, tx, oldIDs, newID)
	if err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

// Similar returns the most similar active entries (best chunk per document)
// within the scope of the project plus global entries.
func (s *pgStore) Similar(ctx context.Context, embedding []float32, project string, exclude []int64, limit int) ([]SimilarEntry, error) {
	if exclude == nil {
		exclude = []int64{} // nil would be encoded as NULL and filter out all rows
	}
	// With a project the candidate set is small, so it is filtered first and sorted
	// exactly. Filtering after an HNSW scan would silently drop most results (see Search).
	const scoped = `
WITH cand AS MATERIALIZED (
	SELECT d.id AS doc_id, d.title, COALESCE(d.project, '') AS project, d.kind, c.content, c.embedding
	FROM chunks c JOIN documents d ON d.id = c.document_id
	WHERE d.status = 'active' AND (d.project IS NULL OR d.project = $2)
	  AND NOT (d.id = ANY($3::bigint[]))
)
SELECT doc_id, title, project, kind, content, 1 - (embedding <=> $1::vector) AS sim
FROM cand ORDER BY embedding <=> $1::vector LIMIT 20`
	// Same parameters as the scoped variant; the project condition is always true here
	// (the project is NULL).
	const unscoped = `
SELECT d.id, d.title, COALESCE(d.project, ''), d.kind, c.content,
       1 - (c.embedding <=> $1::vector) AS sim
FROM chunks c JOIN documents d ON d.id = c.document_id
WHERE d.status = 'active' AND ($2::text IS NULL OR d.project IS NULL OR d.project = $2)
  AND NOT (d.id = ANY($3::bigint[]))
ORDER BY c.embedding <=> $1::vector
LIMIT 20`
	q, useIndex := unscoped, true
	if project != "" {
		q, useIndex = scoped, false
	}
	tx, err := s.beginRead(ctx, useIndex)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, q, vectorLiteral(embedding), nilIfEmpty(project), exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[int64]bool{}
	var out []SimilarEntry
	for rows.Next() {
		var e SimilarEntry
		if err := rows.Scan(&e.ID, &e.Title, &e.Project, &e.Kind, &e.Content, &e.Similarity); err != nil {
			return nil, err
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// beginRead starts a read transaction. If the query will use the HNSW index, iterative
// scans are enabled so that filters applied after the index scan (e.g. status) cannot
// leave the result short (pgvector >= 0.8.0; unknown settings are ignored by older versions).
func (s *pgStore) beginRead(ctx context.Context, useIndex bool) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if useIndex {
		for _, q := range []string{"SET LOCAL hnsw.iterative_scan = relaxed_order", "SET LOCAL hnsw.ef_search = 100"} {
			if _, err := tx.Exec(ctx, q); err != nil {
				tx.Rollback(ctx) //nolint:errcheck
				return nil, err
			}
		}
	}
	return tx, nil
}

// Search combines vector and full-text search via Reciprocal Rank Fusion.
//
// pgvector applies WHERE filters after the approximate HNSW scan. With many
// projects that silently drops most results (measured on 150k chunks / 150 projects:
// 8.9 of 50 rows, recall 0.18). So whenever a project or tag filter is present, the
// matching rows are selected first and sorted exactly (4.9 ms in the same benchmark,
// recall 1.0). Without such a filter the HNSW index is used with iterative scans.
func (s *pgStore) Search(ctx context.Context, query string, embedding []float32, project string, tags []string, limit int) ([]SearchHit, error) {
	if len(tags) == 0 {
		tags = nil
	}
	const tail = `
SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.tags, c.idx, c.content,
       COALESCE(1.0 / (60 + vec.rnk), 0) + COALESCE(1.0 / (60 + fts.rnk), 0) AS score,
       1 - (c.embedding <=> $1::vector) AS sim
FROM vec FULL OUTER JOIN fts ON vec.id = fts.id
JOIN chunks c ON c.id = COALESCE(vec.id, fts.id)
JOIN documents d ON d.id = c.document_id
ORDER BY score DESC
LIMIT $4`
	const scoped = `
WITH cand AS MATERIALIZED (
	SELECT c.id, c.embedding, c.tsv
	FROM chunks c JOIN documents d ON d.id = c.document_id
	WHERE d.status = 'active' AND ($3::text[] IS NULL OR d.tags && $3::text[])
	  AND ($5::text IS NULL OR d.project IS NULL OR d.project = $5)
), vec AS (
	SELECT id, ROW_NUMBER() OVER (ORDER BY dist) AS rnk
	FROM (SELECT id, embedding <=> $1::vector AS dist FROM cand ORDER BY dist LIMIT 50) v
), fts AS (
	SELECT id, ROW_NUMBER() OVER (ORDER BY rank DESC) AS rnk
	FROM (SELECT cand.id, ts_rank_cd(cand.tsv, wq.q) AS rank
	      FROM cand, websearch_to_tsquery('simple', $2) AS wq(q)
	      WHERE cand.tsv @@ wq.q ORDER BY rank DESC LIMIT 50) f
)` + tail
	const unscoped = `
WITH vec AS (
	SELECT c.id, ROW_NUMBER() OVER (ORDER BY c.embedding <=> $1::vector) AS rnk
	FROM chunks c JOIN documents d ON d.id = c.document_id
	WHERE d.status = 'active' AND ($3::text[] IS NULL OR d.tags && $3::text[])
	  AND ($5::text IS NULL OR d.project IS NULL OR d.project = $5)
	ORDER BY c.embedding <=> $1::vector
	LIMIT 50
), fts AS (
	SELECT c.id, ROW_NUMBER() OVER (ORDER BY ts_rank_cd(c.tsv, wq.q) DESC) AS rnk
	FROM chunks c JOIN documents d ON d.id = c.document_id,
	     websearch_to_tsquery('simple', $2) AS wq(q)
	WHERE c.tsv @@ wq.q AND d.status = 'active' AND ($3::text[] IS NULL OR d.tags && $3::text[])
	  AND ($5::text IS NULL OR d.project IS NULL OR d.project = $5)
	ORDER BY ts_rank_cd(c.tsv, wq.q) DESC
	LIMIT 50
)` + tail
	q, useIndex := unscoped, true
	if project != "" || tags != nil {
		q, useIndex = scoped, false
	}
	tx, err := s.beginRead(ctx, useIndex)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Both variants take the same parameters. In the unscoped variant the project/tag
	// conditions are always true (both are NULL there).
	rows, err := tx.Query(ctx, q, vectorLiteral(embedding), query, tags, limit, nilIfEmpty(project))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.DocumentID, &h.Title, &h.Source, &h.Project, &h.Kind, &h.Tags, &h.ChunkIndex, &h.Content, &h.Score, &h.Sim); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *pgStore) ListDocuments(ctx context.Context, tag, project string, includeSuperseded bool, limit int) ([]DocumentInfo, error) {
	rows, err := s.pool.Query(ctx, `
SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.status, d.superseded_by,
       d.tags, d.updated_at, (SELECT count(*) FROM chunks c WHERE c.document_id = d.id)
FROM documents d
WHERE ($1::text IS NULL OR $1 = ANY(d.tags))
  AND ($2::text IS NULL OR d.project IS NULL OR d.project = $2)
  AND ($3 OR d.status = 'active')
ORDER BY d.updated_at DESC
LIMIT $4`, nilIfEmpty(tag), nilIfEmpty(project), includeSuperseded, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocumentInfo
	for rows.Next() {
		var d DocumentInfo
		if err := rows.Scan(&d.ID, &d.Title, &d.Source, &d.Project, &d.Kind, &d.Status, &d.SupersededBy,
			&d.Tags, &d.UpdatedAt, &d.Chunks); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.fillUsage(ctx, out)
	return out, nil
}

// GetDocument reconstructs the text from the chunks (overlaps included).
func (s *pgStore) GetDocument(ctx context.Context, id int64) (*Document, error) {
	var d Document
	err := s.pool.QueryRow(ctx,
		`SELECT id, title, COALESCE(source, ''), COALESCE(project, ''), kind, status, superseded_by, tags, updated_at
		 FROM documents WHERE id = $1`, id).
		Scan(&d.ID, &d.Title, &d.Source, &d.Project, &d.Kind, &d.Status, &d.SupersededBy, &d.Tags, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Usage = s.usageOf(ctx, []int64{id})[id]
	rows, err := s.pool.Query(ctx, `SELECT content FROM chunks WHERE document_id = $1 ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		parts = append(parts, c)
	}
	d.Chunks = len(parts)
	d.Content = strings.Join(parts, "\n\n[...]\n\n")
	return &d, rows.Err()
}

// DeleteDocument removes an entry. Entries it had superseded take over its place in the
// history (see sqliteRelinkSuccessors).
func (s *pgStore) DeleteDocument(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
UPDATE documents p SET superseded_by = d.superseded_by,
	status = CASE WHEN d.superseded_by IS NULL THEN 'active' ELSE 'superseded' END,
	updated_at = now()
FROM documents d WHERE d.id = $1 AND p.superseded_by = $1`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
