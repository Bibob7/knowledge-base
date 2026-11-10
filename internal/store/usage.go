package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Usage counts how often an entry was put into a session: injected by the prompt hook,
// or retrieved by Claude (a search_knowledge hit or a get_document call). Entries that
// are never used are candidates for a review; the counters are only a hint, an entry
// can matter without ever being found (and vice versa).
type Usage struct {
	Injected  int
	Retrieved int
	LastUsed  *time.Time // nil: never used since usage tracking started
}

// UseKind tells RecordUse which counter to increment.
type UseKind int

const (
	UseInjected UseKind = iota
	UseRetrieved
)

func (k UseKind) counts() (injected, retrieved int) {
	if k == UseInjected {
		return 1, 0
	}
	return 0, 1
}

// The usage table is separate from documents, so that older versions of the program
// keep working on a database that has it (no schema version change). It is created by
// the migration of the MCP server; stores opened with OpenReadOnly (hooks) only write to
// it if it exists and ignore it otherwise.
const (
	sqliteUsageTable = `CREATE TABLE IF NOT EXISTS doc_usage (
			document_id  INTEGER PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
			injected     INTEGER NOT NULL DEFAULT 0,
			retrieved    INTEGER NOT NULL DEFAULT 0,
			last_used_at TEXT    NOT NULL
		)`
	pgUsageTable = `CREATE TABLE IF NOT EXISTS doc_usage (
			document_id  BIGINT PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
			injected     INT NOT NULL DEFAULT 0,
			retrieved    INT NOT NULL DEFAULT 0,
			last_used_at TIMESTAMPTZ NOT NULL
		)`
	// usageSinceKey in kb_meta records when usage tracking started: before that, no entry
	// can have been counted, so "unused" means nothing.
	usageSinceKey = "usage_since"
)

func uniqueIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// --- SQLite ---

func (s *sqliteStore) RecordUse(ctx context.Context, ids []int64, kind UseKind) error {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	list, _ := json.Marshal(ids)
	inj, ret := kind.counts()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO doc_usage (document_id, injected, retrieved, last_used_at)
SELECT d.id, ?, ?, ? FROM documents d WHERE d.id IN (SELECT value FROM json_each(?))
ON CONFLICT (document_id) DO UPDATE SET
	injected = doc_usage.injected + excluded.injected,
	retrieved = doc_usage.retrieved + excluded.retrieved,
	last_used_at = excluded.last_used_at`, inj, ret, time.Now().UTC().Format(timeLayout), string(list))
	return err
}

func (s *sqliteStore) UsageSince(ctx context.Context) (time.Time, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kb_meta WHERE key = ?`, usageSinceKey).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return parseTime(v)
}

// usageOf loads the counters of the given entries. Errors (e.g. a database whose MCP
// server has not created the table yet) yield no counters: they are informational.
func (s *sqliteStore) usageOf(ctx context.Context, ids []int64) map[int64]Usage {
	out := map[int64]Usage{}
	if len(ids) == 0 {
		return out
	}
	list, _ := json.Marshal(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT document_id, injected, retrieved, last_used_at FROM doc_usage
		WHERE document_id IN (SELECT value FROM json_each(?))`, string(list))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var u Usage
		var last string
		if rows.Scan(&id, &u.Injected, &u.Retrieved, &last) != nil {
			continue
		}
		if t, err := parseTime(last); err == nil {
			u.LastUsed = &t
		}
		out[id] = u
	}
	return out
}

func (s *sqliteStore) Unused(ctx context.Context, project string, before time.Time, limit int) ([]DocumentInfo, error) {
	cut := before.UTC().Format(timeLayout)
	cond := []string{"d.status = 'active'", "d.created_at < ?", "(u.last_used_at IS NULL OR u.last_used_at < ?)"}
	args := []any{cut, cut}
	if project != "" {
		cond = append(cond, "(d.project IS NULL OR d.project = ?)")
		args = append(args, project)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.status, d.superseded_by,
       d.tags, d.updated_at, (SELECT count(*) FROM chunks c WHERE c.document_id = d.id)
FROM documents d LEFT JOIN doc_usage u ON u.document_id = d.id
WHERE `+strings.Join(cond, " AND ")+`
ORDER BY COALESCE(u.last_used_at, d.created_at), d.id LIMIT ?`, append(args, limit)...)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.fillUsage(ctx, out)
	return out, nil
}

func (s *sqliteStore) fillUsage(ctx context.Context, docs []DocumentInfo) {
	ids := make([]int64, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	u := s.usageOf(ctx, ids)
	for i := range docs {
		docs[i].Usage = u[docs[i].ID]
	}
}

// --- Postgres ---

func (s *pgStore) RecordUse(ctx context.Context, ids []int64, kind UseKind) error {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	inj, ret := kind.counts()
	_, err := s.pool.Exec(ctx, `
INSERT INTO doc_usage (document_id, injected, retrieved, last_used_at)
SELECT d.id, $2, $3, now() FROM documents d WHERE d.id = ANY($1)
ON CONFLICT (document_id) DO UPDATE SET
	injected = doc_usage.injected + excluded.injected,
	retrieved = doc_usage.retrieved + excluded.retrieved,
	last_used_at = excluded.last_used_at`, ids, inj, ret)
	return err
}

func (s *pgStore) UsageSince(ctx context.Context) (time.Time, error) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM kb_meta WHERE key = $1`, usageSinceKey).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return parseTime(v)
}

func (s *pgStore) usageOf(ctx context.Context, ids []int64) map[int64]Usage {
	out := map[int64]Usage{}
	if len(ids) == 0 {
		return out
	}
	rows, err := s.pool.Query(ctx, `SELECT document_id, injected, retrieved, last_used_at FROM doc_usage
		WHERE document_id = ANY($1)`, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var u Usage
		var last time.Time
		if rows.Scan(&id, &u.Injected, &u.Retrieved, &last) != nil {
			continue
		}
		u.LastUsed = &last
		out[id] = u
	}
	return out
}

func (s *pgStore) Unused(ctx context.Context, project string, before time.Time, limit int) ([]DocumentInfo, error) {
	rows, err := s.pool.Query(ctx, `
SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.status, d.superseded_by,
       d.tags, d.updated_at, (SELECT count(*) FROM chunks c WHERE c.document_id = d.id)
FROM documents d LEFT JOIN doc_usage u ON u.document_id = d.id
WHERE d.status = 'active' AND d.created_at < $1 AND (u.last_used_at IS NULL OR u.last_used_at < $1)
  AND ($2::text IS NULL OR d.project IS NULL OR d.project = $2)
ORDER BY COALESCE(u.last_used_at, d.created_at), d.id
LIMIT $3`, before, nilIfEmpty(project), limit)
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

func (s *pgStore) fillUsage(ctx context.Context, docs []DocumentInfo) {
	ids := make([]int64, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	u := s.usageOf(ctx, ids)
	for i := range docs {
		docs[i].Usage = u[docs[i].ID]
	}
}
