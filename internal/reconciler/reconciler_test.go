package reconciler

import (
	"testing"
	"time"

	"github.com/andrewwormald/everflow/internal/refactorsweep"
)

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
