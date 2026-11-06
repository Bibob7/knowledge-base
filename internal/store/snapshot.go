package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SnapshotDoc is one entry with its text and its mean (document-level) embedding.
type SnapshotDoc struct {
	ID           int64
	Title        string
	Project      string
	Kind         string
	Status       string
	SupersededBy *int64
	Source       string
	Tags         []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Content      string
	Vec          []float32
}

// Snapshot is a read-only view of the whole knowledge base for visualisation.
type Snapshot struct {
	Docs    []SnapshotDoc
	Total   int // entries in the database (may exceed len(Docs) when limited)
	Model   string
	DBBytes int64
}

// Snapshot returns the newest `limit` entries including their document embeddings
// (the mean of the chunk embeddings).
func (s *Store) Snapshot(ctx context.Context, limit int) (*Snapshot, error) {
	snap := &Snapshot{}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM documents`).Scan(&snap.Total); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&snap.DBBytes); err != nil {
		return nil, err
	}
	// kb_meta may not exist in very old databases: ignore errors, the model is cosmetic.
	_ = s.pool.QueryRow(ctx, `SELECT value FROM kb_meta WHERE key = 'embedding_model'`).Scan(&snap.Model)

	rows, err := s.pool.Query(ctx, `
SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.status, d.superseded_by,
       d.tags, d.created_at, d.updated_at,
       COALESCE(string_agg(c.content, E'\n' ORDER BY c.idx), ''), avg(c.embedding)::text
FROM documents d
LEFT JOIN chunks c ON c.document_id = d.id
GROUP BY d.id
ORDER BY d.id DESC
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d SnapshotDoc
		var vec *string
		if err := rows.Scan(&d.ID, &d.Title, &d.Source, &d.Project, &d.Kind, &d.Status, &d.SupersededBy,
			&d.Tags, &d.CreatedAt, &d.UpdatedAt, &d.Content, &vec); err != nil {
			return nil, err
		}
		if vec != nil {
			if d.Vec, err = parseVector(*vec); err != nil {
				return nil, fmt.Errorf("document %d: %w", d.ID, err)
			}
		}
		snap.Docs = append(snap.Docs, d)
	}
	return snap, rows.Err()
}

// parseVector parses pgvector's text form "[0.1,0.2,...]".
func parseVector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, err
		}
		out[i] = float32(f)
	}
	return out, nil
}
