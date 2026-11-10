package extract

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bibob7/knowledge-base/internal/store"
)

func vecAt(angles ...float32) []float32 {
	v := make([]float32, runDim)
	copy(v, angles)
	return v
}

func TestGroupSimilar(t *testing.T) {
	mk := func(id int64, v []float32) consEntry {
		return consEntry{doc: store.SnapshotDoc{ID: id, Content: fmt.Sprint("entry ", id)}, vec: normalize(v)}
	}
	entries := []consEntry{
		mk(1, vecAt(1, 0.1)), mk(2, vecAt(1, 0.12)), mk(3, vecAt(1, 0.15)), // one cluster
		mk(4, vecAt(0, 1)),                              // alone
		mk(5, vecAt(0, 0, 1)), mk(6, vecAt(0, 0.05, 1)), // second cluster
	}
	groups := groupSimilar("p", entries, 0.95)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %+v", groups)
	}
	got := map[string]bool{}
	for _, g := range groups {
		got[fmt.Sprint(ids(g.Entries))] = true
		if g.Project != "p" || g.Action != "keep" || g.Similarity < 0.95 {
			t.Errorf("group %+v", g)
		}
	}
	if !got["[1 2 3]"] || !got["[5 6]"] {
		t.Errorf("groups %v", got)
	}

	// Large clusters are cut at maxGroupSize.
	var many []consEntry
	for i := 0; i < maxGroupSize+3; i++ {
		many = append(many, mk(int64(i+1), vecAt(1, float32(i)*0.001)))
	}
	for _, g := range groupSimilar("", many, 0.9) {
		if len(g.Entries) > maxGroupSize {
			t.Errorf("group of %d entries", len(g.Entries))
		}
	}
}

type fixedReply string

func (f fixedReply) Run(context.Context, string, string) (string, error) { return string(f), nil }

func consolidateEnv(t *testing.T, reply string) (Deps, store.Store, []int64) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "kb.db"), runDim, "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	var ids []int64
	for i, c := range []struct {
		text string
		vec  []float32
		proj string
		kind string
	}{
		{"The deploy runs via a Git tag on main.", vecAt(1, 0.1), "app", "fact"},
		{"Deployments are triggered by tagging main.", vecAt(1, 0.11), "app", "fact"},
		{"Releases: tag main, the pipeline deploys.", vecAt(1, 0.12), "app", "fact"},
		{"Prices are stored in cents.", vecAt(0, 1), "app", "fact"},
		{"Deploy happens via Git tag.", vecAt(1, 0.1), "shop", "fact"}, // other project
		{"Long manual about deploying.", vecAt(1, 0.1), "app", "document"},
	} {
		res, err := st.AddDocument(ctx, store.DocumentInput{Title: fmt.Sprint("t", i), Project: c.proj, Kind: c.kind,
			Tags: []string{"deploy"}, Chunks: []store.ChunkInput{{Content: c.text, Embedding: c.vec}}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.ID)
	}
	d := Deps{Store: st, Embed: fakeEmbedServer(t), Runner: fixedReply(reply), Logf: func(string, ...any) {}}
	return d, st, ids
}

