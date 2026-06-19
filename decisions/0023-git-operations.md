# ADR-0023: Git operations — shell out to `git`, host-managed auth, local BaseRepo for v1

**Status**: Accepted
**Date**: 2026-06-19

## Context

The state machine bodies are wired but inert until git operations land:
`work()` calls `Provider.CreateMR` with a branch name that doesn't exist
remotely, and `resume()` can't actually commit a runner's fix to address a
review comment. This ADR records how git enters the picture.

Three orthogonal decisions baked in:

1. **How to talk to git**: shell out to the `git` binary, vs use a pure-Go
   library like `go-git`.
2. **How to authenticate pushes**: own credentials in the daemon, vs lean
   on the host's git configuration.
3. **Where the worktree base comes from**: a pre-existing local clone on
   disk (the v0 model), vs clone-from-provider on demand.

## Decisions

### 1. Shell out to the `git` CLI

```go
type Git interface {
    EnsureBranch(ctx, dir, baseRepo, baseBranch, branchName string) error
    HasChanges(ctx, dir string) (bool, error)
    Commit(ctx, dir, message string) error
    Push(ctx, dir, branchName string) error
    RemoveWorktree(ctx, baseRepo, dir string) error
}
```

`ExecGit` (the production impl) is ~150 LOC of `exec.Command("git", ...)`
wrappers. The fake in tests is ~50 LOC.

Rationale for choosing the CLI over `go-git`:

- The CLI is the *contract* every other tool integrates against. Behaviour
  matches what the user would observe running the same commands locally;
  bug reports translate one-to-one.
- `go-git` is ~70 KLOC of Go with documented edge-case gaps (large repos,
  worktrees, credential helpers, sparse checkout). Pulling it in means
  diverging from real-git in ways that are hard to find and harder to fix.
- Worktree semantics are *what* we use most. `git worktree add` /
  `worktree remove` are well-tested CLI surfaces; go-git's support is
  partial.
- The CLI is already installed on every dev machine and CI runner. Adding
  a 70-KLOC library to avoid one PATH check is a bad trade.

The downside is that we're now sensitive to git version drift. We pin
nothing and hope the surface we use is conservative. If we ever hit
version skew, add a version check at daemon startup.

### 2. Host-managed authentication for pushes

The daemon does **not** manage push credentials. Auth uses whatever the
host has configured:

- SSH keys in `~/.ssh/` (most common for `git@github.com:...` /
  `git@gitlab.com:...` remotes)
- Credential helpers (`git config credential.helper`)
- `GIT_ASKPASS` env vars
- Per-host `~/.netrc` (rare, but works)

We set `GIT_TERMINAL_PROMPT=0` so a missing credential fails fast instead
of hanging on an interactive prompt. The error propagates up — `work()`
returns `StatusFailed`; `resume()` returns `StatusPaused` (the MR exists,
recovery is possible by fixing creds and replying `/everflow retry`).

Rationale:

- Same pattern every other CI tool uses (GitLab Runner, GitHub Actions,
  Tekton). Operators already know how to set this up.
- Embedding tokens in HTTPS clone URLs (the alternative) creates secrets-
  in-config landmines and is the #1 source of accidental leaks in CI
  pipelines.
- Future ADRs may add a `--use-provider-token-for-push` mode that does
  embed a per-Run token in a credential helper. Out of scope for v1.

The daemon deployment story (README) needs a one-liner: "the system user
running everflow must have git push access to your project."

### 3. v1: `AgentState.BaseRepo` points at a pre-existing local clone

The daemon does **not** clone repos on demand. The user provides
`--base-repo /path/to/checkout` at `everflow start`, and that path is
captured on `AgentState.BaseRepo`. Worktrees are created off it via
`git worktree add`.

Constraints this imposes:

- The host must have a clone of the target repo somewhere readable +
  writable by the daemon user.
- The clone must have an `origin` remote pointing at the provider's
  repository (so `git fetch origin <base>` works).
- `git push -u origin <branch>` from inside any worktree pushes to the
  same `origin`, so push auth applies to that remote.

A future commit will add a "clone-from-provider" mode where everflow
clones the repo into `<RunsRoot>/<runID>/repo/` on demand. That's a
strict superset of this ADR; it doesn't supersede this one.

### 4. Worktree lifecycle = `git worktree`, one per unit

```
<RunsRoot>/<runID>/worktrees/<unitID>/
```

Created lazily by `EnsureBranch` when `work()` is about to invoke the
runner. Removed (best-effort) when the unit reaches a terminal state
(`markUnitMerged` / `markUnitBlacklisted`).

Removal failures are non-fatal — the refactor moves on; orphans get
cleaned up out-of-band via a future `everflow gc` command. We *do not*
let cleanup errors block forward progress because an unremovable
worktree shouldn't keep an otherwise-successful refactor from picking
up its next unit.

`EnsureBranch` is idempotent: if the dir is already a worktree on the
right branch, it's a no-op. If it's a worktree on the wrong branch,
that's an error (something is genuinely confused). If it's not a
worktree, create one.

### 5. Commit messages are templated by phase

| Phase | Subject |
|---|---|
| Initial unit change | `<Goal>: <unitID>` |
| Address review comment | `Address review feedback on <unitID> from @<reviewer>` |
| Fix CI | `Fix CI on <unitID> (pipeline <ID>)` |

Each body ends with `Generated by everflow run <shortRunID>.` so `git
log` of a refactor maps onto MRs in the UI.

### 6. Behaviour when runner returns `Done` with a clean worktree

The runner can legitimately decide "this unit doesn't need changes"
(checked the codebase, the standardisation is already applied). v1's
default for this in `work()`:

- Blacklist the unit with reason `"runner returned Done with no changes"`
- Remove the worktree
- Return `StatusDiscovering` (pick the next unit)

The blacklist entry makes it observable in `everflow status` (future
CLI). The "no changes needed" outcome isn't a failure — it's a real
result that needs to be in the audit trail.

For `resume()` (addressing a comment), `Done`-without-changes posts an
informational comment ("(No code changes were needed.)") and stays in
`AwaitingMerge`. Distinct from `work()` because the MR already exists;
the comment thread is the right place for the human to clarify if needed.

## Alternatives considered

- **Use `go-git` instead of the CLI** — see §1. Net negative for the
  surface we need.
- **Daemon owns push credentials (PAT injected into HTTPS URLs)** — see
  §2. Secrets-in-config risk too high for v1.
- **Daemon clones on demand** — see §3. Strict-superset feature deferred.
- **One commit per file change** — over-noisy; the runner already
  produces a coherent set of changes per invocation, so one commit per
  invocation matches the MR conversation.
- **Squash-on-merge convention** — out of scope; that's a provider/repo
  policy, not an everflow concern.

## Consequences

- The host running the daemon must have `git` on `$PATH` and a clone of
  the target repo. README needs a deployment-prereqs section.
- Every `git push` from the daemon goes through whatever credential
  helper / SSH agent is configured for the daemon user. Production
  deployments will want a dedicated `everflow-bot` system user with its
  own SSH key in the provider's allowed keys list.
- `git` version drift is a future risk. Mitigation deferred until it
  bites; add a version check then.
- Tests use a `fakeGit` in step-body tests (so they don't shell out) plus
  a real ExecGit suite (`internal/git/git_test.go`) that exercises the
  full lifecycle against an on-disk repo created in `t.TempDir()`.
- Adding clone-from-provider is additive: it sets `BaseRepo` automatically
  by cloning into a known path before `work()` runs. No interface
  changes needed.
