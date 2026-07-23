# ADR-0071: Retention sweeper loop wired into the daemon

**Status**: Accepted
**Date**: 2026-07-23

## Context

ADR-0070 added `RecordStore.ListTerminalRuns` and `RecordStore.DeleteRun` —
the storage-layer half of the 31-day-default retention sweep the full spec
goal calls for. Its Consequences section is explicit that nothing calls
either method yet: no sweeper loop, no CLI flag, no on-disk run directory
cleanup. This ADR covers that remaining half.

## Decision

Add `internal/retention.Sweeper`, following the same shape as
`internal/reconciler.Sweeper` (ADR-0053): a struct with a `Run(ctx)` method
that ticks a `time.Ticker` until `ctx` is cancelled, calling `sweepOnce` each
tick.

```go
type Sweeper struct {
    Store           Store // ListTerminalRuns / Lookup / DeleteRun
    Git             git.Git
    RunsRoot        string
    RetentionPeriod time.Duration
    Interval        time.Duration
    Logger          *slog.Logger
}
```

Each tick: `ListTerminalRuns(ctx, now.Add(-RetentionPeriod))`, then for each
Run: remove its on-disk run directory (`<RunsRoot>/<runID>`), then
`DeleteRun`. Filesystem cleanup runs first and is best-effort (logged, not
fatal) — a failure there shouldn't block the row deletion that actually
stops the Run from being listed again next tick.

Before `os.RemoveAll`-ing the run directory, the sweeper looks up the Run's
record, decodes `refactorsweep.AgentState`, and calls `Git.RemoveWorktree`
for every worktree it can still find (each `InFlight` unit's worktree, plus
the spec-mode planning worktree). This is belt-and-braces: normal operation
already removes a unit's worktree as it completes (see
`internal/refactorsweep/workflow.go`), so by the time a Run is terminal its
worktrees are almost always already gone. But a Run that crashed mid-flight
before its own cleanup ran would otherwise leave a stale `git worktree`
registration in `BaseRepo`'s `.git` after `os.RemoveAll` deletes the
directory out from under it — `RemoveWorktree` un-registers it properly
first. This mirrors the `abandon` command's existing best-effort worktree
removal loop in `main.go`.

`RetentionPeriod <= 0` disables the sweep outright (nothing is ever old
enough), rather than treating it as "no cutoff" and deleting every terminal
Run on the first tick — the safer failure mode for a zero-value/misconfigured
flag.

The daemon wires it in `cmdDaemon` alongside `pollerLoop` and the reconciler
`sweeper`, via a new `--retention-period` flag (`time.Duration`, default
`31 * 24 * time.Hour`, i.e. 744h — the spec's 31-day default). The sweep's
own tick `Interval` is a fixed constant (1 hour), not a flag: unlike the
reconciler sweep, which reacts to a stuck Run needing a re-trigger,
retention cleanup firing an hour later instead of a minute later is
immaterial — matching `internal/poller`'s hardcoded 30s `Interval`, which
also isn't a flag.

## Alternatives considered

- **Expose sweep interval as a flag too.** Rejected for the same reason
  `internal/poller`'s Interval isn't a flag — there's no operational
  scenario where tuning "how often we check whether it's time to sweep"
  (as opposed to "how old before we sweep") matters enough to justify a
  second knob.
- **Skip worktree cleanup, just `os.RemoveAll` the run directory.** Simpler,
  but leaves a dangling `git worktree` registration in `BaseRepo`'s `.git`
  for any Run whose worktree wasn't already removed by normal operation —
  a slow, silent leak in the shared base repo that `git worktree list`
  would show growing forever. Rejected in favour of the belt-and-braces
  `RemoveWorktree` pass, which is cheap and already idempotent (see
  `git.RemoveWorktree`'s doc comment).
- **Take `*store.RecordStore` directly instead of a narrow `Store`
  interface.** `internal/reconciler` takes `workflow.RecordStore` (the
  library interface) rather than a concrete type, so it can be tested
  against a fake. `ListTerminalRuns`/`DeleteRun` aren't part of that library
  interface — they're `internal/store`-specific — so `retention.Store` is a
  small interface naming exactly the three methods the sweeper calls,
  keeping the package unit-testable without a real sqlite file.

## Consequences

- Every terminal Run older than `--retention-period` (default 31 days) is
  now actually deleted — records, timeouts, outbox, event_log rows (ADR-0070)
  and its on-disk run directory — instead of accumulating forever.
- A daemon operator can disable the sweep entirely with
  `--retention-period=0`, or shorten/lengthen it per environment.
- The sweeper depends on `refactorsweep.AgentState`'s shape (`BaseRepo`,
  `InFlight`) to find worktrees to clean up. If that shape changes,
  `internal/retention`'s decode step needs updating alongside it — same
  coupling `internal/reconciler` and `internal/poller` already have via
  `decodeActiveRun`/`refactorsweep.AgentStatus`.
