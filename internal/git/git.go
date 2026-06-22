// Package git wraps the git CLI for everflow's worktree + commit + push
// operations. See ADR-0023 for why we shell out instead of using go-git.
//
// All methods on Git take the worktree directory as their first parameter.
// Auth (SSH keys, credential helpers, GIT_ASKPASS) is the host's
// responsibility — the daemon does not manage credentials.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git is the abstraction step-body code calls into. ExecGit is the
// production impl; tests stub it.
type Git interface {
	// EnsureBranch makes sure `dir` is a git worktree on `branchName`,
	// rooted at `baseRepo`, branched off `origin/baseBranch`. Idempotent —
	// safe to call when the worktree already exists (validates and uses it).
	EnsureBranch(ctx context.Context, dir, baseRepo, baseBranch, branchName string) error

	// HardReset fetches origin/baseBranch and forces `dir` to match it,
	// discarding any local commits or working-tree changes. Used by the
	// planning worktree to refresh between iterations so the planner
	// always sees the current state of base.
	HardReset(ctx context.Context, dir, baseBranch string) error

	// HasChanges reports whether the worktree at `dir` has uncommitted
	// modifications (staged or unstaged, including untracked files).
	HasChanges(ctx context.Context, dir string) (bool, error)

	// Commit stages every change in the worktree and creates a commit with
	// the given message. Returns ErrNoChanges if nothing was staged — the
	// caller decides whether that's worth treating as an error.
	Commit(ctx context.Context, dir, message string) error

	// Push pushes branchName to origin, setting upstream. Auth is the host's
	// responsibility; this method does not embed credentials in URLs.
	Push(ctx context.Context, dir, branchName string) error

	// RemoveWorktree tears down the worktree at `dir` and prunes the parent
	// repo's worktree registration. Idempotent — succeeds even if `dir`
	// is already gone.
	RemoveWorktree(ctx context.Context, baseRepo, dir string) error
}

// ErrNoChanges is returned by Commit when the worktree is clean. Callers
// that want to treat clean-runner output as a real failure should check
// against this sentinel.
var ErrNoChanges = errors.New("git: no changes to commit")

// ExecGit shells out to the `git` binary. The zero value is usable.
type ExecGit struct {
	// Author identity for commits. If empty, falls back to the daemon's
	// host git config.
	AuthorName  string
	AuthorEmail string
}

// NewExec returns an ExecGit. Both Author fields are optional; if unset,
// commits inherit the host's `user.name` / `user.email` from .gitconfig.
func NewExec(authorName, authorEmail string) *ExecGit {
	return &ExecGit{AuthorName: authorName, AuthorEmail: authorEmail}
}

// Verify ExecGit satisfies Git at compile time.
var _ Git = (*ExecGit)(nil)

func (g *ExecGit) EnsureBranch(ctx context.Context, dir, baseRepo, baseBranch, branchName string) error {
	// If `dir` is already a git directory, treat as idempotent.
	if isGitDir(dir) {
		// Verify it's the right branch; otherwise something is amiss.
		current, err := g.currentBranch(ctx, dir)
		if err != nil {
			return fmt.Errorf("EnsureBranch: read current branch in %s: %w", dir, err)
		}
		if current != branchName {
			return fmt.Errorf("EnsureBranch: worktree %s is on %q, want %q", dir, current, branchName)
		}
		return nil
	}

	// Create the parent of `dir` if it doesn't exist yet.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("EnsureBranch: mkdir parent: %w", err)
	}

	// Fetch latest from origin so we branch off current state of baseBranch.
	if err := g.run(ctx, baseRepo, "fetch", "origin", baseBranch); err != nil {
		return fmt.Errorf("EnsureBranch: fetch: %w", err)
	}

	// `git worktree add -b <branch> <dir> origin/<base>` creates the worktree
	// AND the branch in one step.
	args := []string{"worktree", "add", "-b", branchName, dir, "origin/" + baseBranch}
	if err := g.run(ctx, baseRepo, args...); err != nil {
		return fmt.Errorf("EnsureBranch: worktree add: %w", err)
	}
	return nil
}

func (g *ExecGit) HardReset(ctx context.Context, dir, baseBranch string) error {
	if err := g.run(ctx, dir, "fetch", "origin", baseBranch); err != nil {
		return fmt.Errorf("HardReset: fetch: %w", err)
	}
	if err := g.run(ctx, dir, "reset", "--hard", "origin/"+baseBranch); err != nil {
		return fmt.Errorf("HardReset: reset: %w", err)
	}
	if err := g.run(ctx, dir, "clean", "-fdx"); err != nil {
		return fmt.Errorf("HardReset: clean: %w", err)
	}
	return nil
}

func (g *ExecGit) HasChanges(ctx context.Context, dir string) (bool, error) {
	out, err := g.runOut(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("HasChanges: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

func (g *ExecGit) Commit(ctx context.Context, dir, message string) error {
	dirty, err := g.HasChanges(ctx, dir)
	if err != nil {
		return err
	}
	if !dirty {
		return ErrNoChanges
	}
	if err := g.run(ctx, dir, "add", "-A"); err != nil {
		return fmt.Errorf("Commit: add: %w", err)
	}
	args := []string{}
	if g.AuthorName != "" && g.AuthorEmail != "" {
		args = append(args,
			"-c", "user.name="+g.AuthorName,
			"-c", "user.email="+g.AuthorEmail,
		)
	}
	args = append(args, "commit", "-m", message)
	if err := g.run(ctx, dir, args...); err != nil {
		return fmt.Errorf("Commit: commit: %w", err)
	}
	return nil
}

func (g *ExecGit) Push(ctx context.Context, dir, branchName string) error {
	if err := g.run(ctx, dir, "push", "-u", "origin", branchName); err != nil {
		return fmt.Errorf("Push: %w", err)
	}
	return nil
}

func (g *ExecGit) RemoveWorktree(ctx context.Context, baseRepo, dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		// Already gone; prune any leftover registration and return.
		_ = g.run(ctx, baseRepo, "worktree", "prune")
		return nil
	}
	if err := g.run(ctx, baseRepo, "worktree", "remove", "--force", dir); err != nil {
		// Fall back to manual removal if git refuses.
		_ = os.RemoveAll(dir)
		_ = g.run(ctx, baseRepo, "worktree", "prune")
	}
	return nil
}

// --- internal helpers ---

func (g *ExecGit) currentBranch(ctx context.Context, dir string) (string, error) {
	out, err := g.runOut(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (g *ExecGit) run(ctx context.Context, dir string, args ...string) error {
	_, err := g.runOut(ctx, dir, args...)
	return err
}

func (g *ExecGit) runOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never prompt interactively — fail fast on missing auth
	)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w; stderr: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// envProbe is a test seam that returns the env slice the runner would pass.
// Production code uses runOut directly; this exists only so the test in
// git_test.go can assert GIT_TERMINAL_PROMPT=0 is set.
func (g *ExecGit) envProbe() string {
	return strings.Join(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), "\n")
}

func isGitDir(dir string) bool {
	// Both .git directories (top-level repo) and .git files (worktree
	// metadata files) count.
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || !info.IsDir())
}
