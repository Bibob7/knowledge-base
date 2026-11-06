package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // pure Go driver ("sqlite"), FTS5 and JSON included
)

// sqliteStore keeps the knowledge base in a single SQLite file. Vectors are stored as
// little-endian float32 blobs and compared exactly in Go (no approximate index: at the
// size of a personal knowledge base a full scan takes milliseconds and never loses
// results to a filter). Full text uses FTS5.
type sqliteStore struct {
	db    *sql.DB
	path  string
	dim   int
	model string
}

const sqliteSchemaVersion = "1"

// timeLayout has a fixed width, so that the text sorts like the time.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func isSQLiteURL(u string) bool { return strings.HasPrefix(u, "sqlite:") }

// sqlitePath extracts the file path from sqlite:///abs/path, sqlite:/abs/path or
// sqlite:~/path.
func sqlitePath(u string) (string, error) {
	p := strings.TrimPrefix(u, "sqlite:")
	p = strings.TrimPrefix(p, "//")
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	}
	if p == "" || strings.ContainsAny(p, "?#") {
		return "", fmt.Errorf("invalid SQLite URL %q (expected sqlite:///path/to/knowledge.db)", u)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("SQLite path must be absolute: %q", p)
	}
	return filepath.Clean(p), nil
}

func sqliteDSN(path, mode string) string {
	q := url.Values{}
	q.Set("mode", mode)
	// IMMEDIATE transactions take the write lock up front: with several processes
	// (MCP servers of parallel sessions, hooks, extraction) a deferred transaction that
	// upgrades to a writer could fail with SQLITE_BUSY despite busy_timeout.
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return (&url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}).String()
}

// openSQLiteReadOnly opens an existing database without migrating it (fast, for hooks).
// Despite the name it can write (extraction stores facts); it only never creates the
// file or the schema.
func openSQLiteReadOnly(ctx context.Context, u string, dim int) (*sqliteStore, error) {
	path, err := sqlitePath(u)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("knowledge base %s not found (it is created when the MCP server starts; run /knowledge-base:setup): %w", path, err)
	}
	return openSQLiteFile(ctx, path, "rw", dim)
}

func openSQLiteFile(ctx context.Context, path, mode string, dim int) (*sqliteStore, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, mode))
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return &sqliteStore{db: db, path: path, dim: dim}, nil
}

// openSQLite creates the database file and schema if needed and checks that the stored
// vectors match the configured model and dimension.
func openSQLite(ctx context.Context, u string, dim int, model string) (*sqliteStore, error) {
	path, err := sqlitePath(u)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Create the file with private permissions; SQLite gives the -wal and -shm files the
	// same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	s, err := openSQLiteFile(ctx, path, "rwc", dim)
	if err != nil {
		return nil, err
	}
	s.model = normalizeModel(model)
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *sqliteStore) Close() { s.db.Close() }

