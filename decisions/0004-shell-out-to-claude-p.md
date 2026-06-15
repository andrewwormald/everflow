# ADR-0004: Shell out to `claude -p`, not the Anthropic SDK directly

**Status**: Accepted
**Date**: 2026-06-15

## Context

A step in the agent loop has to invoke "the agent" somehow. Two shapes:

- **Shell out** — `exec.Command("claude", "-p", prompt, ...)`. Use the
  user's existing `claude` CLI install. Each invocation is a fresh process.
- **SDK direct** — link `github.com/anthropics/anthropic-sdk-go` (or
  equivalent) and call the model API from Go. We own the tool-use loop.

## Decision

Shell out to the agent CLI (`claude -p` for the `claude` runner;
equivalents for other runners). The Go process never holds an LLM client.

## Alternatives considered

- **SDK direct** — faster (no cold start), full control over tool-use,
  streaming visible in our process. But: we'd reimplement what Claude
  Code already does (tool dispatch, MCP integration, permission prompts,
  output formatting), each runner would need separate SDK bindings, and
  authentication would have to be re-solved (Claude Code already handles
  OAuth, API keys, env-var precedence — we'd duplicate it).
- **Hybrid** — shell out by default, SDK for performance-critical paths.
  Killed: no v1 path is performance-critical enough to justify the
  duplication.

## Consequences

- The user must have the runner CLI installed and authenticated. We
  document this prerequisite; we don't manage it.
- Each invocation is a fresh process with its own cold-start cost
  (~hundreds of ms for `claude`). Acceptable when the loop runs every
  minutes-to-hours, not seconds.
- We can run any tool the user has installed without writing a per-tool
  Go integration — `gemini`, `codex`, custom CLIs all work the same way
  by implementing a Runner adapter that knows the right argv shape.
- Context cannot live inside the runner subprocess (since it's a fresh
  process each invocation). It must live somewhere durable on our side —
  see [ADR-0005](0005-context-in-workflow-run.md).
