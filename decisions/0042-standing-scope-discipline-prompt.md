# ADR-0042: Standing scope-discipline instruction on every runner prompt

**Status**: Accepted
**Date**: 2026-07-16

## Context

ADR-0039 fixed one cause of scope-creep: the runner's `Goal` used to carry
the whole-spec goal verbatim, so a unit labelled `increment-2` couldn't
tell it was only meant to cover one item. That ADR threads the planner's
per-increment rationale into the prompt so the runner knows *what* it's
scoped to.

It doesn't address the complementary failure mode: even when the Goal
*is* narrow, nothing in the prompt tells the model to stay narrow. Given
a small, well-defined task and a fully-writable worktree, a capable model
will often also fix an adjacent bug, tidy up a nearby function, or add a
test it noticed was missing — reasonable behaviour in isolation, but it
turns single-concern units into multi-concern MRs, which is exactly what
ADR-0039 and the early-access-hardening spec's "one concern per MR" rule
were trying to prevent.

## Decision

`BuildPrompt` (`internal/runner/claude/claude.go`) now always appends a
`## Scope discipline` section, between the task/event blocks and the
`## How to finish` decision-marker protocol. It instructs the runner to
do only what's asked, not bundle in unrelated fixes/refactors, and to
mention (not act on) other work it notices.

This is a standing instruction like `decisionProtocol` — always
appended, not conditional on any `Request` field — so every runner
invocation gets it regardless of caller (`work()`, `invokeForEvent()`,
sweep mode, spec mode).

## Alternatives considered

- **Fold the reminder into `decisionProtocol`.** Simpler (one constant),
  but conflates "how to behave while working" with "how to signal you're
  done" — two different concerns the model should read as separate
  instructions at separate times.
- **Only add it when the planner's rationale (ADR-0039) is present**,
  reasoning that sweep-mode units are already narrow by construction.
  Rejected: sweep-mode units can still drift (a rename touching an
  adjacent file the model decides to "clean up too"), and a single
  always-on constant is simpler to reason about than conditional
  wiring.
- **Enforce scope via a diff-size gate post-hoc.** Considered and
  deferred in ADR-0039 for the same reason it's deferred here: blunt,
  false-positives on legitimately-wide mechanical changes, and treats
  the symptom instead of under-briefing the model up front. A prompt
  instruction is the cheaper first line of defence; a size gate remains
  a possible follow-up.

## Consequences

- Every runner prompt is slightly longer (a fixed ~400 characters);
  negligible next to a multi-KB Goal and full worktree context.
- Complements ADR-0039 rather than replacing it: ADR-0039 narrows *what*
  the Goal says, this ADR reinforces *how* the model should treat that
  narrowing once it's writing code.
- Doesn't guarantee scope discipline — it's a prompt instruction, not an
  enforcement mechanism. A future diff-size or file-count gate remains
  available as defence-in-depth if prompt-only steering proves
  insufficient in practice.

## Tests

`internal/runner/claude/claude_test.go`:

- `TestBuildPrompt_AllFields` — asserts the `## Scope discipline` header
  and its "small and narrowly scoped" text land in a fully-populated
  prompt.
- `TestBuildPrompt_MinimalFields` — asserts the section is present even
  with only `Goal` set (i.e. it's unconditional, unlike the Skill/Unit/
  Worktree/Comment/CI headers).
- `TestBuildPrompt_ScopeDisciplinePrecedesDecisionProtocol` — asserts
  ordering: the scope reminder appears before the `## How to finish`
  decision-marker protocol.