func (s *sqliteStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil) // IMMEDIATE: parallel starts migrate one after the other
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS kb_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		// AUTOINCREMENT: IDs of deleted entries are never reused (they are referenced in
		// superseded_by and in conversations).
		`CREATE TABLE IF NOT EXISTS documents (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			title         TEXT NOT NULL,
			source        TEXT,
			project       TEXT,
			kind          TEXT NOT NULL DEFAULT 'document',
			status        TEXT NOT NULL DEFAULT 'active',
			superseded_by INTEGER REFERENCES documents(id) ON DELETE SET NULL,
			tags          TEXT NOT NULL DEFAULT '[]',
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS documents_active_source_key ON documents (source) WHERE source IS NOT NULL AND status = 'active'`,
		`CREATE INDEX IF NOT EXISTS documents_project_idx ON documents (project)`,
		`CREATE INDEX IF NOT EXISTS documents_superseded_by_idx ON documents (superseded_by)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
			idx         INTEGER NOT NULL,
			content     TEXT NOT NULL,
			embedding   BLOB NOT NULL,
			UNIQUE (document_id, idx)
		)`,
		// Like Postgres' 'simple' configuration: lower-cased words, no stemming, so German
		// and English both work. Diacritics are folded as well ("uber" finds "über").
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
			content, content='chunks', content_rowid='id', tokenize='unicode61 remove_diacritics 2'
		)`,
		`CREATE TRIGGER IF NOT EXISTS chunks_fts_insert AFTER INSERT ON chunks BEGIN
			INSERT INTO chunks_fts (rowid, content) VALUES (new.id, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_fts_delete AFTER DELETE ON chunks BEGIN
			INSERT INTO chunks_fts (chunks_fts, rowid, content) VALUES ('delete', old.id, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_fts_update AFTER UPDATE OF content ON chunks BEGIN
			INSERT INTO chunks_fts (chunks_fts, rowid, content) VALUES ('delete', old.id, old.content);
			INSERT INTO chunks_fts (rowid, content) VALUES (new.id, new.content);
		END`,
		`INSERT INTO kb_meta (key, value) VALUES ('schema_version', '` + sqliteSchemaVersion + `') ON CONFLICT (key) DO NOTHING`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}
	meta := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM kb_meta`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		meta[k] = v
	}
	rows.Close()
	if v := meta["schema_version"]; v != sqliteSchemaVersion {
		return fmt.Errorf("the database has schema version %s, this version of knowledge-base-mcp supports %s (update the plugin)", v, sqliteSchemaVersion)
	}
	// A new database takes the configured model and dimension.
	if meta["embedding_dim"] == "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_meta (key, value) VALUES ('embedding_dim', ?), ('embedding_model', ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, strconv.Itoa(s.dim), s.model); err != nil {
			return err
		}
		return tx.Commit()
	}
	if meta["embedding_dim"] != strconv.Itoa(s.dim) {
		return fmt.Errorf("EMBEDDING_DIM=%d, but the database was created with dimension %s "+
			"(switching models requires re-embedding: run 'knowledge-base-mcp reindex')", s.dim, meta["embedding_dim"])
	}
	if stored := meta["embedding_model"]; normalizeModel(stored) != s.model {
		return fmt.Errorf("the database was embedded with model %q but %q is configured "+
			"(run 'knowledge-base-mcp reindex' to re-embed everything with the configured model)", stored, s.model)
	}
	return tx.Commit()
}

