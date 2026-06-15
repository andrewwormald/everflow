# ADR-0001: Everflow is an orchestrator, not a Claude replacement

**Status**: Accepted
**Date**: 2026-06-15

## Context

We considered building everflow as a standalone AI agent runtime — its own CLI
that users would invoke instead of Claude Code, OpenHands, or similar tools.
This was rejected early.

Adoption is the constraint. Tools that try to *replace* Claude Code see almost
no uptake; tools that *augment* it via the Skill mechanism see substantial
uptake despite (or because of) being smaller in scope. The pattern that works
in the wild is "ship a markdown Skill that Claude Code loads, plus any
binaries the Skill calls."

## Decision

Everflow is **the durable orchestration and scheduling layer that an
AI coding tool calls into**. It is not the agent runtime itself.

Claude Code (or Qwen Code, or OpenHands) remains the user's primary interface.
The user asks their tool to start a long-running task; the tool calls
`everflow` to register the task; everflow runs the durable loop, invoking the
agent as a *subprocess* on a schedule or in response to events.

## Alternatives considered

- **Everflow as the primary CLI** — replaced Claude Code entirely. Killed:
  no adoption path. Users do not switch agent runtimes for one feature.
- **Everflow as a library, no CLI** — agent tools would link to it directly.
  Killed: agent tools are not Go and don't want to be; a binary + Skill is
  a smaller integration ask.

## Consequences

- Everflow's primary distribution artifact is a Claude Code Skill plus a Go
  binary. See [ADR-0002](0002-distribute-as-claude-skill.md).
- Everflow does **not** own the prompt/tool-use loop. The runner (claude/qwen/
  openhands) does. Everflow only owns *when and where* the runner is invoked.
- Adding support for a new agent tool = writing a small Runner adapter + a
  Skill in that tool's distribution format. See [ADR-0007](0007-pluggable-runner-interface.md).
