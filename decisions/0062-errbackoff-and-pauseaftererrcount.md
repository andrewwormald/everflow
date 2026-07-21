# ADR-0062: Configure ErrBackOff and PauseAfterErrCount on every step

**Status**: Accepted
**Date**: 2026-07-21

## Context

`internal/refactorsweep.Build` never configured `workflow.ErrBackOff`
or any retry ceiling on any of its `AddStep` calls, so every step ran
with `luno/workflow`'s bare defaults: a flat 1-second backoff, retried
forever, no cap.

Observed live, on the same running daemon, over a ~46 minute window:

1. A `StatusDiscovering` step (`d.discover` → `planner: claude exec`)
   failed 783 times at a ~3.5s cadence, continuously hammering the
   `claude` CLI/API with no growing backoff and no ceiling. Stopping it
   required killing the daemon process directly — even an explicit
   `/syntropy abandon` never got a chance to be processed, since
   `StatusDiscovering` has no registered callback and the same stuck
   consumer loop never yielded to anything else.
2. A `StatusInitiated` step (`d.setup` → `authenticated user: gitlab
   api`) failed continuously on a genuinely expired GitLab OAuth token
   (`buildProviders` in `main.go` captures the token once at daemon
   startup and never refreshes it — a separate bug), which — because
   of the same unthrottled retry — started tripping GitLab's own rate
   limiter from repeated hammering with credentials that were never
   going to succeed without a daemon restart.

Both are the same underlying gap: no real backoff, and nothing to stop
a step from retrying a doomed action forever. `github.com/luno/workflow`
v0.5.0 (upgraded from v0.4.0) adds `workflow.PauseAfterErrCount(count
int)`, an `Option` that moves a record's `RunState` to `RunStatePaused`
after `count` consecutive errors on a step — the library's own words,
"similar to a Dead Letter Queue... won't block the workflow's consumers
and can be investigated and retried later on."

## Decision

Every `AddStep`-based step in `Build` (`StatusInitiated`,
`StatusDiscovering`, `StatusWorking`) is configured with:

- `workflow.ErrBackOff(45 * time.Second)` — a flat duration (this
  library version has no exponential backoff option), far above the
  1s default, low enough that a genuinely transient blip still
  recovers reasonably fast.
- `workflow.PauseAfterErrCount(6)` — six consecutive failures (~4.5
  minutes at the 45s cadence) trips the breaker, moving the Run to
  `RunStatePaused` instead of retrying a seventh time.

`AddCallback`-based steps (`d.resume` on `StatusAwaitingMerge`/
`StatusPaused`/`StatusAwaitingAbandonConfirm`) are untouched — this
library version's `AddCallback` returns no `stepUpdater` and has no
`.WithOptions`, so this fix only covers `AddStep`.

The library's own `pausedRecordsRetry` (auto-resume a paused record
after 1 hour, enabled by default) is left **enabled** rather than
disabled. A `b.OnPause(d.onAutoPause)` hook fires on every transition
into `RunStatePaused` — including a re-pause after a failed auto-retry
— and posts a bot comment on every in-flight MR/PR, so a genuinely
broken step (e.g. the expired-credential case) still surfaces
repeatedly to a human, while a merely transient one (a rate-limited
call, a network blip) self-heals without anyone needing to reply
`/syntropy resume` by hand.

`RunStatePaused` (a workflow-library concept) is distinct from our own
`AgentStatus.StatusPaused` (a human decision point awaiting a reply,
set by `d.cmdPause`) — a Run can be `RunStatePaused` while its
`AgentStatus` still reads `Working`/`Discovering`/`Initiated`, whatever
step it was stuck on. Left unaddressed, this would be invisible: today
`syntropy status`/`list` only ever read `AgentStatus`. Fixed by:

- `recordToStatus` (feeds both the `/status` JSON endpoint and the
  `status`/`list` CLI text output) appends `(auto-paused: <reason>)`
  to the status string when `RunState == RunStatePaused` and
  `AgentStatus != StatusPaused`.
- `directList` (the sqlite-only `syntropy list` fallback) does the
  same, more tersely.
- `directResume` (the direct-store-write escape hatch used when the
  daemon is unreachable, or explicitly for this case) now handles
  `RunState == RunStatePaused` as a distinct branch from the existing
  `Cancelled`/`Failed`/`StatusPaused` cases: it restores the
  `AgentStatus` the step was actually stuck in rather than forcing it
  back to `StatusDiscovering` the way the pre-existing revival path
  does, since a step auto-paused mid-`Working` should resume in
  `Working`, not restart planning.

## Alternatives considered

- **Disable the library's pause-retry entirely
  (`workflow.DisablePauseRetry()`), requiring an explicit
  `/syntropy resume` for every auto-pause.** Considered and reversed
  during review: since `onAutoPause` posts a comment on every pause —
  including repeats after a failed auto-retry — auto-retry isn't
  silent. Disabling it would trade away real self-healing value (the
  `claude exec` incident above was, on reproduction, a transient
  failure that any retry would likely have cleared) for marginal extra
  safety the circuit breaker already provides on its own.
- **Hand-roll exponential backoff on top of `ErrBackOff`.** Not
  attempted — `ErrBackOff` is a flat duration in this library version;
  layering exponential growth on top would need to intercept and
  re-wrap the consumer loop, more complexity than the incident
  actually calls for. A single conservative flat value plus a hard
  circuit breaker addresses the observed failure mode without it.
- **Different `PauseAfterErrCount`/`ErrBackOff` per step** (e.g. a
  lower count for `setup`'s single auth check vs. `work`'s
  multi-request runner calls). Not done — no evidence yet that the
  failure modes differ enough to justify different tuning; revisit if
  operational experience says otherwise.

## Consequences

- A step that fails 6 times in a row (about 4.5 minutes) now stops
  itself and notifies a human instead of retrying indefinitely — the
  783-retry, 46-minute incident this ADR responds to cannot recur in
  that form.
- A Run auto-paused this way is genuinely invisible to `syntropy
  status`/`list` if the surfacing fix above is ever reverted or
  bypassed — anyone touching `recordToStatus`/`directList` again
  should keep the `RunState` check.
- `directResume`'s two branches (pre-existing terminal-status revival
  vs. this ADR's `RunStatePaused` revival) must be kept distinct: the
  first intentionally forces `StatusDiscovering`, the second
  intentionally does not. Collapsing them would silently reintroduce
  the "always restart planning" behavior for a Run that was actually
  stuck mid-`Working`.
- `onAutoPause` cannot include the specific triggering error in its bot
  comment — the failing step's own `AgentState` mutations (e.g.
  `LastError`) are never persisted on the erroring attempt that trips
  the breaker, only `record.Meta.RunStateReason` (set by the library's
  own `Pause()` call, a fixed string) is guaranteed current. The
  comment points readers at the daemon log instead of guessing.
- The GitLab-token-staleness bug that caused incident #2 is not fixed
  by this ADR — it's a separate root cause (tracked separately) that
  this circuit breaker merely contains the blast radius of.
