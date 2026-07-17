package reconciler

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luno/workflow"
	"github.com/luno/workflow/adapters/memrecordstore"
	"github.com/luno/workflow/adapters/memrolescheduler"

	"github.com/andrewwormald/everflow/internal/eventstream"
	"github.com/andrewwormald/everflow/internal/refactorsweep"
	"github.com/andrewwormald/everflow/internal/store"
)

const testWorkflowName = "refactor-sweep-reconciler-test"

func seedRecord(t *testing.T, rs workflow.RecordStore, runID string, runState workflow.RunState, status refactorsweep.AgentStatus, state refactorsweep.AgentState, createdAt time.Time) {
	t.Helper()
	objJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal AgentState: %v", err)
	}
	rec := &workflow.Record{
		WorkflowName: testWorkflowName,
		ForeignID:    "fid-" + runID,
		RunID:        runID,
		RunState:     runState,
		Status:       int(status),
		Object:       objJSON,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}
	if err := rs.Store(t.Context(), rec); err != nil {
		t.Fatalf("seed record %s: %v", runID, err)
	}
}

func TestIsStuck(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	threshold := 30 * time.Minute

	tests := []struct {
		name         string
		status       refactorsweep.AgentStatus
		lastProgress time.Time
		want         bool
	}{
		{
			name:         "stale in-flight Run is flagged",
			status:       refactorsweep.StatusWorking,
			lastProgress: now.Add(-time.Hour),
			want:         true,
		},
		{
			name:         "fresh in-flight Run is not flagged",
			status:       refactorsweep.StatusWorking,
			lastProgress: now.Add(-time.Minute),
			want:         false,
		},
		{
			name:         "stale Discovering Run is flagged",
			status:       refactorsweep.StatusDiscovering,
			lastProgress: now.Add(-time.Hour),
			want:         true,
		},
		{
			name:         "stale non-in-flight status is never flagged",
			status:       refactorsweep.StatusAwaitingMerge,
			lastProgress: now.Add(-time.Hour),
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsStuck(tt.status, tt.lastProgress, now, threshold)
			if got != tt.want {
				t.Errorf("IsStuck() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLastProgress(t *testing.T) {
	fallback := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	started := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)
	ended := time.Date(2026, 7, 17, 11, 5, 0, 0, time.UTC)

	tests := []struct {
		name  string
		state refactorsweep.AgentState
		want  time.Time
	}{
		{
			name: "history present uses last turn's end time",
			state: refactorsweep.AgentState{
				History: []refactorsweep.Turn{
					{StartedAt: started.Add(-time.Hour), EndedAt: started.Add(-time.Hour + time.Minute)},
					{StartedAt: started, EndedAt: ended},
				},
			},
			want: ended,
		},
		{
			name:  "history empty uses fallback",
			state: refactorsweep.AgentState{},
			want:  fallback,
		},
		{
			name: "turn still in-flight uses started time",
			state: refactorsweep.AgentState{
				History: []refactorsweep.Turn{
					{StartedAt: started, EndedAt: time.Time{}},
				},
			},
			want: started,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LastProgress(tt.state, fallback)
			if !got.Equal(tt.want) {
				t.Errorf("LastProgress() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScan(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	threshold := 30 * time.Minute
	stale := now.Add(-time.Hour)
	fresh := now.Add(-time.Minute)

	rs := memrecordstore.New()

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000001", workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000002", workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: fresh, EndedAt: fresh}}}, fresh)

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000003", workflow.RunStateRunning, refactorsweep.StatusAwaitingMerge,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000004", workflow.RunStatePaused, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000005", workflow.RunStateCompleted, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	got, err := Scan(t.Context(), rs, testWorkflowName, now, threshold)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	want := []string{"00000000-0000-0000-0000-000000000001"}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Errorf("Scan() = %v, want %v", got, want)
	}
}

// retriggerObj and retriggerStatus are a minimal workflow Type/Status pair
// used only to exercise Retrigger against a real workflow.Workflow, so the
// tests below drive the vendored library's own step consumer rather than
// re-implementing its idempotency guard.
type retriggerObj struct{}

type retriggerStatus int

const (
	retriggerStatusA retriggerStatus = 1
	retriggerStatusB retriggerStatus = 2
)

func (s retriggerStatus) String() string {
	if s == retriggerStatusB {
		return "B"
	}
	return "A"
}

func TestRetrigger_EventShape(t *testing.T) {
	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())

	record := workflow.Record{
		WorkflowName: "retrigger-shape-test",
		ForeignID:    "fid-1",
		RunID:        "00000000-0000-0000-0000-000000000001",
		RunState:     workflow.RunStateRunning,
		Status:       int(retriggerStatusA),
		Meta:         workflow.Meta{Version: 3},
	}

	rec, err := streamer.NewReceiver(t.Context(), workflow.Topic(record.WorkflowName, record.Status), "shape-test")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	if err := Retrigger(t.Context(), streamer, record); err != nil {
		t.Fatalf("Retrigger() error = %v", err)
	}

	e, ack, err := rec.Recv(t.Context())
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	defer ack()

	wantTopic := workflow.Topic(record.WorkflowName, record.Status)
	wantHeaders := map[workflow.Header]string{
		workflow.HeaderForeignID:     record.ForeignID,
		workflow.HeaderWorkflowName:  record.WorkflowName,
		workflow.HeaderTopic:         wantTopic,
		workflow.HeaderRunID:         record.RunID,
		workflow.HeaderRunState:      "2",
		workflow.HeaderRecordVersion: "3",
	}

	// Event.ForeignID carries the RunID (what the step consumer's
	// lookupFn keys on), not the business ForeignID — see Retrigger's
	// comment on this.
	if e.ForeignID != record.RunID {
		t.Errorf("ForeignID = %q, want %q (the RunID)", e.ForeignID, record.RunID)
	}
	if e.Type != record.Status {
		t.Errorf("Type = %d, want %d", e.Type, record.Status)
	}
	for k, want := range wantHeaders {
		if got := e.Headers[k]; got != want {
			t.Errorf("header %q = %q, want %q", k, got, want)
		}
	}
}

// TestRetrigger_SkipsStaleOrDuplicate drives a real workflow.Workflow (using
// the project's sqlite-backed EventStreamer) so the assertion exercises the
// vendored library's own stepConsumer idempotency guard rather than a
// reimplementation of it: an event whose HeaderRecordVersion no longer
// matches the record's current version is skipped, not double-processed.
func TestRetrigger_SkipsStaleOrDuplicate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())
	recordStore := memrecordstore.New()

	var processed atomic.Int32
	builder := workflow.NewBuilder[retriggerObj, retriggerStatus]("retrigger-skip-test")
	builder.AddStep(retriggerStatusA, func(ctx context.Context, r *workflow.Run[retriggerObj, retriggerStatus]) (retriggerStatus, error) {
		processed.Add(1)
		return retriggerStatusB, nil
	}, retriggerStatusB)

	wf := builder.Build(streamer, recordStore, memrolescheduler.New(), workflow.WithoutOutbox())
	wf.Run(ctx)
	t.Cleanup(wf.Stop)

	const foreignID = "fid-1"
	obj, err := workflow.Marshal(&retriggerObj{})
	if err != nil {
		t.Fatalf("marshal retriggerObj: %v", err)
	}
	seeded := &workflow.Record{
		WorkflowName: wf.Name(),
		ForeignID:    foreignID,
		RunID:        "00000000-0000-0000-0000-000000000001",
		RunState:     workflow.RunStateRunning,
		Status:       int(retriggerStatusA),
		Object:       obj,
	}
	if err := recordStore.Store(ctx, seeded); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	// Snapshot exactly what reconciler.Scan would have seen: the stuck
	// record before anything re-triggers it.
	stale, err := recordStore.Latest(ctx, wf.Name(), foreignID)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}

	if err := Retrigger(ctx, streamer, *stale); err != nil {
		t.Fatalf("Retrigger() error = %v", err)
	}

	waitFor(t, func() bool { return processed.Load() == 1 })

	current, err := recordStore.Latest(ctx, wf.Name(), foreignID)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if current.Status != int(retriggerStatusB) {
		t.Fatalf("record status = %d, want %d (StatusB) after first retrigger processed", current.Status, retriggerStatusB)
	}

	// Retrigger again using the now-stale snapshot (Meta.Version from
	// before the first retrigger advanced it). The real consumer must
	// skip this rather than reprocessing the record.
	if err := Retrigger(ctx, streamer, *stale); err != nil {
		t.Fatalf("Retrigger() (stale) error = %v", err)
	}

	// Give the consumer a chance to (wrongly) reprocess before asserting
	// it didn't.
	time.Sleep(100 * time.Millisecond)
	if got := processed.Load(); got != 1 {
		t.Errorf("processed = %d, want 1 (duplicate/stale retrigger must be skipped)", got)
	}
}

