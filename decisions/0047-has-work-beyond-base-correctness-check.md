# ADR-0047: `HasWorkBeyondBase` — the "did the runner do anything" check

**Status**: Accepted
**Date**: 2026-07-17

## Context

`internal/refactorsweep/workflow.go`'s `work()` step decides, after a
runner returns `DecisionDone` (or `DecisionContinue`, ADR-0045), whether
the runner actually produced anything or just claimed to. Getting this
wrong in either direction is costly: false-negative discards real work as
a no-op blacklist, false-positive ships an empty diff as an MR.

That check used `Git.HasChanges`, which is `git status --porcelain` —
purely a working-tree dirtiness check. It missed one case entirely: a
runner that stages and commits its own work (some runners do, rather than
leaving the tree dirty for the harness to commit) leaves a *clean* tree.
`HasChanges` then reports `false`, and `work()` read that as "runner did
nothing" and blacklisted a unit that had, in fact, produced a real commit
sitting on the branch, unpushed. The runner's work was silently discarded.

`HasChanges` itself isn't wrong — it's still the right check for `Commit`
("is there anything left to stage right now?"). The bug was using a
dirty-tree check to answer a different question: "did this unit produce
work at all, across however many commits and however much uncommitted
diff it left behind?"

## Decision

Add `HasWorkBeyondBase(ctx, dir, baseBranch string) (bool, error)` to the
`Git` interface (`internal/git/git.go`), and use it — not `HasChanges` —
everywhere `work()` and `invokeForEvent` need to answer "did the runner do
anything?".

Implementation: `true` if `HasChanges` is `true` (uncommitted diff), OR if
`rev-list --count <merge-base(origin/baseBranch, HEAD)>..HEAD` is nonzero
(commits beyond the merge-base). Using the merge-base rather than
comparing HEAD to `origin/<baseBranch>` directly matters: if base moves
forward while a unit sits idle, that's not the unit's work, and counting
it as such would wrongly treat an idle unit as productive.

The four cases the method distinguishes:
- fresh worktree, runner did nothing → `false`
- runner left uncommitted changes (dirty tree) → `true`
- runner committed its own work (clean tree) → `true` — the case
  `HasChanges` got wrong
- base moved forward while the unit sat idle → `false`

`HasWorkBeyondBase` is purely local: it reads the `origin/<baseBranch>`
tracking ref as last fetched and does not fetch itself. Call sites that
need a current view of base fetch or sync first (see `SyncWithBase`,
ADR-0046) — this method only answers "relative to what we already know
about base, is there unit work here."

Both call sites in `internal/refactorsweep/workflow.go` were switched from
`HasChanges` to `HasWorkBeyondBase`:
- `work()` (~line 660): after a runner returns Done/Continue, decides
  blacklist-as-no-op vs. proceed to commit+push.
- `invokeForEvent` (~line 1122): the equivalent check for the
  address-comment / fix-CI conflict-resolution phases.

`Commit` itself keeps using `HasChanges` — staging cares only about the
working tree at the moment of the call, not the branch's full history
against base.

## Alternatives considered

- **Compare `HEAD` to `origin/<baseBranch>` directly instead of via
  merge-base.** Simpler, but wrong the moment base advances while a unit
  is idle: `HEAD != origin/<baseBranch>` would then read as "unit did
  work" when really the unit did nothing and base just moved. Merge-base
  isolates the unit's own commits from upstream's.
- **Make `HasChanges` itself commit-aware.** Would silently change the
  contract for `Commit`'s existing use (and any other future caller
  expecting a pure working-tree check), and conflate two different
  questions ("anything to stage right now" vs. "did this unit produce
  work at all") into one method. Kept as two methods with narrower,
  named contracts instead.
- **Fetch inside `HasWorkBeyondBase`.** Would make every "did the runner
  do anything" check also a network call and silently change the
  tracking ref underneath a caller that didn't ask for that. Left
  fetching to callers that explicitly want a fresher base (`SyncWithBase`,
  ADR-0046); `HasWorkBeyondBase` stays a fast, local, read-only check.

## Consequences

- `Git` interface gained one method; `ExecGit` and the `fakeGit` test
  double (`internal/refactorsweep/workflow_test.go`) both implement it.
- Both `work()` and `invokeForEvent` now correctly recognize
  self-committing runners as having done work, instead of blacklisting
  their output as a no-op.
- `Commit`'s `ErrNoChanges` path in `work()` still has to distinguish "the
  runner committed its own work, so there's nothing left to stage" from
  "nothing stageable at all (e.g. filtered-out binary blobs)" — it does
  so via `DiffShortstat` against base, which is unaffected by this
  change.

## Tests

`internal/git/git_test.go`:
- `TestExecGit_HasWorkBeyondBase` — covers all four cases: fresh worktree
  (false), dirty tree (true), committed-but-clean tree (true — the
  motivating case), and base moved forward while idle (false); also
  asserts a nonexistent base branch is a real error, not a silent false.

`internal/refactorsweep/workflow_test.go`:
- `fakeGit` gained a `HasWorkBeyondBase` stub alongside the existing
  `HasChanges`, and the `work()` / `invokeForEvent` test cases were
  updated to exercise the self-committed-clean-tree scenario that
  `HasChanges` alone would have gotten wrong.
