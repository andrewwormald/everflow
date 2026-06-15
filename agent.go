package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/luno/workflow"
	"github.com/luno/workflow/adapters/memrecordstore"
	"github.com/luno/workflow/adapters/memrolescheduler"
	"github.com/luno/workflow/adapters/memstreamer"
	"github.com/luno/workflow/adapters/memtimeoutstore"
)

type AgentStatus int

const (
	StatusUnknown   AgentStatus = 0
	StatusInitiated AgentStatus = 1 // Run created; worktree not yet built
	StatusIdle      AgentStatus = 2 // waiting for the next interval to fire
	StatusRunning   AgentStatus = 3 // skill is executing right now
)

func (s AgentStatus) String() string {
	switch s {
	case StatusInitiated:
		return "Initiated"
	case StatusIdle:
		return "Idle"
	case StatusRunning:
		return "Running"
	default:
		return "Unknown"
	}
}

// AgentState is the durable per-Run state. It's the L3 memory from the
// proposal — survives daemon restarts (with a durable RecordStore; the PoC
// uses memrecordstore so it doesn't, yet).
type AgentState struct {
	// Set at Trigger, immutable after:
	Goal         string        `json:"goal"`
	RunnerName   string        `json:"runner_name"`
	SkillCommand string        `json:"skill_command"`
	Interval     time.Duration `json:"interval"`
	BaseRepo     string        `json:"base_repo"`
	BaseBranch   string        `json:"base_branch"`
	Worktree     string        `json:"worktree"`
	Branch       string        `json:"branch"`

	// Updated each pass:
	History []Turn `json:"history"`
}

type Turn struct {
	Index     int       `json:"index"`
	Runner    string    `json:"runner"`
	Summary   string    `json:"summary"`
	Stderr    string    `json:"stderr,omitempty"`
	ExitCode  int       `json:"exit_code"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Error     string    `json:"error,omitempty"`
}

// BuildWorkflow wires the scheduled-skill state machine:
//
//	Initiated --setupStep--> Idle <--timeout--> Running --runPass--> Idle
//
// On startup the daemon calls Trigger() with an initial AgentState; the
// workflow drives the rest. The Idle->Running transition is timer-driven
// (AddTimeout fires at +Interval); the Running->Idle transition is the
// runPass step returning StatusIdle. The cycle repeats until the Run is
// Cancelled or the process is stopped.
func BuildWorkflow(name string) *workflow.Workflow[AgentState, AgentStatus] {
	b := workflow.NewBuilder[AgentState, AgentStatus](name)

	b.AddStep(StatusInitiated, setupStep, StatusIdle).
		WithOptions(workflow.PauseAfterErrCount(3))

	// Idle is a holding state. The timeout below drives the cycle.
	b.AddTimeout(StatusIdle,
		func(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], now time.Time) (time.Time, error) {
			return now.Add(r.Object.Interval), nil
		},
		func(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], now time.Time) (AgentStatus, error) {
			return StatusRunning, nil
		},
		StatusRunning,
	)

	b.AddStep(StatusRunning, runPass, StatusIdle).
		WithOptions(workflow.PauseAfterErrCount(5))

	b.OnComplete(func(ctx context.Context, r *workflow.TypedRecord[AgentState, AgentStatus]) error {
		log.Printf("run %s completed", r.RunID)
		return nil
	})

	return b.Build(
		memstreamer.New(),
		memrecordstore.New(),
		memrolescheduler.New(),
		workflow.WithTimeoutStore(memtimeoutstore.New()),
	)
}

func setupStep(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	log.Printf("[%s] setup worktree at %s (branch %s, base %s)",
		r.RunID[:8], r.Object.Worktree, r.Object.Branch, r.Object.BaseBranch)
	if err := setupWorktree(ctx, r.Object.BaseRepo, r.Object.BaseBranch, r.Object.Worktree, r.Object.Branch); err != nil {
		return 0, fmt.Errorf("setup worktree: %w", err)
	}
	return StatusIdle, nil
}

func runPass(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	turnIdx := len(r.Object.History)
	log.Printf("[%s] pass #%d starting (skill: %s)", r.RunID[:8], turnIdx, r.Object.SkillCommand)

	if err := refreshWorktree(ctx, r.Object.Worktree, r.Object.BaseBranch); err != nil {
		t := Turn{Index: turnIdx, StartedAt: time.Now(), EndedAt: time.Now(), Error: err.Error()}
		r.Object.History = append(r.Object.History, t)
		return 0, fmt.Errorf("refresh worktree: %w", err)
	}

	runner, err := getRunner(r.Object.RunnerName)
	if err != nil {
		return 0, err
	}

	// Cap a single pass at 2x the interval so a runaway invocation can't
	// stall the cycle indefinitely. Tunable; this is a PoC default.
	timeout := r.Object.Interval * 2
	if timeout < 5*time.Minute {
		timeout = 5 * time.Minute
	}

	resp, runErr := runner.Run(ctx, RunRequest{
		Worktree:     r.Object.Worktree,
		SkillCommand: r.Object.SkillCommand,
		Timeout:      timeout,
	})

	turn := Turn{
		Index:     turnIdx,
		Runner:    runner.Name(),
		Summary:   resp.Summary,
		Stderr:    resp.Stderr,
		ExitCode:  resp.ExitCode,
		StartedAt: resp.StartedAt,
		EndedAt:   resp.EndedAt,
	}
	if runErr != nil {
		turn.Error = runErr.Error()
	}
	r.Object.History = append(r.Object.History, turn)

	log.Printf("[%s] pass #%d done (exit %d, %s)", r.RunID[:8], turnIdx, resp.ExitCode, resp.EndedAt.Sub(resp.StartedAt).Round(time.Second))
	if runErr != nil {
		return 0, runErr
	}
	return StatusIdle, nil
}
