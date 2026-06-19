package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecGit_FullLifecycle exercises EnsureBranch → modify file → HasChanges
// → Commit → (Push skipped — needs a remote) → RemoveWorktree against a
// real on-disk git repo set up in t.TempDir().
//
// Skips if `git` isn't on PATH (CI environments without git).
func TestExecGit_FullLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	// Initialise a real repo with one commit on main, and an "origin" remote
	// that's just a bare clone of itself — so `fetch origin main` works.
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("everflow-test", "everflow@test.invalid")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "everflow/test/svc-a"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	// Worktree should exist as a real git checkout.
	if _, err := os.Stat(filepath.Join(worktreeDir, ".git")); err != nil {
		t.Fatalf("expected .git in worktree: %v", err)
	}

	// HasChanges: should be clean.
	dirty, err := g.HasChanges(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("HasChanges (clean): %v", err)
	}
	if dirty {
		t.Errorf("HasChanges: want false on a fresh worktree, got true")
	}

	// Commit on a clean tree should return ErrNoChanges.
	if err := g.Commit(ctx, worktreeDir, "noop"); !errors.Is(err, ErrNoChanges) {
		t.Errorf("Commit on clean tree: want ErrNoChanges, got %v", err)
	}

	// Modify a file. HasChanges should flip; Commit should succeed.
	writeFile(t, worktreeDir, "NEW.md", "hello from everflow\n")
	dirty, err = g.HasChanges(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("HasChanges (dirty): %v", err)
	}
	if !dirty {
		t.Errorf("HasChanges: want true after writing a file")
	}
	if err := g.Commit(ctx, worktreeDir, "add NEW.md"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// After commit, working tree should be clean again.
	dirty, _ = g.HasChanges(ctx, worktreeDir)
	if dirty {
		t.Errorf("HasChanges: want false after commit")
	}

	// EnsureBranch on the existing dir should be a no-op.
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "everflow/test/svc-a"); err != nil {
		t.Errorf("EnsureBranch should be idempotent: %v", err)
	}

	// EnsureBranch on a dir that's a worktree for a DIFFERENT branch must error.
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "wrong/branch"); err == nil {
		t.Errorf("EnsureBranch should reject worktree on wrong branch")
	}

	// RemoveWorktree should clean up.
	if err := g.RemoveWorktree(ctx, baseRepo, worktreeDir); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone after RemoveWorktree, got err=%v", err)
	}

	// RemoveWorktree is idempotent — second call succeeds.
	if err := g.RemoveWorktree(ctx, baseRepo, worktreeDir); err != nil {
		t.Errorf("RemoveWorktree should be idempotent: %v", err)
	}
}

func TestExecGit_GIT_TERMINAL_PROMPT_DisablesPrompting(t *testing.T) {
	// We can't really test that the env var is set without intercepting
	// the subprocess. Smoke-check that the runner sets it by reading it
	// back via `env`. Skipping if `env` isn't available is fine.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	g := NewExec("", "")
	// `git config --get` will fail on a missing key without error if the
	// terminal-prompt env isn't tripping anything. The real assertion is
	// just that g.run doesn't hang on missing creds — but proving "doesn't
	// hang" in a unit test isn't feasible. Instead, verify the helper's
	// env-construction is the one that includes GIT_TERMINAL_PROMPT=0.
	// (Static check via reading the source is the honest cover here; this
	// test exists as a placeholder so future changes don't drop the var.)
	if !strings.Contains(g.envProbe(), "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("ExecGit should disable terminal prompts")
	}
}

// --- helpers ---

func runMust(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" && dir != "." {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
