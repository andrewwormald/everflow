// Package filter evaluates the agent-authored Starlark filter on every event.
// See ADR-0018.
//
// The interface is defined here; the Starlark implementation lives behind it
// so we can stub it for tests and so a future filter language could replace
// the implementation without touching callers.
package filter

import (
	"github.com/andrewwormald/everflow/internal/provider"
)

// Outcome is what the filter tells the workflow to do with the event.
type Outcome int

const (
	OutcomeUnknown        Outcome = 0
	OutcomeSkip           Outcome = 1 // ignore the event; no LLM call, no state change
	OutcomeInvokeSubagent Outcome = 2 // run a subagent against this event
	OutcomeControlCommand Outcome = 3 // /everflow ... — route to control handler
	OutcomePause          Outcome = 4 // mark the Run paused for author intervention
)

func (o Outcome) String() string {
	return [...]string{"Unknown", "Skip", "InvokeSubagent", "ControlCommand", "Pause"}[o]
}

// Filter evaluates a per-event decision. v1 implementations:
//   - StarlarkFilter: load a .star file, evaluate filter(event, state, phrases)
//   - StubFilter: hardcoded for tests and for the scaffold commit
//
// state is the workflow's AgentState passed as `any` to avoid an import
// cycle (refactorsweep imports filter; filter would otherwise import
// refactorsweep). The Starlark adapter will marshal it into a dict; the
// StubFilter ignores it.
type Filter interface {
	Eval(event provider.Event, state any, phrases PhraseSet) (Outcome, error)
}

// PhraseSet is the per-Run skip-phrase store. Read-only from the filter's
// perspective; writes happen in the runner's response-handling code.
type PhraseSet interface {
	Contains(text string) bool
	All() []string
}

// StubFilter is the placeholder implementation for the scaffold commit.
// Returns InvokeSubagent on everything that doesn't match the control-command
// prefix, so the scaffold compiles + behaves predictably until the real
// Starlark eval lands.
type StubFilter struct{}

func (StubFilter) Eval(event provider.Event, _ any, _ PhraseSet) (Outcome, error) {
	if event.Kind == provider.EventNoteAdded && event.IsAuthor &&
		len(event.Note.Body) > 0 && event.Note.Body[0] == '/' {
		return OutcomeControlCommand, nil
	}
	return OutcomeInvokeSubagent, nil
}
