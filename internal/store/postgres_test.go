package store

import (
	"context"
	"net/url"
	"os"
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
}
