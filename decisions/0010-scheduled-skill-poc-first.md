# ADR-0010: Build the scheduled-skill PoC before the interactive loop

**Status**: Accepted
**Date**: 2026-06-15

## Context

The full design ([DESIGN.md](../DESIGN.md)) describes an interactive
`Iterating → Awaiting → Iterating` loop with `Decision`-based control flow
and human-in-the-loop callbacks. That's a lot of surface to build on day one.

In parallel, the user has an existing Claude Code skill (a review-babysit
loop) that is *already* designed for `/loop 30m /<skill>` — a polling-based
sweep. What it lacks is durability: `/loop` requires Claude Code to stay open.

## Decision

Build a **scheduled-skill workflow first** as the PoC:

```
Initiated → Idle ⇄ Running
              ↑       │
              └───────┘  (AddTimeout re-arms each time we re-enter Idle)
```

One skill, fixed interval, single runner, fire-and-forget per pass. No
`Decision`, no `Awaiting`, no callbacks. Just durable scheduling.

This validates the *foundations* (daemon, worktree, Runner interface,
workflow state machine) without the full agent surface. The interactive
loop is layered on later.

## Alternatives considered

- **Build the full interactive loop on day one** — high risk; lots of
  surface area to design *and* validate at once, with no concrete use case
  forcing the abstractions to be right. The scheduled-skill use case is
  immediately useful and forces just enough of the architecture to be
  sound.
- **PoC as a one-off, not extracted** — leave it as `_examples/agentloop/`
  in `luno/workflow`, throw away post-experiment. Rejected: it's already
  the start of a real project.

## Consequences

- Everflow v1 is "scheduled durable invocation of a Claude Code skill."
  That's the README headline.
- The interactive loop (Iterating/Awaiting/Decision) is a clear layer to add
  next, on the same workflow definition, with no breaking changes.
- Validating "two runners share one interface" is deferred — we ship
  `claude` + `mock` in v1, not `claude` + `qwen` or `claude` + `openhands`.
  Mock is enough to prove the interface; real second runners come post-PoC.
