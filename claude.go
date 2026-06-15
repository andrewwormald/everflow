package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type claudeRunner struct{}

func (claudeRunner) Name() string { return "claude" }

func (claudeRunner) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	args := []string{"-p", req.SkillCommand, "--dangerously-skip-permissions"}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = req.Worktree
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	end := time.Now()

	resp := RunResponse{
		Summary:   strings.TrimSpace(stdout.String()),
		Stderr:    strings.TrimSpace(stderr.String()),
		ExitCode:  cmd.ProcessState.ExitCode(),
		StartedAt: start,
		EndedAt:   end,
	}
	if err != nil {
		return resp, fmt.Errorf("claude -p failed (exit %d): %w; stderr: %s", resp.ExitCode, err, resp.Stderr)
	}
	return resp, nil
}

func init() {
	registerRunner(claudeRunner{})
}
