# ADR-0038 — Poller auth-expiry backoff: Paused + `provider-auth:` marker

**Date:** 2026-07-02  
**Status:** Accepted

## Context

Before this ADR, a provider token expiry caused the poller to log 401 warnings every 30 seconds indefinitely. The Run appeared "alive" in the state machine (`AwaitingMerge`) but was functionally deaf — no new comments or merges reached `resume()`. The in-memory `authBackoff` map (added to `poller.Loop` pre-ADR) already suppressed polling, but this state was invisible: `everflow status` showed `AwaitingMerge` with no explanation, and a daemon restart reset the backoff.

The spec required:
1. The Run's status must reflect the degraded state.
2. A comment should be posted on the in-flight MR so the author knows to refresh.
3. Recovery after a token rotation should be automatic.

## Options considered

### A — New `StatusAwaitingProviderAuth` (10)

Add a dedicated workflow status. The Run transitions `AwaitingMerge → AwaitingProviderAuth` on first auth failure; transitions back on success.

**Pros:** Maximally clear state name; `everflow status` shows a distinct value.  
**Cons:** Requires new `AddCallback` registration (another row in Build()), adds a status value to the String() table, and the practical difference from a distinctly-labelled Paused state is negligible for users.

### B — `StatusPaused` with `provider-auth:` prefix in `PauseReason`

Reuse the existing `Paused` status. The `PauseReason` is set to `"provider-auth: ..."` which is both human-readable and machine-parseable (prefix check). `EventProviderAuthRestored` clears it without requiring a dedicated status callback.

**Pros:** No new status constant; `StatusPaused` callbacks are already registered for `AwaitingMerge` and `Paused` targets; recovery is a single prefix check in `handleProviderAuthEvent`.  
**Cons:** The prefix-based distinction is a convention rather than a type-safe guarantee; `everflow status` shows `Paused` not `AwaitingProviderAuth`.

## Decision

**Option B** — reuse `StatusPaused` with the `provider-auth: ` prefix in `PauseReason`.

Rationale: The `AwaitingProviderAuth` state has identical transition semantics to `Paused`. The marginal clarity of a dedicated status doesn't justify the added state-machine surface area. The `provider-auth:` prefix is easily grepped and is documented in TROUBLESHOOTING.md.

### Implementation

**New event kinds** (in `provider/provider.go`):

- `EventProviderAuthFailure` — poller emits this on the first auth failure for a Run (i.e., `failures == 1` in the `authBackoff` map).
- `EventProviderAuthRestored` — poller emits this on the first clean tick after a prior failure.

**Poller** (`internal/poller/poller.go`):

- On `failures == 1`: dispatch `EventProviderAuthFailure` via `Dispatcher`.
- On recovery (failures reset from >0 to 0): dispatch `EventProviderAuthRestored`.
- Auth events are dispatched *outside* the `authMu` mutex to prevent deadlock if `Dispatcher` calls back into the poller.
- The existing exponential backoff (30s → 2m → 8m → 32m → 2h) remains the cadence; it is in-memory and resets on daemon restart. A future enhancement could persist `NextPollAt` on `AgentState` for daemon-restart durability.

**Workflow** (`internal/refactorsweep/workflow.go`):

- `handleProviderAuthEvent` is called from `resume()` before the "while Paused, drop all events" early-return. This ensures `EventProviderAuthRestored` can clear the pause even when the Run is in `StatusPaused`.
- On `EventProviderAuthFailure`: if not already in an auth-pause, set `PauseReason = "provider-auth: ..."`, post a comment, return `StatusPaused`. Idempotent on repeated events.
- On `EventProviderAuthRestored`: if PauseReason has the `provider-auth:` prefix, clear it and return `StatusAwaitingMerge`. No-op for human-set pause reasons.

## Consequences

- A Run whose token expires stops spamming 401 logs within one poll cycle and enters `StatusPaused` with a `provider-auth:` reason.
- `everflow status <runID>` shows `Paused` and the `provider-auth:` reason in the Pause reason field.
- After refreshing credentials and restarting the daemon, the next 5-min poll tick dispatches `EventProviderAuthRestored` and the Run resumes automatically.
- `everflow resume <runID>` also clears an auth-pause (the control verb `cmdResume` clears `PauseReason` unconditionally).
- TROUBLESHOOTING.md documents this failure mode and its recovery procedure.
