// State machine wiring for the bulk-refactor sweep. See DESIGN.md § "The
// state machine" and ADR-0015.
//
// Step bodies in this file are intentionally stubs — they advance the state
// machine but do not yet talk to providers, runners, or filters. Follow-up
// commits fill them in:
//
//   - setup: register webhook, mirror skill into the worktree
//   - discover: load the discovery rule, refill the queue
//   - work: invoke runner to make the change + open MR
//   - resume: dispatch inbound webhook events via the filter
package refactorsweep

import (
	"context"
	"fmt"
	"io"

	"github.com/luno/workflow"

	"github.com/andrewwormald/everflow/internal/provider"
)

// Deps is the set of collaborators a built workflow needs.
type Deps struct {
	RecordStore   workflow.RecordStore
	TimeoutStore  workflow.TimeoutStore
	EventStreamer workflow.EventStreamer
	RoleScheduler workflow.RoleScheduler
}

// Build wires the state machine described in DESIGN.md. Step bodies are
// stubs; the graph itself is the contract.
func Build(name string, d Deps) *workflow.Workflow[AgentState, AgentStatus] {
	b := workflow.NewBuilder[AgentState, AgentStatus](name)

	b.AddStep(StatusInitiated, setup, StatusDiscovering)

	b.AddStep(StatusDiscovering, discover,
		StatusWorking,   // found a unit
		StatusCompleted, // queue + in-flight + discovery all empty
	)

	b.AddStep(StatusWorking, work,
		StatusAwaitingMerge, // MR opened, now wait for the platform
		StatusFailed,        // subagent gave up before opening MR
	)

	// AwaitingMerge has no AddStep — purely callback-driven. Webhook events
	// dispatch via workflow.Callback into resume(), which returns one of
	// the allowed destinations below.
	b.AddCallback(StatusAwaitingMerge, resume,
		StatusAwaitingMerge,
		StatusDiscovering,
		StatusPaused,
		StatusFailed,
	)

	// Author can resume from Paused via /everflow resume — another callback
	// route. Same handler; it decodes the inbound event and decides.
	b.AddCallback(StatusPaused, resume,
		StatusAwaitingMerge,
		StatusDiscovering,
		StatusFailed,
	)

	return b.Build(
		d.EventStreamer,
		d.RecordStore,
		d.RoleScheduler,
		workflow.WithTimeoutStore(d.TimeoutStore),
	)
}

func setup(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if r.Object.Concurrency <= 0 {
		r.Object.Concurrency = 1
	}
	if r.Object.InFlight == nil {
		r.Object.InFlight = map[string]provider.MR{}
	}
	return StatusDiscovering, nil
}

func discover(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if len(r.Object.Queue) == 0 && len(r.Object.InFlight) == 0 {
		return StatusCompleted, nil
	}
	if len(r.Object.Queue) == 0 {
		return StatusCompleted, nil
	}
	r.Object.CurrentUnit = r.Object.Queue[0]
	r.Object.Queue = r.Object.Queue[1:]
	return StatusWorking, nil
}

func work(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if r.Object.CurrentUnit == "" {
		return StatusFailed, fmt.Errorf("work: no CurrentUnit set")
	}
	return StatusAwaitingMerge, nil
}

func resume(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], _ io.Reader) (AgentStatus, error) {
	return StatusAwaitingMerge, nil
}
