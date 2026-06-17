// Package runner is the abstraction between the workflow loop and a coding
// agent (Claude Code, Qwen Code, OpenHands). See ADR-0007 and ADR-0008.
package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/andrewwormald/everflow/internal/refactorsweep"
)

type Runner interface {
	Name() string
	Run(ctx context.Context, req Request) (Response, error)
}

// Request is what the workflow hands a runner per invocation. Bounded by
// design: only this unit's scope, not the whole refactor's history.
type Request struct {
	Worktree     string
	SkillCommand string // "/refactor-logrus-to-slog services/payments"
	Goal         string
	UnitID       string
	UnitContext  string

	// Replayed inputs for "address comment" / "fix CI" invocations:
	CommentBody string // populated for address-comment invocations
	CIFailure   string // populated for fix-CI invocations (last ~2KB of log)

	Timeout time.Duration
	Budget  refactorsweep.Budget
}

// Response is what a runner reports back. Maps onto the workflow's next state
// transition via Decision.
type Response struct {
	Decision  refactorsweep.Decision
	Summary   string                 // one-paragraph "what I did this invocation"
	Question  string                 // populated when Decision == Ask
	Learnings Learnings              // see ADR-0018; populated when the subagent surfaces patterns worth caching
	Tokens    int
	StartedAt time.Time
	EndedAt   time.Time
}

// Learnings are the subagent's optional output to feed the cheap-filter loop.
// Bounded by the per-Run cap (50 entries) before flagging for review;
// promotion to global phrases is manual via `everflow phrases promote`.
type Learnings struct {
	AddPhrases  []string `json:"add_phrases"`   // skip-phrases to append to the per-Run file
	SkillUpdate string   `json:"skill_update"`  // optional revised SKILL.md content
}

// Registry holds runners by name. Runners self-register at init() time.
type Registry struct {
	runners map[string]Runner
}

func NewRegistry() *Registry { return &Registry{runners: map[string]Runner{}} }

func (r *Registry) Register(rn Runner) { r.runners[rn.Name()] = rn }

func (r *Registry) Get(name string) (Runner, error) {
	rn, ok := r.runners[name]
	if !ok {
		return nil, fmt.Errorf("unknown runner %q (registered: %v)", name, r.Names())
	}
	return rn, nil
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.runners))
	for k := range r.runners {
		out = append(out, k)
	}
	return out
}
