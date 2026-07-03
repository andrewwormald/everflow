# ADR-0039: Thread planner's per-increment rationale into the runner's Goal

**Status**: Accepted
**Date**: 2026-07-03

## Context

Run `b21a0cc6` (the 2026-07-02 dogfood spike against `andrewwormald/everflow`)
produced a **mega-PR (#5) that bundled five unrelated items** in a single
merge — items 2, 3, 4, 5, and 7 of the six-item early-access-hardening
spec all landed in one commit. The spec's cross-cutting constraints
explicitly required *"one concern per MR"*, so this was a discipline
failure by the tool the spec exists to test.

Diagnosis: the runner is under-briefed on scope.

`internal/refactorsweep/workflow.go` constructs the runner Request as:

```go
req := runner.Request{
    Worktree:     worktree,
    SkillCommand: fmt.Sprintf("/everflow-unit %s", unitID),
    Goal:         r.Object.Goal,   // ← whole-spec Goal, verbatim
    UnitID:       unitID,
    Budget:       r.Object.Budget,
}
```

For a spec whose `goal:` field enumerates several deliverables (e.g. *"Add
X, refactor Y, ship Z"*), the whole shopping list ends up in `req.Goal`.
The runner then sees under `## Task`: **the entire multi-item goal**,
alongside an opaque unit-id string like `increment-2`. The planner's
per-increment rationale — which knows this increment covers item Y only
— lives on `AgentState.Plan[i].Rationale` but is never propagated to the
runner's prompt.

Result: a reasonable LLM interpretation of *"Task: five things. Unit:
increment-2."* is *"I'm doing part of a five-item task; my label is 2;
let me be efficient and do as much as I can."* The runner is behaving
rationally given what it can see.

## Decision

The runner's `req.Goal` is prepended with the planner's rationale for
the current increment, under a labelled scope header, above the
whole-spec Goal. Order (top of prompt down):

1. `PromptInjection` — if present, single-use, highest priority (user
   override; unchanged from prior behaviour)
2. **`## Scope for this increment (from the planner)`** — this
   increment's rationale
3. **`## Full spec goal (context)`** — the whole-spec Goal, unchanged

Concretely: a new helper `applyIncrementScope(req, state, unitID)`
looks up the latest `Plan` entry with matching `UnitID` and prepends
its Rationale (with header) to `req.Goal`. Called from both `work()`
and `invokeForEvent()` — the two step-bodies that invoke the per-unit
runner. `discoverSpec()` still runs on the raw spec Goal because the
planner IS the thing choosing scope; wrapping it in scope would be
circular.

When no `Plan` entry exists for the unit (sweep-mode Runs, legacy
Runs, or edge cases), `applyIncrementScope` is a no-op — the runner
gets `req.Goal = r.Object.Goal` as before. Fully backwards-compatible.

## Alternatives considered

- **Add an `IncrementGoal` field to `PlannedIncrement`**, distinct from
  `Rationale`, that the planner is instructed to fill with a
  narrower-than-the-spec sub-goal. More structured, but requires
  changing the planner's prompt protocol AND the workflow types.
  The existing `Rationale` field already contains the per-increment
  scope the planner wrote — reusing it is the smallest change that
  works. If planner output quality degrades, we can add `IncrementGoal`
  as a follow-up.
- **Reject commits above a size threshold as a hard gate.** Blunt; also
  addresses only the symptom, not the cause. A well-scoped increment
  might legitimately touch many files (e.g. a mechanical rename); the
  gate would false-positive there. Leaves the underlying under-briefing
  in place. Kept in the follow-up list; not the root fix.
- **Post-work review agent that grades scope adherence via a second
  LLM call.** Real token cost per increment; the whole point of the
  ADR-0035 hygiene work was to *reduce* runner spend. Rejected as
  disproportionate.

## Consequences

- Per-increment scope propagates through to the runner deterministically.
  The b21a0cc6 mega-PR failure mode requires the planner to explicitly
  say "this increment covers everything" for the runner to over-scope —
  a much narrower failure mode.
- `applyIncrementScope` is called from both `work()` and
  `invokeForEvent()`, so reviewer-comment-triggered runner calls also
  get the same scope narrowing. A comment on MR #5 for item 2 asking
  "please add a test" won't re-invoke the runner against the whole spec.
- `PromptInjection` remains the highest-priority signal. A user's
  `/everflow prompt` still overrides both the rationale AND the spec
  Goal, which is the intended escape hatch.
- Legacy Runs (no Plan entries, or sweep-mode Runs where the discover
  step doesn't emit a Plan) fall through to the previous behaviour —
  no breakage.
- Slight prompt-length increase per work turn (~200-1000 chars for a
  typical planner rationale). Negligible against a 10-20KB spec Goal
  and a full worktree context; certainly cheap compared to a
  second-LLM-call gate.
- Doesn't obsolete the "diff-size warning" idea from the same
  investigation. That remains a useful defence-in-depth for the case
  where the planner itself over-scopes an increment. Deferred as a
  follow-up.

## Tests

Three unit tests in `internal/refactorsweep/workflow_test.go`:

- `TestWork_ThreadsPlanRationaleIntoRunnerGoal` — asserts the runner's
  Goal contains BOTH the per-increment rationale AND the full spec
  Goal, in that order, under the correct scope header.
- `TestWork_NoPlanEntry_UsesGoalAsBefore` — asserts backwards
  compatibility for Runs without a matching Plan entry.
- `TestWork_PlanRationale_StacksWithPromptInjection` — asserts the
  three-layer ordering (user injection → planner rationale → spec
  Goal) and that `PromptInjection` is still consumed single-use.
