// Package refactorsweep defines the v1 state machine: AgentState held in the
// workflow's Run.Object, the AgentStatus enum that drives transitions, and
// the Decision/Turn/Budget types shared with the runner.
//
// See ../../DESIGN.md § "The state machine" and ADRs 0014, 0015, 0017.
package refactorsweep

import (
	"time"

	"github.com/andrewwormald/everflow/internal/provider"
)

// AgentStatus enumerates the workflow states. Cycles allowed:
//
//	Initiated → Discovering → Working → AwaitingMerge → Working (next unit)
//	                                  → Paused (author intervention)
//	                                  → Completed (no units left)
type AgentStatus int

const (
	StatusUnknown       AgentStatus = 0
	StatusInitiated     AgentStatus = 1 // Run created; webhook not yet registered
	StatusDiscovering   AgentStatus = 2 // looking for the next unit
	StatusWorking       AgentStatus = 3 // subagent making the change + opening MR
	StatusAwaitingMerge AgentStatus = 4 // MR open; webhook-driven idle
	StatusPaused        AgentStatus = 5 // author intervention required
	StatusCompleted     AgentStatus = 6 // refactor done; no units left
	StatusFailed        AgentStatus = 7 // unrecoverable; worktree kept for forensics
)

func (s AgentStatus) String() string {
	return [...]string{
		"Unknown", "Initiated", "Discovering", "Working",
		"AwaitingMerge", "Paused", "Completed", "Failed",
	}[s]
}

// AgentState is the per-Run durable object. Everything everflow needs to
// resume after a daemon restart lives here.
type AgentState struct {
	// Set at Trigger, immutable after:
	Goal            string         `json:"goal"`
	ProviderName    string         `json:"provider_name"`     // "gitlab" | "github"
	ProjectID       string         `json:"project_id"`
	BaseBranch      string         `json:"base_branch"`
	Author          provider.User  `json:"author"`            // see ADR-0017
	Concurrency     int            `json:"concurrency"`       // semaphore size; v1 = 1
	Budget          Budget         `json:"budget"`
	SkillPath       string         `json:"skill_path"`        // ~/.everflow/runs/<runID>/SKILL.md
	FilterPath      string         `json:"filter_path"`       // ~/.everflow/runs/<runID>/note_added.star
	DiscoveryPath   string         `json:"discovery_path"`    // optional discovery rule
	WebhookID       string         `json:"webhook_id"`        // platform's hook ID, for cleanup
	WebhookSecret   string         `json:"webhook_secret"`    // HMAC secret we registered with
	WebhookURL      string         `json:"webhook_url"`       // public URL we registered

	// Mutated through the Run's lifecycle:
	Queue            []string             `json:"queue"`              // unit IDs awaiting processing
	InFlight         map[string]provider.MR `json:"in_flight"`        // unit ID → MR
	Completed        []CompletedUnit      `json:"completed"`
	Blacklisted      []BlacklistedUnit    `json:"blacklisted"`
	CurrentUnit      string               `json:"current_unit"`       // populated while StatusWorking | StatusAwaitingMerge
	History          []Turn               `json:"history"`
	LastError        string               `json:"last_error"`
	PauseReason      string               `json:"pause_reason"`       // populated when StatusPaused

	// Counters for the "is learning working?" signal (DESIGN.md open question 1):
	EventsSeen           int `json:"events_seen"`
	EventsSkippedByFilter int `json:"events_skipped_by_filter"`
	SubagentInvocations  int `json:"subagent_invocations"`
}

// Budget caps a Run's cumulative cost. Hit any of these and the Run pauses
// (StatusPaused) for author intervention.
type Budget struct {
	MaxUnits  int `json:"max_units"`   // total units processed (shipped + blacklisted)
	MaxTokens int `json:"max_tokens"`  // cumulative across all subagent invocations
	MaxRuntime time.Duration `json:"max_runtime"`
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

// Decision is what a runner invocation tells the workflow to do next.
// Mapped from the Runner's structured output ([ADR-0008](../../decisions/0008-native-structured-output.md)).
type Decision int

const (
	DecisionUnknown    Decision = 0
	DecisionContinue   Decision = 1 // make progress; stay in current macro-state
	DecisionAsk        Decision = 2 // pause and ask the author via MR comment
	DecisionDone       Decision = 3 // unit complete; ship the MR
	DecisionFail       Decision = 4 // unit unrecoverable; blacklist + move on
	DecisionNoChange   Decision = 5 // nothing to do this invocation (e.g. comment was conversational)
)

func (d Decision) String() string {
	return [...]string{
		"Unknown", "Continue", "Ask", "Done", "Fail", "NoChange",
	}[d]
}
