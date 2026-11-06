package extract

import (
	"context"
	"strings"
	"testing"

	"github.com/Bibob7/knowledge-base/internal/store"
)

const sampleTranscript = `{"type":"mode","mode":"x"}
{"type":"user","message":{"content":"Please use pgx instead of database/sql because we need pgvector."}}
{"type":"user","isMeta":true,"message":{"content":"meta-message"}}
{"type":"user","message":{"content":"<local-command-stdout>Reloaded</local-command-stdout>"}}
{"type":"user","message":{"content":"Real question <system-reminder>secret system text</system-reminder> with rest"}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"internal thoughts"},{"type":"text","text":"Understood, I will use pgx."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo secret"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"TOOL-OUTPUT"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__knowledge-base__remember","input":{"content":"Project uses pgx."}}]}}
{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"SUBAGENT-CHATTER"}]}}
{"type":"user","message":{"content":"Thanks, that works."}}
`

func TestParseTranscript(t *testing.T) {
	c, err := ParseTranscript(strings.NewReader(sampleTranscript), 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserMsgs != 3 {
		t.Errorf("UserMsgs = %d, expected 3", c.UserMsgs)
	}
	for _, want := range []string{"pgx instead of database/sql", "Real question", "with rest", "Understood, I will use pgx",
		"[already saved in the knowledge base] Project uses pgx.", "Thanks, that works."} {
		if !strings.Contains(c.Text, want) {
			t.Errorf("missing from text: %q", want)
		}
	}
	for _, bad := range []string{"geheimer Systemtext", "meta-message", "Reloaded", "internal thoughts", "echo secret",
		"TOOL-OUTPUT", "SUBAGENT-CHATTER"} {
		if strings.Contains(c.Text, bad) {
			t.Errorf("must not appear in text: %q", bad)
		}
	}
	if c.LastLine != 11 {
		t.Errorf("LastLine = %d, expected 11", c.LastLine)
	}
}

func TestParseTranscriptIncremental(t *testing.T) {
	c, err := ParseTranscript(strings.NewReader(sampleTranscript), 10)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserMsgs != 1 || !strings.Contains(c.Text, "Thanks") || strings.Contains(c.Text, "pgx instead") {
		t.Errorf("incremental parsing is wrong: %+v", c)
	}
}

func TestCondense(t *testing.T) {
	long := strings.Repeat("a", 500) + strings.Repeat("b", 500)
	out := Condense(long, 200)
	if !strings.HasPrefix(out, strings.Repeat("a", 40)) || !strings.HasSuffix(out, strings.Repeat("b", 160)) ||
		!strings.Contains(out, "omitted") {
		t.Errorf("Condense: %q", out)
	}
	if Condense("short", 200) != "short" {
		t.Error("short text must not be changed")
	}
}

func TestParseFacts(t *testing.T) {
	cases := map[string]int{
		`{"facts":[{"content":"abc","scope":"project","tags":["x"]}]}`:            1,
		"```json\n{\"facts\":[{\"content\":\"abc\"},{\"content\":\"def\"}]}\n```": 2,
		"Here is the result: {\"facts\":[]} done":                                 0,
		`{"facts":[{"content":"1"},{"content":"2"},{"content":"3"},{"content":"4"},{"content":"5"},{"content":"6"},{"content":"7"},{"content":"8"},{"content":"9"}]}`: 8,
	}
	for in, want := range cases {
		got, err := ParseFacts(in)
		if err != nil || len(got) != want {
			t.Errorf("ParseFacts(%.40q) = %d, %v; expected %d", in, len(got), err, want)
		}
	}
	if _, err := ParseFacts("no json"); err == nil {
		t.Error("error expected")
	}
}

func TestSanitizeAndValidate(t *testing.T) {
	f := sanitize(Fact{Content: "  Text  ", Scope: "irgendwas", Tags: []string{"A", "a", "auto", " ", "b"}})
	if f.Scope != "project" || f.Content != "Text" || strings.Join(f.Tags, ",") != "a,b" {
		t.Errorf("sanitize: %+v", f)
	}
	if validate(Fact{Content: "short"}) == nil {
		t.Error("a fact that is too short must be rejected")
	}
	if validate(Fact{Content: "The password is password = hunter2hunter2 please remember"}) == nil {
		t.Error("a secret must be rejected")
	}
	if err := validate(Fact{Content: "We use pgx instead of database/sql because pgvector needs it."}); err != nil {
		t.Errorf("valid fact rejected: %v", err)
	}
}

func TestClassify(t *testing.T) {
	mk := func(s float64) []store.SimilarEntry { return []store.SimilarEntry{{ID: 1, Similarity: s}} }
	if classify(nil, 0.78) != actionAdd || classify(mk(0.5), 0.78) != actionAdd {
		t.Error("low similarity must be add")
	}
	if classify(mk(0.85), 0.78) != actionJudge {
		t.Error("medium similarity must request a verdict")
	}
	if classify(mk(0.95), 0.78) != actionJudge {
		t.Error("a paraphrase or contradiction (0.95) must get a verdict, not be discarded")
	}
	if classify(mk(0.72), 0.70) != actionJudge {
		t.Error("the floor is configurable")
	}
	if classify(mk(0.99), 0.78) != actionSkip {
		t.Error("high similarity is a duplicate")
	}
}

type fakeRunner struct{ out string }

func (f fakeRunner) Run(context.Context, string, string) (string, error) { return f.out, nil }

func TestJudgeOnlyAcceptsPresentedIDs(t *testing.T) {
	c1 := &candidate{fact: Fact{Content: "new 1"}, similar: []store.SimilarEntry{{ID: 10}, {ID: 11}}}
	c2 := &candidate{fact: Fact{Content: "new 2"}, similar: []store.SimilarEntry{{ID: 20}}}
	c3 := &candidate{fact: Fact{Content: "new 3"}, similar: []store.SimilarEntry{{ID: 30}}}
	c4 := &candidate{fact: Fact{Content: "new 4"}, similar: []store.SimilarEntry{{ID: 40}}}
	out := "```json\n" + `{"decisions":[
		{"n":1,"action":"supersede","supersedes":[10,999]},
		{"n":2,"action":"supersede","supersedes":[999]},
		{"n":3,"action":"add"},
		{"n":4,"action":"nonsense"},
		{"n":9,"action":"add"}]}` + "\n```"
	got, err := Deps{Runner: fakeRunner{out}}.judge(context.Background(), []*candidate{c1, c2, c3, c4})
	if err != nil {
		t.Fatal(err)
	}
	if d := got[c1]; d.action != "supersede" || len(d.supersedes) != 1 || d.supersedes[0] != 10 {
		t.Errorf("c1: %+v (invented ID 999 must be discarded)", d)
	}
	if got[c2].action != "skip" {
		t.Errorf("c2: supersede without a valid ID must be skip: %+v", got[c2])
	}
	if got[c3].action != "add" {
		t.Errorf("c3: %+v", got[c3])
	}
	if got[c4].action != "skip" {
		t.Errorf("c4: unknown action must be skip: %+v", got[c4])
	}
}

func TestParseTranscriptToleratesSpacedJSON(t *testing.T) {
	in := `{"type": "user", "message": {"content": "Hello world"}}` + "\n"
	c, err := ParseTranscript(strings.NewReader(in), 0)
	if err != nil || c.UserMsgs != 1 {
		t.Fatalf("UserMsgs=%d err=%v", c.UserMsgs, err)
	}
}

func TestIsRememberTool(t *testing.T) {
	for name, want := range map[string]bool{
		"mcp__knowledge-base__remember":                   true,
		"mcp__plugin_knowledge-base_kb__remember":         true,
		"mcp__plugin_knowledge-base_kb__search_knowledge": false,
		"mcp__plugin_other_kb__remember":                  false,
		"mcp__memory__remember":                           false,
	} {
		if got := isRememberTool(name); got != want {
			t.Errorf("isRememberTool(%q) = %v, want %v", name, got, want)
		}
	}
}
