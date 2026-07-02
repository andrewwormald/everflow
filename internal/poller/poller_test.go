package poller

import (
	"testing"
	"time"
)

func TestAuthBackoffDuration(t *testing.T) {
	tests := []struct {
		failures int
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{0, 30 * time.Second, 30 * time.Second},
		{1, 2 * time.Minute, 2 * time.Minute},
		{2, 8 * time.Minute, 8 * time.Minute},
		{3, 32 * time.Minute, 32 * time.Minute},
		{4, 2 * time.Hour, 2 * time.Hour}, // capped
		{10, 2 * time.Hour, 2 * time.Hour}, // still capped
	}
	for _, tt := range tests {
		got := authBackoffDuration(tt.failures)
		if got < tt.wantMin || got > tt.wantMax {
			t.Errorf("authBackoffDuration(%d) = %v, want [%v, %v]",
				tt.failures, got, tt.wantMin, tt.wantMax)
		}
	}
}
