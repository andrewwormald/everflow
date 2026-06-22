// Package refactorsweep defines the v1 state machine: AgentState held in the
// workflow's Run.Object, the AgentStatus enum that drives transitions, and
// the Decision/Turn/Budget types shared with the runner.
//
// See ../../DESIGN.md § "The state machine" and ADRs 0014, 0015, 0017.
package refactorsweep

import (
	"time"

	"github.com/andrewwormald/everflow/internal/provider"
	"github.com/andrewwormald/everflow/internal/runner"
)

// AgentStatus enumerates the workflow states. Cycles allowed:
//
//	Initiated → Discovering → Working → AwaitingMerge → Working (next unit)
//	                                  → Paused (author intervention)
//	                                  → Completed (no units left)
type AgentStatus int

const (
	StatusUnknown                AgentStatus = 0
	StatusInitiated              AgentStatus = 1 // Run created; webhook not yet registered
	StatusDiscovering            AgentStatus = 2 // looking for the next unit
	StatusWorking                AgentStatus = 3 // subagent making the change + opening MR
	StatusAwaitingMerge          AgentStatus = 4 // MR open; webhook-driven idle
	StatusPaused                 AgentStatus = 5 // author intervention required
	StatusCompleted              AgentStatus = 6 // refactor done; no units left
	StatusFailed                 AgentStatus = 7 // unrecoverable; worktree kept for forensics
	StatusCancelled              AgentStatus = 8 // author stopped the Run (/everflow stop or /abandon-confirm)
	StatusAwaitingAbandonConfirm AgentStatus = 9 // /everflow abandon issued; awaiting second tap within 12h (ADR-0026)
)

func (s AgentStatus) String() string {
	return [...]string{
		"Unknown", "Initiated", "Discovering", "Working",
		"AwaitingMerge", "Paused", "Completed", "Failed", "Cancelled",
		"AwaitingAbandonConfirm",
	}[s]
}

// AgentState is the per-Run durable object. Everything everflow needs to
// resume after a daemon restart lives here.
type AgentState struct {
	// Set at Trigger, immutable after:
	Goal            string         `json:"goal"`
	ProviderName    string         `json:"provider_name"`     // "gitlab" | "github"
	ProjectID       string         `json:"project_id"`
	BaseRepo        string         `json:"base_repo"`         // local path to a git checkout the daemon can clone worktrees off
	BaseBranch      string         `json:"base_branch"`
	RunnerName      string         `json:"runner_name"`       // "claude" | "qwen" | "openhands" — see ADR-0007
	Budget          runner.Budget  `json:"budget"`
	Author          provider.User  `json:"author"`            // see ADR-0017
	Concurrency     int            `json:"concurrency"`       // semaphore size; v1 = 1

	// Mode picks how discover() behaves: sweep (queue-pop) or spec (planner).
	// See ADR-0024. Empty == ModeSweep for backwards compatibility.
	Mode            string         `json:"mode"`              // "" | "sweep" | "spec"
	SpecPath        string         `json:"spec_path"`         // populated in spec mode
	SpecBody        string         `json:"spec_body"`         // markdown body the planner reads each iteration

	SkillPath       string         `json:"skill_path"`        // ~/.everflow/runs/<runID>/SKILL.md
	FilterPath      string         `json:"filter_path"`       // ~/.everflow/runs/<runID>/note_added.star
	DiscoveryPath   string         `json:"discovery_path"`    // optional discovery rule
	WebhookID       string         `json:"webhook_id"`        // platform's hook ID, for cleanup
	WebhookSecret   string         `json:"webhook_secret"`    // HMAC secret we registered with
	WebhookURL      string         `json:"webhook_url"`       // public URL we registered

	// Mutated through the Run's lifecycle:
	Queue            []string             `json:"queue"`              // unit IDs awaiting processing (sweep mode only)
	Plan             []PlannedIncrement   `json:"plan"`               // planner history (spec mode only) — ADR-0025
	InFlight         map[string]provider.MR `json:"in_flight"`        // unit ID → MR
	Completed        []CompletedUnit      `json:"completed"`
	Blacklisted      []BlacklistedUnit    `json:"blacklisted"`
	CurrentUnit      string               `json:"current_unit"`       // populated while StatusWorking | StatusAwaitingMerge
	History          []Turn               `json:"history"`
	LastError          string             `json:"last_error"`
	PauseReason        string             `json:"pause_reason"`         // populated when StatusPaused
	PromptInjection    string             `json:"prompt_injection"`     // /everflow prompt <text>; consumed by next runner call
	AbandonRequestedAt time.Time          `json:"abandon_requested_at"` // populated when StatusAwaitingAbandonConfirm (ADR-0026)

	// Counters for the "is learning working?" signal (DESIGN.md open question 1):
	EventsSeen           int `json:"events_seen"`
	EventsSkippedByFilter int `json:"events_skipped_by_filter"`
	SubagentInvocations  int `json:"subagent_invocations"`
}

// Mode values for AgentState.Mode. Empty Mode behaves as ModeSweep for
// backwards compatibility with v1 Runs created before this field existed.
const (
	ModeSweep = "sweep"
	ModeSpec  = "spec"
)

// IsSpecMode reports whether this Run plans each increment via the runner
// (spec mode) rather than popping a static queue (sweep mode). Empty Mode
// is treated as sweep.
func (s *AgentState) IsSpecMode() bool {
	return s.Mode == ModeSpec
}

// PlannedIncrement is one entry in the planner's audit trail. Each entry
// records a unit the planner chose, the rationale, and the eventual
// outcome — useful both for debugging "why did the agent pick this?" and
// for re-priming the planner on the next iteration.
type PlannedIncrement struct {
	UnitID    string    `json:"unit_id"`
	Rationale string    `json:"rationale"`            // the planner's stated reason for this increment
	PlannedAt time.Time `json:"planned_at"`
	Outcome   string    `json:"outcome,omitempty"`    // "in_flight" | "completed" | "blacklisted" | ""
}

// CompletedUnit records a shipped MR.
type CompletedUnit struct {
	UnitID    string    `json:"unit_id"`
	MR        provider.MR `json:"mr"`
	MergedAt  time.Time `json:"merged_at"`
	Tokens    int       `json:"tokens"`
}

// BlacklistedUnit records a unit we won't retry. Reason matters for diagnosis
// ("reviewer rejected with: please don't do this here", "CI permanently red").
type BlacklistedUnit struct {
	UnitID    string    `json:"unit_id"`
	MR        provider.MR `json:"mr"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// Turn records one subagent invocation. Useful for audit, debugging,
// and computing the "learning is working" signal.
type Turn struct {
	Index     int       `json:"index"`
	UnitID    string    `json:"unit_id"`         // empty for non-unit invocations
	Runner    string    `json:"runner"`           // "claude" | "qwen" | ...
	Phase     string    `json:"phase"`            // "work" | "address_comment" | "fix_ci"
	Summary   string    `json:"summary"`
	Tokens    int       `json:"tokens"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Error     string    `json:"error,omitempty"`
}

// Decision is re-exported from internal/runner as an alias so step-body
// callers within refactorsweep don't have to type-qualify everywhere.
type Decision = runner.Decision

const (
	DecisionContinue = runner.DecisionContinue
	DecisionAsk      = runner.DecisionAsk
	DecisionDone     = runner.DecisionDone
	DecisionFail     = runner.DecisionFail
	DecisionNoChange = runner.DecisionNoChange
)
