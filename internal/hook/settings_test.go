package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `{
  "model": "x",
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "/other/tool"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "rtk hook claude"}]}]
  },
  "tui": "fullscreen"
}`

func norm(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func TestInstallUninstallRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := Command("/opt/knowledge-base/bin/knowledge-base-mcp")

	changed, err := Install(path, cmd)
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	for _, want := range []string{`"/other/tool"`, `rtk hook claude`, `"UserPromptSubmit"`, `"SessionEnd"`, cmd} {
		if !strings.Contains(s, want) {
			t.Errorf("fehlt nach Install: %s", want)
		}
	}
	// Reihenfolge der Top-Level-Keys bleibt erhalten.
	if !(strings.Index(s, `"model"`) < strings.Index(s, `"hooks"`) && strings.Index(s, `"hooks"`) < strings.Index(s, `"tui"`)) {
		t.Errorf("Key-Reihenfolge veraendert:\n%s", s)
	}

	if changed, err := Install(path, cmd); err != nil || changed {
		t.Fatalf("zweiter install muss no-op sein: changed=%v err=%v", changed, err)
	}
	if n := strings.Count(string(mustRead(t, path)), cmd); n != len(Events) {
		t.Errorf("Hook %d-mal eingetragen, erwartet %d", n, len(Events))
	}

	// Pfadwechsel ersetzt den alten Eintrag statt ihn zu duplizieren.
	cmd2 := Command("/new/place/knowledge-base-mcp")
	if _, err := Install(path, cmd2); err != nil {
		t.Fatal(err)
	}
	s = string(mustRead(t, path))
	if strings.Contains(s, "/opt/knowledge-base") || strings.Count(s, cmd2) != len(Events) {
		t.Errorf("Pfadwechsel fehlerhaft:\n%s", s)
	}

	if changed, err := Uninstall(path); err != nil || !changed {
		t.Fatalf("uninstall: changed=%v err=%v", changed, err)
	}
	if norm(t, mustRead(t, path)) != norm(t, []byte(sample)) {
		t.Errorf("nach Uninstall nicht identisch zum Original:\n%s", mustRead(t, path))
	}
}

func TestInstallRemovesLegacySessionStartHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	old := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/other/tool"}]},` +
		`{"hooks":[{"type":"command","command":"/old/knowledge-base-mcp hook","timeout":10}]}]}}`
	_ = os.WriteFile(path, []byte(old), 0o600)
	if _, err := Install(path, Command("/new/knowledge-base-mcp")); err != nil {
		t.Fatal(err)
	}
	s := string(mustRead(t, path))
	if strings.Contains(s, "/old/knowledge-base-mcp") || !strings.Contains(s, "/other/tool") {
		t.Errorf("SessionStart-Migration fehlerhaft:\n%s", s)
	}
	if strings.Count(s, "/new/knowledge-base-mcp hook") != len(Events) {
		t.Errorf("neuer Hook nicht genau je Event einmal eingetragen:\n%s", s)
	}
}

func TestInstallCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json")
	if changed, err := Install(path, Command("/x/knowledge-base-mcp")); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}

func TestInstallRejectsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte("{kaputt"), 0o600)
	if _, err := Install(path, Command("/x/knowledge-base-mcp")); err == nil {
		t.Fatal("Fehler erwartet")
	}
	if string(mustRead(t, path)) != "{kaputt" {
		t.Error("kaputte Datei darf nicht veraendert werden")
	}
}

func TestIsOurs(t *testing.T) {
	yes := []string{"/a/bin/knowledge-base-mcp hook", "'/a b/bin/knowledge-base-mcp' hook"}
	no := []string{"rtk hook claude", "/a/knowledge-base-mcp", "/x/cc-status"}
	for _, c := range yes {
		if !IsOurs(c) {
			t.Errorf("sollte erkannt werden: %q", c)
		}
	}
	for _, c := range no {
		if IsOurs(c) {
			t.Errorf("faelschlich erkannt: %q", c)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
