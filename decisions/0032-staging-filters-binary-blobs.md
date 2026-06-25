# ADR-0032: Commit stages selectively; binary blobs are filtered out

**Status**: Accepted
**Date**: 2026-06-24

## Context

End-to-end testing surfaced a class of commit failure rooted in
`internal/git.Commit` calling `git add -A` indiscriminately. When the
runner ran `go build` inside the worktree to verify its change locally,
the resulting binary landed alongside the source file; `git add -A`
swept the binary into the index, and repo pre-commit hooks that cap
added-file sizes then aborted the commit. Because `work()` returned
`(StatusFailed, err)`, the luno/workflow library treated the error as
transient and retried the step every ~1s indefinitely.

Two distinct issues:

1. The commit logic was indiscriminate about untracked files — anything
   `git status` reported went in. That makes the runner's side effects
   (build artefacts, test outputs, IDE droppings) part of the MR.
2. The state-machine error contract conflated transient failures
   (network blip, provider 5xx) with permanent ones (hook abort,
   malformed code), and workflow's retry semantics did the wrong thing
   for the latter.

## Decision

**1. `Commit` filters untracked files through a binary heuristic before
staging.** The new flow is:

- `git reset` — drop any stale staged paths from a prior aborted
  attempt.
- `git add -u .` — stage modifications/deletions to already-tracked
  files.
- `git ls-files --others --exclude-standard` — enumerate untracked,
  non-ignored files.
- For each, read the first 512 bytes. If they contain a NUL, skip it
  (the standard "is this binary?" heuristic git itself uses for diff
  colouring). Otherwise `git add -- <path>`.
- If nothing ends up staged after filtering, return `ErrNoChanges`
  rather than letting `git commit` fail with "nothing to commit".

The decision applies only to *untracked* files. If the repo already
tracks a binary blob (rare but legitimate — e.g. a small wasm shim),
`git add -u` still picks up modifications to it.

**2. `StatusFailed` is terminal.** `work()` now returns
`(StatusFailed, nil)` on every operational failure path
(`EnsureBranch`, `runner.Run`, `HasChanges`, `Commit`, `Push`,
`CreateMR`), captures the cause in `AgentState.LastError`, and lets
the workflow library commit the terminal state instead of retrying
forever. The doc comment on `StatusFailed` already said "unrecoverable;
worktree kept for forensics" — this change makes the implementation
match the contract.

**3. `Commit` returning `ErrNoChanges` blacklists the unit.** If the
runner produced only filtered content (binaries, ignored files), we
treat it the same as "runner returned Done with no changes":
blacklist the unit, drop the worktree, return to `Discovering`. The
Run continues to other units rather than terminating.

## Alternatives considered

- **Add `.git/info/exclude` patterns per worktree.** Would require
  knowing every build-artefact name pattern in advance — fragile
  against new languages, language-version changes, tool defaults.
- **Tell the runner not to compile.** Brittle; the runner has good
  reasons to run `go build` / `go test` for self-verification. Better
  to let it do whatever it likes inside the worktree and just be
  selective about what reaches the index.
- **Size-only filter (skip files > 1 MB).** Misses small binaries
  (compiled test helpers, native deps) and would false-positive on
  legitimate large text files like generated SQL dumps or test
  fixtures. The NUL-byte heuristic is more semantically aligned: we
  want to exclude *binary payloads*, not *large files*.
- **Surface the StatusFailed-must-be-nil rule via a typed sentinel
  error.** Considered defining `var ErrTerminal = errors.New(...)` and
  wrapping permanent failures; the workflow library would still treat
  it as a retry signal because it inspects `err != nil`, not the
  error's identity. Returning `nil` is the only contract the library
  currently honours.

## Consequences

- Runners can `go build` / `go test` inside the worktree without risk
  of the resulting binaries ending up in the MR. This matters for any
  workflow that wants the runner to verify its own change locally.
- `Commit` is now more expensive (one extra `ls-files` plus a
  stat-and-read-512B per untracked file). For single-file changes
  this is invisible; for broad refactors with hundreds of untracked
  files it adds a few milliseconds. Acceptable.
- The error contract for step bodies is now: return `nil` for
  committed terminal failures, return a non-nil error only for
  genuinely transient conditions that benefit from luno/workflow's
  automatic retry. This rule applies to *all* steps, not just
  `work()`; future steps should follow it.
- We've introduced a hidden behaviour ("untracked binaries silently
  skipped") that surfaces via stderr only. If this bites us, the next
  step is to surface skipped paths in the MR description or in
  `AgentState.History` so the author can see what wasn't committed.
- The same Commit-failure path no longer retries forever; instead it
  records `LastError` on the AgentState and the Run becomes
  inspectable via the read API. Recovery requires a fresh trigger —
  there is no in-place "fix and retry" yet.
