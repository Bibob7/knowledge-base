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

// Runner executes a single model call (system = instruction, user = data).
type Runner interface {
	Run(ctx context.Context, system, user string) (string, error)
}

// ClaudeCLI uses the local 'claude -p' (headless, without tools, MCP, hooks, settings).
type ClaudeCLI struct {
	Bin   string // empty = auto-detect
	Model string
}

// GuardEnv prevents the background run from triggering further extractions.
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
		"-p", "Follow the instruction in the system prompt and reply with the requested JSON only.",
		"--system-prompt", system,
		"--model", model,
		"--tools", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--setting-sources", "",
		"--output-format", "json",
	)
	cmd.Dir = os.TempDir() // neutral: no project CLAUDE.md, no project hooks
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
		return "", fmt.Errorf("claude -p returned no JSON: %w", err)
	}
	if res.IsError {
		return "", fmt.Errorf("claude -p reported an error: %s", truncateRunes(res.Result, 300))
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
	return "", fmt.Errorf("claude CLI not found (set KB_CLAUDE_BIN)")
}
