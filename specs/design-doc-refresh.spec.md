---
goal: "Refresh DESIGN.md to reflect ADRs 0031-0034 and remove stale roadmap items"
provider: github
project: andrewwormald/everflow
runner: claude
base_branch: main
base_repo: /Users/andreww/dev/everflow
concurrency: 1
draft_mrs: true
status: ready
---

# Refresh DESIGN.md

`DESIGN.md` was last revised before ADRs 0031, 0032, 0033, and 0034
landed. Two sections specifically have stale content:

## Section 1: "What's not yet built"

Currently lists 9 items as TODO. Most of them are now built:

1. Provider abstraction + GitLab adapter — **built** (ADR-0020)
2. HTTP server in the daemon — **built** (ADR-0028)
3. Sqlite store — **built** (ADR-0022); ADR-0032 added the `Meta.Version`
   round-trip fix
4. State machine for the refactor-sweep workflow — **built**
5. Starlark filter integration — **built** (ADRs 0018, 0030)
6. Per-Run filesystem layout + skill mirror — **built**
7. Control command handler — **built** (ADR-0017)
8. `everflow start` CLI + `everflow status` + `everflow phrases promote` —
   **partial**: `start` and `phrases promote` work; `status` is a stub
9. GitHub provider adapter — **built** (ADR-0021); ResolveDiscussion
   landed via ADR-0034

Rewrite this section to:

- Remove the items that are built (cite the ADR where each landed)
- Keep the items that are genuinely incomplete (the `status` CLI stub;
  any other actual gaps the author can spot)
- Add a brief "since v1 baseline shipped" paragraph noting the
  spike-driven hardening: ADR-0031 (polling), ADR-0032 (terminal
  StatusFailed + binary-blob filter), ADR-0033 (eventstream cond.Wait),
  ADR-0034 (Paused self-loop + auto-resolve)

## Section 2: "Open questions"

Four open questions are listed. Some have been answered implicitly by
the recent ADRs; others may still be open. For each one, decide:

- **Q1** ("Signal that learning is working") — still open. Keep as-is
  or note that ADR-0030's phrase-learning loop is the partial answer.
- **Q2** ("Subagent invocation atomicity") — still open in concept.
  Keep, optionally update with what the runner currently does.
- **Q3** ("Questions in reviewer comments") — partially answered by
  ADR-0034's "verbal reply with no code change is OK; thread auto-
  resolves" path. Update to note that.
- **Q4** ("Dependencies between units") — still open; keep.

## Constraints

- Don't rewrite OTHER sections of DESIGN.md (Architecture, State
  Machine, Communication Model, etc.); they're current. This change
  is scoped to the two named sections only.
- British English. The rest of the file is in British English; match it.
- Don't reference customer repos by name. The repo is open source
  (github.com/andrewwormald/everflow); discuss things generically.
- Each MR must include the change to DESIGN.md and nothing else
  unless directly required to make the doc edits compile / pass
  tests (which is unlikely for a docs-only edit).

## Branch / MR

Native everflow branch naming is fine. PR title doesn't need a Jira
ID (this is not a Luno repo).

## Done when

- Both named sections of DESIGN.md reflect the current state of the
  codebase as of commit `fa8185b`.
- No mention of items that are demonstrably built (the relevant ADR
  exists and the code is in `internal/`).
- The "What's not yet built" section gives an honest, current picture
  of what an external contributor would need to build to extend the
  daemon (HTTP read API, GitHub thread auto-resolve when more comment
  types ship, runner adapters beyond Claude, etc.).
