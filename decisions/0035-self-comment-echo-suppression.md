# ADR-0035: Self-comment echo suppression via SHA-256 FIFO on AgentState

**Status**: Accepted
**Date**: 2026-07-01

## Context

The first cross-provider dogfood spike (Runs `cc2383f8` and `f320ad5e`
against `github.com/andrewwormald/everflow`) surfaced a compounding
token leak: the daemon posts several comments per Run on behalf of the
author (via the author's OAuth identity), and every one of those
outgoing comments was picked back up by the next poll tick, classified
as a plain author note by the filter, and dispatched to `invokeForEvent`
— which invoked `claude -p` with the full worktree + spec context.

Post-Run accounting: 22 total `claude -p` calls across the two Runs
where ~9 represented genuine planner or work invocations. **13 calls
were echo-loop waste, mostly `address_comment` iterations on
daemon-posted status messages.** Rough estimate ~200-500K tokens of
runner spend from this single bug, doubling+ the real cost of each
Run.

The obvious fix — compare `note.author.handle` against
`AgentState.Author.Handle` and skip on match — doesn't work because
the daemon posts via the user's OAuth token, so both sides of the
comparison resolve to the same string. There's no signal from note
metadata alone that distinguishes an everflow-originated comment from
a genuine user reply.

## Decision

Every outgoing comment the daemon posts on behalf of a Run passes
through a small helper (`postBotComment`) that:

1. Computes `sha256(body)` as a hex string.
2. Appends the hash to `AgentState.RecentOutgoingHashes` — a bounded
   FIFO capped at 32 entries (see `recentOutgoingHashCap`).
3. Trims the slice to the cap by dropping the oldest entries.
4. Calls `provider.PostComment` with the raw, unmodified body.

The hash write lands **before** the network call so no poll tick can
race in front of the state update.

At the top of `resume()` — after event decode, before any filter,
control-verb parsing, or `invokeForEvent` — inbound `note_added`
events are checked against the FIFO via `isOwnEcho`. A match
increments `EventsSkippedByFilter` and returns the current status
unchanged, short-circuiting the loop at ~microsecond cost with zero
runner spend.

The 32-entry cap absorbs realistic bursts (poll cadence is 30s;
typical peak of daemon comments in that window is single digits) with
headroom for adversarial timing. State overhead is ~2 KB per Run
(32 × 64 hex chars).

## Alternatives considered

- **HTML-comment marker** (`<!-- everflow-bot -->` prepended to every
  outgoing body). Simpler — no state, no serialization concerns. But
  it pollutes the comment body: invisible in rendered markdown, still
  present in "view raw" tools, copy-paste, and every downstream
  consumer of the comment text. Also spoofable by a determined user
  quoting the marker into their own reply. The hash approach keeps
  bodies pristine.
- **Author-handle comparison.** Doesn't work in the OAuth-shared
  identity case that this bug exists to solve; rejected as the design
  premise.
- **Comment ID tracking** (record the returned comment ID from
  `PostComment`, skip notes whose ID is in the set). Requires
  provider adapters to return the created comment's ID from
  `PostComment` — currently the interface returns only `error`.
  Larger surface change; hash approach avoids it.
- **`container/list` linked list instead of slice.** Serialization
  work and interface-boxing overhead outweigh the negligible
  benefit of O(1) front-trim at cap=32. See discussion.

## Consequences

- Roughly halves the runner-token cost of a typical Run (based on the
  2026-06-29 spike accounting). The savings scale with the number of
  daemon-posted comments per Run.
- `AgentState` gains a durable `RecentOutgoingHashes` slice that
  persists via the existing `workflow.RecordStore.Store` path — no
  new state-persistence surface. Daemon restart preserves the ring.
- All existing `p.PostComment(ctx, ...)` call sites in
  `internal/refactorsweep` are routed through `postBotComment(ctx, r, p, ...)`.
  New call sites must follow the same pattern; a direct `p.PostComment`
  call from step-body code is now a bug (the echo will trigger a
  runner call).
- Tiny false-positive risk: a user who pastes a daemon comment
  character-for-character identical as their entire reply would be
  incorrectly skipped. Practical mitigation: hash covers the whole
  body, so any prefix, suffix, or edit breaks the match. Users
  quoting daemon text as part of a longer message land in the "not
  a match" bucket automatically.
- The FIFO is scoped per-Run (lives on `AgentState`), so echo state
  doesn't leak across Runs. The trade-off: if a Run restarts on a
  fresh binary that redeploys the same comment templates, the first
  poll after restart might mis-skip a real echo if the state was
  wiped — but the store is durable, so state survives across
  daemon restarts.
- Runner-token reporting is still unbuilt (Turn tokens all read 0);
  the `AgentState.Budget` field can't be enforced yet. This ADR
  addresses the biggest single cost lever; explicit budget
  enforcement is a separate follow-up.
