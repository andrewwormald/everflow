# ADR-0060: Detect MR merge/close by polling MR state, not by inferring it from comments

**Status**: Accepted
**Date**: 2026-07-20

## Context

A Run sits in `StatusAwaitingMerge` with one MR per in-flight unit
(`AgentState.InFlight`). Historically, the only way this state changed was
via inbound comment/pipeline events matched against `InFlight` — there was
no direct signal for "the human merged (or closed) the MR out-of-band."
Left alone, a merged Run would sit in `AwaitingMerge` forever, and any
stray webhook/poll event for that now-dead MR (e.g. a GitLab "merge request
was closed" system note, or a reviewer leaving a trailing comment) risked
being misrouted to the runner.

## Decision

Poll actual MR state and treat merge/close as first-class lifecycle events,
matched against `InFlight` the same way comment events are:

- `internal/poller/poller.go` (`pollRun`) calls `p.GetMRState` for every
  in-flight MR each tick, alongside the existing `ListNotesSince` comment
  poll. `mrStateEvent` maps a `"merged"`/`"closed"` provider state to a
  synthetic `provider.EventMRMerged` / `EventMRClosed`, dispatched through
  the same `Dispatcher` path a webhook would use. `"opened"`/`"locked"`
  produce no event. On a terminal state the poller skips the note poll for
  that unit this tick — `resume()` removes it from `InFlight` right after.
- `internal/refactorsweep/workflow.go` (`resume`) matches every inbound
  event to an in-flight unit via `unitForMR(r.Object.InFlight, ev.MR)`
  *before* branching on event kind. If no unit matches — because the MR
  was already merged/closed and removed from `InFlight` in an earlier
  turn — the event is dropped unconditionally (`return StatusAwaitingMerge,
  nil`), regardless of kind. This is what protects against the incident
  scenario: a comment arriving for an already-merged MR never reaches the
  filter or the runner.
- For a still-tracked unit, `EventMRMerged`/`EventMRClosed` bypass the
  comment filter entirely and go straight to `markUnitMerged` /
  `markUnitBlacklisted`, which delete the unit from `InFlight`, record it
  in `Completed`/`Blacklisted`, clean up its worktree, and return
  `StatusDiscovering` — letting `discover()` either pull the next queued
  unit or complete the Run when both `Queue` and `InFlight` are empty.

## Alternatives considered

- **Infer merge/close from the absence of further activity (timeout)** —
  fragile and slow; a quiet-but-still-open MR is indistinguishable from a
  merged one until some arbitrary timeout fires.
- **Rely solely on provider webhooks for merge/close** — GitLab does send
  merge/close webhooks, but the daemon already needed a poll-based fallback
  for comments (missed webhooks, self-hosted instances without webhook
  access), so state changes go through the same poll path rather than a
  second, webhook-only mechanism.
- **Filter post-merge comments inside the `filter.Eval` step** — would
  require the filter to know about `InFlight` membership, duplicating what
  `unitForMR` already does structurally as a lookup keyed by the live
  in-flight map. Doing the check once, before any event-kind branching in
  `resume`, means every event kind (not just `NoteAdded`) is protected for
  free.

## Consequences

- `InFlight` is the single source of truth for "does this Run still care
  about this MR." Once a unit is removed (merge, close, or otherwise), any
  further event for its MR is inert by construction — no separate
  denylist or per-event merge check is needed.
- The poller must call `GetMRState` once per in-flight MR per tick, an
  extra provider API call beyond the existing note poll; acceptable at
  current polling intervals and Run counts.
- Regression coverage: `TestResume_MRMerged_MovesToCompleted` /
  `TestResume_MRClosed_MovesToBlacklisted` cover the transition itself;
  `TestResume_CommentAfterMerge_DroppedWithoutInvokingRunner` in
  `internal/refactorsweep/workflow_test.go` covers the incident scenario —
  a comment arriving after the merge event must not invoke the runner.
