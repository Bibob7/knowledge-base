package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// exportDoc is one entry with everything needed to recreate it elsewhere.
type exportDoc struct {
	ID           int64
	Title        string
	Source       string
	Project      string
	Kind         string
	Status       string
	SupersededBy *int64
	Tags         []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Chunks       []ChunkInput // in order (idx)
}

var (
	_ migrator = (*pgStore)(nil)
	_ migrator = (*sqliteStore)(nil)
)

// migrator is implemented by every backend.
type migrator interface {
	// exportAll calls fn for every entry (including superseded ones) in ID order.
	exportAll(ctx context.Context, fn func(exportDoc) error) error
	// importAll inserts the entries with their IDs into an empty database in one
	// transaction; superseded_by is set after all entries exist.
	importAll(ctx context.Context, docs []exportDoc) error
	// counts returns the number of entries and chunks.
	counts(ctx context.Context) (docs, chunks int, err error)
}

// MigrateResult reports what Migrate copied.
type MigrateResult struct {
	Docs, Chunks int
	Model        string
	Dim          int
}

// Migrate copies the whole knowledge base from the database at fromURL into the empty
// database at toURL (created if needed): all entries with their IDs, history, tags,
// timestamps, chunks and embeddings. The source is only read. The target takes the
// source's model and dimension; fallbackModel is used if the source does not record one.
func Migrate(ctx context.Context, fromURL, toURL, fallbackModel string) (MigrateResult, error) {
	var res MigrateResult
	src, err := OpenReadOnly(ctx, fromURL, 0, "")
	if err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	if res.Dim, res.Model, err = src.CurrentEmbedding(ctx); err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	if res.Model == "" {
		res.Model = fallbackModel
	}
	dst, err := Open(ctx, toURL, res.Dim, res.Model)
	if err != nil {
		return res, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()
	from, to := src.(migrator), dst.(migrator)
	if n, _, err := to.counts(ctx); err != nil {
		return res, fmt.Errorf("target: %w", err)
	} else if n > 0 {
		return res, fmt.Errorf("the target already contains %d entries; migrate only into an empty database", n)
	}

	var docs []exportDoc
	if err := from.exportAll(ctx, func(d exportDoc) error {
		for i, c := range d.Chunks {
			if len(c.Embedding) != res.Dim {
				return fmt.Errorf("entry %d chunk %d has dimension %d, expected %d", d.ID, i, len(c.Embedding), res.Dim)
			}
		}
		docs = append(docs, d)
		return nil
	}); err != nil {
		return res, fmt.Errorf("reading the source: %w", err)
	}
	if err := to.importAll(ctx, docs); err != nil {
		return res, fmt.Errorf("writing the target: %w", err)
	}

	sd, sc, err := from.counts(ctx)
	if err != nil {
		return res, err
	}
	if res.Docs, res.Chunks, err = to.counts(ctx); err != nil {
		return res, err
	}
	if sd != res.Docs || sc != res.Chunks {
		return res, fmt.Errorf("verification failed: source has %d entries / %d chunks, target %d / %d", sd, sc, res.Docs, res.Chunks)
	}
	return res, nil
}

// --- Postgres ---

func (s *pgStore) exportAll(ctx context.Context, fn func(exportDoc) error) error {
	rows, err := s.pool.Query(ctx, `
SELECT d.id, d.title, COALESCE(d.source, ''), COALESCE(d.project, ''), d.kind, d.status, d.superseded_by,
       d.tags, d.created_at, d.updated_at
FROM documents d ORDER BY d.id`)
	if err != nil {
		return err
	}
	var docs []exportDoc
	for rows.Next() {
		var d exportDoc
		if err := rows.Scan(&d.ID, &d.Title, &d.Source, &d.Project, &d.Kind, &d.Status, &d.SupersededBy,
			&d.Tags, &d.CreatedAt, &d.UpdatedAt); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range docs {
		crows, err := s.pool.Query(ctx, `SELECT content, embedding::text FROM chunks WHERE document_id = $1 ORDER BY idx`, d.ID)
		if err != nil {
			return err
		}
		for crows.Next() {
			var c ChunkInput
			var vec string
			if err := crows.Scan(&c.Content, &vec); err != nil {
				crows.Close()
				return err
			}
			if c.Embedding, err = parseVector(vec); err != nil {
				crows.Close()
				return fmt.Errorf("entry %d: %w", d.ID, err)
			}
			d.Chunks = append(d.Chunks, c)
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return err
		}
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

func (s *pgStore) importAll(ctx context.Context, docs []exportDoc) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, d := range docs {
		tags := d.Tags
		if tags == nil {
			tags = []string{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO documents (id, title, source, project, kind, status, tags, created_at, updated_at)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			d.ID, d.Title, nilIfEmpty(d.Source), nilIfEmpty(d.Project), d.Kind, d.Status, tags, d.CreatedAt, d.UpdatedAt); err != nil {
			return fmt.Errorf("entry %d: %w", d.ID, err)
		}
		for i, c := range d.Chunks {
			if _, err := tx.Exec(ctx, `INSERT INTO chunks (document_id, idx, content, embedding) VALUES ($1, $2, $3, $4::vector)`,
				d.ID, i, c.Content, vectorLiteral(c.Embedding)); err != nil {
				return fmt.Errorf("entry %d chunk %d: %w", d.ID, i, err)
			}
		}
	}
	for _, d := range docs {
		if d.SupersededBy != nil {
			if _, err := tx.Exec(ctx, `UPDATE documents SET superseded_by = $2 WHERE id = $1`, d.ID, *d.SupersededBy); err != nil {
				return err
			}
		}
	}
	// New entries continue after the highest imported ID.
	if _, err := tx.Exec(ctx, `SELECT setval(pg_get_serial_sequence('documents', 'id'), COALESCE(max(id), 0) + 1, false) FROM documents`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *pgStore) counts(ctx context.Context) (docs, chunks int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM documents), (SELECT count(*) FROM chunks)`).Scan(&docs, &chunks)
	return docs, chunks, err
}

// --- SQLite ---

func (s *sqliteStore) exportAll(ctx context.Context, fn func(exportDoc) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, COALESCE(source, ''), COALESCE(project, ''), kind, status,
		superseded_by, tags, created_at, updated_at FROM documents ORDER BY id`)
	if err != nil {
		return err
	}
	var docs []exportDoc
	for rows.Next() {
		var d exportDoc
		var sup sql.NullInt64
		var tags, created, updated string
		if err := rows.Scan(&d.ID, &d.Title, &d.Source, &d.Project, &d.Kind, &d.Status, &sup, &tags, &created, &updated); err != nil {
			rows.Close()
			return err
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
			return fmt.Errorf("entry %d: %w", d.ID, err)
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range docs {
		crows, err := s.db.QueryContext(ctx, `SELECT content, embedding FROM chunks WHERE document_id = ? ORDER BY idx`, d.ID)
		if err != nil {
			return err
		}
		for crows.Next() {
			var c ChunkInput
			var blob []byte
			if err := crows.Scan(&c.Content, &blob); err != nil {
				crows.Close()
				return err
			}
			c.Embedding = decodeVector(blob)
			d.Chunks = append(d.Chunks, c)
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return err
		}
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteStore) importAll(ctx context.Context, docs []exportDoc) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, d := range docs {
		tags, err := encodeTags(d.Tags)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO documents (id, title, source, project, kind, status, tags, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ID, d.Title, nilIfEmpty(d.Source), nilIfEmpty(d.Project), d.Kind, d.Status, tags,
			d.CreatedAt.UTC().Format(timeLayout), d.UpdatedAt.UTC().Format(timeLayout)); err != nil {
			return fmt.Errorf("entry %d: %w", d.ID, err)
		}
		for i, c := range d.Chunks {
			if _, err := tx.ExecContext(ctx, `INSERT INTO chunks (document_id, idx, content, embedding) VALUES (?, ?, ?, ?)`,
				d.ID, i, c.Content, encodeVector(c.Embedding)); err != nil {
				return fmt.Errorf("entry %d chunk %d: %w", d.ID, i, err)
			}
		}
	}
	for _, d := range docs {
		if d.SupersededBy != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE documents SET superseded_by = ? WHERE id = ?`, *d.SupersededBy, d.ID); err != nil {
				return err
			}
		}
	}
	// AUTOINCREMENT continues after the highest inserted ID on its own.
	return tx.Commit()
}

func (s *sqliteStore) counts(ctx context.Context) (docs, chunks int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM documents), (SELECT count(*) FROM chunks)`).Scan(&docs, &chunks)
	return docs, chunks, err
}
