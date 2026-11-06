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
	minFactRunes = 15
	maxFactRunes = 1200
	// Only near-verbatim copies count as known without asking the model. Contradictions and
	// paraphrases are indistinguishable by similarity (both 0.89-0.96 for bge-m3 and nomic),
	// so everything between the model's "similar" threshold and this value gets a verdict.
	dupThreshold = 0.98
	AutoTag      = "auto"
)

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
func (d Deps) Run(ctx context.Context, o Options) error {
	// One evaluation per session at a time (SessionEnd and sweep must not overlap).
	if d.StateDir != "" && safeID.MatchString(o.SessionID) {
		release, ok := acquireLock(d.StateDir, o.SessionID, sweepLockStale, time.Now())
		if !ok {
			d.Logf("session %s is already being evaluated", o.SessionID)
			return nil
		}
		defer release()
	}
	from := d.loadProgress(o.SessionID)
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
	if conv.UserMsgs < minUserMsgs || utf8.RuneCountInString(conv.Text) < minTextRunes {
		d.Logf("skipped: session too short")
		if !o.DryRun {
			d.saveProgress(o.SessionID, conv.LastLine)
		}
		return nil
	}

	raw, err := d.Runner.Run(ctx, extractSystem, "<transcript>\n"+Condense(conv.Text, maxTextRunes)+"\n</transcript>")
	if err != nil {
		return fmt.Errorf("extraction: %w", err)
	}
	facts, err := ParseFacts(raw)
	if err != nil {
		return fmt.Errorf("extraction: %w (response: %.200s)", err, raw)
	}
	d.Logf("%d candidates from the model", len(facts))

	if err := d.process(ctx, o, facts); err != nil {
		return err
	}
	if !o.DryRun {
		d.saveProgress(o.SessionID, conv.LastLine)
	}
	return nil
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
		d.save(ctx, o, c, dec.supersedes)
	}
	for _, c := range ready {
		d.save(ctx, o, c, nil)
	}
	return nil
}

func (d Deps) save(ctx context.Context, o Options, c *candidate, supersedes []int64) {
	if o.DryRun {
		d.Logf("[dry-run] would store (project=%q, supersedes %v): %s", c.project, supersedes, c.fact.Content)
		return
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
		d.Logf("storing failed: %v", err)
		return
	}
	d.Logf("stored #%d (project=%q, supersedes %v): %s", res.ID, c.project, res.Superseded, c.fact.Content)
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

func (d Deps) loadProgress(session string) int {
	p := d.progressPath(session)
	if p == "" {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	var st struct {
		Lines int `json:"lines"`
	}
	_ = json.Unmarshal(b, &st)
	return st.Lines
}

func (d Deps) saveProgress(session string, lines int) {
	p := d.progressPath(session)
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, _ := json.Marshal(map[string]int{"lines": lines})
	_ = os.WriteFile(p, b, 0o600)
}
