# ADR-0012: Track all meaningful decisions as ADRs

**Status**: Accepted
**Date**: 2026-06-15

## Context

Everflow's purpose is to run AI agents on long-lived tasks autonomously.
The repo itself should be readable by an AI agent — a future Claude Code
session, a Qwen Code session, an OpenHands run — picking up the project
months from now with no prior conversation context.

Code comments and a single README cannot carry the *why* behind structural
choices. Without a written record of "we considered X, we picked Y, here's
why," every contributor (human or otherwise) re-litigates the same trade-
offs.

## Decision

Every meaningful decision is captured as a numbered Architecture Decision
Record under `decisions/`. The template is documented in
[`decisions/README.md`](README.md); the rule for what counts as "meaningful"
is in [`AGENTS.md`](../AGENTS.md).

"Meaningful" specifically excludes:

- Variable naming, gofmt-fixable nits
- Choice between equally good standard-library options
- Anything that can be safely re-derived from the code

It specifically includes:

- Picking a primitive (workflow lib, runner shape, IPC mechanism, storage)
- Scope decisions (what's in v1, what's deferred to v2)
- Naming/structural choices that affect contributors
- Tradeoffs where the alternatives were close

## Alternatives considered

- **No formal decision log; rely on commit messages** — works for the
  active maintainer but rots quickly. An agent reading the repo doesn't
  follow commit chains; it reads files. ADRs are first-class files.
- **A single `DECISIONS.md` running log** — works for ~10 decisions, gets
  unwieldy and hard to grep beyond that. The per-file convention scales.
- **Use a tool like adr-tools** — adds a dependency for what's basically
  "write a markdown file." Avoided.

## Consequences

- Every PR that makes a non-trivial design choice must include an ADR.
- An agent (or human) picking up the repo cold reads `AGENTS.md` →
  `decisions/README.md` → relevant ADRs, in that order.
- Superseded decisions are not deleted. They are marked
  `Superseded by ADR-NNNN` with a link, so the history of "why we
  changed our minds" is preserved.
- This ADR itself is the precedent. Future contributors who skip writing
  an ADR are violating this one.
