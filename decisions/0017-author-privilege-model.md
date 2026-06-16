# ADR-0017: Author-vs-reviewer privilege model and control commands

**Status**: Accepted
**Date**: 2026-06-16

## Context

Given [ADR-0016](0016-mr-comments-only-channel.md) — all human↔workflow
communication happens via MR comments — there's a privilege question:
**who is allowed to tell the workflow what to do?**

Without a privilege model:

- A reviewer commenting "skip this MR" could be a *suggestion* to the
  author or an *instruction* to the workflow. The workflow can't tell.
- A bot comment ("⚠ Danger: title format invalid") could be interpreted
  as a review thread requiring a subagent invocation, or as a hint to
  auto-fix the title. Mishandled either way.
- The author's intent gets lost in the noise.

## Decision

Two classes of commenter, two handling paths:

| Class | Defined as | What they can do |
|---|---|---|
| **Author** | The user identity that authenticated when `everflow start` ran. Captured at Trigger time via `glab api user` (or `gh api user`) and stored immutably on `AgentState.Author`. | Issue **control commands** by posting a comment starting with `/everflow <verb>`. Also write normal comments, which are still filtered as review feedback. |
| **Reviewer** | Any other human commenter. | Normal review feedback. Comments go through the Starlark filter ([ADR-0018](0018-starlark-filter-and-phrase-learning.md)); substantive ones may invoke a subagent to address them. |
| **Bot** | Sender's username matches a configured bot pattern (Danger, sonar, dependabot, etc.) or has a `bot` label per the platform's user metadata. | Per-source handling: some bots have deterministic fix paths (Danger title-check), others are skip-by-default. |

### Control command set (v1)

```
/everflow pause              Pause the Run. Stop processing new units; ignore
                             new review comments on the current MR until /resume.

/everflow resume             Undo /pause; continue from where we stopped.

/everflow skip               Mark the current MR's unit as blacklisted; close
                             the MR; release the slot; pick up the next unit.

/everflow retry              Re-trigger the subagent for the current MR with
                             the same prompt + accumulated context (useful when
                             a transient failure caused everflow to give up).

/everflow prompt <text>      Inject additional instructions for the next
                             subagent invocation on this unit. Useful when the
                             author sees the MR going sideways and wants to
                             redirect.

/everflow status             Workflow replies with a comment summarising
                             progress: units shipped, in-flight, queued,
                             blacklisted, and the current MR's lifecycle phase.

/everflow stop               Cancel the whole Run. Closes any in-flight MRs
                             that haven't been merged.
```

Unknown commands get a polite reply: "Unknown command `/everflow foobar`.
Available: pause, resume, skip, retry, prompt, status, stop."

### Service-account authorship

When everflow runs under a service-account token (e.g. `everflow-bot` on a
shared deployment), the *technical* author per `glab api user` is the bot,
but the *human* author is the person who triggered the Run. Two flags:

- `--author auto` (default): use the token's authenticated user.
- `--author <handle>`: override. Useful for shared deployments where one
  service account starts Runs on behalf of different humans.

The override is the authoritative author from the workflow's perspective —
control commands from that handle are honoured even if the same handle
isn't the token's owner.

## Alternatives considered

- **Anyone can issue control commands**: the obvious open model. Killed
  for v1 — a reviewer accidentally typing `/everflow skip` in a discussion
  about *what* to skip would skip the wrong thing.
- **A separate allow-list of authorised controllers**: more flexible but
  more configuration. Just-the-author is the default for v1; explicit
  multi-user authorisation is v2.
- **No control commands; the workflow is fully autonomous**: too rigid.
  When a refactor goes off-rails the author needs a fast intervention path,
  and "ssh into the daemon and edit state" is unreasonable.

## Consequences

- The `Author` field on `AgentState` is captured at Trigger and is
  immutable. Changing it mid-Run requires `everflow stop` + restart.
- Bot detection requires per-provider configuration. GitLab's user
  metadata exposes `user.bot: true`; GitHub uses the `type: "Bot"` field
  on accounts. Both fine; need a small `is_bot(user)` helper per provider.
- The Starlark filter ([ADR-0018](0018-starlark-filter-and-phrase-learning.md))
  can also distinguish author/reviewer/bot via fields on the event payload,
  enabling rules like "skip non-author 'lgtm' comments without LLM cost."
- Control commands are *also* fed through the comment filter, but only the
  Author's comments matching `^/everflow ` bypass the LLM and route
  directly to state-transition handlers. This is the only privileged code
  path triggered by a comment.
- Audit: every accepted control command results in a workflow reply
  comment ("✓ paused per @andreww at 14:32 UTC"), keeping the MR thread as
  the audit trail.
