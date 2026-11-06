package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// opener opens the backend under test on the same (initially empty) database each
// time it is called.
type opener func(ctx context.Context, dim int, model string) (Store, error)

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

func unitVec(i int) []float32 {
	v := make([]float32, testDim)
	v[i%testDim] = 1
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

// runContract checks the behaviour every Store implementation must have. It leaves
// the database re-embedded with model "other-model" (24 dimensions).
func runContract(t *testing.T, open opener) {
	ctx := context.Background()
	s, err := open(ctx, testDim, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()

	t.Run("basics", func(t *testing.T) { contractBasics(t, s) })
	all := contractScopedSearch(t, s)

	// A different model must be refused until the data is re-embedded.
	if other, err := open(ctx, testDim, "other-model"); err == nil || !strings.Contains(err.Error(), "reindex") {
		if other != nil {
			other.Close()
		}
		t.Fatalf("opening with another model must fail with a reindex hint, got %v", err)
	}

	// Reindex to a different dimension, with a fake embedder.
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

	s, err = open(ctx, newDim, "other-model:latest")
	if err != nil {
		t.Fatalf("open after reindex: %v", err)
	}
	dim, model, err := s.CurrentEmbedding(ctx)
	if err != nil || dim != newDim || model != "other-model" {
		t.Errorf("CurrentEmbedding = %d, %q, %v", dim, model, err)
	}
	qv, _ := embed(ctx, []string{"doc: note gamma 5 about topic"})
	hits, err := s.Search(ctx, "gamma 5", qv[0], "gamma", nil, 3)
	if err != nil || len(hits) == 0 || !strings.Contains(hits[0].Content, "gamma 5") {
		t.Errorf("search after reindex: %+v, err %v", hits, err)
	}
	snap, err := s.Snapshot(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(all) + 2; snap.Total != want { // + the two superseded entries kept from "basics"
		t.Errorf("entries after reindex: %d, want %d", snap.Total, want)
	}
	if len(snap.Docs) != 5 || len(snap.Docs[0].Vec) != newDim {
		t.Errorf("snapshot after reindex: %d docs, first vector %d dims", len(snap.Docs), len(snap.Docs[0].Vec))
	}
}

func contractBasics(t *testing.T, s Store) {
	ctx := context.Background()
	const p = "basics"
	add := func(in DocumentInput) AddResult {
		t.Helper()
		in.Project = p
		res, err := s.AddDocument(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	kanga := add(DocumentInput{Title: "Kangaroo", Kind: "fact", Tags: []string{"animals"},
		Chunks: []ChunkInput{{Content: "The kangaroo carries its young in a pouch.", Embedding: unitVec(0)}}})
	umlaut := add(DocumentInput{Title: "Größe", Kind: "fact", Tags: []string{"db", "sizes"},
		Chunks: []ChunkInput{{Content: "Die Größe der Datenbank beträgt zehn Megabyte.", Embedding: unitVec(1)}}})
	doc := add(DocumentInput{Title: "Manual", Source: "file:///manual.md", Tags: []string{"docs"},
		Chunks: []ChunkInput{
			{Content: "First part of the manual.", Embedding: unitVec(2)},
			{Content: "Second part of the manual.", Embedding: unitVec(3)},
		}})
	if doc.Replaced {
		t.Error("first ingest of a source must not report a replacement")
	}

	// Full text finds the keyword although the query vector points elsewhere.
	for query, want := range map[string]int64{"kangaroo": kanga.ID, "größe": umlaut.ID, "GRÖSSE datenbank": 0} {
		hits, err := s.Search(ctx, query, unitVec(5), p, nil, 5)
		if err != nil {
			t.Fatal(err)
		}
		if want != 0 && (len(hits) == 0 || hits[0].DocumentID != want) {
			t.Errorf("search %q: top hit %+v, want document %d", query, hits, want)
		}
	}
	// The vector side alone finds the closest entry too.
	hits, err := s.Search(ctx, "zzzzqq", unitVec(1), p, nil, 5)
	if err != nil || len(hits) == 0 || hits[0].DocumentID != umlaut.ID || hits[0].Sim < 0.99 {
		t.Errorf("vector search: %+v, err %v", hits, err)
	}
	if h := hits[0]; h.Title != "Größe" || h.Project != p || h.Kind != "fact" || strings.Join(h.Tags, ",") != "db,sizes" {
		t.Errorf("hit fields: %+v", h)
	}
	// A tag filter keeps only entries with one of the tags.
	hits, err = s.Search(ctx, "manual kangaroo", unitVec(2), p, []string{"animals", "nope"}, 10)
	if err != nil || len(hits) != 1 || hits[0].DocumentID != kanga.ID {
		t.Errorf("tag filter: %+v, err %v", hits, err)
	}

	got, err := s.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Chunks != 2 || got.Source != "file:///manual.md" || got.Content != "First part of the manual.\n\n[...]\n\nSecond part of the manual." {
		t.Errorf("GetDocument: %+v", got)
	}

	// Re-ingesting the same source replaces the document.
	doc2 := add(DocumentInput{Title: "Manual v2", Source: "file:///manual.md",
		Chunks: []ChunkInput{{Content: "The rewritten manual.", Embedding: unitVec(2)}}})
	if !doc2.Replaced {
		t.Error("re-ingest must report the replacement")
	}
	if _, err := s.GetDocument(ctx, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("replaced document must be gone, got %v", err)
	}

	// Superseding hides the old entry from search and Similar but keeps it as history.
	newer := add(DocumentInput{Title: "Kangaroo 2", Kind: "fact", Supersedes: []int64{kanga.ID},
		Chunks: []ChunkInput{{Content: "Female kangaroos carry their young in a pouch.", Embedding: unitVec(0)}}})
	if len(newer.Superseded) != 1 || newer.Superseded[0] != kanga.ID {
		t.Errorf("Superseded = %v, want [%d]", newer.Superseded, kanga.ID)
	}
	hits, _ = s.Search(ctx, "kangaroo", unitVec(0), p, nil, 5)
	for _, h := range hits {
		if h.DocumentID == kanga.ID {
			t.Error("superseded entry is still found by Search")
		}
	}
	sims, err := s.Similar(ctx, unitVec(0), p, []int64{}, 5)
	if err != nil || len(sims) == 0 || sims[0].ID != newer.ID || sims[0].Similarity < 0.99 {
		t.Errorf("Similar: %+v, err %v", sims, err)
	}
	for _, e := range sims {
		if e.ID == kanga.ID {
			t.Error("superseded entry is still returned by Similar")
		}
	}
	if sims, _ := s.Similar(ctx, unitVec(0), p, []int64{newer.ID}, 5); len(sims) > 0 && sims[0].ID == newer.ID {
		t.Error("Similar ignores exclude")
	}
	old, err := s.GetDocument(ctx, kanga.ID)
	if err != nil || old.Status != "superseded" || old.SupersededBy == nil || *old.SupersededBy != newer.ID {
		t.Errorf("superseded entry: %+v, err %v", old, err)
	}

	list, err := s.ListDocuments(ctx, "", p, false, 50)
	if err != nil || len(list) != 3 {
		t.Errorf("ListDocuments active: %d entries, err %v", len(list), err)
	}
	list, _ = s.ListDocuments(ctx, "", p, true, 50)
	if len(list) != 4 {
		t.Errorf("ListDocuments with history: %d entries, want 4", len(list))
	}
	list, _ = s.ListDocuments(ctx, "sizes", p, false, 50)
	if len(list) != 1 || list[0].ID != umlaut.ID || list[0].Chunks != 1 || list[0].UpdatedAt.IsZero() {
		t.Errorf("ListDocuments by tag: %+v", list)
	}

	// Supersede: the replacing entry must exist and be active.
	if _, err := s.Supersede(ctx, []int64{umlaut.ID}, kanga.ID); err == nil {
		t.Error("superseding with an inactive entry must fail")
	}
	if _, err := s.Supersede(ctx, []int64{umlaut.ID}, 1<<40); !errors.Is(err, ErrNotFound) {
		t.Errorf("superseding with a missing entry: %v, want ErrNotFound", err)
	}
	ids, err := s.Supersede(ctx, []int64{umlaut.ID, kanga.ID}, newer.ID)
	if err != nil || len(ids) != 1 || ids[0] != umlaut.ID {
		t.Errorf("Supersede = %v, %v; want only the active entry %d", ids, err, umlaut.ID)
	}

	if err := s.DeleteDocument(ctx, doc2.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDocument(ctx, doc2.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}

	snap, err := s.Snapshot(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Total != 3 || len(snap.Docs) != 3 || snap.Docs[0].ID != newer.ID || snap.Model != "test-model" || snap.DBBytes <= 0 {
		t.Errorf("Snapshot: total %d, %d docs, model %q, %d bytes", snap.Total, len(snap.Docs), snap.Model, snap.DBBytes)
	}
	if d := snap.Docs[0]; len(d.Vec) != testDim || cosine(d.Vec, unitVec(0)) < 0.99 || !strings.Contains(d.Content, "Female") {
		t.Errorf("Snapshot doc: %+v", d)
	}
	// Only "Kangaroo 2" is still active in this project; the history stays for the rest.
	if list, _ := s.ListDocuments(ctx, "", p, false, 50); len(list) != 1 {
		t.Errorf("active entries after the basics: %d, want 1", len(list))
	}
	if err := s.DeleteDocument(ctx, newer.ID); err != nil {
		t.Fatal(err)
	}
}

// contractScopedSearch checks that project-scoped search is exact (no recall loss from
// an approximate index) and returns the seeded entries.
func contractScopedSearch(t *testing.T, s Store) []seeded {
	ctx := context.Background()
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
	// and its top hits must equal the brute-force nearest neighbours.
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

	// Unscoped search still returns a full result.
	if hits, err = s.Search(ctx, "zzzzqq", q, "", nil, 8); err != nil || len(hits) != 8 {
		t.Fatalf("unscoped search: %d hits, err %v", len(hits), err)
	}
	return all
}
