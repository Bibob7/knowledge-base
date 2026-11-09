package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bibob7/knowledge-base/internal/embed"
	"github.com/Bibob7/knowledge-base/internal/secrets"
	"github.com/Bibob7/knowledge-base/internal/store"
)

const (
	minUserMsgs  = 3      // shorter sessions are not worth evaluating
	minTextRunes = 800    // likewise very short logs
	maxTextRunes = 100000 // upper limit for the log in the model call
	maxFacts     = 8
	// maxContextRunes is how much of the already evaluated part of a session is shown as
	// context when only its continuation is evaluated.
	maxContextRunes = 6000
	minFactRunes    = 15
	maxFactRunes    = 1200
	// Only near-verbatim copies count as known without asking the model. Contradictions and
	// paraphrases are indistinguishable by similarity (both 0.89-0.96 for bge-m3 and nomic),
	// so everything between the model's "similar" threshold and this value gets a verdict.
	dupThreshold = 0.98
	AutoTag      = "auto"
)

var lockPoll = 5 * time.Second

// Fact is a knowledge entry proposed by the model.
type Fact struct {
	Content string   `json:"content"`
	Scope   string   `json:"scope"`
	Tags    []string `json:"tags"`
}

type Options struct {
	Transcript string
	SessionID  string
	Project    string // empty = cross-project
	DryRun     bool
	// WaitForLock is how long to wait if the session is being evaluated by another process
	// (0: give up at once, as the sweep does).
	WaitForLock time.Duration
}

type Deps struct {
	Store     store.Store
	Embed     *embed.Client
	DocPrefix string
	Runner    Runner
	StateDir  string
	// ConflictFloor is the similarity from which a model verdict (add/skip/supersede) is requested.
	ConflictFloor float64
	Logf          func(format string, a ...any)
}

// Run evaluates a transcript and stores new knowledge.
func Run(ctx context.Context, d Deps, o Options) error { return d.Run(ctx, o) }

// Run evaluates a transcript and stores new knowledge.
//
// Progress is kept per session: "lines" is how far the transcript was read, "evaluated"
// how far it was actually given to the model. A part that is too short to be worth a
// model call is left pending and evaluated together with what the session adds later;
// the part after an evaluated one is always evaluated (with the end of the earlier part
// as context), so a short correction after a resume is not lost.
func (d Deps) Run(ctx context.Context, o Options) error {
	// One evaluation per session at a time (SessionEnd and sweep must not overlap).
	if d.StateDir != "" && safeID.MatchString(o.SessionID) {
		release, ok := d.waitForLock(ctx, o.SessionID, o.WaitForLock)
		if !ok {
			d.Logf("session %s is already being evaluated", o.SessionID)
			return nil
		}
		defer release()
	}
	from := d.loadProgress(o.SessionID).Evaluated
	f, err := os.Open(o.Transcript)
	if err != nil {
		return err
	}
	conv, err := ParseTranscript(f, from)
	f.Close()
	if err != nil {
		return err
	}
	d.Logf("session=%s project=%q new lines %d..%d, %d user messages", o.SessionID, o.Project, from+1, conv.LastLine, conv.UserMsgs)
	tooShort := conv.UserMsgs < minUserMsgs || utf8.RuneCountInString(conv.Text) < minTextRunes
	if from > 0 {
		tooShort = conv.UserMsgs == 0 // a continuation: the earlier part gives the context
	}
	if tooShort {
		d.Logf("skipped for now: too short (evaluated together with what the session adds later)")
		if !o.DryRun {
			d.saveProgress(o.SessionID, progress{Lines: conv.LastLine, Evaluated: from})
		}
		return nil
	}

	input := "<transcript>\n" + Condense(conv.Text, maxTextRunes) + "\n</transcript>"
	if from > 0 {
		if earlier := d.earlierContext(o.Transcript, from); earlier != "" {
			input = "<earlier_context>\n" + earlier + "\n</earlier_context>\n" + input
		}
	}
	raw, err := d.Runner.Run(ctx, extractSystem, input)
	if err != nil {
		return fmt.Errorf("extraction: %w", err)
	}
	facts, err := ParseFacts(raw)
	if err != nil {
		return fmt.Errorf("extraction: %w (response: %.200s)", err, raw)
	}
	d.Logf("%d candidates from the model", len(facts))

	// On an error nothing is marked as evaluated: the next run (SessionEnd of a resumed
	// session or the sweep) evaluates the part again; facts stored before the error are
	// then recognised as known.
	if err := d.process(ctx, o, facts); err != nil {
		return err
	}
	if !o.DryRun {
		d.saveProgress(o.SessionID, progress{Lines: conv.LastLine, Evaluated: conv.LastLine})
	}
	return nil
}