func TestPlanAndApplyMerge(t *testing.T) {
	ctx := context.Background()
	// The model merges group 1 and also names an ID outside the group (4), which must be dropped.
	d, st, ids := consolidateEnv(t, `{"groups":[{"g":1,"action":"merge",
		"content":"Deployments run by tagging main with a Git tag; the pipeline deploys.","supersedes":[1,2,3,4]}]}`)
	plan, err := d.Plan(ctx, ConsolidateOptions{Project: "app", MinSim: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("only the three app facts form a group (not the shop fact or the document): %+v", plan.Actions)
	}
	a := plan.Actions[0]
	if a.Action != "merge" || fmt.Sprint(a.Supersedes) != fmt.Sprint(ids[:3]) || a.Project != "app" {
		t.Fatalf("plan %+v", a)
	}
	if !strings.Contains(strings.Join(a.Tags, ","), "consolidated") || !strings.Contains(strings.Join(a.Tags, ","), "deploy") {
		t.Errorf("tags %v", a.Tags)
	}

	path := filepath.Join(t.TempDir(), "plan.json")
	if err := SavePlan(path, plan); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Apply(ctx, loaded, "other"); err == nil {
		t.Fatal("a plan must only be applied with its own ID")
	}
	applied, skipped, err := d.Apply(ctx, loaded, plan.ID)
	if err != nil || applied != 1 || skipped != 0 {
		t.Fatalf("apply: %d applied, %d skipped, %v", applied, skipped, err)
	}
	list, _ := st.ListDocuments(ctx, "consolidated", "app", false, 10)
	if len(list) != 1 || list[0].Project != "app" {
		t.Fatalf("merged entry: %+v", list)
	}
	for _, id := range ids[:3] {
		if doc, _ := st.GetDocument(ctx, id); doc.Status != "superseded" || *doc.SupersededBy != list[0].ID {
			t.Errorf("#%d: %s", id, doc.Status)
		}
	}
	// Applying the same plan again changes nothing: the entries are no longer active.
	if applied, skipped, _ := d.Apply(ctx, loaded, loaded.ID); applied != 0 || skipped != 1 {
		t.Errorf("second apply: %d applied, %d skipped", applied, skipped)
	}
}

func TestPlanSupersedeAndInvalidDecisions(t *testing.T) {
	ctx := context.Background()
	d, st, ids := consolidateEnv(t, `{"groups":[{"g":1,"action":"supersede","keep_id":3,"supersedes":[1,2,3]}]}`)
	plan, err := d.Plan(ctx, ConsolidateOptions{Project: "app", MinSim: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	a := plan.Actions[0]
	if a.Action != "supersede" || a.KeepID != ids[2] || fmt.Sprint(a.Supersedes) != fmt.Sprint(ids[:2]) {
		t.Fatalf("keep_id must not supersede itself: %+v", a)
	}
	if _, _, err := d.Apply(ctx, plan, plan.ID); err != nil {
		t.Fatal(err)
	}
	if doc, _ := st.GetDocument(ctx, ids[2]); doc.Status != "active" {
		t.Error("the kept entry must stay active")
	}

	for _, reply := range []string{
		`{"groups":[{"g":1,"action":"supersede","keep_id":99,"supersedes":[1]}]}`, // keep_id outside the group
		`{"groups":[{"g":1,"action":"merge","content":"too short","supersedes":[1,2]}]}`,
		`{"groups":[{"g":1,"action":"merge","content":"Deploy via tag; the api_key=sk-abcdefghijklmnopqrstuvwxyz123456 is needed.","supersedes":[1,2]}]}`,
		`{"groups":[{"g":1,"action":"merge","content":"A merged text that replaces only one entry.","supersedes":[1]}]}`,
		`{"groups":[{"g":7,"action":"merge","content":"A merged text for a group that does not exist.","supersedes":[1,2]}]}`,
		`{"groups":[{"g":1,"action":"delete"}]}`,
	} {
		d, _, _ := consolidateEnv(t, reply)
		plan, err := d.Plan(ctx, ConsolidateOptions{Project: "app", MinSim: 0.95})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Changes()) != 0 {
			t.Errorf("%s: must fall back to keep, got %+v", reply, plan.Changes())
		}
	}
}

func TestApplyRefusesOldPlans(t *testing.T) {
	d, _, _ := consolidateEnv(t, `{"groups":[]}`)
	old := &Plan{ID: "p1", CreatedAt: time.Now().Add(-48 * time.Hour), Actions: []PlanAction{{Action: "merge"}}}
	if _, _, err := d.Apply(context.Background(), old, "p1"); err == nil {
		t.Error("a plan older than a day must be refused")
	}
	if _, err := LoadPlan(filepath.Join(t.TempDir(), "missing.json")); err == nil || !strings.Contains(err.Error(), "consolidate") {
		t.Errorf("missing plan: %v", err)
	}
}

// Entries that changed between planning and applying (superseded by someone else, or a
// different text under the same ID, e.g. a plan made against another database) make
// the group be skipped.
func TestApplySkipsChangedEntries(t *testing.T) {
	ctx := context.Background()
	reply := `{"groups":[{"g":1,"action":"supersede","keep_id":3,"supersedes":[1,2]}]}`

	d, st, ids := consolidateEnv(t, reply)
	plan, err := d.Plan(ctx, ConsolidateOptions{Project: "app", MinSim: 0.95})
	if err != nil || len(plan.Changes()) != 1 {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	if _, err := st.Supersede(ctx, []int64{ids[0]}, ids[3]); err != nil {
		t.Fatal(err)
	}
	if applied, skipped, err := d.Apply(ctx, plan, plan.ID); err != nil || applied != 0 || skipped != 1 {
		t.Errorf("superseded meanwhile: %d applied, %d skipped, %v", applied, skipped, err)
	}

	d, st, _ = consolidateEnv(t, reply)
	plan, err = d.Plan(ctx, ConsolidateOptions{Project: "app", MinSim: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	plan.Actions[0].Entries[0].Content = "A different text that was shown to the user."
	if applied, skipped, _ := d.Apply(ctx, plan, plan.ID); applied != 0 || skipped != 1 {
		t.Errorf("different content: %d applied, %d skipped", applied, skipped)
	}
	if doc, _ := st.GetDocument(ctx, plan.Actions[0].Entries[1].ID); doc.Status != "active" {
		t.Error("nothing may be superseded when the group is skipped")
	}
}

func TestRemovePlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := RemovePlan(path); err != nil {
		t.Errorf("removing a missing plan: %v", err)
	}
	if err := SavePlan(path, &Plan{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := RemovePlan(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlan(path); err == nil {
		t.Error("plan still there")
	}
}
