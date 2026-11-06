package extract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type env struct {
	root, state string
	d           Deps
	logs        *[]string
}

func newEnv(t *testing.T) env {
	t.Helper()
	e := env{root: t.TempDir(), state: t.TempDir()}
	var logs []string
	e.logs = &logs
	e.d = Deps{StateDir: e.state, Logf: func(f string, a ...any) { logs = append(logs, f) }}
	// The feature "exists" since 10 days before t0.
	touch(filepath.Join(e.state, "sweep.since"), t0.Add(-10*24*time.Hour))
	return e
}

// transcript writes <root>/<project>/<session>.jsonl with the given age.
func (e env) transcript(t *testing.T, session string, age time.Duration, body string) string {
	t.Helper()
	dir := filepath.Join(e.root, "-home-user-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, session+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, t0.Add(-age), t0.Add(-age)); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e env) opts() SweepOptions {
	return SweepOptions{Root: e.root, Idle: 30 * time.Minute, MaxAge: 7 * 24 * time.Hour, Max: 3, Now: func() time.Time { return t0 }}
}

func sessions(c []Candidate) string {
	var s []string
	for _, x := range c {
		s = append(s, x.Session)
	}
	return strings.Join(s, ",")
}

func TestFindCandidatesFilters(t *testing.T) {
	e := newEnv(t)
	e.transcript(t, "live", 5*time.Minute, "{}\n")           // still being written: idle filter
	e.transcript(t, "crashed", 2*time.Hour, "{}\n")          // candidate
	e.transcript(t, "done", 2*time.Hour, "{}\n")             // evaluated after the last change
	touch(e.d.progressPath("done"), t0.Add(-time.Hour))      // progress newer than the transcript
	e.transcript(t, "resumed", time.Hour, "{}\n")            // changed after its last evaluation
	touch(e.d.progressPath("resumed"), t0.Add(-3*time.Hour)) // progress older than the transcript
	e.transcript(t, "ancient", 10*24*time.Hour, "{}\n")      // older than MaxAge
	e.transcript(t, "me", 3*time.Hour, "{}\n")               // the session that triggered the sweep
	e.transcript(t, "failed", 3*time.Hour, "{}\n")           // failed recently: back-off
	touch(e.d.failPath("failed"), t0.Add(-10*time.Minute))
	e.transcript(t, "bad id!", 3*time.Hour, "{}\n") // not a valid session id

	o := e.opts()
	o.CurrentSession = "me"
	got, err := e.d.FindCandidates(o, t0)
	if err != nil {
		t.Fatal(err)
	}
	if s := sessions(got); s != "resumed,crashed" { // newest change first
		t.Errorf("candidates = %q, want %q", s, "resumed,crashed")
	}

	o.Force = true // ignores idle time and the failure back-off, but not the evaluated state
	got, _ = e.d.FindCandidates(o, t0)
	if s := sessions(got); s != "live,resumed,crashed" { // capped at 3
		t.Errorf("forced candidates = %q, want %q", s, "live,resumed,crashed")
	}
}

func TestFindCandidatesIgnoresTranscriptsFromBeforeTheFeature(t *testing.T) {
	e := newEnv(t)
	touch(filepath.Join(e.state, "sweep.since"), t0.Add(-time.Hour)) // feature started 1h ago
	e.transcript(t, "before", 2*time.Hour, "{}\n")
	e.transcript(t, "after", 30*time.Minute+time.Second, "{}\n")
	got, _ := e.d.FindCandidates(e.opts(), t0)
	if s := sessions(got); s != "after" {
		t.Errorf("candidates = %q, want only %q", s, "after")
	}
}

func TestFirstSweepOnlySetsTheStartMarker(t *testing.T) {
	e := newEnv(t)
	os.Remove(filepath.Join(e.state, "sweep.since"))
	e.transcript(t, "old", 3*time.Hour, "{}\n")
	got, err := e.d.FindCandidates(e.opts(), t0)
	if err != nil || len(got) != 0 {
		t.Fatalf("first run must not pick up history: %v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(e.state, "sweep.since")); err != nil {
		t.Error("marker not created")
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	rel, ok := acquireLock(dir, "x", time.Hour, t0)
	if !ok {
		t.Fatal("first lock must succeed")
	}
	if _, ok2 := acquireLock(dir, "x", time.Hour, t0); ok2 {
		t.Fatal("second lock must fail while held")
	}
	rel()
	rel, ok = acquireLock(dir, "x", time.Hour, t0)
	if !ok {
		t.Fatal("lock must be available after release")
	}
	// A lock of a crashed owner is taken over once it is stale.
	if _, ok := acquireLock(dir, "x", time.Hour, time.Now().Add(2*time.Hour)); !ok {
		t.Error("stale lock must be taken over")
	}
	rel()
}

func TestTranscriptCwd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"mode"}` + "\n" + `{"type":"user","cwd":"/home/user/repos/demo","message":{"content":"hi"}}` + "\n"
	_ = os.WriteFile(p, []byte(body), 0o600)
	if got := TranscriptCwd(p); got != "/home/user/repos/demo" {
		t.Errorf("cwd = %q", got)
	}
	if TranscriptCwd(filepath.Join(t.TempDir(), "missing.jsonl")) != "" {
		t.Error("missing file must give empty cwd")
	}
}

func TestSweepMarksShortSessionsAsDoneAndThrottles(t *testing.T) {
	e := newEnv(t)
	// Too short to evaluate (no user messages): Run saves the progress without calling the model.
	e.transcript(t, "short1", 2*time.Hour, `{"type":"mode"}`+"\n")
	e.transcript(t, "short2", 3*time.Hour, `{"type":"mode"}`+"\n")
	o := e.opts()
	o.ProjectOf = func(context.Context, string) string { return "demo" }

	if err := e.d.Sweep(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"short1", "short2"} {
		if _, err := os.Stat(e.d.progressPath(s)); err != nil {
			t.Errorf("session %s was not marked as evaluated", s)
		}
	}
	// Nothing left, and a second sweep within MinInterval does not even look.
	e.transcript(t, "later", 2*time.Hour, `{"type":"mode"}`+"\n")
	if err := e.d.Sweep(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.d.progressPath("later")); err == nil {
		t.Error("throttled sweep must not evaluate anything")
	}
	// After the interval it picks the new transcript up.
	o.Now = func() time.Time { return t0.Add(11 * time.Minute) }
	if err := e.d.Sweep(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.d.progressPath("later")); err != nil {
		t.Error("sweep after the interval must evaluate the new transcript")
	}
}

func TestSweepSkipsWhenAnotherSweepRuns(t *testing.T) {
	e := newEnv(t)
	e.transcript(t, "short", 2*time.Hour, `{"type":"mode"}`+"\n")
	if _, ok := acquireLock(e.state, "sweep", time.Hour, t0); !ok {
		t.Fatal("setup")
	}
	if err := e.d.Sweep(context.Background(), e.opts()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.d.progressPath("short")); err == nil {
		t.Error("a second concurrent sweep must do nothing")
	}
}
