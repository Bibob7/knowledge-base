package store

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
)

// Integration test against a real Postgres with pgvector. It needs a DEDICATED, empty
// database, e.g.:
//
//	docker exec knowledge-base-postgres psql -U kb -d postgres -c 'CREATE DATABASE kb_test'
//	KB_TEST_DATABASE_URL='postgres://kb:kb@localhost:5433/kb_test?sslmode=disable' go test ./internal/store
func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("KB_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KB_TEST_DATABASE_URL not set")
	}
	return url
}

const testDim = 16

type seeded struct {
	project string
	docID   int64
	vec     []float32
}

func randVec(r *rand.Rand, center []float32, noise float64) []float32 {
	v := make([]float32, len(center))
	for i := range v {
		v[i] = center[i] + float32((r.Float64()-0.5)*noise)
	}
	return v
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i] * b[i])
		na += float64(a[i] * a[i])
		nb += float64(b[i] * b[i])
	}
	return dot / math.Sqrt(na*nb)
}

func TestScopedSearchIsExactAndReindexWorks(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	s, err := Open(ctx, url, testDim, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 6 projects with their own topic cluster, 300 entries each, plus some global ones.
	r := rand.New(rand.NewSource(1))
	centers := map[string][]float32{}
	var all []seeded
	for _, p := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", ""} {
		c := make([]float32, testDim)
		for i := range c {
			c[i] = float32(r.NormFloat64())
		}
		centers[p] = c
		n := 300
		if p == "" {
			n = 20
		}
		for i := 0; i < n; i++ {
			v := randVec(r, c, 0.8)
			res, err := s.AddDocument(ctx, DocumentInput{Title: fmt.Sprintf("%s %d", p, i), Project: p, Kind: "fact",
				Chunks: []ChunkInput{{Content: fmt.Sprintf("note %s %d about topic", p, i), Embedding: v}}})
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, seeded{p, res.ID, v})
		}
	}

	// A query inside project "gamma" must return a full result from gamma (or global) only,
	// and its top hit must equal the brute-force nearest neighbour.
	q := randVec(r, centers["gamma"], 0.4)
	// The query text matches no content, so only the vector ranking counts.
	hits, err := s.Search(ctx, "zzzzqq", q, "gamma", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 8 {
		t.Fatalf("scoped search returned %d hits, want 8", len(hits))
	}
	for _, h := range hits {
		if h.Project != "gamma" && h.Project != "" {
			t.Errorf("hit from foreign project %q", h.Project)
		}
	}
	var visible []seeded
	for _, e := range all {
		if e.project == "gamma" || e.project == "" {
			visible = append(visible, e)
		}
	}
	sort.Slice(visible, func(i, j int) bool { return cosine(q, visible[i].vec) > cosine(q, visible[j].vec) })
	got := map[int64]bool{}
	for _, h := range hits {
		got[h.DocumentID] = true
	}
	for i := 0; i < 8; i++ {
		if !got[visible[i].docID] {
			t.Errorf("exact nearest neighbour #%d (doc %d) is missing from the scoped result", i+1, visible[i].docID)
		}
	}

	sims, err := s.Similar(ctx, q, "gamma", nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(sims) != 4 || sims[0].ID != visible[0].docID || sims[0].Similarity < sims[1].Similarity {
		t.Errorf("Similar is not the exact nearest neighbours: %+v (want first %d)", sims, visible[0].docID)
	}

	// Without a project (global facts) Similar must work too and look at everything.
	all4, err := s.Similar(ctx, q, "", nil, 4)
	if err != nil || len(all4) != 4 {
		t.Fatalf("unscoped Similar: %d entries, err %v", len(all4), err)
	}

	// Unscoped search (HNSW with iterative scan) still works and returns a full result.
	if hits, err = s.Search(ctx, "zzzzqq", q, "", nil, 8); err != nil || len(hits) != 8 {
		t.Fatalf("unscoped search: %d hits, err %v", len(hits), err)
	}

	// A different model must be refused until the data is re-embedded.
	if _, err := Open(ctx, url, testDim, "other-model"); err == nil || !strings.Contains(err.Error(), "reindex") {
		t.Fatalf("opening with another model must fail with a reindex hint, got %v", err)
	}

	// Reindex to a different dimension, resumable and with a fake embedder.
	const newDim = 24
	calls := 0
	embed := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		out := make([][]float32, len(texts))
		for i, tx := range texts {
			v := make([]float32, newDim)
			h := 0
			for _, c := range tx {
				h = h*31 + int(c)
			}
			rr := rand.New(rand.NewSource(int64(h)))
			for j := range v {
				v[j] = float32(rr.NormFloat64())
			}
			out[i] = v
		}
		return out, nil
	}
	if err := s.Reindex(ctx, ReindexOptions{Model: "other-model", Dim: newDim, DocPrefix: "doc: ", Batch: 100, Embed: embed}); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Errorf("expected several embedding batches, got %d", calls)
	}
	s.Close()

	s2, err := Open(ctx, url, newDim, "other-model:latest")
	if err != nil {
		t.Fatalf("open after reindex: %v", err)
	}
	defer s2.Close()
	dim, model, err := s2.CurrentEmbedding(ctx)
	if err != nil || dim != newDim || model != "other-model" {
		t.Errorf("CurrentEmbedding = %d, %q, %v", dim, model, err)
	}
	qv, _ := embed(ctx, []string{"doc: note gamma 5 about topic"})
	hits, err = s2.Search(ctx, "gamma 5", qv[0], "gamma", nil, 3)
	if err != nil || len(hits) == 0 || !strings.Contains(hits[0].Content, "gamma 5") {
		t.Errorf("search after reindex: %+v, err %v", hits, err)
	}
	var n int
	if err := s2.pool.QueryRow(ctx, `SELECT count(*) FROM chunks`).Scan(&n); err != nil || n != len(all) {
		t.Errorf("chunks after reindex: %d, want %d (err %v)", n, len(all), err)
	}
	if err := s2.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'chunk_reindex'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("staging table must be gone (found %d, err %v)", n, err)
	}
}
