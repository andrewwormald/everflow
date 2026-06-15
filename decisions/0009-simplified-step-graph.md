# ADR-0009: Collapse the step graph to `Initiated → Iterating → terminals`

**Status**: Accepted
**Date**: 2026-06-15

## Context

An early draft had a finer-grained step graph modeled on the classical
ReAct loop:

```
Initiated → Planning → Acting → Observing → Reflecting → Planning (loop)
                                                       → Awaiting
                                                       → Completed
                                                       → Failed
```

Four LLM calls per turn (plan, act, observe, reflect). Workflow controlled
each transition.

This broke when we added [ADR-0007](0007-pluggable-runner-interface.md): a
runner like OpenHands does its own inner Plan/Act/Observe loop internally
and only exits after solving a whole subtask. Wrapping fine-grained ReAct
phases around an agent that already does ReAct is double-orchestration.

## Decision

Collapse to a single iterating state:

```
Initiated → Iterating ──┬─→ Iterating       (continue)
                        ├─→ Awaiting        (ask human)
                        ├─→ Completed
                        └─→ Failed

Awaiting → Iterating  (via Callback when human answers)
```

The single `iterate` step invokes the runner once. The runner decides
internally whether it did one logical turn or many. Workflow only sees the
`Decision` field of the response.

## Alternatives considered

- **Keep the fine-grained graph, force OpenHands to fit** — would require
  per-runner step graphs (defeats [ADR-0007](0007-pluggable-runner-interface.md))
  or OpenHands adapter has to fake Plan/Act/Observe/Reflect phases. Both
  bad.
- **Runner declares its preferred graph shape** — overcomplicated; one
  size *does* fit all if the size is "one step, runner decides granularity."

## Consequences

- We trade fine-grained workflow-side control of turn boundaries for the
  ability to support any runner shape. The runner decides what it does
  per invocation; we just durably record the outcome and decide whether to
  loop.
- The four-phase model (Plan/Act/Observe/Reflect) is now a *runner-internal*
  concept, if a runner wants it at all. Workflow does not impose it.
- Step-level metrics get coarser. We see "iterations" not "plans" and
  "acts." For most purposes that's the right granularity anyway.
