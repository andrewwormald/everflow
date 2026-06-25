# ADR-0014: Bulk-refactor sweeps are the primary use case

**Status**: Accepted (supersedes the framing in ADR-0010)
**Date**: 2026-06-16

## Context

Earlier ADRs ([ADR-0001](0001-orchestrator-not-replacement.md),
[ADR-0010](0010-scheduled-skill-poc-first.md)) framed everflow as a generic
"durable host for AI coding agents" with a scheduled-skill PoC. That framing
was too vague — it didn't answer the obvious question "why use this instead
of just running Claude Code directly?"

In a design conversation on 2026-06-16 the headline answer emerged:

> Claude can already do durable work if you give it a tunnel and a server.
> What Claude *cannot* do cheaply is run the **same repetitive crank** —
> bulk refactor sweeps, large-monorepo standardisation — without LLM tokens
> blowing up across hundreds of near-identical units.

Everflow's job is to **crunch through a lot of similar changes**, shipping
one MR per increment, while keeping the human author and reviewers in a
sustainable rhythm.

## Decision

The primary use case is **bulk-refactor sweeps over large codebases**. The
canonical example: standardising a pattern across all services in a
monorepo (logger migration, deprecated-API replacement, lint-rule cleanup,
schema migration, dependency update). Everflow drives the sweep MR-by-MR.

This narrows the project's mandate to a sharper, more defensible product
story than "long-running agent host." It also unlocks the bounded-context
property that makes everflow cheap: each unit's subagent invocation gets
only that unit's scope, not the whole refactor's history.

Specifically:

- The **headline marketing pitch** is "drives bulk refactors across large
  codebases."
- The **headline technical pitch** is "amortises LLM cost across many
  similar units; the workflow handles the repetitive cheap parts, the
  subagent only fires on novel reasoning."
- Adjacent use cases (single-MR babysit, CI green-keeping across all MRs)
  fall out of the same primitives but are not the headline.

## Alternatives considered

- **Generic durable agent host (the prior framing)** — too broad; couldn't
  answer "why this instead of Claude". Killed.
- **CI babysitting as the headline use case** (a review-babysit skill
  shape) — also valid, but reactive in nature; less of a value
  differentiator vs `/loop`. Demoted to a secondary use case that the same
  primitives support.
- **Code review automation** — different product; not aimed at this.

## Consequences

- The state machine, queue model, and concurrency throttle described in
  [ADR-0015](0015-throttled-sequential-mr-flow.md) are first-class
  primitives, not afterthoughts.
- The MR-only communication model
  ([ADR-0016](0016-mr-comments-only-channel.md)) is justified by this
  mandate — refactor work *lives* on MRs, so concentrating UI there is
  natural.
- The scheduled-skill PoC ([ADR-0010](0010-scheduled-skill-poc-first.md))
  is reclassified as v0 reference code. It demonstrated the workflow
  state-machine primitives work; it is not where the project is heading.
  The code remains in the repo as historical reference but the README and
  DESIGN.md describe the v1 refactor-sweep flow.
- Adjacent skills (review babysitters, on-call routers, etc.) become
  *applications built on top of* everflow, not the project's main pitch.