// earlierContext returns the end of the already evaluated part of a session.
func (d Deps) earlierContext(transcript string, until int) string {
	f, err := os.Open(transcript)
	if err != nil {
		return ""
	}
	defer f.Close()
	c, err := ParseTranscriptUntil(f, 0, until)
	if err != nil {
		return ""
	}
	r := []rune(c.Text)
	if len(r) > maxContextRunes {
		r = append([]rune("[...] "), r[len(r)-maxContextRunes:]...)
	}
	return string(r)
}

// waitForLock takes the evaluation lock of a session, waiting up to wait for another
// evaluation (e.g. a sweep that started just before the session ended) to finish.
func (d Deps) waitForLock(ctx context.Context, session string, wait time.Duration) (func(), bool) {
	deadline := time.Now().Add(wait)
	for {
		if release, ok := acquireLock(d.StateDir, session, sweepLockStale, time.Now()); ok {
			return release, true
		}
		if wait <= 0 || time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(lockPoll):
		}
	}
}

type candidate struct {
	fact    Fact
	project string
	vec     []float32
	similar []store.SimilarEntry // entries at or above conflictFloor
}

func (d Deps) process(ctx context.Context, o Options, facts []Fact) error {
	var ready, ambiguous []*candidate
	for _, f := range facts {
		f = sanitize(f)
		if err := validate(f); err != nil {
			d.Logf("discarded (%v): %.80s", err, f.Content)
			continue
		}
		project := o.Project
		if f.Scope == "global" {
			project = ""
		} else if project == "" {
			// The project is unknown (no git repository, or its folder is gone, e.g. a
			// deleted worktree). Stored without project the fact would show up in every
			// project, so it is dropped.
			d.Logf("discarded (project fact, but the project is unknown): %.80s", f.Content)
			continue
		}
		vecs, err := d.Embed.Embed(ctx, []string{d.DocPrefix + f.Content})
		if err != nil {
			return err
		}
		sims, err := d.Store.Similar(ctx, vecs[0], project, nil, 4)
		if err != nil {
			return err
		}
		c := &candidate{fact: f, project: project, vec: vecs[0]}
		switch classify(sims, d.ConflictFloor) {
		case actionSkip:
			d.Logf("known (similarity %.2f to #%d): %.80s", sims[0].Similarity, sims[0].ID, f.Content)
		case actionAdd:
			ready = append(ready, c)
		default:
			for _, s := range sims {
				if s.Similarity >= d.ConflictFloor {
					c.similar = append(c.similar, s)
				}
			}
			ambiguous = append(ambiguous, c)
		}
	}

	var failed int
	save := func(c *candidate, supersedes []int64) {
		if err := d.save(ctx, o, c, supersedes); err != nil {
			d.Logf("storing failed: %v", err)
			failed++
		}
	}
	decisions := map[*candidate]decision{}
	if len(ambiguous) > 0 {
		var err error
		if decisions, err = d.judge(ctx, ambiguous); err != nil {
			d.Logf("verdict failed, skipping %d cases: %v", len(ambiguous), err)
		}
	}
	for _, c := range ambiguous {
		dec, ok := decisions[c]
		if !ok || dec.action == "skip" {
			d.Logf("known/skipped: %.80s", c.fact.Content)
			continue
		}
		if dec.action == "supersede" {
			c.fact.Tags = append(c.fact.Tags, "correction")
		}
		save(c, dec.supersedes)
	}
	for _, c := range ready {
		save(c, nil)
	}
	if failed > 0 {
		return fmt.Errorf("%d fact(s) could not be stored, the session will be evaluated again", failed)
	}
	return nil
}

func (d Deps) save(ctx context.Context, o Options, c *candidate, supersedes []int64) error {
	if o.DryRun {
		d.Logf("[dry-run] would store (project=%q, supersedes %v): %s", c.project, supersedes, c.fact.Content)
		return nil
	}
	res, err := d.Store.AddDocument(ctx, store.DocumentInput{
		Title:      deriveTitle(c.fact.Content),
		Project:    c.project,
		Kind:       "fact",
		Tags:       append(c.fact.Tags, AutoTag),
		Supersedes: supersedes,
		Chunks:     []store.ChunkInput{{Content: c.fact.Content, Embedding: c.vec}},
	})
	if err != nil {
		return err
	}
	d.Logf("stored #%d (project=%q, supersedes %v): %s", res.ID, c.project, res.Superseded, c.fact.Content)
	return nil
}

// --- Decision logic ---

type action int

const (
	actionAdd action = iota
	actionSkip
	actionJudge
)

