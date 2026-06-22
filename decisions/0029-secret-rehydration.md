# ADR-0029: Secret rehydration on daemon startup

**Status**: Accepted
**Date**: 2026-06-22

## Context

[ADR-0016](0016-mr-comments-only-channel.md) makes webhooks load-bearing
for everflow — every inbound provider event flows through HMAC
verification by the in-memory `webhook.SecretRegistry`. [ADR-0022](0022-sqlite-store.md)
gives Runs durable persistence across daemon restarts. The gap noted in
the `resume()` commit (and reiterated in inline TODOs throughout v1):

> The in-memory SecretRegistry empties on restart and isn't repopulated
> from durable AgentState.WebhookSecret until setup() runs again — which
> it won't for Runs past Initiated. Daemon restart breaks webhook
> ingress for existing Runs.

This ADR closes that gap.

## Decisions

### 1. At startup, iterate active Runs and re-populate the registry

main.go calls `rehydrateSecrets(ctx, recordStore, secrets, logger)`
*before* mounting the webhook server. The function:

1. Pages through `recordStore.List(workflowName, offset, 200, asc)`
2. For each record:
   - Skip if `record.RunState.Finished()` (workflow-level terminal)
   - Skip if our status is `Completed`/`Failed`/`Cancelled`
   - Unmarshal `record.Object` into `refactorsweep.AgentState`
   - Skip if `WebhookSecret == ""` or `ProviderName == ""`
   - `secrets.Set(state.ProviderName, record.RunID, state.WebhookSecret)`
3. Logs a count when done

Total cost: one pass over the active Run set at startup. For a daemon
hosting dozens of long-lived Runs, this is hundreds of records to
deserialize — milliseconds at most.

### 2. Failures don't block startup

If `rehydrateSecrets` returns an error (e.g. the store is unreachable on
the first paginated call), main.go logs a `Warn` and continues. The
daemon starts; webhook ingress for affected Runs returns 401 until
their setup() runs again or the daemon next restarts cleanly. The
operator gets a log entry to investigate.

Why not fail-fast: rehydration is a *recovery* mechanism. Failing
startup because recovery had a hiccup punishes the operator twice
(restart + restart of restart).

### 3. Per-record failures are logged, not fatal

If a single record fails to unmarshal (corrupt JSON, schema drift),
`rehydrateSecrets` logs and moves on. Better to skip one bad row than
abort and leave 99 good Runs with empty registry entries.

### 4. `isActiveStatus` lives in main.go, not refactorsweep

The function is short (one switch) and exists specifically to gate
rehydration. Putting it in `refactorsweep` would invite callers to use
it for other "is this Run done?" checks, where the right answer is to
ask the workflow library directly via `RunState.Finished()`.

By keeping it in main.go as `isActiveStatus`, we make it clear this is
a local-to-this-function decision.

## Alternatives considered

- **Don't rehydrate; let each restart be a "rebuild" by re-running
  setup()** — Runs past `Initiated` don't re-enter setup, so this is a
  non-starter without a separate "re-run setup on restart" mechanism
  that defeats the durable-state model.
- **Store the registry on disk separately** — another piece of state to
  persist and reconcile. The store already has the AgentState; rehydrate
  from there.
- **Use a workflow OnComplete hook to clean up secrets** — that handles
  the *removal* side cleanly (and we should add it as a follow-up), but
  doesn't help with the *load on startup* problem.
- **Skip rehydration; assume the daemon is short-lived** — defeats the
  durability story everflow exists for.

## Consequences

- main.go's `cmdDaemon` gains a rehydration step before the webhook
  server is mounted. Adds ~50 LOC including the helper functions.
- `workflow.Unmarshal[T]` is used to deserialise records, mirroring how
  the workflow library itself reads them — keeps the format authoritative
  in one place.
- One log line at startup if rehydration found anything ("rehydrated
  webhook secrets from store" with `count`); silent if there's nothing
  to do.
- Future: a *teardown* hook would remove entries from the registry when
  a Run transitions to Completed/Failed/Cancelled. Not strictly
  necessary (the registry tolerates a lookup miss as "unknown runID"),
  but tidy. Track as a v2 nicety.
- The pagination loop uses 200-record pages and stops when a partial
  page is returned. Standard offset-based pagination — fine for v1's
  expected scale (single-digit-to-low-hundreds of active Runs).
