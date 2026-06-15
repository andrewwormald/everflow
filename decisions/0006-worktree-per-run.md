# ADR-0006: One git worktree per Run

**Status**: Accepted
**Date**: 2026-06-15

## Context

An autonomous agent cannot prompt the user for filesystem permissions on a
per-write basis the way an interactive agent does. So either:

1. We run the agent with full filesystem access — unacceptable; one rogue
   pass could overwrite the user's in-progress work.
2. We sandbox it — but to what?

We also need a filesystem location where the agent can make commits without
polluting the user's main branch.

## Decision

Each Run is given a dedicated **git worktree** under `~/.everflow/wt/<runID>/`,
checked out on a branch named `wf-<runID>` off the user's configured
`--base-branch`. The runner runs *inside* that directory with
`--dangerously-skip-permissions` (or per-runner equivalent).

The worktree is the blast-radius boundary:

- Writes can only land in the worktree dir, not the user's main checkout
- Commits land on the per-Run branch, never on `main`
- The user pre-authorizes by triggering the Run; no further prompts needed

## Alternatives considered

- **No worktree, run in `--base-repo` directly** — simplest, but breaks the
  isolation promise. The agent would see (and could clobber) the user's
  uncommitted work.
- **Docker container per Run** — stronger isolation, but heavyweight setup,
  doesn't work the same on macOS vs Linux, and the agent's git/auth/secrets
  config has to be threaded through. Defers without ruling out for v2.
- **Worktree per *invocation* (fresh each pass)** — see
  [ADR-0011](0011-persistent-worktree-with-rebase.md) for why persistent
  worktrees won out.

## Consequences

- Yolo mode is *safe within the worktree*. The Runner adapter passes the
  appropriate skip-permissions flag.
- The agent can commit and push freely on `wf-<runID>`. It cannot affect
  `main` or any branch the user is working on.
- We do not (yet) grant `gh` credentials, deploy keys, `kubectl` config,
  or anything outside the worktree. Those remain blocked.
- Each Run has a real filesystem footprint (one full checkout). For a
  large monorepo this is non-trivial — could be a problem if many Runs
  pile up; addressable later via shared-blob worktrees or sparse checkout.