func (s *sqliteStore) AddDocument(ctx context.Context, in DocumentInput) (AddResult, error) {
	var res AddResult
	if in.Tags == nil {
		in.Tags = []string{}
	}
	if in.Kind == "" {
		in.Kind = "document"
	}
	tags, err := json.Marshal(in.Tags)
	if err != nil {
		return res, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck
	if in.Source != "" {
		r, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE source = ? AND status = 'active'`, in.Source)
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Replaced = n > 0
	}
	now := nowText()
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO documents (title, source, project, kind, tags, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		in.Title, nilIfEmpty(in.Source), nilIfEmpty(in.Project), in.Kind, string(tags), now, now).Scan(&res.ID); err != nil {
		return res, err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO chunks (document_id, idx, content, embedding) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return res, err
	}
	defer ins.Close()
	for i, c := range in.Chunks {
		if s.dim > 0 && len(c.Embedding) != s.dim {
			return res, fmt.Errorf("chunk %d has dimension %d, expected %d", i, len(c.Embedding), s.dim)
		}
		if _, err := ins.ExecContext(ctx, res.ID, i, c.Content, encodeVector(c.Embedding)); err != nil {
			return res, err
		}
	}
	if len(in.Supersedes) > 0 {
		if res.Superseded, err = sqliteMarkSuperseded(ctx, tx, in.Supersedes, res.ID); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}

func sqliteMarkSuperseded(ctx context.Context, tx *sql.Tx, oldIDs []int64, newID int64) ([]int64, error) {
	ids, err := json.Marshal(oldIDs)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
UPDATE documents SET status = 'superseded', superseded_by = ?2, updated_at = ?3
WHERE id IN (SELECT value FROM json_each(?1)) AND id <> ?2 AND status = 'active'
RETURNING id`, string(ids), newID, nowText())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, rows.Err()
}

func (s *sqliteStore) Supersede(ctx context.Context, oldIDs []int64, newID int64) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM documents WHERE id = ?`, newID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("new_id %d: %w", newID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, fmt.Errorf("new_id %d is itself not active (%s)", newID, status)
	}
	ids, err := sqliteMarkSuperseded(ctx, tx, oldIDs, newID)
	if err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

// scope is the filter shared by Similar and Search: active entries of the project plus
// global ones (all projects if empty), optionally with at least one of the tags.
type scope struct {
	project string
	tags    []string
	exclude []int64
}

// where returns the condition on documents d and its arguments.
func (sc scope) where() (string, []any) {
	cond := []string{"d.status = 'active'"}
	var args []any
	if sc.project != "" {
		cond = append(cond, "(d.project IS NULL OR d.project = ?)")
		args = append(args, sc.project)
	}
	if len(sc.tags) > 0 {
		t, _ := json.Marshal(sc.tags)
		cond = append(cond, "EXISTS (SELECT 1 FROM json_each(d.tags) t WHERE t.value IN (SELECT value FROM json_each(?)))")
		args = append(args, string(t))
	}
	if len(sc.exclude) > 0 {
		e, _ := json.Marshal(sc.exclude)
		cond = append(cond, "d.id NOT IN (SELECT value FROM json_each(?))")
		args = append(args, string(e))
	}
	return strings.Join(cond, " AND "), args
}

type scored struct {
	chunkID, docID int64
	sim            float64
}

// nearest scans all chunks in scope and returns them sorted by cosine similarity to q
// (best first; ties by chunk ID).
func (s *sqliteStore) nearest(ctx context.Context, q []float32, sc scope) ([]scored, error) {
	where, args := sc.where()
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.document_id, c.embedding
		FROM chunks c JOIN documents d ON d.id = c.document_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var qn float64
	for _, f := range q {
		qn += float64(f) * float64(f)
	}
	qn = math.Sqrt(qn)
	var out []scored
	for rows.Next() {
		var e scored
		var blob []byte
		if err := rows.Scan(&e.chunkID, &e.docID, &blob); err != nil {
			return nil, err
		}
		if len(blob) != 4*len(q) {
			return nil, fmt.Errorf("chunk %d has dimension %d, the query %d (run 'knowledge-base-mcp reindex')", e.chunkID, len(blob)/4, len(q))
		}
		e.sim = cosineBlob(blob, q, qn)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].sim != out[j].sim {
			return out[i].sim > out[j].sim
		}
		return out[i].chunkID < out[j].chunkID
	})
	return out, nil
}

func (s *sqliteStore) Similar(ctx context.Context, embedding []float32, project string, exclude []int64, limit int) ([]SimilarEntry, error) {
	near, err := s.nearest(ctx, embedding, scope{project: project, exclude: exclude})
	if err != nil {
		return nil, err
	}
	// Best chunk per document.
	seen := map[int64]bool{}
	var best []scored
	for _, e := range near {
		if seen[e.docID] {
			continue
		}
		seen[e.docID] = true
		best = append(best, e)
		if len(best) >= limit {
			break
		}
	}
	out := make([]SimilarEntry, 0, len(best))
	for _, e := range best {
		x := SimilarEntry{ID: e.docID, Similarity: e.sim}
		if err := s.db.QueryRowContext(ctx, `SELECT d.title, COALESCE(d.project, ''), d.kind, c.content
			FROM chunks c JOIN documents d ON d.id = c.document_id WHERE c.id = ?`, e.chunkID).
			Scan(&x.Title, &x.Project, &x.Kind, &x.Content); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

// rrfK and rrfDepth are the Reciprocal Rank Fusion parameters (as in the Postgres backend).
const (
	rrfK     = 60
	rrfDepth = 50
)

func (s *sqliteStore) Search(ctx context.Context, query string, embedding []float32, project string, tags []string, limit int) ([]SearchHit, error) {
	sc := scope{project: project, tags: tags}
	near, err := s.nearest(ctx, embedding, sc)
	if err != nil {
		return nil, err
	}
	sims := make(map[int64]float64, len(near))
	for _, e := range near {
		sims[e.chunkID] = e.sim
	}
	score := map[int64]float64{}
	for i, e := range near {
		if i >= rrfDepth {
			break
		}
		score[e.chunkID] += 1.0 / float64(rrfK+i+1)
	}
	if m := ftsQuery(query); m != "" {
		where, args := sc.where()
		rows, err := s.db.QueryContext(ctx, `SELECT f.rowid FROM chunks_fts f
			JOIN chunks c ON c.id = f.rowid JOIN documents d ON d.id = c.document_id
			WHERE chunks_fts MATCH ? AND `+where+` ORDER BY bm25(chunks_fts), f.rowid LIMIT ?`,
			append(append([]any{m}, args...), rrfDepth)...)
		if err != nil {
			return nil, fmt.Errorf("full-text search: %w", err)
		}
		rank := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			rank++
			score[id] += 1.0 / float64(rrfK+rank)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	ids := make([]int64, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	hits := make([]SearchHit, 0, len(ids))
	for _, id := range ids {
		h := SearchHit{Score: score[id], Sim: sims[id]}
		var tagsJSON string
		if err := s.db.QueryRowContext(ctx, `SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.tags, c.idx, c.content
			FROM chunks c JOIN documents d ON d.id = c.document_id WHERE c.id = ?`, id).
			Scan(&h.DocumentID, &h.Title, &h.Source, &h.Project, &h.Kind, &tagsJSON, &h.ChunkIndex, &h.Content); err != nil {
			return nil, err
		}
		if h.Tags, err = decodeTags(tagsJSON); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, nil
}

// ftsQuery turns a search query into an FTS5 expression with the semantics of
// Postgres' websearch_to_tsquery: all words must occur, "quoted text" is a phrase,
// "or" between terms is an alternative and -word excludes a word. Words are split like
// the unicode61 tokenizer does (letters and digits), so user input can never be FTS5
// syntax. An empty result means: no full-text part.
func ftsQuery(q string) string {
	type term struct {
		text   string
		negate bool
	}
	var groups [][]term // OR of AND-groups
	var cur []term
	words := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	}
	quote := func(ws []string) string { return `"` + strings.Join(ws, " ") + `"` }
	pendingOr := false
	for rest := strings.TrimSpace(q); rest != ""; rest = strings.TrimSpace(rest) {
		negate := false
		if rest[0] == '-' {
			negate, rest = true, rest[1:]
		}
		var tok string
		phrase := false
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				tok, rest = rest[1:], ""
			} else {
				tok, rest = rest[1:end+1], rest[end+2:]
			}
			phrase = true
		} else if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
			tok, rest = rest[:i], rest[i:]
		} else {
			tok, rest = rest, ""
		}
		if !phrase && !negate && strings.EqualFold(tok, "or") {
			if len(cur) > 0 {
				pendingOr = true
			}
			continue
		}
		ws := words(tok)
		if len(ws) == 0 {
			continue
		}
		if pendingOr {
			groups, cur, pendingOr = append(groups, cur), nil, false
		}
		// A word with inner punctuation ("gamma-5") is a phrase of its parts, like in Postgres.
		cur = append(cur, term{text: quote(ws), negate: negate})
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	var parts []string
	for _, g := range groups {
		var pos, neg []string
		for _, t := range g {
			if t.negate {
				neg = append(neg, t.text)
			} else {
				pos = append(pos, t.text)
			}
		}
		if len(pos) == 0 { // FTS5 cannot express "everything except": drop a purely negative group
			continue
		}
		e := strings.Join(pos, " AND ")
		for _, n := range neg {
			e += " NOT " + n
		}
		parts = append(parts, "("+e+")")
	}
	return strings.Join(parts, " OR ")
}

func (s *sqliteStore) ListDocuments(ctx context.Context, tag, project string, includeSuperseded bool, limit int) ([]DocumentInfo, error) {
	cond := []string{"1 = 1"}
	var args []any
	if tag != "" {
		cond = append(cond, "EXISTS (SELECT 1 FROM json_each(d.tags) t WHERE t.value = ?)")
		args = append(args, tag)
	}
	if project != "" {
		cond = append(cond, "(d.project IS NULL OR d.project = ?)")
		args = append(args, project)
	}
	if !includeSuperseded {
		cond = append(cond, "d.status = 'active'")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.status, d.superseded_by,
       d.tags, d.updated_at, (SELECT count(*) FROM chunks c WHERE c.document_id = d.id)
FROM documents d WHERE `+strings.Join(cond, " AND ")+`
ORDER BY d.updated_at DESC, d.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocumentInfo
	for rows.Next() {
		d, err := scanDocumentInfo(rows.Scan, true)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// scanDocumentInfo scans id, title, source, project, kind, status, superseded_by, tags,
// updated_at and (withChunks) the chunk count.
func scanDocumentInfo(scan func(...any) error, withChunks bool) (DocumentInfo, error) {
	var d DocumentInfo
	var sup sql.NullInt64
	var tags, updated string
	dest := []any{&d.ID, &d.Title, &d.Source, &d.Project, &d.Kind, &d.Status, &sup, &tags, &updated}
	if withChunks {
		dest = append(dest, &d.Chunks)
	}
	if err := scan(dest...); err != nil {
		return d, err
	}
	if sup.Valid {
		d.SupersededBy = &sup.Int64
	}
	var err error
	if d.Tags, err = decodeTags(tags); err != nil {
		return d, err
	}
	if d.UpdatedAt, err = parseTime(updated); err != nil {
		return d, err
	}
	return d, nil
}

func (s *sqliteStore) GetDocument(ctx context.Context, id int64) (*Document, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, title, COALESCE(source, ''), COALESCE(project, ''), kind, status,
		superseded_by, tags, updated_at FROM documents d WHERE id = ?`, id)
	info, err := scanDocumentInfo(row.Scan, false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d := Document{DocumentInfo: info}
	rows, err := s.db.QueryContext(ctx, `SELECT content FROM chunks WHERE document_id = ? ORDER BY idx`, id)
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

func (s *sqliteStore) DeleteDocument(ctx context.Context, id int64) error {
	r, err := s.db.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) Snapshot(ctx context.Context, limit int) (*Snapshot, error) {
	snap := &Snapshot{}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM documents`).Scan(&snap.Total); err != nil {
		return nil, err
	}
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM kb_meta WHERE key = 'embedding_model'`).Scan(&snap.Model)
	for _, f := range []string{s.path, s.path + "-wal"} {
		if fi, err := os.Stat(f); err == nil {
			snap.DBBytes += fi.Size()
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, title, COALESCE(source, ''), COALESCE(project, ''), kind, status,
		superseded_by, tags, created_at, updated_at FROM documents ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	byID := map[int64]int{}
	for rows.Next() {
		var d SnapshotDoc
		var sup sql.NullInt64
		var tags, created, updated string
		if err := rows.Scan(&d.ID, &d.Title, &d.Source, &d.Project, &d.Kind, &d.Status, &sup, &tags, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		if sup.Valid {
			d.SupersededBy = &sup.Int64
		}
		if d.Tags, err = decodeTags(tags); err == nil {
			if d.CreatedAt, err = parseTime(created); err == nil {
				d.UpdatedAt, err = parseTime(updated)
			}
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		byID[d.ID] = len(snap.Docs)
		snap.Docs = append(snap.Docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snap.Docs) == 0 {
		return snap, nil
	}

	// Text and mean vector per document.
	minID := snap.Docs[len(snap.Docs)-1].ID
	crow, err := s.db.QueryContext(ctx, `SELECT document_id, content, embedding FROM chunks WHERE document_id >= ? ORDER BY document_id, idx`, minID)
	if err != nil {
		return nil, err
	}
	defer crow.Close()
	counts := map[int64]int{}
	var texts = map[int64][]string{}
	for crow.Next() {
		var id int64
		var content string
		var blob []byte
		if err := crow.Scan(&id, &content, &blob); err != nil {
			return nil, err
		}
		i, ok := byID[id]
		if !ok {
			continue
		}
		v := decodeVector(blob)
		d := &snap.Docs[i]
		if d.Vec == nil {
			d.Vec = make([]float32, len(v))
		}
		if len(v) != len(d.Vec) {
			return nil, fmt.Errorf("document %d has chunks of different dimensions", id)
		}
		for j, f := range v {
			d.Vec[j] += f
		}
		counts[id]++
		texts[id] = append(texts[id], content)
	}
	if err := crow.Err(); err != nil {
		return nil, err
	}
	for i := range snap.Docs {
		d := &snap.Docs[i]
		if n := counts[d.ID]; n > 0 {
			for j := range d.Vec {
				d.Vec[j] /= float32(n)
			}
		}
		d.Content = strings.Join(texts[d.ID], "\n")
	}
	return snap, nil
}

func (s *sqliteStore) CurrentEmbedding(ctx context.Context) (dim int, model string, err error) {
	var d string
	if err = s.db.QueryRowContext(ctx, `SELECT value FROM kb_meta WHERE key = 'embedding_dim'`).Scan(&d); err != nil {
		return 0, "", fmt.Errorf("reading the embedding dimension: %w", err)
	}
	if dim, err = strconv.Atoi(d); err != nil {
		return 0, "", err
	}
	err = s.db.QueryRowContext(ctx, `SELECT value FROM kb_meta WHERE key = 'embedding_model'`).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return dim, model, err
}

// Reindex re-embeds all chunks. New vectors are first written to a staging table, so an
// interrupted run loses nothing and continues where it stopped; the switch to the new
// vectors happens in one transaction. Without a vector index there is no dimension limit.
func (s *sqliteStore) Reindex(ctx context.Context, o ReindexOptions) error {
	if o.Dim <= 0 {
		return fmt.Errorf("invalid dimension %d", o.Dim)
	}
	if o.Batch <= 0 {
		o.Batch = 32
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	target := fmt.Sprintf("%s|%d", normalizeModel(o.Model), o.Dim)
	var prev string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kb_meta WHERE key = 'reindex_target'`).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if prev != target {
		if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS chunk_reindex`); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS chunk_reindex (chunk_id INTEGER PRIMARY KEY, embedding BLOB NOT NULL)`,
		`INSERT INTO kb_meta (key, value) VALUES ('reindex_target', '` + sqlQuote(target) + `') ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}

	for {
		rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.content FROM chunks c
			WHERE NOT EXISTS (SELECT 1 FROM chunk_reindex r WHERE r.chunk_id = c.id) ORDER BY c.id LIMIT ?`, o.Batch)
		if err != nil {
			return err
		}
		var ids []int64
		var texts []string
		for rows.Next() {
			var id int64
			var content string
			if err := rows.Scan(&id, &content); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			texts = append(texts, o.DocPrefix+content)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		vecs, err := o.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embedding: %w", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for i, id := range ids {
			if len(vecs[i]) != o.Dim {
				tx.Rollback() //nolint:errcheck
				return fmt.Errorf("model returned dimension %d, expected %d", len(vecs[i]), o.Dim)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_reindex (chunk_id, embedding) VALUES (?, ?)
				ON CONFLICT (chunk_id) DO NOTHING`, id, encodeVector(vecs[i])); err != nil {
				tx.Rollback() //nolint:errcheck
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		var staged int
		_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM chunk_reindex`).Scan(&staged)
		o.Logf("embedded %d chunks so far", staged)
	}

	o.Logf("switching to the new vectors (%d dimensions)", o.Dim)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var missing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM chunks c
		WHERE NOT EXISTS (SELECT 1 FROM chunk_reindex r WHERE r.chunk_id = c.id)`).Scan(&missing); err != nil {
		return err
	}
	if missing > 0 { // added after the last batch: nothing was changed, simply run again
		return fmt.Errorf("%d chunks were added during the reindex; nothing was changed, please run reindex again", missing)
	}
	for _, q := range []string{
		`UPDATE chunks SET embedding = (SELECT r.embedding FROM chunk_reindex r WHERE r.chunk_id = chunks.id)`,
		`INSERT INTO kb_meta (key, value) VALUES ('embedding_dim', '` + strconv.Itoa(o.Dim) + `') ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		`INSERT INTO kb_meta (key, value) VALUES ('embedding_model', '` + sqlQuote(normalizeModel(o.Model)) + `') ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		`DELETE FROM kb_meta WHERE key = 'reindex_target'`,
		`DROP TABLE chunk_reindex`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.dim, s.model = o.Dim, normalizeModel(o.Model)
	o.Logf("done")
	return nil
}

func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVector(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// cosineBlob is the cosine similarity between an encoded vector and q (with norm qn),
// without decoding into a slice first. A zero vector has similarity 0.
func cosineBlob(b []byte, q []float32, qn float64) float64 {
	var dot, n float64
	for i, qf := range q {
		f := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])))
		dot += f * float64(qf)
		n += f * f
	}
	if n == 0 || qn == 0 {
		return 0
	}
	return dot / (math.Sqrt(n) * qn)
}

func decodeTags(s string) ([]string, error) {
	tags := []string{}
	if s == "" {
		return tags, nil
	}
	if err := json.Unmarshal([]byte(s), &tags); err != nil {
		return nil, fmt.Errorf("tags %q: %w", s, err)
	}
	return tags, nil
}

func nowText() string { return time.Now().UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
