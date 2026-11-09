package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bibob7/knowledge-base/internal/embed"
	"github.com/Bibob7/knowledge-base/internal/store"
)

const testDim = 4

// constEmbed answers every text with the same vector, so every stored entry with that
// vector is a perfect match.
func constEmbed(t *testing.T) *embed.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []item `json:"data"`
		}
		for i := range req.Input {
			out.Data = append(out.Data, item{i, []float32{1, 0, 0, 0}})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return embed.New(srv.URL, "m", "", testDim)
}

func TestPromptHookCountsInjectedEntries(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // the "already shown" list lives in the temp dir
	t.Setenv("KB_PROJECT", "app")
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "kb.db"), testDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := st.AddDocument(ctx, store.DocumentInput{Title: "commits", Project: "app", Kind: "fact",
		Chunks: []store.ChunkInput{{Content: "Commits use the Conventional Commits format.", Embedding: []float32{1, 0, 0, 0}}}})
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Store: st, Embed: constEmbed(t), MinSim: 0.5}
	in := `{"session_id":"s1","cwd":"/nowhere","hook_event_name":"UserPromptSubmit","prompt":"How do I write the commit message?"}`

	var out bytes.Buffer
	if err := Run(ctx, strings.NewReader(in), &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Conventional Commits") {
		t.Fatalf("entry not injected: %s", out.String())
	}
	doc, err := st.GetDocument(ctx, res.ID)
	if err != nil || doc.Usage.Injected != 1 || doc.Usage.Retrieved != 0 {
		t.Errorf("usage after injection: %+v, err %v", doc.Usage, err)
	}

	// Already shown in this session: not injected again, not counted again.
	out.Reset()
	if err := Run(ctx, strings.NewReader(in), &out, d); err != nil {
		t.Fatal(err)
	}
	if doc, _ := st.GetDocument(ctx, res.ID); out.Len() != 0 || doc.Usage.Injected != 1 {
		t.Errorf("second prompt: output %q, usage %+v", out.String(), doc.Usage)
	}
}
