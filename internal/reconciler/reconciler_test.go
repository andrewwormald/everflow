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
