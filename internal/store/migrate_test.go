package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// seedForMigration fills s with entries covering everything Migrate has to keep:
// sources, projects, tags, several chunks, the supersede history and an ID gap.
func seedForMigration(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	add := func(in DocumentInput) int64 {
		t.Helper()
		res, err := s.AddDocument(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res.ID
	}
	old := add(DocumentInput{Title: "Old", Project: "p", Kind: "fact", Tags: []string{"a", "b"},
		Chunks: []ChunkInput{{Content: "Old fact.", Embedding: unitVec(0)}}})
	gone := add(DocumentInput{Title: "Gone", Chunks: []ChunkInput{{Content: "Deleted.", Embedding: unitVec(1)}}})
	add(DocumentInput{Title: "New", Project: "p", Kind: "fact", Supersedes: []int64{old},
		Chunks: []ChunkInput{{Content: "New fact with Größe.", Embedding: unitVec(0)}}})
	add(DocumentInput{Title: "Manual", Source: "file:///m.md", Tags: []string{"docs"},
		Chunks: []ChunkInput{{Content: "Part one.", Embedding: unitVec(2)}, {Content: "Part two.", Embedding: unitVec(3)}}})
	if err := s.DeleteDocument(ctx, gone); err != nil {
		t.Fatal(err)
	}
}

// checkMigrated compares source and target entry by entry.
func checkMigrated(t *testing.T, from, to Store, dim int) {
	t.Helper()
	ctx := context.Background()
	a, err := from.ListDocuments(ctx, "", "", true, 100000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := to.ListDocuments(ctx, "", "", true, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("entries: source %d, target %d", len(a), len(b))
	}
	byID := map[int64]DocumentInfo{}
	for _, d := range b {
		byID[d.ID] = d
	}
	for _, want := range a {
		got, ok := byID[want.ID]
		if !ok {
			t.Errorf("entry %d is missing in the target", want.ID)
			continue
		}
		// Postgres keeps microseconds, SQLite nanoseconds.
		if !got.UpdatedAt.Truncate(time.Microsecond).Equal(want.UpdatedAt.Truncate(time.Microsecond)) {
			t.Errorf("entry %d: updated_at %v, want %v", want.ID, got.UpdatedAt, want.UpdatedAt)
		}
		got.UpdatedAt, want.UpdatedAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("entry %d differs:\n got %+v\nwant %+v", want.ID, got, want)
		}
		ga, _ := from.GetDocument(ctx, want.ID)
		gb, _ := to.GetDocument(ctx, want.ID)
		if ga == nil || gb == nil || ga.Content != gb.Content {
			t.Errorf("entry %d: content differs", want.ID)
		}
	}
	// The vectors arrive unchanged: the vector-only ranking is the same.
	q := make([]float32, dim)
	for i := range q {
		q[i] = float32(i%5) - 2
	}
	ha, _ := from.Search(ctx, "zzzzqq", q, "", nil, 10)
	hb, _ := to.Search(ctx, "zzzzqq", q, "", nil, 10)
	if len(ha) == 0 || len(ha) != len(hb) {
		t.Fatalf("search: source %d hits, target %d", len(ha), len(hb))
	}
	for i := range ha {
		if ha[i].DocumentID != hb[i].DocumentID || ha[i].ChunkIndex != hb[i].ChunkIndex || abs(ha[i].Sim-hb[i].Sim) > 1e-5 {
			t.Errorf("search hit %d: source %d/%d (%.6f), target %d/%d (%.6f)", i,
				ha[i].DocumentID, ha[i].ChunkIndex, ha[i].Sim, hb[i].DocumentID, hb[i].ChunkIndex, hb[i].Sim)
		}
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestMigrateSQLiteToSQLite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fromURL, toURL := "sqlite://"+filepath.Join(dir, "a.db"), "sqlite://"+filepath.Join(dir, "b.db")
	src, err := Open(ctx, fromURL, testDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	seedForMigration(t, src)

	res, err := Migrate(ctx, fromURL, toURL, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if res.Docs != 3 || res.Chunks != 4 || res.Model != "m" || res.Dim != testDim {
		t.Errorf("result %+v", res)
	}
	dst, err := Open(ctx, toURL, testDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	checkMigrated(t, src, dst, testDim)

	// New entries in the target continue after the highest migrated ID (the deleted ID
	// 2 is not reused, nor is any other).
	r, err := dst.AddDocument(ctx, DocumentInput{Title: "after", Chunks: []ChunkInput{{Content: "x", Embedding: unitVec(4)}}})
	if err != nil || r.ID != 5 {
		t.Errorf("first new ID after migration = %d (err %v), want 5", r.ID, err)
	}
	if _, err := Migrate(ctx, fromURL, toURL, ""); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("migrating into a non-empty database: %v", err)
	}
}
