# ADR-0036 — Budget enforcement: MaxUnits, MaxTokens, MaxRuntime

**Status:** Accepted  
**Date:** 2026-07-02

## Context

`runner.Budget` has carried `MaxUnits`, `MaxTokens`, and `MaxRuntime` fields
since the scaffold, but no code enforced them. A misconfigured Run could spend
unbounded time and tokens before the author noticed. We need a lightweight gate
that pauses the Run when a budget cap is hit so the author can review and
extend if appropriate.

## Decision

Budget enforcement runs at the top of `discover()`, which is the only entry
point to runner invocations. Checking here rather than inside `work()` keeps
the enforcement in one place and avoids adding `StatusPaused` as a valid
transition from `work()`.

Token totals are accumulated per-turn into `AgentState.TotalTokens`. The Claude
runner still returns 0 tokens (no JSON output-mode parsing yet); the field
accumulates correctly and will start reflecting real spend once the runner
emits counts.

`AgentState.StartedAt` is set once in `setup()` on the run's first transition
to `Discovering`. `MaxRuntime` is checked as `time.Since(StartedAt) ≥ MaxRuntime`.

When a budget limit is hit, the Run transitions to `StatusPaused` with a
human-readable `PauseReason`. The author sees the reason in the next
`/everflow status` reply and can either `/everflow resume` (if they raised the
budget on the same run object, which isn't currently possible) or accept that
the run is done. A future increment could add a `/everflow set-budget` verb to
adjust caps without restarting.

## Alternatives considered

**Enforce in `work()`**: Requires adding `StatusPaused` to work's valid
transitions and duplicating the budget check in `invokeForEvent`. Rejected as
more complex for equivalent behaviour — the budget check already runs before
the next unit starts.

**Fail instead of pause**: The run is not in an unrecoverable state when a
budget cap fires; the author may want to continue by restarting with a higher
cap. `Fail` is semantically wrong here. `Pause` gives the author agency.

**Enforce MaxTokens per invocation**: Would require the runner to report
accurate token counts. Deferred until the Claude runner emits real counts.

## Consequences

- Runs with `Budget.MaxUnits > 0` now stop after processing that many units.
- `AgentState` gains two new fields: `TotalTokens` and `StartedAt`; both are
  zero-valued and backward-compatible with older stored records.
- `/everflow status` (MR comment) now shows token usage and the budget limit.
