# ADR-0016: The MR comment thread is everflow's only communication channel

**Status**: Accepted
**Date**: 2026-06-16

## Context

A long-running workflow needs to talk to humans for: status updates, pause
notifications when stuck, completion signals, and accepting control
instructions back. Early design drafts proposed multiple channels:

- A JSONL inbox file at `~/.everflow/inbox.jsonl`
- Outbound webhooks to user-configured URLs (Slack, Discord, ntfy.sh)
- An optional `Notifier` interface with pluggable backends
- The MR comment thread

All four would work. Maintaining four would be exhausting.

## Decision

**The MR comment thread is the only communication channel.** Everflow
posts comments to talk; the author replies to control; the workflow
listens to webhooks for both event types.

Concretely:

| Direction | What it looks like |
|---|---|
| **Workflow → human (status)** | MR comment: "✓ pushed `abc123`: addressed review thread from @alice" |
| **Workflow → human (paused)** | MR comment: "Paused: tests still red after 3 fix attempts. Last attempt: `def456`. Reply `/everflow resume`, `/skip`, or push a fix yourself and I'll continue." |
| **Workflow → human (complete)** | MR comment on the *last* MR: "This was the last unit. Refactor complete: 23 MRs shipped, 2 blacklisted. Stopping." |
| **Human → workflow (review feedback)** | Normal MR review comment. Workflow filters via Starlark, may invoke subagent. |
| **Human → workflow (control)** | MR comment starting with `/everflow ...` from the Run author. See [ADR-0017](0017-author-privilege-model.md). |

This collapses the "where do I check on my workflow?" question to "look at
the MRs." The MR thread becomes the audit trail, the inbox, and the
control surface for that unit.

## Alternatives considered

- **JSONL inbox + MR comments**: extra surface for users to check, hard to
  notice. Rejected.
- **Slack DM + MR comments**: requires Slack auth wiring (the user's
  `claude.ai` Slack MCP doesn't work headlessly — see
  [ADR-0004](0004-shell-out-to-claude-p.md) consequences). Doable later as
  a `Notifier` interface but not v1.
- **Email + MR comments**: too async for a workflow that's already polling
  webhooks. Rejected.
- **A `Notifier` plugin interface with multiple backends**: over-engineered
  for v1. The MR-only model defers this complexity until a real use case
  forces it.

## Consequences

- All status/pause/completion messages go through the platform's API
  (GitLab `glab api projects/:id/merge_requests/:iid/notes`, GitHub `gh api
  repos/.../issues/:n/comments`).
- The platform's webhook is the input side: incoming comments → filter
  ([ADR-0018](0018-starlark-filter-and-phrase-learning.md)) → workflow
  state transitions.
- Two consequences for testing: the workflow is **observable** by reading
  the MR thread (no special tooling needed), and **debuggable** by
  replaying webhook payloads against the state machine.
- For refactors with many MRs, a "fan-in" summary view is useful but
  *not* v1. The user can `glab mr list --label everflow:<runID>` (we set
  a label on each MR) for a manual rollup until we build something
  fancier.
- For the rare case where the platform is unreachable (downtime, network
  partition), the workflow buffers state internally — the next time the
  platform responds, the workflow flushes the comments it would have made.
  No comment loss.
- This design *requires* webhooks. Without inbound events, the workflow
  can't see comments arriving. Polling fallback (see DESIGN.md) covers the
  case where webhooks haven't fired in N minutes — but the steady state
  is webhook-driven.