// TestSweeper_Run drives the real ticker-based loop (rather than calling
// sweepOnce directly) so the assertion exercises Run's tick-and-sweep wiring,
// not just the underlying Scan/Retrigger calls it composes.
func TestSweeper_Run(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())
	recordStore := memrecordstore.New()

	stale := time.Now().Add(-time.Hour)
	fresh := time.Now().Add(-time.Minute)
	const threshold = 30 * time.Minute

	stuckRunID := "00000000-0000-0000-0000-000000000010"
	freshRunID := "00000000-0000-0000-0000-000000000011"

	// seedRecord always stamps WorkflowName as testWorkflowName, so the
	// Sweeper under test must target that same workflow name.
	seedRecord(t, recordStore, stuckRunID, workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)
	seedRecord(t, recordStore, freshRunID, workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: fresh, EndedAt: fresh}}}, fresh)

	topic := workflow.Topic(testWorkflowName, int(refactorsweep.StatusWorking))
	rec, err := streamer.NewReceiver(ctx, topic, "sweeper-test")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	sweeper := &Sweeper{
		Store:        recordStore,
		Streamer:     streamer,
		WorkflowName: testWorkflowName,
		Interval:     10 * time.Millisecond,
		Threshold:    threshold,
	}
	go sweeper.Run(ctx)

	recvCtx, recvCancel := context.WithTimeout(ctx, 2*time.Second)
	defer recvCancel()
	e, ack, err := rec.Recv(recvCtx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	ack()

	if e.ForeignID != stuckRunID {
		t.Errorf("retriggered run = %q, want %q (the stuck run)", e.ForeignID, stuckRunID)
	}

	// The fresh run must never be retriggered. Nothing here advances the
	// stuck record's status (there's no step consumer wired up), so the
	// loop keeps re-sending for it every tick; drain a few more of those
	// and confirm the fresh run's ID never appears among them.
	for i := 0; i < 5; i++ {
		recvCtx2, recvCancel2 := context.WithTimeout(ctx, 200*time.Millisecond)
		e2, ack2, err := rec.Recv(recvCtx2)
		recvCancel2()
		if err != nil {
			break
		}
		ack2()
		if e2.ForeignID == freshRunID {
			t.Fatalf("fresh run %q was retriggered, want only %q", freshRunID, stuckRunID)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout")
}
