package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// maxIndexedDim is the largest dimension pgvector can index with the plain vector type.
const maxIndexedDim = 2000

// CurrentEmbedding reports the dimension of the stored vectors and the model they
// were created with (empty if unknown).
func (s *pgStore) CurrentEmbedding(ctx context.Context) (dim int, model string, err error) {
	if err = s.pool.QueryRow(ctx, `SELECT atttypmod FROM pg_attribute
		WHERE attrelid = 'chunks'::regclass AND attname = 'embedding'`).Scan(&dim); err != nil {
		return 0, "", err
	}
	// kb_meta may not exist yet on databases from before the model check.
	if _, err = s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS kb_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return 0, "", err
	}
	err = s.pool.QueryRow(ctx, `SELECT value FROM kb_meta WHERE key = 'embedding_model'`).Scan(&model)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return dim, model, err
}

// Reindex re-embeds every chunk (including those of superseded entries) with the
// configured model and switches the vector column to the new dimension. IDs, history
// and all other data stay untouched.
//
// New vectors are first written to a staging table, so an interrupted run loses
// nothing and can simply be started again. Only the final swap changes the chunks
// table, in a single transaction.
func (s *pgStore) Reindex(ctx context.Context, o ReindexOptions) error {
	if o.Dim <= 0 || o.Dim > maxIndexedDim {
		return fmt.Errorf("dimension %d cannot be indexed (supported: 1-%d; larger models would need halfvec)", o.Dim, maxIndexedDim)
	}
	if o.Batch <= 0 {
		o.Batch = 32
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if _, _, err := s.CurrentEmbedding(ctx); err != nil {
		return err
	}

	// A staging table from an aborted run is only reusable for the same model and dimension.
	target := fmt.Sprintf("%s|%d", normalizeModel(o.Model), o.Dim)
	var prev string
	err := s.pool.QueryRow(ctx, `SELECT value FROM kb_meta WHERE key = 'reindex_target'`).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if prev != target {
		if _, err := s.pool.Exec(ctx, `DROP TABLE IF EXISTS chunk_reindex`); err != nil {
			return err
		}
	}
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS chunk_reindex (chunk_id BIGINT PRIMARY KEY, embedding vector(%d) NOT NULL)`, o.Dim)); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO kb_meta (key, value) VALUES ('reindex_target', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, target); err != nil {
		return err
	}

	// Phase 1: embed everything that is not staged yet (repeat until nothing is missing,
	// which also catches chunks added while this was running).
	for {
		rows, err := s.pool.Query(ctx, `SELECT c.id, c.content FROM chunks c
			WHERE NOT EXISTS (SELECT 1 FROM chunk_reindex r WHERE r.chunk_id = c.id)
			ORDER BY c.id LIMIT $1`, o.Batch)
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
		batch := &pgx.Batch{}
		for i, id := range ids {
			if len(vecs[i]) != o.Dim {
				return fmt.Errorf("model returned dimension %d, expected %d", len(vecs[i]), o.Dim)
			}
			batch.Queue(`INSERT INTO chunk_reindex (chunk_id, embedding) VALUES ($1, $2::vector)
				ON CONFLICT (chunk_id) DO NOTHING`, id, vectorLiteral(vecs[i]))
		}
		br := s.pool.SendBatch(ctx, batch)
		for range ids {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return err
			}
		}
		if err := br.Close(); err != nil {
			return err
		}
		var staged int
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM chunk_reindex`).Scan(&staged)
		o.Logf("embedded %d chunks so far", staged)
	}

	// Phase 2: swap the column atomically.
	o.Logf("switching the vector column to %d dimensions and rebuilding the index", o.Dim)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	steps := []string{
		`LOCK TABLE chunks IN ACCESS EXCLUSIVE MODE`,
		`DROP INDEX IF EXISTS chunks_embedding_idx`,
		`ALTER TABLE chunks DROP COLUMN embedding`,
		fmt.Sprintf(`ALTER TABLE chunks ADD COLUMN embedding vector(%d)`, o.Dim),
		`UPDATE chunks c SET embedding = r.embedding FROM chunk_reindex r WHERE r.chunk_id = c.id`,
	}
	for _, q := range steps {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	var missing int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE embedding IS NULL`).Scan(&missing); err != nil {
		return err
	}
	if missing > 0 { // chunks added between phase 1 and the lock: rolled back, simply run again
		return fmt.Errorf("%d chunks were added during the swap; nothing was changed, please run reindex again", missing)
	}
	for _, q := range []string{
		`ALTER TABLE chunks ALTER COLUMN embedding SET NOT NULL`,
		`CREATE INDEX chunks_embedding_idx ON chunks USING hnsw (embedding vector_cosine_ops)`,
		`INSERT INTO kb_meta (key, value) VALUES ('embedding_model', '` + sqlQuote(normalizeModel(o.Model)) + `')
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		`DELETE FROM kb_meta WHERE key = 'reindex_target'`,
		`DROP TABLE chunk_reindex`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.dim, s.model = o.Dim, normalizeModel(o.Model)
	o.Logf("done")
	return nil
}

func sqlQuote(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(out)
}
