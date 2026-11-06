package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner fuehrt einen einzelnen Modell-Aufruf aus (system = Anweisung, user = Daten).
type Runner interface {
	Run(ctx context.Context, system, user string) (string, error)
}

// ClaudeCLI nutzt das lokale 'claude -p' (headless, ohne Tools, MCP, Hooks, Settings).
type ClaudeCLI struct {
	Bin   string // leer = automatisch suchen
	Model string
}

// GuardEnv verhindert, dass der Hintergrundlauf selbst wieder Extraktionen ausloest.
const GuardEnv = "KB_EXTRACTING"

func (c ClaudeCLI) Run(ctx context.Context, system, user string) (string, error) {
	bin := c.Bin
	if bin == "" {
		var err error
		if bin, err = findClaude(); err != nil {
			return "", err
		}
	}
	model := c.Model
	if model == "" {
		model = "sonnet"
	}
	cmd := exec.CommandContext(ctx, bin,
		"-p", "Befolge die Anweisung im System-Prompt und antworte ausschliesslich mit dem geforderten JSON.",
		"--system-prompt", system,
		"--model", model,
		"--tools", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--setting-sources", "",
		"--output-format", "json",
	)
	cmd.Dir = os.TempDir() // neutral: kein Projekt-CLAUDE.md, keine Projekt-Hooks
	cmd.Env = append(os.Environ(), GuardEnv+"=1")
	cmd.Stdin = strings.NewReader(user)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude -p: %w (%s)", err, strings.TrimSpace(truncateRunes(stderr.String(), 300)))
	}
	var res struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("claude -p lieferte kein JSON: %w", err)
	}
	if res.IsError {
		return "", fmt.Errorf("claude -p meldet Fehler: %s", truncateRunes(res.Result, 300))
	}
	return res.Result, nil
}

func findClaude() (string, error) {
	if p := os.Getenv("KB_CLAUDE_BIN"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("claude CLI nicht gefunden (KB_CLAUDE_BIN setzen)")
}
