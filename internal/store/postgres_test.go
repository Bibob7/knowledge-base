package store

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPostgres runs the contract against a real Postgres with pgvector. It needs a
// DEDICATED database whose name contains "test"; its tables are dropped first. E.g.:
//
//	docker exec knowledge-base-postgres psql -U kb -d postgres -c 'CREATE DATABASE kb_test'
//	KB_TEST_DATABASE_URL='postgres://kb:kb@localhost:5433/kb_test?sslmode=disable' go test ./internal/store
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("KB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KB_TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.Contains(u.Path, "test") {
		t.Fatalf("KB_TEST_DATABASE_URL must name a dedicated test database (name containing \"test\"), got path %q", u.Path)
	}
	ctx := context.Background()
	reset, err := openPostgresReadOnly(ctx, dsn, testDim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reset.pool.Exec(ctx, `DROP TABLE IF EXISTS chunk_reindex, chunks, documents, kb_meta CASCADE`); err != nil {
		t.Fatal(err)
	}
	reset.Close()

	runContract(t, func(ctx context.Context, dim int, model string) (Store, error) {
		return Open(ctx, dsn, dim, model)
	})

	s, err := openPostgresReadOnly(ctx, dsn, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'chunk_reindex'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("staging table must be gone after reindex (found %d, err %v)", n, err)
	}

	// Round trip of what the contract left behind (~1,800 entries with history):
	// Postgres -> SQLite, then back into an emptied Postgres.
	t.Run("migrate", func(t *testing.T) {
		const dim, model = 24, "other-model"
		sqliteURL := "sqlite://" + filepath.Join(t.TempDir(), "knowledge.db")
		if _, err := Migrate(ctx, dsn, sqliteURL, ""); err != nil {
			t.Fatal(err)
		}
		pg, err := Open(ctx, dsn, dim, model)
		if err != nil {
			t.Fatal(err)
		}
		lite, err := Open(ctx, sqliteURL, dim, model)
		if err != nil {
			t.Fatal(err)
		}
		defer lite.Close()
		checkMigrated(t, pg, lite, dim)
		pg.Close()

		if _, err := s.pool.Exec(ctx, `DROP TABLE IF EXISTS chunk_reindex, chunks, documents, kb_meta CASCADE`); err != nil {
			t.Fatal(err)
		}
		res, err := Migrate(ctx, sqliteURL, dsn, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Model != model || res.Dim != dim {
			t.Errorf("result %+v", res)
		}
		pg, err = Open(ctx, dsn, dim, model)
		if err != nil {
			t.Fatal(err)
		}
		defer pg.Close()
		checkMigrated(t, lite, pg, dim)
		var maxID int64
		if err := s.pool.QueryRow(ctx, `SELECT max(id) FROM documents`).Scan(&maxID); err != nil {
			t.Fatal(err)
		}
		r, err := pg.AddDocument(ctx, DocumentInput{Title: "after", Chunks: []ChunkInput{{Content: "x", Embedding: make([]float32, dim)}}})
		if err != nil || r.ID != maxID+1 {
			t.Errorf("first new ID after migration = %d (err %v), want %d", r.ID, err, maxID+1)
		}
	})
}
