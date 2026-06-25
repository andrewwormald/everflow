# ADR-0034: Comment-driven loop is the primary interaction; Paused permits self-loop

**Status**: Accepted
**Date**: 2026-06-25

## Context

The first poll-mode spike opened a real Draft MR, exercised the
`Initiated → Working → AwaitingMerge` path cleanly, and then jammed at
the first author comment. The daemon went silent and the log spammed
once every 30 seconds with:

```
poller: dispatch note event err="workflow.Callback: current status not
defined in graph: current=Paused, next=Paused"
```

Three independent issues compounded into the symptom:

1. **The Paused state had no permitted self-loop.** The
   `b.AddCallback(StatusPaused, d.resume, ...)` permitted next-statuses
   of `AwaitingMerge / Discovering / Failed / Cancelled /
   AwaitingAbandonConfirm` — every plausible exit — but not
   `StatusPaused` itself. So when an event arrived for a Paused Run and
   `resume()` legitimately returned `(StatusPaused, nil)` (stay paused;
   nothing to do), the workflow library refused the no-op transition.
   The whole event was dropped, including any `/everflow resume`
   control command — making the Paused state unreachable in
   practice.

2. **`invokeForEvent` treated `git.ErrNoChanges` from `Commit` as a
   fatal pause.** The flow is: HasChanges (true) → run runner → Commit.
   When the runner ran `go build` to verify a change (producing the
   compiled binary as an untracked artefact) but didn't actually modify
   any source files, the binary made the worktree dirty, but our
   binary-filter (ADR-0032) correctly excluded it from staging. The
   resulting `ErrNoChanges` was treated as fatal — pausing the Run with
   reason "git Commit failed during address_comment: no changes to
   commit". The sister `!dirty` branch already handled "runner made no
   changes" gracefully (info comment, stay AwaitingMerge); the
   `ErrNoChanges` branch was inconsistent.

3. **Discussion threads were never resolved.** When the runner pushed a
   change in response to a reviewer comment, the thread stayed open
   forever — the reviewer had to mark it resolved manually, breaking
   the autonomy promise. The provider interface had no
   `ResolveDiscussion` method at all.

## Decision

1. **Allow `StatusPaused → StatusPaused`** in the callback graph. The
   `resume()` handler can now return `StatusPaused` to acknowledge an
   event without exiting Paused — covering "non-control note arrived
   during pause" cases without losing the event. The same self-loop is
   added to `StatusAwaitingAbandonConfirm` for symmetric defensive
   handling of events that don't trigger a confirm/cancel transition.

2. **`invokeForEvent` ErrNoChanges path → `StatusAwaitingMerge`**,
   matching the existing `!dirty` semantics. Both mean the same thing:
   "the runner addressed the comment without producing commit-worthy
   diff". The system posts an info comment, resolves the discussion
   (the comment was answered, even if verbally), and stays
   AwaitingMerge. Pause is reserved for genuine operational failures
   (push auth, runner timeout) where the author must intervene.

3. **`Provider.ResolveDiscussion(ctx, projectID, mrIID, discussionID)`**
   added to the interface. `Note.DiscussionID` and
   `NotePoll.DiscussionID` carry the platform-specific thread ID from
   webhook decode and `ListNotesSince`. After a successful push in
   `invokeForEvent`, the daemon calls `ResolveDiscussion` to close the
   thread automatically. GitLab implements it via
   `PUT /merge_requests/.../discussions/<id>?resolved=true`; GitHub's
   stub is a no-op until the GraphQL `resolveReviewThread` path lands.
   Empty `discussionID` is a no-op so callers don't need to guard.

## Alternatives considered

- **Make `default.star` classify author comments as PAUSE.** Was the
  initial design hypothesis. Inverted because the MR comment thread is
  the *primary* interaction channel for change-request loops — pausing
  on every author comment defeats the purpose. The filter already
  classifies plain notes as INVOKE; the Paused path was being entered
  via the failed-commit branch, not the filter.
- **Persist the poller's `LastSeenNoteIDs` via a SaveSnapshot hook
  independent of workflow.Callback.** Considered as a defence against
  any future dispatch-error replay loop. Deferred: once the dispatch
  errors are fixed (this ADR), `resume()` advances
  `LastSeenNoteIDs` on the workflow callback path naturally. Adding
  another persistence channel duplicates the watermark logic and
  introduces a write-conflict surface. Revisit if a new class of
  transient dispatch failures emerges.
- **Resolve threads via PostComment containing a "resolved" marker.**
  Cosmetic only — doesn't actually collapse the thread in either
  platform's UI. Real resolve API is required.
- **Block GitHub Runs entirely until `resolveReviewThread` is
  implemented.** Too disruptive. The no-op stub means GitHub Runs
  function exactly as they did before this ADR — reviewers close
  threads manually — while GitLab gets the autoresolve UX.

## Consequences

- An MR comment thread is now a complete round-trip with everflow: the
  reviewer types a request, the runner addresses it, the change pushes,
  the thread closes automatically. This is the interaction primitive
  the system is built around — the spike's failure here was the first
  signal that the loop hadn't actually been validated end-to-end before.
- The Paused state is now actually entered-and-recoverable.
  `/everflow resume` and `/everflow status` issued while paused will
  route through `resume()` and exit Paused via the appropriate next
  status. Until this ADR, those control commands would have been
  silently dropped on the dispatch-error floor.
- GitHub gains a `ResolveDiscussion` stub but no real implementation.
  Tracking debt: switch to the GraphQL `resolveReviewThread` mutation
  once we surface review-thread node IDs (currently we only have note
  IDs from the REST webhook payload).
- The provider interface grew by one method. Every adapter (real and
  test fake) now needs `ResolveDiscussion`. The fakes return nil; the
  test `fakeProvider` records calls for assertion.
- The `_ = p.PostComment(...)` and `_ = p.ResolveDiscussion(...)`
  best-effort error handling in `invokeForEvent` means a misconfigured
  provider can silently fail to resolve threads. The post-push comment
  ("Pushed but couldn't resolve") makes the misconfiguration visible to
  the reviewer.
