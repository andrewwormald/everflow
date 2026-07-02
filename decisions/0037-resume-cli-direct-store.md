# ADR-0037 — `everflow abandon` + `everflow resume` CLI: direct-store fallback

**Date:** 2026-07-02  
**Status:** Accepted

## Context

The `everflow abandon` and `everflow resume` CLI commands previously delegated entirely to the daemon via `POST /control`. This works when the daemon is running, but fails as a rescue tool for the exact situations where it's most needed: a crashed daemon, a stuck Run that caused the daemon to wedge, or a Run that ended up in `StatusCancelled`/`StatusFailed` which the `/control` endpoint cannot revive (because `resume()` is only registered as a callback for `StatusAwaitingMerge` and `StatusPaused`, not terminal states).

The 2026-06-29 dogfood spike illustrated this: Run `cc2383f8` required a manual `sqlite3 UPDATE records ...` to recover — which doesn't trigger the workflow library's outbox and therefore doesn't actually reanimate the state machine.

## Decision

Both commands now:

1. **Try the daemon first** (unchanged behaviour when the daemon is running).
2. **Fall back to direct sqlite manipulation** when the daemon is unreachable (connection refused).

The direct path uses `store.Open(storePath)` → `RecordStore.Lookup` → mutate the `workflow.Record` → `RecordStore.Store`. The `Store()` method is part of the `workflow.RecordStore` interface and performs an atomic upsert + outbox event insertion in a single transaction. This is the same path the workflow runtime uses internally, so the daemon picks up the outbox event on its next consumer poll without manual intervention.

### `everflow abandon` (direct path)

- Reads the record; refuses if RunState already Finished.
- Sets `AgentState.LastError`, `Status = StatusCancelled`, `RunState = RunStateCancelled`, `Meta.Version++`.
- Closes in-flight MRs best-effort via the provider (loaded from env/glab/gh credentials).
- Removes per-unit worktrees best-effort.
- **No two-tap confirmation** — the direct path is an explicit rescue tool, not the normal workflow interaction.

### `everflow resume` (direct path)

- Reads the record; accepts only `StatusCancelled`, `StatusFailed`, or `StatusPaused` (direct-revive is for terminal/stuck Runs).
- Sets `AgentState.LastError = ""`, `AgentState.PauseReason = ""`, `Status = StatusDiscovering`, `RunState = RunStateRunning`, `Meta.Version++`.
- Calls `Store()`, which inserts an outbox event. The workflow engine fires `discover()` on the next consumer poll.
- **Daemon must be running (or restarted)** to process the outbox event. The CLI prints a reminder.

### Why `Meta.Version++`?

The workflow library's consumer checks that the outbox event's embedded version matches the record's current version. If we didn't bump the version, the consumer would see the event's version ≠ record version and treat it as stale. Bumping before `Store()` ensures the outbox event and the record share the same version number.

### Why not a dedicated `Resuming` status?

An alternative considered: add `StatusResuming` with an `AddStep` that transitions to `Discovering`. This avoids writing directly to the store. It was rejected because:

1. Adding a status for a CLI-only flow adds state-machine complexity with no runtime benefit.
2. The direct-store approach is already safe: `RecordStore.Store()` is the authorised mutation path (the workflow runtime calls it for every state update).
3. The `Resuming` status would still require knowing the daemon is running to process it — no advantage over the current design.

## Consequences

- `everflow abandon <runID>` works without the daemon (as documented in TROUBLESHOOTING.md).
- `everflow resume <runID>` works without the daemon, but the daemon must be restarted to pick up the revived Run.
- The `--store` flag on both commands accepts a custom store path for non-default deployments.
- Test coverage: `directAbandon`/`directResume` are exercised in `main_test.go` (TODO: integration test seeding a Failed Run, calling resume, restarting the workflow, asserting the Run advances).
