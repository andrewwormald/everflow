package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// setupWorktree creates a git worktree under `dir` if one doesn't already
// exist, based on `baseRepo`@`baseBranch`. The worktree is on a fresh branch
// named `branchName`. Safe to re-invoke on daemon restart: if the worktree
// already exists at `dir`, it's reused.
func setupWorktree(ctx context.Context, baseRepo, baseBranch, dir, branchName string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return nil // already set up
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}

	out, err := runGit(ctx, baseRepo, "worktree", "add", "-b", branchName, dir, baseBranch)
	if err != nil {
		return fmt.Errorf("git worktree add: %w: %s", err, out)
	}
	return nil
}

// refreshWorktree resets the worktree to the latest baseBranch from origin.
// Called before each skill invocation so the agent always works against
// fresh code, and any local commits from the prior pass are wiped (the agent
// is expected to push directly to MR branches, not to the worktree's branch).
func refreshWorktree(ctx context.Context, dir, baseBranch string) error {
	if _, err := runGit(ctx, dir, "fetch", "origin", baseBranch); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	if _, err := runGit(ctx, dir, "reset", "--hard", "origin/"+baseBranch); err != nil {
		return fmt.Errorf("git reset --hard: %w", err)
	}
	if _, err := runGit(ctx, dir, "clean", "-fdx"); err != nil {
		return fmt.Errorf("git clean: %w", err)
	}
	return nil
}

// removeWorktree tears down the worktree directory and prunes the registration
// from the base repo. Idempotent.
func removeWorktree(ctx context.Context, baseRepo, dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	if _, err := runGit(ctx, baseRepo, "worktree", "remove", "--force", dir); err != nil {
		// fall back to manual removal if git refuses
		_ = os.RemoveAll(dir)
		_, _ = runGit(ctx, baseRepo, "worktree", "prune")
		return nil
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(buf.String()), err
	}
	return strings.TrimSpace(buf.String()), nil
}
