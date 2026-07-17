package reconciler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/luno/workflow"
	"github.com/luno/workflow/adapters/memrecordstore"

	"github.com/andrewwormald/everflow/internal/refactorsweep"
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
