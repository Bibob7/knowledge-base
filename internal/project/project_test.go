package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestFromDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	base := t.TempDir()
	repo := filepath.Join(base, "My-Repo")
	sub := filepath.Join(repo, "internal", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(repo, ".claude", "worktrees", "agent-a1b2")
	git(t, repo, "worktree", "add", "-q", wt)
	elsewhere := filepath.Join(base, "feature-checkout")
	git(t, repo, "worktree", "add", "-q", elsewhere)
	plain := filepath.Join(base, "no-repo")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	for dir, want := range map[string]string{
		repo:                        "my-repo",
		sub:                         "my-repo",
		wt:                          "my-repo", // worktree inside the repository
		elsewhere:                   "my-repo", // worktree somewhere else
		plain:                       "",
		filepath.Join(base, "gone"): "", // deleted checkout
		filepath.Join(repo, ".claude", "worktrees", "agent-gone", "sub"): "my-repo", // removed agent worktree
		"": "",
	} {
		if got := FromDir(ctx, dir); got != want {
			t.Errorf("FromDir(%s) = %q, want %q", dir, got, want)
		}
	}

	// The override file in the main repository applies to its worktrees too.
	if err := os.WriteFile(filepath.Join(repo, OverrideFile), []byte("# name for the knowledge base\n  Shop-Backend \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{repo, sub, elsewhere} {
		if got := FromDir(ctx, dir); got != "shop-backend" {
			t.Errorf("with override, FromDir(%s) = %q", dir, got)
		}
	}
}
