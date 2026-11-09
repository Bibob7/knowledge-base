// Package project derives the project name that scopes knowledge entries.
//
// The name is the lowercase folder name of the main repository: a git worktree (e.g.
// .claude/worktrees/agent-xyz, or a checkout made with git worktree add) belongs to
// the repository it was created from, so it shares that project's knowledge. A file
// named .knowledge-base-project in the repository root overrides the name (for two
// repositories with the same folder name, or a renamed folder).
package project

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OverrideFile is the name of the optional file in the repository root whose first line
// is the project name to use.
const OverrideFile = ".knowledge-base-project"

// agentWorktrees is where Claude Code creates worktrees for agents (isolation: worktree).
const agentWorktrees = "/.claude/worktrees/"

// FromDir returns the project of dir, or "" outside a git repository (or if dir no
// longer exists).
func FromDir(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		// A worktree of Claude Code's agents that has been removed since (the sweep evaluates
		// sessions later): its path still tells the repository it belonged to.
		if i := strings.Index(filepath.ToSlash(dir), agentWorktrees); i > 0 {
			dir = filepath.FromSlash(filepath.ToSlash(dir)[:i])
		} else {
			return ""
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse",
		"--path-format=absolute", "--show-toplevel", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return ""
	}
	top, main := strings.TrimSpace(lines[0]), mainRoot(strings.TrimSpace(lines[1]))
	for _, root := range []string{top, main} {
		if name := readOverride(root); name != "" {
			return name
		}
	}
	switch {
	case main != "":
		return normalize(filepath.Base(main))
	case top != "":
		return normalize(filepath.Base(top))
	}
	return ""
}

// mainRoot maps the common git directory to the folder that names the repository:
// /repo/.git -> /repo (also for worktrees, whose common dir is the main .git);
// /super/.git/modules/sub (submodule) -> .../sub.
func mainRoot(common string) string {
	if common == "" {
		return ""
	}
	common = filepath.Clean(common)
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common
}

func readOverride(root string) string {
	if root == "" {
		return ""
	}
	f, err := os.Open(filepath.Join(root, OverrideFile))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			return normalize(line)
		}
	}
	return ""
}

func normalize(name string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".git")
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}
