// Package reconciler detects Runs stuck on a lost in-memory event: the
// sync.Cond EventStreamer (ADR-0033) has no durable queue, so an event
// dropped between a daemon restart and its delivery leaves the Run's
// AgentState in StatusWorking or StatusDiscovering forever, with nothing
// to wake it back up. See DESIGN.md § "The state machine".
package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/refactorsweep"
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

// Retrigger builds and sends an event for record to its current status
// topic, carrying the same headers a normal transition would produce (see
// MakeOutboxEventData in the vendored library's event.go). Because the
// event's HeaderRecordVersion is taken from record.Meta.Version, the step
// consumer's existing idempotency guard (step.go's stepConsumer, which
// skips any event whose HeaderRecordVersion doesn't match the record's
// current version) applies unchanged: retriggering a record more than
// once, or retriggering a stale snapshot after the record has since
// advanced, is a no-op rather than a double-process.
func Retrigger(ctx context.Context, streamer workflow.EventStreamer, record workflow.Record) error {
	topic := workflow.Topic(record.WorkflowName, record.Status)

	headers := map[workflow.Header]string{
		workflow.HeaderForeignID:     record.ForeignID,
		workflow.HeaderWorkflowName:  record.WorkflowName,
		workflow.HeaderTopic:         topic,
		workflow.HeaderRunID:         record.RunID,
		workflow.HeaderRunState:      strconv.FormatInt(int64(record.RunState), 10),
		workflow.HeaderRecordVersion: strconv.FormatInt(int64(record.Meta.Version), 10),
	}

	sender, err := streamer.NewSender(ctx, topic)
	if err != nil {
		return fmt.Errorf("new sender for topic %q: %w", topic, err)
	}
	defer sender.Close()

	// The step consumer looks up records by RunID (see stepConsumer in the
	// vendored library's step.go), so despite the Event.ForeignID field
	// name, the value sent here must be the RunID — mirroring how
	// purgeOutbox in outbox.go sends outboxRecord.RunId, not the business
	// ForeignID, as the event's foreign ID.
	if err := sender.Send(ctx, record.RunID, record.Status, headers); err != nil {
		return fmt.Errorf("send retrigger event: %w", err)
	}
	return nil
}

// Sweeper periodically sweeps a workflow's Runs for ones stuck on a lost
// in-memory event (see the package doc) and re-triggers them. It ticks on
// Interval (defaulting to 30s, matching internal/poller's cadence) for as
// long as Run's ctx is live.
type Sweeper struct {
	Store        workflow.RecordStore
	Streamer     workflow.EventStreamer
	WorkflowName string
	Interval     time.Duration
	Threshold    time.Duration
	Logger       *slog.Logger
}

// Run ticks every s.Interval, sweeping stuck Runs each tick. It returns when
// ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = 30 * time.Second
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}

	t := time.NewTicker(s.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce runs a single Scan + Retrigger pass over s.WorkflowName.
func (s *Sweeper) sweepOnce(ctx context.Context) {
	stuck, err := Scan(ctx, s.Store, s.WorkflowName, time.Now(), s.Threshold)
	if err != nil {
		s.Logger.Warn("reconciler: scan", "err", err)
		return
	}

	for _, runID := range stuck {
		record, err := s.Store.Lookup(ctx, runID)
		if err != nil {
			s.Logger.Warn("reconciler: lookup stuck run", "run_id", runID, "err", err)
			continue
		}
		if err := Retrigger(ctx, s.Streamer, *record); err != nil {
			s.Logger.Warn("reconciler: retrigger stuck run", "run_id", runID, "err", err)
			continue
		}
		s.Logger.Info("reconciler: re-triggered stuck run", "run_id", runID)
	}
}
