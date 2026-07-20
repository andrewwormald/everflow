# ADR-0059: Retry-with-backoff on fetch to ride out shared base_repo ref-lock contention

**Status**: Accepted
**Date**: 2026-07-20

## Context

Multiple units run concurrently against the same shared `base_repo`:
`EnsureBranch` fetches directly in `base_repo` to branch a new worktree off
current `origin/<baseBranch>`, and `HardReset`/`SyncWithBase` fetch inside
worktrees of that same repo — worktrees share the parent repo's `.git`
(refs, `packed-refs`, remote-tracking refs) even though each has its own
working directory. `git fetch` takes locks on some of those shared files
(e.g. `refs/remotes/origin/<branch>.lock`, `packed-refs.lock`) for the
duration of the update.

When two Runs' fetches land at the same moment, one wins the lock and the
other fails immediately with an error like `fatal: Unable to create
'.../refs/remotes/origin/main.lock': File exists.` or `another git process
seems to be running in this repository`. That failure is transient — the
losing fetch would succeed a moment later once the winner releases the
lock — but today it propagates straight up as a hard error from
`EnsureBranch`/`HardReset`/`SyncWithBase`, failing a unit's turn for a
condition that resolves itself within milliseconds.

## Decision

Add a generic `withRetry` helper in `internal/git/git.go` and wire it
around the `fetch` invocation in each of the three affected methods
(`EnsureBranch`, `HardReset`, `SyncWithBase`).

```go
type retryConfig struct {
	attempts  int
	baseDelay time.Duration
	sleep     func(time.Duration)
}

var defaultFetchRetry = retryConfig{
	attempts:  5,
	baseDelay: 200 * time.Millisecond,
	sleep:     time.Sleep,
}

func withRetry(ctx context.Context, cfg retryConfig, isRetryable func(error) bool, fn func() error) error
```

`withRetry` calls `fn` up to `cfg.attempts` times, doubling the delay
between attempts (200ms, 400ms, 800ms, 1.6s across 5 attempts — roughly 3s
worst case) and stopping early if `ctx` is done or `isRetryable` returns
false. The final attempt's error is returned unwrapped so callers keep
their existing `fmt.Errorf("...: fetch: %w", err)` wrapping unchanged.

`isRetryable` is `isLockContention`, which matches git's own stderr text
for lock contention (`"unable to create"`, `"cannot lock ref"`, `"could
not lock"`, `"another git process"`) case-insensitively. Anything else —
bad remote, auth failure, network down — returns immediately without
retrying, since retrying those wastes time on a failure that won't
resolve itself.

`sleep` is a struct field (not a package-level `var`) specifically so
tests can substitute a no-op or call-recording function without a global
swap — following the file's existing preference for narrow, explicit test
seams (`envProbe`) over package-level indirection.

## Alternatives considered

- **Retry every git subprocess call, not just fetch.** Broader, but the
  other mutating calls (`commit`, `push`, `worktree add`, `reset --hard`)
  either don't touch the shared `base_repo`'s refs the same way fetch does,
  or have their own distinct failure semantics (e.g. `push` rejecting on
  a real non-fast-forward, which must not be retried blindly). Scoping
  the retry to fetch — the specific operation that contends on shared
  ref locks — avoids papering over unrelated failures with the same
  blanket retry.
- **Detect contention by checking for a `.lock` file on disk before
  fetching, rather than parsing git's stderr after a failed attempt.**
  Rejected: the lock file's existence between the check and the actual
  fetch call is inherently racy (TOCTOU), and different git operations
  take different lock files depending on what they're updating — parsing
  the error git already produced when the fetch itself collides is more
  reliable than guessing which lock file to look for beforehand.
- **Use `context` deadline/timeout alone instead of a bounded attempt
  count.** A raw timeout doesn't give a predictable, boundable worst-case
  latency contract the way "5 attempts, doubling from 200ms" does, and
  doesn't distinguish "still retrying a resolvable lock" from "stuck in a
  tight loop on a permanent failure" the way `isRetryable` does. Attempt
  count with backoff, plus a ctx-done escape hatch, covers both.

## Consequences

- `internal/git/git.go` gained `retryConfig`, `defaultFetchRetry`,
  `withRetry`, and `isLockContention` — all unexported, package-internal.
  No change to the `Git` interface or its method signatures, so
  `internal/refactorsweep/workflow_test.go`'s `fakeGit` double needs no
  updates.
- `EnsureBranch`, `HardReset`, and `SyncWithBase` each retry their fetch up
  to 5 times (≈3s worst case) before surfacing a fetch failure — a unit
  that used to fail its turn on transient lock contention now succeeds
  transparently in the common case.
- A genuine fetch failure (bad ref, auth, network) still fails on the
  first attempt, unchanged from today — `isLockContention` only recognizes
  git's lock-contention wording.
- Left for a follow-on increment (per the spec's own suggested split):
  cleaning up orphaned partial state left by a `worktree add` that fails
  mid-creation in `EnsureBranch` — this increment only adds retry around
  the fetch that precedes it.

## Tests

`internal/git/git_test.go`:
- `TestWithRetry_SucceedsAfterTransientFailures` — two lock-contention
  failures followed by success; asserts `withRetry` returns `nil`, calls
  `fn` exactly 3 times, and sleeps with the expected doubling delays
  (10ms, 20ms) between retries only, not after the final success.
- `TestWithRetry_BoundedFailure_ReturnsLastError` — every attempt fails
  with a retryable error; asserts `withRetry` stops at exactly
  `cfg.attempts` calls (doesn't loop forever) and surfaces the last
  attempt's error.
- `TestWithRetry_NonRetryableError_ReturnsImmediately` — a non-lock error;
  asserts `withRetry` calls `fn` exactly once and never sleeps.
