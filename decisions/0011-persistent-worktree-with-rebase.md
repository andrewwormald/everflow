# ADR-0011: Persistent worktree, reset --hard to base each pass

**Status**: Accepted
**Date**: 2026-06-15

## Context

Given [ADR-0006](0006-worktree-per-run.md) — one worktree per Run — there's
a sub-decision about how the worktree behaves *between* invocations.

For a scheduled-skill workflow firing every 30 minutes, three patterns
were considered:

1. **Fresh worktree per pass** — tear down old worktree, create new one
   off `origin/main`, every pass.
2. **Persistent worktree, rebase/reset before each pass** — created once
   at Trigger, stays for the Run's life; `git fetch && git reset --hard
   origin/<base>` before each invocation.
3. **No worktree** — `cd $base_repo` each pass.

## Decision

**Persistent worktree, `fetch + reset --hard origin/<base> + clean -fdx`
before each pass.**

The worktree directory is created once during `setupStep` and lives for
the Run's lifetime. Each `runPass` starts by hard-resetting it to the
latest `origin/<base-branch>`. Any local changes from a previous pass that
weren't pushed to a real branch are wiped — a deliberate property; agents
that want their work to persist must push to a real MR/PR branch (which
the review-babysit reference skill already does).

## Alternatives considered

- **Fresh worktree per pass** — strongest "clean slate" guarantee but
  creates/destroys a full checkout every interval, which for a 30-min
  cadence on a large monorepo is expensive and adds 5-30s to every pass.
- **No worktree (`cd $base_repo`)** — simplest, but the agent sees the
  user's uncommitted work and can clobber it. Breaks [ADR-0006](0006-worktree-per-run.md)'s
  isolation promise.
- **Worktree without reset** — leave the worktree in whatever state the
  previous pass left it. Encourages accidental local-state dependence
  between passes, which makes resumption-after-restart non-equivalent to
  fresh-runs.

## Consequences

- Setup cost is one-time per Run.
- Each pass starts from a known-clean state matching `origin/<base>`.
- Agents must push their work to real branches (MRs) during a pass.
  Anything left in the worktree is forfeit at the start of the next pass.
  This is fine for the babysit-MR pattern; may not be fine for future
  patterns that accumulate state inside the worktree itself. If such a
  pattern emerges, this ADR may be superseded.
- The worktree is *not* automatically purged when the Run completes — it
  stays for inspection until the user runs a future `everflow purge` (not
  yet implemented).