// classify decides based on the similarity to existing entries.
func classify(sims []store.SimilarEntry, conflictFloor float64) action {
	if len(sims) == 0 {
		return actionAdd
	}
	if sims[0].Similarity >= dupThreshold {
		return actionSkip
	}
	if sims[0].Similarity >= conflictFloor {
		return actionJudge
	}
	return actionAdd
}

type decision struct {
	action     string
	supersedes []int64
}

func (d Deps) judge(ctx context.Context, cands []*candidate) (map[*candidate]decision, error) {
	type existing struct {
		ID      int64  `json:"id"`
		Content string `json:"content"`
	}
	type item struct {
		N        int        `json:"n"`
		New      string     `json:"new"`
		Existing []existing `json:"existing"`
	}
	items := make([]item, len(cands))
	for i, c := range cands {
		items[i] = item{N: i + 1, New: c.fact.Content}
		for _, s := range c.similar {
			items[i].Existing = append(items[i].Existing, existing{s.ID, s.Content})
		}
	}
	in, _ := json.Marshal(map[string]any{"items": items})
	raw, err := d.Runner.Run(ctx, judgeSystem, string(in))
	if err != nil {
		return nil, err
	}
	var out struct {
		Decisions []struct {
			N          int     `json:"n"`
			Action     string  `json:"action"`
			Supersedes []int64 `json:"supersedes"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &out); err != nil {
		return nil, fmt.Errorf("verdict not readable: %w", err)
	}
	res := map[*candidate]decision{}
	for _, dec := range out.Decisions {
		if dec.N < 1 || dec.N > len(cands) {
			continue
		}
		c := cands[dec.N-1]
		allowed := map[int64]bool{}
		for _, s := range c.similar {
			allowed[s.ID] = true
		}
		r := decision{action: dec.Action}
		for _, id := range dec.Supersedes {
			if allowed[id] { // only IDs that were actually presented to the model
				r.supersedes = append(r.supersedes, id)
			}
		}
		switch {
		case dec.Action == "add":
		case dec.Action == "supersede" && len(r.supersedes) > 0:
		default:
			r = decision{action: "skip"}
		}
		res[c] = r
	}
	return res, nil
}

// --- Parsing and sanitizing ---

var fenceRe = regexp.MustCompile("(?s)^```[a-zA-Z]*\\s*|\\s*```$")

// extractJSON cuts the first JSON object out of a model response (also with a code fence).
func extractJSON(s string) string {
	s = strings.TrimSpace(fenceRe.ReplaceAllString(strings.TrimSpace(s), ""))
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// ParseFacts reads the response of the extraction model.
func ParseFacts(raw string) ([]Fact, error) {
	var out struct {
		Facts []Fact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &out); err != nil {
		return nil, fmt.Errorf("response is not valid JSON: %w", err)
	}
	if len(out.Facts) > maxFacts {
		out.Facts = out.Facts[:maxFacts]
	}
	return out.Facts, nil
}

func sanitize(f Fact) Fact {
	f.Content = strings.TrimSpace(f.Content)
	if f.Scope != "global" {
		f.Scope = "project"
	}
	seen := map[string]bool{}
	var tags []string
	for _, t := range f.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || utf8.RuneCountInString(t) > 30 || seen[t] || t == AutoTag {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
		if len(tags) == 5 {
			break
		}
	}
	f.Tags = tags
	return f
}

func validate(f Fact) error {
	n := utf8.RuneCountInString(f.Content)
	switch {
	case n < minFactRunes:
		return fmt.Errorf("too short")
	case n > maxFactRunes:
		return fmt.Errorf("too long")
	}
	return secrets.Check(f.Content)
}

func deriveTitle(content string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	if utf8.RuneCountInString(first) > 80 {
		return string([]rune(first)[:80]) + "..."
	}
	return first
}

// --- Progress per session (incremental evaluation when a session is resumed) ---

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func (d Deps) progressPath(session string) string {
	if d.StateDir == "" || !safeID.MatchString(session) {
		return ""
	}
	return filepath.Join(d.StateDir, session+".json")
}

// progress of a session: Lines read, Evaluated given to the model (see Run).
type progress struct {
	Lines     int `json:"lines"`
	Evaluated int `json:"evaluated"`
}

func (d Deps) loadProgress(session string) progress {
	var st progress
	p := d.progressPath(session)
	if p == "" {
		return st
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return st
	}
	var raw struct {
		Lines     int  `json:"lines"`
		Evaluated *int `json:"evaluated"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return st
	}
	st.Lines, st.Evaluated = raw.Lines, raw.Lines // files of older versions: all read lines were evaluated
	if raw.Evaluated != nil {
		st.Evaluated = *raw.Evaluated
	}
	return st
}

func (d Deps) saveProgress(session string, st progress) {
	p := d.progressPath(session)
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, _ := json.Marshal(st)
	_ = os.WriteFile(p, b, 0o600)
}
