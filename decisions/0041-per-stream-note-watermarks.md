# ADR-0041 — Per-stream comment watermarks (fix cross-stream drop)

**Date:** 2026-07-16
**Status:** Accepted

## Context

`ListNotesSince` (GitHub provider) merges three separate comment endpoints
into one chronological list every poll tick:

- `issue_comment` — `/repos/.../issues/{n}/comments` (PR conversation)
- `pull_request_review` — `/repos/.../pulls/{n}/reviews` (top-level reviews)
- `pull_request_review_comment` — `/repos/.../pulls/{n}/comments` (inline line comments)

The original implementation filtered all three against a single scalar
watermark (`AgentState.LastSeenNoteIDs[mrIID]`), on the stated assumption
that "GitHub IDs are globally monotonic across all resources." That
assumption is false: each endpoint's `id` is drawn from its own sequence,
independent of the others. Nothing guarantees that an inline review comment
posted a minute ago has a higher `id` than an issue comment posted a minute
later.

Effect: once any stream advanced the shared watermark past some value `N`,
any comment on a *different* stream with `id <= N` was filtered out as
"already seen" — even though it had never been delivered. The drop was
silent (no error, no log) and permanent (the watermark never moves
backwards), so an inline review comment could vanish from the poller with
no trace, and no way for the author to know their comment was ignored
besides noticing the bot never responded.

## Decision

Track a watermark **per comment stream**, not one scalar per MR.

`provider.NoteCursor` replaces the raw `int64` argument to `ListNotesSince`:

```go
type NoteCursor struct {
    ByStream map[string]int64 // stream key -> high-water mark
    Legacy   int64            // pre-migration scalar; floor for untracked streams
}
```

Each provider defines its own stream keys (GitHub: `issue_comment`,
`pull_request_review_comment`, `pull_request_review`, matching its webhook
event names; GitLab: a single `note` stream, since GitLab has one notes
endpoint and no cross-stream hazard). `NotePoll` and `Note` both gained a
`Stream` field so a comment's origin travels with it from
`ListNotesSince`/webhook decode through to the code that persists the
watermark.

### Additive `AgentState` migration

`AgentState.LastSeenNoteIDs map[int]int64` (the old scalar, keyed by MR
IID) is **kept, not removed**, and now serves as `NoteCursor.Legacy`. A new
field is added alongside it:

```go
LastSeenNoteIDsByStream map[int]map[string]int64 `json:"last_seen_note_ids_by_stream,omitempty"`
```

For a Run created before this ADR, `LastSeenNoteIDsByStream` is nil/empty.
`ListNotesSince` falls back to `Legacy` for any stream key absent from
`ByStream`. This means:

- No JSON migration step is needed — old persisted state deserializes as-is
  (`omitempty` on the new field, zero value on read).
- No comment already delivered under the old scheme gets redelivered: the
  legacy scalar is the max `id` ever seen across all streams, so it's a
  safe (if conservative) floor for every stream until that stream gets its
  own entry.
- Streams the old scheme was dropping start working immediately: once
  `LastSeenNoteIDsByStream[mrIID][stream]` is set, that stream's future
  comments are filtered only against its own cursor, not the others'.

`resume()` (`internal/refactorsweep/workflow.go`) advances both the legacy
scalar and the per-stream map on every `EventNoteAdded`, so `resume()`
remains the single place watermark state is persisted (matches ADR-0035's
existing design — see its "advances LastSeenNoteIDs even on the echo-skip
path" fix). The poller's own `LastSeenNoteIDs`/`LastSeenNoteCursors`/
`SaveSnapshot` plumbing mirrors the same shape for symmetry, though in the
current daemon wiring `SaveSnapshot` is unset — `resume()` is what actually
persists state, reached via the same `Dispatcher` → `workflow.Callback`
path poll and webhook sources share.

## Alternatives considered

- **Keep one watermark, but take the min across streams instead of the
  max.** Prevents the "already seen" false positive but reintroduces the
  opposite failure: a fast-moving stream would be re-fetched and
  re-filtered every tick indefinitely (correctness restored, but wasted API
  calls and log noise, similar in shape to the bug ADR-0035 fixed).
- **Fetch each stream with its own request always ordered so streams
  never interleave (e.g. sort globally by created_at instead of id).**
  GitHub's endpoints don't support ordering by a cross-stream-comparable
  key, and even if they did, `created_at` has second-level resolution and
  ties are common — not a reliable substitute for a real per-stream cursor.
- **Replace `LastSeenNoteIDs` outright instead of keeping it as a
  migration floor.** Requires either a one-time backfill migration (this
  repo has no such mechanism — state is plain JSON via
  `workflow.RecordStore`, see `types.go`) or accepting redelivery of every
  in-flight Run's already-seen comments on upgrade. The additive approach
  avoids both.

## Consequences

- A lower-id comment on one GitHub stream is delivered even after a
  higher-id comment on a different stream, fixing the silent drop.
- `AgentState` grows one more optional map field; existing serialized Runs
  need no migration and keep working through the `Legacy` floor.
- Providers must tag `Note.Stream`/`NotePoll.Stream` for the per-stream
  fix to apply to them; GitLab's single-stream case only exists for shape
  consistency with `NoteCursor` and has no bug to fix.
- Future providers with multiple independent comment-id sequences get
  correct behaviour for free by defining their own stream keys — no changes
  needed to `poller.go` or `resume()`.

## Addendum (2026-07-17) — Legacy is not a safe floor once any stream is tracked

The original decision states: "the legacy scalar is the max `id` ever seen
across all streams, so it's a safe (if conservative) floor for every stream
until that stream gets its own entry." This was wrong, and it reintroduced
the exact bug this ADR fixes (PR #30).

`resume()` (`internal/refactorsweep/workflow.go`) advances `Legacy`
(`LastSeenNoteIDs`) unconditionally on every `EventNoteAdded`, regardless of
stream — that's true both before and after an MR gets its first
`ByStream` entry. So once one stream has posted even a single comment after
migration, `Legacy` keeps climbing past ids on *other* streams that have
never posted anything yet. The next time an untracked stream's first
comment arrives, `ListNotesSince`'s `threshold` fallback compared that
comment's id against the now-polluted `Legacy` — not against a true
"nothing on this stream has ever been seen" floor — and silently, permanently
dropped it if its id happened to be lower. This is the same failure mode the
ADR set out to fix, just deferred to the first post-migration comment on a
second stream instead of happening from day one.

The fix (`internal/provider/github/github.go`'s `threshold` closure): `Legacy`
is only a valid floor while `since.ByStream` is entirely empty — the true
pre-fix case where this MR has never had any per-stream entry recorded at
all, so `Legacy` really is the max id ever seen for the MR and no stream can
have an undelivered comment below it. The moment `ByStream` has even one
entry, a stream still missing from it floors at `0` instead of `Legacy`:
worst case that stream's full history is refetched and re-filtered once
(bounded by the 100-per-endpoint pagination cap), not silently dropped
forever.
