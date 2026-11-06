package extract

import (
	"context"
	"strings"
	"testing"

	"github.com/kevin/knowledge-base/internal/store"
)

const sampleTranscript = `{"type":"mode","mode":"x"}
{"type":"user","message":{"content":"Bitte nutze pgx statt database/sql, weil wir pgvector brauchen."}}
{"type":"user","isMeta":true,"message":{"content":"meta-nachricht"}}
{"type":"user","message":{"content":"<local-command-stdout>Reloaded</local-command-stdout>"}}
{"type":"user","message":{"content":"Echte Frage <system-reminder>geheimer Systemtext</system-reminder> mit Rest"}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"interne Gedanken"},{"type":"text","text":"Verstanden, ich nutze pgx."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo geheim"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"AUSGABE-DES-TOOLS"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__knowledge-base__remember","input":{"content":"Projekt nutzt pgx."}}]}}
{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"SUBAGENT-GEPLAPPER"}]}}
{"type":"user","message":{"content":"Danke, das passt."}}
`

func TestParseTranscript(t *testing.T) {
	c, err := ParseTranscript(strings.NewReader(sampleTranscript), 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserMsgs != 3 {
		t.Errorf("UserMsgs = %d, erwartet 3", c.UserMsgs)
	}
	for _, want := range []string{"pgx statt database/sql", "Echte Frage", "mit Rest", "Verstanden, ich nutze pgx",
		"[bereits in der knowledge-base gespeichert] Projekt nutzt pgx.", "Danke, das passt."} {
		if !strings.Contains(c.Text, want) {
			t.Errorf("fehlt im Text: %q", want)
		}
	}
	for _, bad := range []string{"geheimer Systemtext", "meta-nachricht", "Reloaded", "interne Gedanken", "echo geheim",
		"AUSGABE-DES-TOOLS", "SUBAGENT-GEPLAPPER"} {
		if strings.Contains(c.Text, bad) {
			t.Errorf("darf nicht im Text stehen: %q", bad)
		}
	}
	if c.LastLine != 11 {
		t.Errorf("LastLine = %d, erwartet 11", c.LastLine)
	}
}

func TestParseTranscriptIncremental(t *testing.T) {
	c, err := ParseTranscript(strings.NewReader(sampleTranscript), 10)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserMsgs != 1 || !strings.Contains(c.Text, "Danke") || strings.Contains(c.Text, "pgx statt") {
		t.Errorf("inkrementelles Parsen fehlerhaft: %+v", c)
	}
}

func TestCondense(t *testing.T) {
	long := strings.Repeat("a", 500) + strings.Repeat("b", 500)
	out := Condense(long, 200)
	if !strings.HasPrefix(out, strings.Repeat("a", 40)) || !strings.HasSuffix(out, strings.Repeat("b", 160)) ||
		!strings.Contains(out, "gekuerzt") {
		t.Errorf("Condense: %q", out)
	}
	if Condense("kurz", 200) != "kurz" {
		t.Error("kurzer Text darf nicht veraendert werden")
	}
}

func TestParseFacts(t *testing.T) {
	cases := map[string]int{
		`{"facts":[{"content":"abc","scope":"project","tags":["x"]}]}`:            1,
		"```json\n{\"facts\":[{\"content\":\"abc\"},{\"content\":\"def\"}]}\n```": 2,
		"Hier das Ergebnis: {\"facts\":[]} fertig":                                0,
		`{"facts":[{"content":"1"},{"content":"2"},{"content":"3"},{"content":"4"},{"content":"5"},{"content":"6"},{"content":"7"},{"content":"8"},{"content":"9"}]}`: 8,
	}
	for in, want := range cases {
		got, err := ParseFacts(in)
		if err != nil || len(got) != want {
			t.Errorf("ParseFacts(%.40q) = %d, %v; erwartet %d", in, len(got), err, want)
		}
	}
	if _, err := ParseFacts("kein json"); err == nil {
		t.Error("Fehler erwartet")
	}
}

func TestSanitizeAndValidate(t *testing.T) {
	f := sanitize(Fact{Content: "  Text  ", Scope: "irgendwas", Tags: []string{"A", "a", "auto", " ", "b"}})
	if f.Scope != "project" || f.Content != "Text" || strings.Join(f.Tags, ",") != "a,b" {
		t.Errorf("sanitize: %+v", f)
	}
	if validate(Fact{Content: "kurz"}) == nil {
		t.Error("zu kurzer Fakt muss abgelehnt werden")
	}
	if validate(Fact{Content: "Das Passwort lautet password = hunter2hunter2 bitte merken"}) == nil {
		t.Error("Secret muss abgelehnt werden")
	}
	if err := validate(Fact{Content: "Wir nutzen pgx statt database/sql, weil pgvector das braucht."}); err != nil {
		t.Errorf("gueltiger Fakt abgelehnt: %v", err)
	}
}

func TestClassify(t *testing.T) {
	mk := func(s float64) []store.SimilarEntry { return []store.SimilarEntry{{ID: 1, Similarity: s}} }
	if classify(nil) != actionAdd || classify(mk(0.5)) != actionAdd {
		t.Error("niedrige Aehnlichkeit muss add sein")
	}
	if classify(mk(0.85)) != actionJudge {
		t.Error("mittlere Aehnlichkeit muss Urteil einholen")
	}
	if classify(mk(0.95)) != actionSkip {
		t.Error("hohe Aehnlichkeit ist Dublette")
	}
}

type fakeRunner struct{ out string }

func (f fakeRunner) Run(context.Context, string, string) (string, error) { return f.out, nil }

func TestJudgeOnlyAcceptsPresentedIDs(t *testing.T) {
	c1 := &candidate{fact: Fact{Content: "neu 1"}, similar: []store.SimilarEntry{{ID: 10}, {ID: 11}}}
	c2 := &candidate{fact: Fact{Content: "neu 2"}, similar: []store.SimilarEntry{{ID: 20}}}
	c3 := &candidate{fact: Fact{Content: "neu 3"}, similar: []store.SimilarEntry{{ID: 30}}}
	c4 := &candidate{fact: Fact{Content: "neu 4"}, similar: []store.SimilarEntry{{ID: 40}}}
	out := "```json\n" + `{"decisions":[
		{"n":1,"action":"supersede","supersedes":[10,999]},
		{"n":2,"action":"supersede","supersedes":[999]},
		{"n":3,"action":"add"},
		{"n":4,"action":"quatsch"},
		{"n":9,"action":"add"}]}` + "\n```"
	got, err := Deps{Runner: fakeRunner{out}}.judge(context.Background(), []*candidate{c1, c2, c3, c4})
	if err != nil {
		t.Fatal(err)
	}
	if d := got[c1]; d.action != "supersede" || len(d.supersedes) != 1 || d.supersedes[0] != 10 {
		t.Errorf("c1: %+v (erfundene ID 999 muss verworfen werden)", d)
	}
	if got[c2].action != "skip" {
		t.Errorf("c2: supersede ohne gueltige ID muss skip sein: %+v", got[c2])
	}
	if got[c3].action != "add" {
		t.Errorf("c3: %+v", got[c3])
	}
	if got[c4].action != "skip" {
		t.Errorf("c4: unbekannte Aktion muss skip sein: %+v", got[c4])
	}
}

func TestParseTranscriptToleratesSpacedJSON(t *testing.T) {
	in := `{"type": "user", "message": {"content": "Hallo Welt"}}` + "\n"
	c, err := ParseTranscript(strings.NewReader(in), 0)
	if err != nil || c.UserMsgs != 1 {
		t.Fatalf("UserMsgs=%d err=%v", c.UserMsgs, err)
	}
}
