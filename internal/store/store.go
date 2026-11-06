// Package store kapselt den Zugriff auf Postgres/pgvector.
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

type Store struct {
	pool *pgxpool.Pool
	dim  int
}

// OpenReadOnly verbindet ohne Migration (schnell, fuer Hooks).
func OpenReadOnly(ctx context.Context, url string, dim int) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("datenbank nicht erreichbar (laeuft 'docker compose up -d'?): %w", err)
	}
	return &Store{pool: pool, dim: dim}, nil
}

func Open(ctx context.Context, url string, dim int) (*Store, error) {
	s, err := OpenReadOnly(ctx, url, dim)
	if err != nil {
		return nil, err
	}
	pool := s.pool
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
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
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}
	// Dimension der bestehenden Tabelle muss zur Konfiguration passen.
	var typmod int
	err := s.pool.QueryRow(ctx, `SELECT atttypmod FROM pg_attribute
		WHERE attrelid = 'chunks'::regclass AND attname = 'embedding'`).Scan(&typmod)
	if err != nil {
		return fmt.Errorf("dimension pruefen: %w", err)
	}
	if typmod != s.dim {
		return fmt.Errorf("EMBEDDING_DIM=%d, die Datenbank wurde aber mit Dimension %d angelegt "+
			"(Modellwechsel erfordert neues Schema/Re-Indexierung)", s.dim, typmod)
	}
	return nil
}

// ChunkInput ist ein Textstueck samt Embedding.
type ChunkInput struct {
	Content   string
	Embedding []float32
}

// DocumentInput beschreibt ein neues Dokument bzw. einen neuen Fakt.
type DocumentInput struct {
	Title      string
	Source     string
	Project    string // leer = projektuebergreifend
	Kind       string // "document" oder "fact"
	Tags       []string
	Supersedes []int64 // aktive Eintraege, die dieser hier ersetzt
	Chunks     []ChunkInput
}

type AddResult struct {
	ID         int64
	Replaced   bool    // vorhandenes Dokument gleicher source wurde ersetzt
	Superseded []int64 // tatsaechlich als veraltet markierte Eintraege
}

// AddDocument speichert ein Dokument. Existiert bereits ein aktives mit
// gleicher source, wird es ersetzt (Re-Ingest). Eintraege in Supersedes werden
// als veraltet markiert (und bleiben fuer die Historie erhalten).
func (s *Store) AddDocument(ctx context.Context, in DocumentInput) (AddResult, error) {
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

	var src, proj *string
	if in.Source != "" {
		src = &in.Source
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

// Supersede markiert oldIDs als durch newID ersetzt. newID muss aktiv sein.
func (s *Store) Supersede(ctx context.Context, oldIDs []int64, newID int64) ([]int64, error) {
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
		return nil, fmt.Errorf("new_id %d ist selbst nicht aktiv (%s)", newID, status)
	}
	ids, err := markSuperseded(ctx, tx, oldIDs, newID)
	if err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

type SimilarEntry struct {
	ID         int64
	Title      string
	Project    string
	Kind       string
	Content    string
	Similarity float64
}

// Similar liefert die aehnlichsten aktiven Eintraege (je Dokument der beste
// Chunk) im Scope des Projekts plus globaler Eintraege.
func (s *Store) Similar(ctx context.Context, embedding []float32, project string, exclude []int64, limit int) ([]SimilarEntry, error) {
	if exclude == nil {
		exclude = []int64{} // nil wuerde als NULL kodiert und alle Zeilen ausfiltern
	}
	rows, err := s.pool.Query(ctx, `
SELECT d.id, d.title, COALESCE(d.project, ''), d.kind, c.content,
       1 - (c.embedding <=> $1::vector) AS sim
FROM chunks c JOIN documents d ON d.id = c.document_id
WHERE d.status = 'active'
  AND ($2::text IS NULL OR d.project IS NULL OR d.project = $2)
  AND NOT (d.id = ANY($3::bigint[]))
ORDER BY c.embedding <=> $1::vector
LIMIT 20`, vectorLiteral(embedding), nilIfEmpty(project), exclude)
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

type SearchHit struct {
	DocumentID int64
	Title      string
	Source     string
	Project    string
	Kind       string
	Tags       []string
	ChunkIndex int
	Content    string
	Score      float64
	Sim        float64 // Cosine-Aehnlichkeit des Chunks zur Anfrage (absolut, anders als Score)
}

// Search kombiniert Vektor- und Volltextsuche per Reciprocal Rank Fusion.
func (s *Store) Search(ctx context.Context, query string, embedding []float32, project string, tags []string, limit int) ([]SearchHit, error) {
	if len(tags) == 0 {
		tags = nil
	}
	const q = `
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
)
SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.tags, c.idx, c.content,
       COALESCE(1.0 / (60 + vec.rnk), 0) + COALESCE(1.0 / (60 + fts.rnk), 0) AS score,
       1 - (c.embedding <=> $1::vector) AS sim
FROM vec FULL OUTER JOIN fts ON vec.id = fts.id
JOIN chunks c ON c.id = COALESCE(vec.id, fts.id)
JOIN documents d ON d.id = c.document_id
ORDER BY score DESC
LIMIT $4`
	rows, err := s.pool.Query(ctx, q, vectorLiteral(embedding), query, tags, limit, nilIfEmpty(project))
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

type DocumentInfo struct {
	ID           int64
	Title        string
	Source       string
	Project      string
	Kind         string
	Status       string
	SupersededBy *int64
	Tags         []string
	Chunks       int
	UpdatedAt    time.Time
}

func (s *Store) ListDocuments(ctx context.Context, tag, project string, includeSuperseded bool, limit int) ([]DocumentInfo, error) {
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
	return out, rows.Err()
}

type Document struct {
	DocumentInfo
	Content string
}

var ErrNotFound = errors.New("dokument nicht gefunden")

// GetDocument rekonstruiert den Text aus den Chunks (Ueberlappungen inklusive).
func (s *Store) GetDocument(ctx context.Context, id int64) (*Document, error) {
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

func (s *Store) DeleteDocument(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
