# ADR-0025: Planner-driven `discover()` for spec mode

**Status**: Accepted
**Date**: 2026-06-22

## Context

[ADR-0024](0024-spec-as-run.md) introduced spec mode but left `discover()`
as a queue-popper. This ADR records *how* the planner integrates: what
the runner sees as input, what it returns, how the workflow translates
that into state transitions.

The shape is constrained by the existing `Runner` interface (ADR-0007 +
ADR-0008). We don't add fields to `Runner.Request` / `Runner.Response`;
instead, the planning call distinguishes itself from a work call via
SkillCommand and the absence of a UnitID, and the existing Decision
enum already carries the four signals we need.

## Decisions

### 1. Planning is a runner invocation, not a separate mechanism

`discover()` in spec mode calls `runner.Run(ctx, req)` with:
- `SkillCommand = "/everflow-plan"`
- `UnitID = ""` (planning is not unit-scoped — that's the signal to the runner adapter that this is a planning call)
- `Goal = <built-up planning prompt with spec body + plan history>`

The runner adapter is responsible for routing the planning command to a
prompt that asks "what should we do next?" — different adapters may
ship different planning skill prompts, but the contract back to
everflow is the same Decision enum.

Trade-off accepted: this couples the planner to the same Runner used
for work. A specialised "planner-only" Runner (e.g. Opus for planning,
Sonnet for work) would need a separate registry slot. v1 keeps one
Runner for both phases; v2 can add a `PlannerName string` field on
AgentState if the cost case demands it.

### 2. Decision enum carries the planner's outcome

| Decision | Planning meaning | Workflow transition |
|---|---|---|
| `Continue` | "Next increment is X" — Summary is the rationale | Generate `increment-N` unitID, append `PlannedIncrement`, set CurrentUnit → `StatusWorking` |
| `Done` | "Spec is fully implemented" | `StatusCompleted` |
| `NoChange` | "Nothing actionable to plan right now" | `StatusCompleted` (treated as Done) |
| `Ask` | "I need the author's input before deciding" | `StatusPaused` with Question stored in PauseReason |
| `Fail` | "Planning is impossible" | `StatusFailed` |

The choice to treat `NoChange` as terminal (rather than "no-op, try again
later") reflects v1's lack of a "try again after a delay" mechanism for
planning. If the planner can't find anything actionable, the spec is
either done or genuinely stuck; both are correct exits from this Run.
v2 may add a "planning timer" that re-attempts after N hours.

### 3. UnitIDs are auto-generated `increment-N`

The planner doesn't need to produce a meaningful unit ID — its
`Continue` response carries a Summary (the rationale), which is what
the next iteration's planner-prompt will surface. We assign
`increment-1`, `increment-2`, ... in sequence based on `len(Plan)+1`.

Alternatives:
- **Planner names the unit** — adds complexity for no real win; the
  rationale is the meaningful payload, not the ID.
- **UUID** — opaque; loses the human-readable ordering in `everflow
  status` output.
- **MR-number-based** — circular; we don't have an MR yet when we plan.

Sequential `increment-N` is debuggable, stable, and the audit trail in
git commits + MR titles + `everflow status` reads naturally.

### 4. Planner sees plan history, not full event log

The planning prompt assembled by `buildPlanningPrompt(state)` includes:

```
# Goal
<state.Goal>

# Spec
<state.SpecBody>

# Plan history (last N entries)
- increment-1 [completed]: <rationale>
- increment-2 [blacklisted]: <rationale>
- ...

# Merged so far: K. Blacklisted: M.

# Your task
Decide the next increment... (instructions)
```

It does *not* include:
- The full event log (`r.Object.History`)
- Comment threads from completed MRs
- Per-turn token counts

Reason: context budget. The planner's job is to plan, not to recap. The
plan history is the curated summary of what's been done; the merged/
blacklisted counts give scale. Anything richer would inflate the
context window and the cost per planning call without changing the
decision the planner actually makes.

### 5. /everflow prompt injection applies to planning calls too

If `r.Object.PromptInjection` is set, it's prepended to the planning
Goal and then cleared (single-use, same as in work() and
invokeForEvent). This is how the author steers spec-driven planning:
`/everflow prompt focus on the auth services first` lands on the next
planning iteration, not the next code change.

### 6. Plan outcome updates on terminal unit events

`markUnitMerged` and `markUnitBlacklisted` now call `updatePlanOutcome`
to flip a planned increment's outcome from `in_flight` to `completed`
or `blacklisted`. The next planning iteration sees the fresh state in
the prompt.

`work()`'s "runner returned Done but worktree is clean" path also calls
`updatePlanOutcome("blacklisted")` for consistency — the increment
didn't ship, but it didn't get blacklisted via an MR either.

## Alternatives considered

- **Add `NextIncrement *Increment` to `runner.Response`** — would let the
  planner explicitly name the unit. Adds a field to the cross-runner
  contract for one mode's benefit. Rejected: Summary + Continue is
  enough.
- **Separate `Planner` interface** — clean conceptually, but doubles
  the registry surface and makes "what model are we using?" a per-Run-
  twice question. The Runner-with-SkillCommand approach is one
  abstraction doing two jobs cheaply.
- **Planning runs without a worktree** — initially considered, but the
  planner often needs to inspect the codebase to make decisions ("which
  services still import logrus?"). For v1 we pass a `planning/` worktree
  path; setting it up as a real read-only git worktree is a follow-up
  (a fakeRunner doesn't care; real adapters may).

## Consequences

- The first planning call costs one LLM invocation that *doesn't* produce
  code. This is the cost of agent-driven incremental work — the win is
  that the agent can make 47 contextual decisions over a refactor that
  a static `--units` list couldn't capture.
- Cost scales linearly with the number of increments. For a refactor
  that needs 10 increments: 10 planning calls + 10 work calls + N event
  calls. v2 budget enforcement (max-tokens/max-runs on AgentState.Budget)
  remains the relief valve.
- `Plan` history is the durable artefact of spec mode. `everflow status`
  output (currently from `/everflow status`) should include the Plan in
  spec mode; sweep mode shows Queue+Completed instead.
- The planning runner's reliability is now a load-bearing concern: a
  flaky planner means a flaky refactor. The `Ask` path is the explicit
  out — when the planner is uncertain, parking for human input is
  better than a wrong decision.
- Real planner-runner adapters are still TODO. The tests use the
  in-package `fakeRunner` to drive every Decision branch; production
  will need a `claude` adapter that translates `/everflow-plan` into a
  meaningful prompt template.
