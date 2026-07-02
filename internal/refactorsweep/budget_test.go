package refactorsweep

import (
	"testing"
	"time"

	"github.com/andrewwormald/everflow/internal/provider"
	"github.com/andrewwormald/everflow/internal/runner"
)

func TestCheckBudget(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		state   AgentState
		wantHit bool
	}{
		{
			name:    "no budget configured",
			state:   AgentState{},
			wantHit: false,
		},
		{
			name: "MaxUnits not reached",
			state: AgentState{
				Budget:    runner.Budget{MaxUnits: 5},
				Completed: []CompletedUnit{{}, {}},
			},
			wantHit: false,
		},
		{
			name: "MaxUnits exactly reached via completed",
			state: AgentState{
				Budget:    runner.Budget{MaxUnits: 2},
				Completed: []CompletedUnit{{}, {}},
			},
			wantHit: true,
		},
		{
			name: "MaxUnits reached via completed + blacklisted",
			state: AgentState{
				Budget:      runner.Budget{MaxUnits: 3},
				Completed:   []CompletedUnit{{}, {}},
				Blacklisted: []BlacklistedUnit{{}},
			},
			wantHit: true,
		},
		{
			name: "MaxTokens not reached",
			state: AgentState{
				Budget:      runner.Budget{MaxTokens: 1000},
				TotalTokens: 500,
			},
			wantHit: false,
		},
		{
			name: "MaxTokens exactly reached",
			state: AgentState{
				Budget:      runner.Budget{MaxTokens: 1000},
				TotalTokens: 1000,
			},
			wantHit: true,
		},
		{
			name: "MaxTokens exceeded",
			state: AgentState{
				Budget:      runner.Budget{MaxTokens: 1000},
				TotalTokens: 1001,
			},
			wantHit: true,
		},
		{
			name: "MaxRuntime not reached",
			state: AgentState{
				Budget:    runner.Budget{MaxRuntime: 2 * time.Hour},
				StartedAt: now.Add(-1 * time.Hour),
			},
			wantHit: false,
		},
		{
			name: "MaxRuntime exactly reached",
			state: AgentState{
				Budget:    runner.Budget{MaxRuntime: time.Hour},
				StartedAt: now.Add(-1 * time.Hour),
			},
			wantHit: true,
		},
		{
			name: "MaxRuntime — StartedAt zero is safe (no check)",
			state: AgentState{
				Budget: runner.Budget{MaxRuntime: time.Hour},
				// StartedAt zero-value
			},
			wantHit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := checkBudget(&tt.state, now)
			hit := reason != ""
			if hit != tt.wantHit {
				t.Errorf("checkBudget() hit=%v want=%v (reason=%q)", hit, tt.wantHit, reason)
			}
		})
	}
}

// TestDiscover_BudgetPause verifies that discover() transitions to StatusPaused
// when a budget limit is exceeded, rather than proceeding to the next unit.
func TestDiscover_BudgetPause(t *testing.T) {
	tests := []struct {
		name  string
		state AgentState
	}{
		{
			name: "MaxUnits reached",
			state: AgentState{
				Budget:      runner.Budget{MaxUnits: 1},
				Completed:   []CompletedUnit{{UnitID: "svc-a"}},
				Queue:       []string{"svc-b"},
				InFlight:    map[string]provider.MR{},
			},
		},
		{
			name: "MaxTokens reached",
			state: AgentState{
				Budget:      runner.Budget{MaxTokens: 100},
				TotalTokens: 150,
				Queue:       []string{"svc-a"},
				InFlight:    map[string]provider.MR{},
			},
		},
		{
			name: "MaxRuntime reached",
			state: AgentState{
				Budget:    runner.Budget{MaxRuntime: time.Hour},
				StartedAt: time.Now().Add(-2 * time.Hour),
				Queue:     []string{"svc-a"},
				InFlight:  map[string]provider.MR{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDeps(t, &fakeProvider{})
			r := newRun(t, &tt.state)
			next, err := d.discover(t.Context(), r)
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			if next != StatusPaused {
				t.Errorf("want StatusPaused, got %v", next)
			}
			if r.Object.PauseReason == "" {
				t.Error("PauseReason should be set when budget exceeded")
			}
		})
	}
}
