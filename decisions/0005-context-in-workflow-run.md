# ADR-0005: Context lives in the workflow Run, not in the runner

**Status**: Accepted
**Date**: 2026-06-15

## Context

Following [ADR-0004](0004-shell-out-to-claude-p.md) — each runner invocation
is a fresh process — there has to be a durable home for the agent's "memory":
the goal, prior turns, accumulated notes, the question being asked (if
paused), the human's answer (if resuming).

Options:

- **In the runner** — keep one Claude Code session alive across many turns.
  The Run owns the subprocess handle.
- **In the workflow Run object** — every invocation is a pure function of
  Run state; the runner is given everything it needs each time.

## Decision

The agent's full durable state lives on the workflow `Run.Object` —
specifically the `AgentState` struct (goal, history, scratchpad, question,
answer, runner config). Each runner invocation receives this state as input
and returns updated state in its response.

This is what makes L3 (Stateful Memory) of the L1/L2/L3 model fall out for
free: `luno/workflow` already persists `Run.Object` durably. Resumption after
daemon restart is "rehydrate the Run, re-invoke the runner with its state" —
no special code.

## Alternatives considered

- **Long-lived runner subprocess** — keep one `claude` (or `claude-code` in
  REPL mode) alive per Run, send turns to it via stdin. Faster (warm cache),
  but you're now managing a long-lived subprocess across daemon restarts,
  signal handling, and OOMs. The complexity is not worth the perf win for
  loops that run on minute/hour cadences.
- **External KV store for agent memory** — pull memory from sqlite/Redis on
  invocation. Adds an infrastructure dependency that workflow doesn't need.

## Consequences

- The runner is a **pure function** of Run state. This makes runners trivial
  to test, swap, and reason about.
- Resumption is automatic and free — no special "reattach" logic.
- The `AgentState` struct is the durable contract. Changing its shape needs
  care if there are in-flight runs — a future ADR may record a schema
  migration approach.
- For long-running tasks, the context sent to the runner grows. The runner
  adapter is responsible for truncating/summarizing if its model's context
  window is exceeded.
