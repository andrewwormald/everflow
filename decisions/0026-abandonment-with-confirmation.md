# ADR-0026: Abandonment-with-confirmation (two-tap stop)

**Status**: Accepted
**Date**: 2026-06-22

## Context

[ADR-0017](0017-author-privilege-model.md) gave the Run author
`/everflow stop` — a one-tap cancellation that closes in-flight MRs and
transitions to `StatusCancelled`. That's the right primitive for "I know
this is wrong, kill it."

For spec-driven Runs ([ADR-0024](0024-spec-as-run.md)), there's a softer
ask: "let me sleep on it." The author may want to halt the loop without
the irrevocable feel of `/stop`. Specifically articulated by the user on
2026-06-22: "ability to abandon via comment with a confirmation step
('are you sure?') for the author."

This ADR records the two-tap abandonment flow that satisfies that ask.

## Decisions

### 1. `/everflow abandon` is a separate verb from `/everflow stop`

Both terminate the Run. They differ in tap-count:

| Verb | Taps | Use when |
|---|---|---|
| `/everflow stop` | 1 (immediate) | You're sure. Run terminates on this comment. |
| `/everflow abandon` | 2 within 12h | You want to reconsider. First tap requests confirmation; second tap confirms. |

Both end at the same terminal state (`StatusCancelled`) with the same
cleanup (close in-flight MRs, remove worktrees). Only the path differs.

### 2. New state: `StatusAwaitingAbandonConfirm`

```
AwaitingMerge ── /everflow abandon ──► AwaitingAbandonConfirm
   Paused    ── /everflow abandon ──► AwaitingAbandonConfirm
                          │
            ┌─────────────┼─────────────┐
            │             │             │
       /everflow      timer 12h    any other
       abandon                     activity
       (second tap)                  │
            │             │           │
            ▼             ▼           ▼
         Cancelled  AwaitingMerge  AwaitingMerge
       (cleanup,    (post "window  (post "abandon
        final MR     expired" ack)  cancelled" ack)
        comment)
```

A dedicated state makes the intent observable. A future `everflow status`
showing "this Run is awaiting your abandon confirmation, expires in 6h"
is immediately useful — a flag on AgentState wouldn't surface that.

### 3. 12-hour confirmation window

`AddTimeout` on `StatusAwaitingAbandonConfirm` fires at +12h with
destination `StatusAwaitingMerge`. Window length picked to span:
- A normal sleep cycle (catch "I changed my mind in the morning")
- A weekend (Friday abandon, Monday review)
- One business day at worst

Shorter (1h, 4h) feels rushed; longer (24h, 48h) leaves the state
machine in a weird "almost dead but not really" zone for too long.

The timer handler posts "⏰ Abandon confirmation window (12h) expired —
staying with the Run." on the latest in-flight MR so the author sees
the window closed.

### 4. Restrictive semantics during the confirmation window

`StatusAwaitingAbandonConfirm` has the strictest event-handling rules
in the state machine. From `resume()`:

```
if r.Status == StatusAwaitingAbandonConfirm:
    if event is author /everflow abandon:
        → cmdAbandon → StatusCancelled (confirm)
    else:
        → dropAbandonConfirm → StatusAwaitingMerge
```

This means:

- **/everflow abandon from the author** → confirms
- **/everflow abandon from a NON-author** → drops back (the privilege
  model from ADR-0017 still applies)
- **/everflow pause (or any other verb) from the author** → drops back.
  The author's pause isn't honored; they have to /pause again from
  AwaitingMerge. This is intentional — only abandon-related verbs are
  processed in the confirmation window.
- **A normal review comment from a reviewer** → drops back. The
  comment itself is dropped (we don't process review feedback while
  awaiting an abandonment decision); the reviewer must re-comment.
- **CI events (pipeline_succeeded/failed, mr_merged/closed)** → drop
  back. Once dropped back to AwaitingMerge the next event will be
  processed normally.

Each drop posts a one-line ack comment so the audit shows the abandon
was cancelled.

### 5. Available from AwaitingMerge AND Paused

`/everflow abandon` from a paused Run also enters the confirmation
window. The author may want to abandon precisely *because* the Run is
stuck. Don't gate the verb on "must be making progress."

### 6. AbandonRequestedAt on AgentState

A `time.Time` field stamped at first-tap. Cleared on confirm (Run is
terminal anyway), on timer expiry, or on any other drop-back. Used
for:

- `everflow status` display ("requested 4h ago; 8h remaining")
- Audit/forensics
- Future: a "remind me at 11h" hook before the window expires (out of
  scope for v1)

## Alternatives considered

- **One verb (/stop) with an optional `--confirm` flag** — confused two
  semantics into one verb; harder to teach. Two verbs with distinct
  names (stop / abandon) make the choice obvious.
- **No confirmation; only /stop exists** — works, but doesn't satisfy
  the customer ask (sleep on it without committing).
- **Confirmation via a different mechanism (emoji react, separate
  comment)** — reactions aren't reliably exposed in webhook payloads;
  a separate comment ID would require introducing a new state-mapping
  field. Two-tap of the same verb is the simplest pattern users already
  understand from "shutdown now/?".
- **Tracked on AgentState only (no new status)** — would lose the
  "this Run is in a confirmation window" affordance in audit / status
  views. The state is real; making it explicit is worth one enum value.

## Consequences

- `AgentStatus` gains one value (`StatusAwaitingAbandonConfirm` = 9).
  Build() adds the state's callback + timer registration.
- `AwaitingMerge` and `Paused` both get `StatusAwaitingAbandonConfirm`
  as a new allowed callback destination.
- `resume()` gains a pre-dispatch check: if status is AwaitingAbandonConfirm,
  early-route to confirm-or-drop. This is the only state with
  restrictive event handling — every other state processes events via
  the normal filter/dispatch path.
- `controls.go` gains `cmdAbandon` (handles both taps, branches on
  current status) and the help message mentions both `/stop` and
  `/abandon`.
- `time` import added to controls.go.
- 7 new tests in `abandon_test.go` cover first-tap, second-tap-confirm,
  non-abandon-drops-back, non-author-cannot-confirm,
  other-verb-drops-back, timeout-drops-back, and abandon-from-paused.
- Timer runs at a *minimum* of 12h after first tap. The workflow
  library's TimeoutStore polls; actual fire latency depends on poll
  frequency. Acceptable — minutes of skew on a 12h window is invisible.
- Idle cost during the window is zero (event-driven; no LLM activity).
  The Run is *paused* in the operational sense even though
  `StatusPaused` isn't the workflow status.
