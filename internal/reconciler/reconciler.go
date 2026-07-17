// Package reconciler detects Runs stuck on a lost in-memory event: the
// sync.Cond EventStreamer (ADR-0033) has no durable queue, so an event
// dropped between a daemon restart and its delivery leaves the Run's
// AgentState in StatusWorking or StatusDiscovering forever, with nothing
// to wake it back up. See DESIGN.md § "The state machine".
package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/everflow/internal/refactorsweep"
)

// IsStuck reports whether a Run sitting in status since lastProgress has
// gone stale: only StatusWorking and StatusDiscovering are in-flight states
// that depend on an in-memory event to advance, so any other status is
// never considered stuck. now and lastProgress are passed in rather than
// read from time.Now() so callers (and tests) control elapsed time.
func IsStuck(status refactorsweep.AgentStatus, lastProgress time.Time, now time.Time, threshold time.Duration) bool {
	switch status {
	case refactorsweep.StatusWorking, refactorsweep.StatusDiscovering:
	default:
		return false
	}
	return now.Sub(lastProgress) >= threshold
}

// LastProgress returns when state last made progress: the EndedAt of the
// last Turn in state.History, or its StartedAt if the turn is still
// in-flight (EndedAt zero). If History is empty, it returns fallback
// (typically the Run record's own timestamp), since a Run with no turns
// yet has no history to derive progress from.
func LastProgress(state refactorsweep.AgentState, fallback time.Time) time.Time {
	if len(state.History) == 0 {
		return fallback
	}
	turn := state.History[len(state.History)-1]
	if turn.EndedAt.IsZero() {
		return turn.StartedAt
	}
	return turn.EndedAt
}

// scanPageSize is the page size used when paginating through the
// RecordStore in Scan, mirroring rehydrateSecrets in main.go.
const scanPageSize = 200

// Scan queries rs for Runs in RunStateRunning (excluding Paused, which is a
// deliberate stop rather than a lost event — see Pause in the workflow
// library) and returns the RunIDs of those IsStuck flags as stale. now is
// passed in rather than read from time.Now() so callers control elapsed
// time in tests.
func Scan(ctx context.Context, rs workflow.RecordStore, workflowName string, now time.Time, threshold time.Duration) ([]string, error) {
	var stuck []string
	var offset int64
	for {
		records, err := rs.List(ctx, workflowName, offset, scanPageSize, workflow.OrderTypeAscending,
			workflow.FilterByRunState(workflow.RunStateRunning))
		if err != nil {
			return nil, fmt.Errorf("list records at offset %d: %w", offset, err)
		}
		if len(records) == 0 {
			break
		}
		for _, rec := range records {
			status := refactorsweep.AgentStatus(rec.Status)
			var state refactorsweep.AgentState
			if err := workflow.Unmarshal(rec.Object, &state); err != nil {
				continue
			}
			lastProgress := LastProgress(state, rec.CreatedAt)
			if IsStuck(status, lastProgress, now, threshold) {
				stuck = append(stuck, rec.RunID)
			}
		}
		if int64(len(records)) < scanPageSize {
			break
		}
		offset += int64(len(records))
	}
	return stuck, nil
}
