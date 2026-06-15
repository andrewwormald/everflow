package main

import (
	"context"
	"fmt"
	"time"
)

// mockRunner is a no-op runner for demos and local testing without a claude
// install or API credits. It just sleeps briefly and reports a fake summary.
type mockRunner struct{}

func (mockRunner) Name() string { return "mock" }

func (mockRunner) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	start := time.Now()
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return RunResponse{StartedAt: start, EndedAt: time.Now()}, ctx.Err()
	}
	return RunResponse{
		Summary:   fmt.Sprintf("[mock] would have run %q in %s", req.SkillCommand, req.Worktree),
		ExitCode:  0,
		StartedAt: start,
		EndedAt:   time.Now(),
	}, nil
}

func init() {
	registerRunner(mockRunner{})
}
