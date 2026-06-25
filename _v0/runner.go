package main

import (
	"context"
	"fmt"
	"time"
)

// Runner is the integration point between the workflow loop and an underlying
// coding agent (Claude Code, Qwen Code, OpenHands, ...). The workflow does not
// care what the runner does internally; it only cares that an invocation maps
// (RunRequest) -> (RunResponse).
//
// For the PoC, "iteration" = "one full skill execution." A future Iterating
// loop will reuse the same interface with finer-grained semantics.
type Runner interface {
	Name() string
	Run(ctx context.Context, req RunRequest) (RunResponse, error)
}

type RunRequest struct {
	Worktree string
	// SkillCommand is the slash command we pass to the runner, e.g.
	// "/review-babysit --request-reviewers".
	SkillCommand string
	Timeout      time.Duration
}

type RunResponse struct {
	Summary   string // captured stdout (trimmed)
	Stderr    string
	ExitCode  int
	StartedAt time.Time
	EndedAt   time.Time
}

var registry = map[string]Runner{}

func registerRunner(r Runner) {
	registry[r.Name()] = r
}

func getRunner(name string) (Runner, error) {
	r, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown runner %q (registered: %v)", name, runnerNames())
	}
	return r, nil
}

func runnerNames() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
