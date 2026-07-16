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

func TestExecGit_HardReset_DiscardsLocalChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "v1")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "everflow/plan/abc"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Plant a local modification and an untracked file.
	writeFile(t, worktreeDir, "README.md", "tampered\n")
	writeFile(t, worktreeDir, "untracked.txt", "should be gone\n")

	if err := g.HardReset(ctx, worktreeDir, "main"); err != nil {
		t.Fatalf("HardReset: %v", err)
	}

	// README.md should be back to the original; untracked.txt should be gone.
	readme, err := os.ReadFile(filepath.Join(worktreeDir, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if string(readme) != "v1\n" {
		t.Errorf("README.md not restored: %q", readme)
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, "untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked.txt should have been removed; err=%v", err)
	}
}

// TestExecGit_SyncWithBase_FastForwardsWithoutLosingLocalCommits covers the
// common case: base has moved on, the feature branch has its own commit,
// and the two don't conflict. SyncWithBase should bring base's commit in
// while keeping the feature branch's own commit intact.
func TestExecGit_SyncWithBase_FastForwardsWithoutLosingLocalCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	writeFile(t, baseRepo, "other.md", "unrelated\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "everflow/test/sync"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Feature branch gets its own commit, touching a file base never touches.
	writeFile(t, worktreeDir, "feature.md", "my change\n")
	if err := g.Commit(ctx, worktreeDir, "feature commit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Meanwhile, base moves forward: push a new commit to origin/main that
	// doesn't touch feature.md.
	writeFile(t, baseRepo, "other.md", "unrelated v2\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "base moved on")
	runMust(t, baseRepo, "push", "origin", "main")

	if err := g.SyncWithBase(ctx, worktreeDir, "main"); err != nil {
		t.Fatalf("SyncWithBase: %v", err)
	}

	// Base's new content should now be visible in the worktree.
	other, err := os.ReadFile(filepath.Join(worktreeDir, "other.md"))
	if err != nil {
		t.Fatalf("read other.md: %v", err)
	}
	if string(other) != "unrelated v2\n" {
		t.Errorf("other.md not synced from base: %q", other)
	}
	// The feature branch's own commit should still be there.
	feature, err := os.ReadFile(filepath.Join(worktreeDir, "feature.md"))
	if err != nil {
		t.Fatalf("read feature.md: %v", err)
	}
	if string(feature) != "my change\n" {
		t.Errorf("feature.md lost during sync: %q", feature)
	}
	// No leftover conflict state.
	dirty, err := g.HasChanges(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("HasChanges: %v", err)
	}
	if dirty {
		t.Errorf("worktree should be clean after a conflict-free sync")
	}
}

// TestExecGit_SyncWithBase_LeavesConflictForRunner covers the case that
// motivates this method: base and the feature branch both touched the same
// file. SyncWithBase must NOT error — the merge conflict is exactly the
// state the runner (address-comment / fix-CI) is meant to resolve.
func TestExecGit_SyncWithBase_LeavesConflictForRunner(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "shared.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "everflow/test/conflict"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Feature branch edits shared.md.
	writeFile(t, worktreeDir, "shared.md", "feature version\n")
	if err := g.Commit(ctx, worktreeDir, "feature edit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Base also edits shared.md, differently, and gets pushed.
	writeFile(t, baseRepo, "shared.md", "base version\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "base edit")
	runMust(t, baseRepo, "push", "origin", "main")

	if err := g.SyncWithBase(ctx, worktreeDir, "main"); err != nil {
		t.Fatalf("SyncWithBase should not error on an ordinary merge conflict, got: %v", err)
	}

	// The worktree should be left with unmerged paths / conflict markers so
	// the runner can resolve them.
	dirty, err := g.HasChanges(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("HasChanges: %v", err)
	}
	if !dirty {
		t.Errorf("worktree should show conflict state as changes")
	}
	content, err := os.ReadFile(filepath.Join(worktreeDir, "shared.md"))
	if err != nil {
		t.Fatalf("read shared.md: %v", err)
	}
	if !strings.Contains(string(content), "<<<<<<<") {
		t.Errorf("shared.md should contain conflict markers, got: %q", content)
	}
}

// TestExecGit_SyncWithBase_FetchErrorPropagates ensures a genuine
// infrastructure failure (no such base branch) is returned as an error,
// not silently swallowed like a real conflict would be.
func TestExecGit_SyncWithBase_FetchErrorPropagates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "everflow/test/badbase"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	if err := g.SyncWithBase(ctx, worktreeDir, "does-not-exist"); err == nil {
		t.Errorf("SyncWithBase against a nonexistent base branch should error")
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

// TestExecGit_Commit_SkipsUntrackedBinaries regression-tests the case
// where a runner runs `go build` inside the worktree, the resulting
// binary lands alongside the source, and `git add -A` would otherwise
// sweep it into the commit (triggering pre-commit hooks that cap file
// size). After the fix, Commit must stage the text file and leave the
// binary out.
func TestExecGit_Commit_SkipsUntrackedBinaries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "everflow/test/bin"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Simulate the runner's output: a source file plus a compiled binary
	// (NUL bytes in the leading 512 — `\x7fELF` is the standard Linux
	// magic, but any NUL triggers the binary heuristic).
	writeFile(t, worktreeDir, "main.go", "package main\nfunc main() {}\n")
	const binName = "app"
	if err := os.WriteFile(filepath.Join(worktreeDir, binName),
		[]byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0, 1, 0, 0, 0}, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	if err := g.Commit(ctx, worktreeDir, "add main.go"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The commit should contain main.go but NOT the binary.
	out, err := exec.Command("git", "-C", worktreeDir, "show", "--name-only", "--pretty=").Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	files := strings.Fields(string(out))
	hasMain, hasBin := false, false
	for _, f := range files {
		if f == "main.go" {
			hasMain = true
		}
		if f == binName {
			hasBin = true
		}
	}
	if !hasMain {
		t.Errorf("commit should include main.go, got %v", files)
	}
	if hasBin {
		t.Errorf("commit must NOT include the %s binary, got %v", binName, files)
	}

	// The binary should still be present in the worktree (we skip it from
	// staging, we don't delete it — the runner may still want to use it).
	if _, err := os.Stat(filepath.Join(worktreeDir, binName)); err != nil {
		t.Errorf("binary should still exist in worktree: %v", err)
	}
}

// TestExecGit_Commit_OnlyBinaries_ReturnsNoChanges covers the edge case
// where the runner produced nothing but binary blobs — after filtering,
// nothing is staged and Commit should report ErrNoChanges rather than
// invoke `git commit` with an empty index.
func TestExecGit_Commit_OnlyBinaries_ReturnsNoChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()
	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "everflow/test/only-bin"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "blob"),
		[]byte{0, 0, 0, 0, 0}, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := g.Commit(ctx, worktreeDir, "msg"); !errors.Is(err, ErrNoChanges) {
		t.Errorf("Commit with only-binary untracked: want ErrNoChanges, got %v", err)
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
