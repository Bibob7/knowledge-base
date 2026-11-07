package store

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSQLite(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "sub", "knowledge.db")
	runContract(t, func(ctx context.Context, dim int, model string) (Store, error) {
		return Open(ctx, url, dim, model)
	})
	path, _ := sqlitePath(url)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("database file mode %o, want 600", perm)
	}
}

func TestSQLiteReadOnlyNeedsExistingDatabase(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(t.TempDir(), "knowledge.db")
	if _, err := OpenReadOnly(ctx, url, testDim, "m"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("OpenReadOnly on a missing file: %v", err)
	}
	s, err := Open(ctx, url, testDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	ro, err := OpenReadOnly(ctx, url, testDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	// Extraction writes through a store opened this way.
	if _, err := ro.AddDocument(ctx, DocumentInput{Title: "x", Chunks: []ChunkInput{{Content: "x", Embedding: unitVec(0)}}}); err != nil {
		t.Fatal(err)
	}
}

// Several processes (MCP servers of parallel sessions, hooks, extraction) use the same
// file; here several connections write at the same time.
func TestSQLiteConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(t.TempDir(), "knowledge.db")
	stores := make([]Store, 4)
	for i := range stores {
		s, err := Open(ctx, url, testDim, "m") // parallel opens migrate one after the other
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		stores[i] = s
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := s.AddDocument(ctx, DocumentInput{Title: "t", Project: "p",
					Chunks: []ChunkInput{{Content: "concurrent", Embedding: unitVec(i + j)}}}); err != nil {
					errs <- err
					return
				}
				if _, err := s.Search(ctx, "concurrent", unitVec(j), "p", nil, 3); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	snap, err := stores[0].Snapshot(ctx, 1)
	if err != nil || snap.Total != 100 {
		t.Errorf("entries: %d, err %v", snap.Total, err)
	}
}

func TestSQLiteRejectsOtherDimension(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(t.TempDir(), "knowledge.db")
	s, err := Open(ctx, url, testDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, url, testDim+1, "m"); err == nil || !strings.Contains(err.Error(), "reindex") {
		t.Fatalf("other dimension: %v", err)
	}
}

func TestFTSQuery(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"   ":                    "",
		"kangaroo":               `("kangaroo")`,
		"Größe der Datenbank":    `("Größe" AND "der" AND "Datenbank")`,
		`"exact phrase" word`:    `("exact phrase" AND "word")`,
		"cats or dogs":           `("cats") OR ("dogs")`,
		"a b OR c":               `("a" AND "b") OR ("c")`,
		"or alone":               `("alone")`,
		"pets -cats":             `("pets" NOT "cats")`,
		"-only":                  "",
		"gamma-5 x":              `("gamma 5" AND "x")`,
		`NEAR(a b) "unclosed`:    `("NEAR a" AND "b" AND "unclosed")`,
		`col:val * ^x AND`:       `("col val" AND "x" AND "AND")`,
		"what's up?":             `("what s" AND "up")`,
		`"" ... !!`:              "",
		"trailing or":            `("trailing")`,
		"mcp__plugin_kb__search": `("mcp plugin kb search")`,
	} {
		if got := ftsQuery(in); got != want {
			t.Errorf("ftsQuery(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSQLitePath(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"sqlite:///tmp/kb/knowledge.db":  "/tmp/kb/knowledge.db",
		"sqlite:/tmp/kb/../knowledge.db": "/tmp/knowledge.db",
		"sqlite:~/kb.db":                 filepath.Join(home, "kb.db"),
		"sqlite://relative.db":           "",
		"sqlite:///tmp/x.db?mode=ro":     "",
		"sqlite:":                        "",
	} {
		got, err := sqlitePath(in)
		if want == "" {
			if err == nil {
				t.Errorf("sqlitePath(%q) = %q, want an error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("sqlitePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// BenchmarkSQLiteHookSearch measures what the retrieval hook does per prompt: open the
// database (a new process each time) and run one project-scoped search, on 10,000
// chunks with 1024 dimensions (bge-m3).
//
//	go test -run '^$' -bench SQLiteHookSearch ./internal/store
func BenchmarkSQLiteHookSearch(b *testing.B) {
	const dim, docs = 1024, 10000
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(b.TempDir(), "knowledge.db")
	s, err := Open(ctx, url, dim, "m")
	if err != nil {
		b.Fatal(err)
	}
	r := rand.New(rand.NewSource(1))
	vec := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = float32(r.NormFloat64())
		}
		return v
	}
	for i := 0; i < docs; i++ {
		if _, err := s.AddDocument(ctx, DocumentInput{Title: "t", Project: fmt.Sprintf("p%d", i%20),
			Chunks: []ChunkInput{{Content: fmt.Sprintf("note %d about some topic in the knowledge base", i), Embedding: vec()}}}); err != nil {
			b.Fatal(err)
		}
	}
	s.Close()
	q := vec()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ro, err := OpenReadOnly(ctx, url, dim, "m")
		if err != nil {
			b.Fatal(err)
		}
		// Global entries make up a share of every project's scope; here 1/20 of the base
		// plus the project itself, the worst case is a search without project.
		if _, err := ro.Search(ctx, "topic knowledge", q, "", nil, 8); err != nil {
			b.Fatal(err)
		}
		ro.Close()
	}
}

// A process that opened the database before a reindex (an MCP server of an older session,
// the background extraction) must not write vectors of its model into it afterwards.
func TestSQLiteRefusesWritesAfterModelChange(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(t.TempDir(), "knowledge.db")
	s, err := Open(ctx, url, testDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	doc := DocumentInput{Title: "x", Chunks: []ChunkInput{{Content: "x", Embedding: unitVec(0)}}}
	for model, ok := range map[string]bool{"m": true, "m:latest": true, "": true, "other": false} {
		ro, err := OpenReadOnly(ctx, url, testDim, model)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ro.AddDocument(ctx, doc)
		ro.Close()
		if ok != (err == nil) || (!ok && !strings.Contains(err.Error(), "nothing was stored")) {
			t.Errorf("write with model %q: %v", model, err)
		}
	}
	// The long-running server notices a reindex done by another process.
	other, err := OpenReadOnly(ctx, url, testDim, "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Reindex(ctx, ReindexOptions{Model: "new", Dim: testDim, Batch: 10,
		Embed: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range out {
				out[i] = unitVec(1)
			}
			return out, nil
		}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDocument(ctx, doc); err == nil {
		t.Error("the server opened with the old model must refuse to write after the reindex")
	}
}
