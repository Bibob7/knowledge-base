package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bibob7/knowledge-base/internal/embed"
	"github.com/Bibob7/knowledge-base/internal/store"
)

const runDim = 8

// fakeEmbedServer answers like an OpenAI-compatible /embeddings endpoint with a vector
// derived from the text, so identical texts get identical vectors.
func fakeEmbedServer(t *testing.T) *embed.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []item `json:"data"`
		}
		for i, s := range req.Input {
			h := fnv.New64a()
			h.Write([]byte(s))
			x := h.Sum64()
			v := make([]float32, runDim)
			for j := range v {
				v[j] = float32(int64(x>>(j*8)&0xff) - 128)
			}
			out.Data = append(out.Data, item{i, v})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return embed.New(srv.URL, "m", "", runDim)
}

// recordingRunner returns one fact per call (a new one each time) and records the inputs.
type recordingRunner struct{ inputs *[]string }

func (r recordingRunner) Run(_ context.Context, _, input string) (string, error) {
	*r.inputs = append(*r.inputs, input)
	return fmt.Sprintf(`{"facts":[{"content":"Learned fact number %d from the session, with enough text.","scope":"project"}]}`, len(*r.inputs)), nil
}

type failingStore struct{ store.Store }

func (failingStore) AddDocument(context.Context, store.DocumentInput) (store.AddResult, error) {
	return store.AddResult{}, errors.New("database is locked")
}

type runEnv struct {
	d      Deps
	path   string
	inputs *[]string
	st     store.Store
}

func newRunEnv(t *testing.T) runEnv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "kb.db"), runDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	var inputs []string
	e := runEnv{st: st, inputs: &inputs, path: filepath.Join(t.TempDir(), "s.jsonl")}
	e.d = Deps{Store: st, Embed: fakeEmbedServer(t), Runner: recordingRunner{&inputs}, StateDir: t.TempDir(),
		ConflictFloor: 0.99, Logf: func(string, ...any) {}}
	return e
}

func userLine(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": text}})
	return string(b) + "\n"
}

func (e runEnv) appendLines(t *testing.T, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(e.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l); err != nil {
			t.Fatal(err)
		}
	}
}

func (e runEnv) run(t *testing.T) error {
	t.Helper()
	return e.d.Run(context.Background(), Options{Transcript: e.path, SessionID: "s1", Project: "p"})
}

var long = strings.Repeat("Details about the build setup and the reasons behind it. ", 8)

func TestRunKeepsSessionPendingWhenStoringFails(t *testing.T) {
	e := newRunEnv(t)
	e.d.Store = failingStore{e.st}
	e.appendLines(t, userLine("first "+long), userLine("second "+long), userLine("third "+long))
	if err := e.run(t); err == nil {
		t.Fatal("a failed write must make the run fail")
	}
	if p := e.d.loadProgress("s1"); p.Evaluated != 0 {
		t.Errorf("progress after a failed write: %+v, the session must stay unevaluated", p)
	}
	// The next run (sweep or a later SessionEnd) evaluates it again and stores the fact.
	e.d.Store = e.st
	if err := e.run(t); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.st.ListDocuments(context.Background(), "", "p", false, 10); len(list) != 1 {
		t.Errorf("entries after the retry: %d, want 1", len(list))
	}
}

func TestRunEvaluatesShortContinuationWithContext(t *testing.T) {
	e := newRunEnv(t)
	e.appendLines(t, userLine("first "+long), userLine("second "+long), userLine("third "+long))
	if err := e.run(t); err != nil || len(*e.inputs) != 1 {
		t.Fatalf("first evaluation: %v, %d model calls", err, len(*e.inputs))
	}
	// Resumed session: one short correction only.
	e.appendLines(t, userLine("No, we deploy with make release."))
	if err := e.run(t); err != nil {
		t.Fatal(err)
	}
	if len(*e.inputs) != 2 {
		t.Fatalf("the continuation was not evaluated (%d model calls)", len(*e.inputs))
	}
	in := (*e.inputs)[1]
	ctxPart, transcript, ok := strings.Cut(in, "<transcript>")
	if !ok || !strings.Contains(ctxPart, "<earlier_context>") || !strings.Contains(ctxPart, "third") {
		t.Errorf("the earlier part must be given as context: %.300q", in)
	}
	if !strings.Contains(transcript, "make release") || strings.Contains(transcript, "first") {
		t.Errorf("the transcript part must hold only the continuation: %.300q", transcript)
	}
	if p := e.d.loadProgress("s1"); p.Evaluated != 4 || p.Lines != 4 {
		t.Errorf("progress: %+v", p)
	}
}

func TestRunEvaluatesShortStartTogetherWithItsContinuation(t *testing.T) {
	e := newRunEnv(t)
	e.appendLines(t, userLine("Please always run the linter before committing."))
	if err := e.run(t); err != nil || len(*e.inputs) != 0 {
		t.Fatalf("a short session must not be evaluated yet: %v, %d calls", err, len(*e.inputs))
	}
	if p := e.d.loadProgress("s1"); p.Lines != 1 || p.Evaluated != 0 {
		t.Errorf("progress of the pending part: %+v", p)
	}
	e.appendLines(t, userLine("second "+long), userLine("third "+long))
	if err := e.run(t); err != nil || len(*e.inputs) != 1 {
		t.Fatalf("second run: %v, %d calls", err, len(*e.inputs))
	}
	if in := (*e.inputs)[0]; !strings.Contains(in, "linter") || strings.Contains(in, "<earlier_context>") {
		t.Errorf("the pending start must be evaluated with the rest: %.300q", in)
	}
}

func TestLoadProgressOfOlderVersions(t *testing.T) {
	d := Deps{StateDir: t.TempDir()}
	if err := os.WriteFile(d.progressPath("old"), []byte(`{"lines":42}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := d.loadProgress("old"); p.Lines != 42 || p.Evaluated != 42 {
		t.Errorf("old progress file: %+v, want everything read counted as evaluated", p)
	}
}

func TestRunWaitsForARunningEvaluation(t *testing.T) {
	old := lockPoll
	lockPoll = 10 * time.Millisecond
	t.Cleanup(func() { lockPoll = old })

	e := newRunEnv(t)
	e.appendLines(t, userLine("first "+long), userLine("second "+long), userLine("third "+long))
	release, ok := acquireLock(e.d.StateDir, "s1", sweepLockStale, time.Now()) // a sweep is busy with it
	if !ok {
		t.Fatal("lock")
	}
	go func() { time.Sleep(50 * time.Millisecond); release() }()
	err := e.d.Run(context.Background(), Options{Transcript: e.path, SessionID: "s1", Project: "p", WaitForLock: time.Second})
	if err != nil || len(*e.inputs) != 1 {
		t.Errorf("SessionEnd must evaluate once the sweep is done: %v, %d calls", err, len(*e.inputs))
	}
}
