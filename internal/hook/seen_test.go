package hook

import "testing"

func TestForgetShown(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	saveSeen("sess-1", []int64{3, 7})
	saveSeen("sess-2", []int64{9})
	if got := loadSeen("sess-1"); len(got) != 2 {
		t.Fatalf("loadSeen = %v", got)
	}
	ForgetShown("sess-1") // after /compact
	if got := loadSeen("sess-1"); len(got) != 0 {
		t.Errorf("after ForgetShown: %v, want nothing", got)
	}
	if got := loadSeen("sess-2"); len(got) != 1 {
		t.Errorf("other sessions must keep their list: %v", got)
	}
	ForgetShown("../evil") // invalid IDs are ignored
}
