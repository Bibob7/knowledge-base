package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Events for which hooks are registered.
var Events = []string{"UserPromptSubmit", "SessionStart", "SessionEnd"}

// legacyEvents are events that used to have hooks of ours but no longer do; Install and
// Uninstall clean them up. Currently none (SessionStart came back for the silent sweep).
var legacyEvents []string

const hookTimeoutSeconds = 10

// IsOurs recognizes hook commands of this program ("<path>/knowledge-base-mcp hook").
func IsOurs(command string) bool {
	c := strings.TrimSpace(command)
	return strings.Contains(c, "knowledge-base-mcp") && strings.HasSuffix(c, " hook")
}

// Command builds the hook command for the binary at exe.
func Command(exe string) string {
	if strings.ContainsAny(exe, " \t") {
		exe = "'" + exe + "'"
	}
	return exe + " hook"
}

// DefaultSettingsPath is ~/.claude/settings.json (or $KB_SETTINGS).
func DefaultSettingsPath() (string, error) {
	if p := os.Getenv("KB_SETTINGS"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// Install adds the hooks to settings.json (idempotent). Existing entries,
// key order and foreign hooks are preserved.
func Install(path, command string) (changed bool, err error) {
	return rewrite(path, func(hooks *object) {
		removeFrom(hooks, legacyEvents)
		for _, ev := range Events {
			groups := removeOurs(hooks.getArray(ev))
			groups = append(groups, mustMarshal(map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": hookTimeoutSeconds}},
			}))
			hooks.setArray(ev, groups)
		}
	})
}

// Uninstall removes only our own hook entries.
func Uninstall(path string) (changed bool, err error) {
	return rewrite(path, func(hooks *object) {
		removeFrom(hooks, append(append([]string{}, Events...), legacyEvents...))
	})
}

// removeFrom removes our hooks from the events and deletes events that become empty.
func removeFrom(hooks *object, events []string) {
	for _, ev := range events {
		if _, ok := hooks.get(ev); !ok {
			continue
		}
		groups := removeOurs(hooks.getArray(ev))
		if len(groups) == 0 {
			hooks.delete(ev)
		} else {
			hooks.setArray(ev, groups)
		}
	}
}

func rewrite(path string, mutate func(hooks *object)) (bool, error) {
	orig, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if len(bytes.TrimSpace(orig)) == 0 {
		orig = []byte("{}")
	}
	top, err := parseObject(orig)
	if err != nil {
		return false, fmt.Errorf("%s is not a valid JSON object: %w", path, err)
	}
	hooks := &object{}
	if raw, ok := top.get("hooks"); ok {
		if hooks, err = parseObject(raw); err != nil {
			return false, fmt.Errorf("'hooks' in %s is not an object: %w", path, err)
		}
	}
	mutate(hooks)
	top.set("hooks", hooks.render())

	out, err := indent(top.render())
	if err != nil {
		return false, err
	}
	before, _ := indent(orig)
	if bytes.Equal(before, out) {
		return false, nil
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if err := os.WriteFile(path+".kb-backup", orig, mode); err != nil {
			return false, fmt.Errorf("backup: %w", err)
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// removeOurs removes our own hook entries from a list of hook groups.
func removeOurs(groups []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for _, g := range groups {
		var probe struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if json.Unmarshal(g, &probe) != nil {
			out = append(out, g)
			continue
		}
		ours, other := 0, 0
		for _, h := range probe.Hooks {
			if IsOurs(h.Command) {
				ours++
			} else {
				other++
			}
		}
		switch {
		case ours == 0:
			out = append(out, g)
		case other == 0:
			// ganze Gruppe war unsere
		default:
			// mixed group: filter out only our hooks
			var m map[string]any
			if json.Unmarshal(g, &m) != nil {
				out = append(out, g)
				continue
			}
			var keep []any
			for _, h := range m["hooks"].([]any) {
				if hm, ok := h.(map[string]any); ok {
					if c, _ := hm["command"].(string); IsOurs(c) {
						continue
					}
				}
				keep = append(keep, h)
			}
			m["hooks"] = keep
			out = append(out, mustMarshal(m))
		}
	}
	return out
}

// --- small helpers for JSON objects that preserve key order ---

type kv struct {
	key string
	val json.RawMessage
}

type object struct{ items []kv }

func parseObject(raw []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("object expected")
	}
	o := &object{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("invalid key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.items = append(o.items, kv{key, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *object) get(k string) (json.RawMessage, bool) {
	for _, it := range o.items {
		if it.key == k {
			return it.val, true
		}
	}
	return nil, false
}

func (o *object) set(k string, v json.RawMessage) {
	for i := range o.items {
		if o.items[i].key == k {
			o.items[i].val = v
			return
		}
	}
	o.items = append(o.items, kv{k, v})
}

func (o *object) delete(k string) {
	for i := range o.items {
		if o.items[i].key == k {
			o.items = append(o.items[:i], o.items[i+1:]...)
			return
		}
	}
}

func (o *object) getArray(k string) []json.RawMessage {
	raw, ok := o.get(k)
	if !ok {
		return nil
	}
	var arr []json.RawMessage
	_ = json.Unmarshal(raw, &arr)
	return arr
}

func (o *object) setArray(k string, arr []json.RawMessage) {
	if arr == nil {
		arr = []json.RawMessage{}
	}
	o.set(k, mustMarshal(arr))
}

func (o *object) render() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, it := range o.items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(mustMarshal(it.key))
		b.WriteByte(':')
		b.Write(it.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func mustMarshal(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSpace(b.Bytes())
}

func indent(raw []byte) ([]byte, error) {
	var c, out bytes.Buffer
	if err := json.Compact(&c, raw); err != nil {
		return nil, err
	}
	if err := json.Indent(&out, c.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
