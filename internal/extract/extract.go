package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kevin/knowledge-base/internal/embed"
	"github.com/kevin/knowledge-base/internal/secrets"
	"github.com/kevin/knowledge-base/internal/store"
)

const (
	minUserMsgs   = 3      // kuerzere Sitzungen lohnen die Auswertung nicht
	minTextRunes  = 800    // ebenso sehr kurze Protokolle
	maxTextRunes  = 100000 // Obergrenze fuer das Protokoll im Modell-Aufruf
	maxFacts      = 8
	minFactRunes  = 15
	maxFactRunes  = 1200
	dupThreshold  = 0.92 // ab hier gilt ein Fakt als bereits bekannt
	conflictFloor = 0.78 // ab hier wird ein Modell-Urteil (add/skip/supersede) eingeholt
	AutoTag       = "auto"
)

// Fact ist ein vom Modell vorgeschlagener Wissenseintrag.
type Fact struct {
	Content string   `json:"content"`
	Scope   string   `json:"scope"`
	Tags    []string `json:"tags"`
}

type Options struct {
	Transcript string
	SessionID  string
	Project    string // leer = projektuebergreifend
	DryRun     bool
}

type Deps struct {
	Store     *store.Store
	Embed     *embed.Client
	DocPrefix string
	Runner    Runner
	StateDir  string
	Logf      func(format string, a ...any)
}

// Run wertet ein Transkript aus und speichert neues Wissen.
func Run(ctx context.Context, d Deps, o Options) error {
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
	d.Logf("session=%s project=%q neue Zeilen %d..%d, %d Nutzer-Nachrichten", o.SessionID, o.Project, from+1, conv.LastLine, conv.UserMsgs)
	if conv.UserMsgs < minUserMsgs || utf8.RuneCountInString(conv.Text) < minTextRunes {
		d.Logf("uebersprungen: Sitzung zu kurz")
		if !o.DryRun {
			d.saveProgress(o.SessionID, conv.LastLine)
		}
		return nil
	}

	raw, err := d.Runner.Run(ctx, extractSystem, "<transcript>\n"+Condense(conv.Text, maxTextRunes)+"\n</transcript>")
	if err != nil {
		return fmt.Errorf("extraktion: %w", err)
	}
	facts, err := ParseFacts(raw)
	if err != nil {
		return fmt.Errorf("extraktion: %w (Antwort: %.200s)", err, raw)
	}
	d.Logf("%d Kandidaten vom Modell", len(facts))

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
	similar []store.SimilarEntry // Eintraege ab conflictFloor
}

func (d Deps) process(ctx context.Context, o Options, facts []Fact) error {
	var ready, ambiguous []*candidate
	for _, f := range facts {
		f = sanitize(f)
		if err := validate(f); err != nil {
			d.Logf("verworfen (%v): %.80s", err, f.Content)
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
		switch classify(sims) {
		case actionSkip:
			d.Logf("bekannt (Aehnlichkeit %.2f zu #%d): %.80s", sims[0].Similarity, sims[0].ID, f.Content)
		case actionAdd:
			ready = append(ready, c)
		default:
			for _, s := range sims {
				if s.Similarity >= conflictFloor {
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
			d.Logf("Urteil fehlgeschlagen, %d Faelle werden uebersprungen: %v", len(ambiguous), err)
		}
	}
	for _, c := range ambiguous {
		dec, ok := decisions[c]
		if !ok || dec.action == "skip" {
			d.Logf("bekannt/uebersprungen: %.80s", c.fact.Content)
			continue
		}
		if dec.action == "supersede" {
			c.fact.Tags = append(c.fact.Tags, "korrektur")
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
		d.Logf("[dry-run] wuerde speichern (project=%q, ersetzt %v): %s", c.project, supersedes, c.fact.Content)
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
		d.Logf("speichern fehlgeschlagen: %v", err)
		return
	}
	d.Logf("gespeichert #%d (project=%q, ersetzt %v): %s", res.ID, c.project, res.Superseded, c.fact.Content)
}

// --- Entscheidungslogik ---

type action int

const (
	actionAdd action = iota
	actionSkip
	actionJudge
)

// classify entscheidet anhand der Aehnlichkeit zu bestehenden Eintraegen.
func classify(sims []store.SimilarEntry) action {
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
		return nil, fmt.Errorf("urteil nicht lesbar: %w", err)
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
			if allowed[id] { // nur IDs, die dem Modell tatsaechlich vorgelegt wurden
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

// --- Parsen und Bereinigen ---

var fenceRe = regexp.MustCompile("(?s)^```[a-zA-Z]*\\s*|\\s*```$")

// extractJSON schneidet das erste JSON-Objekt aus einer Modell-Antwort (auch mit Code-Fence).
func extractJSON(s string) string {
	s = strings.TrimSpace(fenceRe.ReplaceAllString(strings.TrimSpace(s), ""))
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// ParseFacts liest die Antwort des Extraktions-Modells.
func ParseFacts(raw string) ([]Fact, error) {
	var out struct {
		Facts []Fact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &out); err != nil {
		return nil, fmt.Errorf("antwort ist kein gueltiges JSON: %w", err)
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
		return fmt.Errorf("zu kurz")
	case n > maxFactRunes:
		return fmt.Errorf("zu lang")
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

// --- Fortschritt je Session (inkrementelles Auswerten bei Wiederaufnahme) ---

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
