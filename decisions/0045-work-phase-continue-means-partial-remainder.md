# ADR-0045: Work-phase `DecisionContinue` means "partial slice, remainder follows"

**Status**: Accepted
**Date**: 2026-07-16

## Context

`runner.DecisionContinue` was originally a planning-phase-only concept:
the planner returns it to say "there's another increment after this
one." The work phase (the runner invocation that actually implements a
unit) had no legitimate use for it — a work turn was expected to either
finish the unit (`Done`) or fail/ask.

In practice, some units are bigger than a single work turn can safely
complete. Before this change, a work-phase runner facing an oversized
unit had two bad options: silently ship a partial diff as if the unit
were finished (the planner then believes the unit is done and moves on,
losing the remainder), or stall/hallucinate a `Done` it hadn't earned.
Neither lets the unit be split further.

`internal/refactorsweep/workflow.go`'s `work()` step body already
special-cases `DecisionContinue` from a work turn (see the comment
above the `case DecisionContinue` block, added alongside the
`RemainderNote` field on `PlannedIncrement` in the prior increment):
treat it like `Done` for shipping purposes (ship what's in the worktree
if dirty, blacklist if clean), but additionally record the runner's own
account of what's left onto the Plan entry via
`updatePlanRemainder`. That mechanism existed, but two things were
still missing: the `decisionProtocol` string the runner is prompted
with described `continue` as planning-only, so a work-phase agent had
no instruction that returning `continue` mid-unit was a sanctioned
outcome; and `buildPlanningPrompt` never rendered `RemainderNote`, so
even when a work turn did populate it, the planner's next iteration
couldn't see it.

## Decision

Redefine `<everflow-decision>continue</everflow-decision>` to have a
context-dependent meaning, and make both ends of the loop honor it:

1. **`decisionProtocol`** (`internal/runner/claude/claude.go`) now
   tells the agent that during a work turn, `continue` means the unit
   didn't fit in one turn, a real partial slice was shipped, and the
   summary should state clearly what's done and what's left. This is
   the same prompt text for both planning and work invocations — the
   protocol doesn't split by call site, since the runner's behavior is
   already conditioned on the same marker regardless of who's asking.

2. **`buildPlanningPrompt`** (`internal/refactorsweep/workflow.go`) now
   renders each Plan entry's `RemainderNote`, when set, as a nested
   line: `<unit> shipped a partial slice; remaining work: <note>`. The
   planner sees this on its very next iteration and can schedule the
   remainder as a follow-on increment instead of assuming the unit is
   closed.

The work-phase handling of `DecisionContinue` itself (ship-if-dirty /
blacklist-if-clean, record `RemainderNote` via `updatePlanRemainder`)
was implemented in the prior increment; this increment closes the loop
by making the redefinition legible to the agent and visible to the
planner.

## Alternatives considered

- **Separate marker for work-phase partial completion** (e.g.
  `<everflow-decision>partial: <note></everflow-decision>`), distinct
  from planning's `continue`. More explicit, but doubles the marker
  vocabulary the agent has to track and requires `ParseDecision` to
  learn a new verb. Reusing `continue` with a context-dependent meaning
  keeps the protocol small; the two call sites already interpret the
  same Decision differently (planner: schedule next increment; work:
  ship partial + record remainder), so overloading it is consistent
  with how the rest of the Decision enum is already call-site-scoped.
- **Split `decisionProtocol` into two variants** (one appended for
  planning invocations, one for work invocations). Rejected for the
  same reason `BuildPrompt` doesn't otherwise branch on call site: it
  would add a parameter to every call site and a second string to keep
  in sync. A single paragraph covering both cases is small enough not
  to need that.

## Consequences

- An oversized unit can now be split across turns without the runner
  having to pretend it's finished or silently truncate scope. The
  spec's original goal — "let the work-phase agent split an oversized
  unit further and hand the remainder back to the planner" — is now
  achievable end-to-end: work turn signals `continue` + remainder →
  `updatePlanRemainder` records it → `buildPlanningPrompt` surfaces it
  → planner schedules the follow-on increment.
- `ParseDecision` needed no changes — `continue` already parses to
  `runner.DecisionContinue` regardless of call site; only the prompt
  text and the planning-prompt renderer changed.
- The agent must now judge, from prompt text alone, whether a unit
  genuinely doesn't fit in one turn versus reaching for `continue` as
  an easy out on a unit it could have finished. The protocol text says
  explicitly not to use it to avoid finishing small units; if this
  proves insufficient in practice, a follow-up could add a stricter
  gate (e.g. requiring a non-trivial diff to already exist before
  `continue` is accepted from a work turn).
- No change to legacy Runs or sweep-mode Runs: `RemainderNote` is
  `omitempty` and `buildPlanningPrompt` only renders the extra line
  when it's set, so Plan entries without one render exactly as before.

## Tests

- `TestBuildPlanningPrompt_SurfacesRemainderNote`
  (`internal/refactorsweep/workflow_test.go`) — asserts a Plan entry's
  `RemainderNote` renders as `"<unit> shipped a partial slice;
  remaining work: <note>"` in the built planning prompt.
- `TestWork_RunnerContinue_RecordsRemainderOnPlanEntry` and
  `TestWork_RunnerDone_NoRemainderNote` (same file, prior increment)
  cover the work-phase side: `continue` records the remainder,
  `done` never does.
