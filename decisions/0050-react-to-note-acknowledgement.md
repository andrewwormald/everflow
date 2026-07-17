# ADR-0050 — `ReactToNote`: instant emoji acknowledgement of picked-up comments

**Date:** 2026-07-17
**Status:** Accepted

## Context

Today the only signal an author gets that their comment was received is the
eventual reply everflow posts after the subagent finishes — which can be
minutes. If the poll/webhook tick is slow, or the subagent invocation is
long, the author has no way to tell "did everflow see this?" from "is
everflow stuck?" apart from waiting.

The fix: react to the triggering comment with an emoji (e.g. 👀) the moment
everflow picks it up — before the subagent runs — then optionally swap or
leave the reaction once processing finishes. Both GitLab and GitHub support
adding a reaction to an existing comment without posting a new one, so this
doesn't add noise to the thread the way a "working on it..." reply would.

This ADR covers the `Provider` interface addition only. Wiring the call into
the pickup path (`internal/refactorsweep/workflow.go`) is deferred to a
follow-on increment — this one just establishes the shape.

## Decision

Add one method to `provider.Provider`:

```go
ReactToNote(ctx context.Context, projectID string, mrIID int, noteID int64, stream, emoji string) error
```

- `noteID` / `stream` identify the comment exactly as reported on
  `Note.ID`/`Note.Stream` (webhook path) or `NotePoll.ID`/`NotePoll.Stream`
  (poll path, ADR-0041) — no new identifier type needed, the caller already
  has these from the event it's reacting to.
- `emoji` is a platform-neutral short name (`"eyes"`, `"hourglass"`, ...).
  Both platforms use the same GitHub-style short-name vocabulary for award
  emoji, so one string works for both without a translation table.
- Reacting is **best-effort acknowledgement, not durable Run state**. It is
  not recorded on `AgentState` and isn't retried on failure — a missed
  reaction degrades to today's behaviour (silence until the reply lands),
  it never blocks or fails the pickup. Implementations should treat a
  platform/stream that can't be reacted to as success (`nil`), not an error,
  so callers can fire-and-forget without special-casing unsupported streams.

### Per-provider mapping

- **GitLab**: `POST /projects/:id/merge_requests/:iid/notes/:note_id/award_emoji`
  with `{"name": emoji}`. GitLab has one notes endpoint (`streamNote`), so
  `stream` is accepted but unused — kept in the signature for parity with
  GitHub, which needs it.
- **GitHub**: reactions live on a per-comment-type endpoint, so `stream`
  picks the path:
  - `issue_comment` → `POST /repos/{o}/{r}/issues/comments/{id}/reactions`
  - `pull_request_review_comment` → `POST /repos/{o}/{r}/pulls/comments/{id}/reactions`
  - `pull_request_review` (top-level review) → GitHub has no reactions
    endpoint for this object type. `ReactToNote` returns `nil` rather than
    erroring — the comment simply doesn't get a visible ack, same as the
    behaviour before this ADR.

## Alternatives considered

- **Post a "👀 picked this up" reply comment instead of reacting.** Rejected:
  adds a permanent line to the thread for every comment received, doubling
  comment volume without adding information once the real reply lands.
  ADR-0026 already ruled out relying on emoji reactions as an *inbound*
  signal (not reliably exposed in webhook payloads), but that's orthogonal —
  this ADR is about everflow reacting *outbound*, which both platforms
  expose as a normal authenticated write, no payload-parsing risk involved.
- **A richer `Reaction` struct instead of a bare `emoji string`.** No second
  field is needed yet (no "remove reaction" or "list reactions" caller), and
  a struct of one field is just indirection. Revisit if a future increment
  needs to swap/clear the reaction after the subagent finishes.
- **Encode noteID/stream in a small struct mirroring `NotePoll`.** Rejected
  for the same reason `PostComment`/`ResolveDiscussion` take bare
  `projectID`/`mrIID` rather than an `MR` value — callers already hold the
  individual fields from the event they're reacting to; wrapping them adds
  a conversion step with no new information.

## Consequences

- `provider.Provider` grows one method; both `gitlab.Provider` and
  `github.Provider` must implement it (this increment), and any test double
  asserting `var _ provider.Provider` must add a stub.
- No `AgentState` or persisted-state changes — this is a stateless,
  fire-and-forget call.
- Calling `ReactToNote` from the pickup path in
  `internal/refactorsweep/workflow.go` is out of scope for this increment
  and left for a follow-on.
