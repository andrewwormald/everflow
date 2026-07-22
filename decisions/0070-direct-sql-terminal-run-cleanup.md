# ADR-0070: Direct-SQL deletion for terminal-Run retention cleanup

**Status**: Accepted
**Date**: 2026-07-22

## Context

Everflow keeps every Run's `records` row, its `timeouts`, and (per
ADR-0049) its `event_log` entries forever — nothing ever deletes them.
ADR-0049's Consequences section already flagged the `event_log` half of
this: "grows unboundedly... a retention/compaction pass... is a
reasonable follow-up if the table becomes large in practice." The full
spec goal for this work is a 31-day-default retention sweep that removes
old, terminal Runs' records and on-disk run directories. This ADR covers
only the storage layer: how a terminal Run's rows get deleted, not the
sweeper loop or filesystem cleanup (those are later increments).

The obvious reuse candidate is `workflow.RunStateController.DeleteData`
— the library already has a concept of removing a Run's data. It doesn't
fit:

- `DeleteData` is obtained from a live, typed `*workflow.Run[Type,
  Status]` (`runstate.go`'s `NewRunStateController` takes a `storeFunc`
  and `*Record` bound to one workflow's generic `Type`/`Status`, built
  via `buildRun` inside a running `Workflow[Type, Status]`). A retention
  sweep has to work across every registered workflow from the outside —
  it only has a `RecordStore` and a `run_id` string, not a typed `Run`.
  There's no exported constructor that turns `(store, run_id)` into a
  `RunStateController` without the generic workflow instance in hand.
- Even if it were reachable, `DeleteData` doesn't delete rows. It sets
  `RunState` to `RunStateRequestedDataDeleted`, which routes an event to
  the delete topic; `deleteConsumer` picks it up, replaces `Object` with
  a redacted payload (or a `customDelete`-provided one), and moves the
  Run to `RunStateDataDeleted` (`delete.go`). The record row, its
  timeouts, and its event-log history all still exist afterwards. That's
  the right behaviour for GDPR-style redaction of live data — it's not
  row cleanup.

## Decision

Add two methods directly to `internal/store.RecordStore`, operating on
raw SQL against the same tables `RecordStore`/`TimeoutStore` already own,
no workflow-runtime involvement:

```go
// TerminalRun is a Run eligible for retention cleanup.
type TerminalRun struct {
    RunID        string
    WorkflowName string
    ForeignID    string
    UpdatedAt    time.Time
}

// ListTerminalRuns returns Runs in RunStateCancelled or RunStateCompleted
// whose UpdatedAt is at or before olderThan.
func (r *RecordStore) ListTerminalRuns(ctx context.Context, olderThan time.Time) ([]TerminalRun, error)

// DeleteRun permanently removes a Run's records, outbox, timeouts and
// event_log rows. Idempotent: deleting an unknown or already-deleted
// run_id is a no-op, not an error.
func (r *RecordStore) DeleteRun(ctx context.Context, runID string) error
```

`ListTerminalRuns` is a plain `SELECT ... WHERE run_state IN (4, 5) AND
updated_at <= ?` against `records` — both columns are already indexed
for other queries and this adds nothing new to the schema.

`DeleteRun` runs one transaction:

```sql
DELETE FROM records    WHERE run_id = ?;
DELETE FROM timeouts   WHERE run_id = ?;
DELETE FROM outbox     WHERE run_id = ?;
DELETE FROM event_log  WHERE run_id = ?;
```

`records` and `timeouts` already have a `run_id` column. `outbox` and
`event_log` don't — today `outbox` only carries `run_id` inside its
protobuf-serialised `data` blob (`outboxpb.OutboxRecord`, an *internal*
package of the `luno/workflow` module we cannot import), and `event_log`
only carries it inside its JSON `headers` blob. Decoding either at
delete time to recover `run_id` would mean either duplicating the
protobuf schema or scanning-and-JSON-decoding every row for the target
workflow. Instead, this increment adds a `run_id TEXT NOT NULL DEFAULT
''` column to both tables, populated from data the writer already has in
hand — `RecordStore.Store` already holds `record.RunID` when it inserts
the outbox row, and `eventstream.sender.Send` already receives
`headers[workflow.HeaderRunID]` when it inserts the event-log row.
Existing sqlite files get the column via a startup `ALTER TABLE ... ADD
COLUMN` (guarded against `duplicate column name`, sqlite has no `ADD
COLUMN IF NOT EXISTS`), matching ADR-0022's "additive schema changes run
at startup" story. Both tables get a `run_id` index so the delete is
indexed, not a table scan.

`event_cursors` is explicitly **excluded** from `DeleteRun`, despite
being one of the tables this increment's planning research named. A
cursor row is keyed by `(receiver name, topic)`, not by Run — it's the
high-water mark a *receiver* has processed on a topic that many Runs'
events flow through over the receiver's lifetime. There is no "this
Run's cursor rows" to delete; deleting a Run's `event_log` rows doesn't
invalidate any cursor either, since a cursor is just an integer
high-water mark; not a foreign key into specific rows. Retention for
`event_cursors` is a different problem (a defunct receiver name lingering
after a workflow is decommissioned) that ADR-0049's "future
retention/compaction pass" note already anticipated as separate from
per-Run cleanup, and stays out of scope here.

## Alternatives considered

- **Route through `workflow.DeleteData`.** Rejected — see Context. Not
  reachable from an untyped external sweep, and semantically wrong even
  if it were (redacts, doesn't delete).
- **Decode `outbox.data` / `event_log.headers` at delete time instead of
  adding `run_id` columns.** Avoids a schema change but means importing
  an internal package we don't have access to (`outboxpb`) for outbox,
  and a JSON-decode-every-row-then-filter pass for event_log on every
  sweep tick. Rejected — the writers already have `run_id` in hand; a
  plain indexed column is simpler and cheaper than decode-to-filter, and
  consistent with how `records`/`timeouts` already store it.
- **Soft-delete (mark rows, sweep them out on a slower cadence)
  instead of a straight `DELETE`.** Adds a second cleanup cycle and a new
  "marked but not yet physically removed" state to reason about, for no
  benefit here — `DeleteRun` fires only after the retention sweep has
  already decided the Run is old enough, so there's no undo window worth
  protecting.
- **Delete `event_cursors` rows matching the Run's topics.** Considered
  and rejected — see Decision. A cursor isn't scoped to one Run, so
  there's nothing correct to delete per-Run; doing so would either be a
  no-op (nothing matches) or, if implemented via topic match, would risk
  deleting a live receiver's position for *other* Runs still flowing
  through that topic.

## Consequences

- `outbox` and `event_log` each gain a `run_id` column and index. Startup
  cost is one `ALTER TABLE` per table per process start (skipped once
  the column exists); negligible at everflow's table sizes.
- `DeleteRun` is safe to call from an external sweep with only a
  `run_id` string — no generic workflow instance, no live `*workflow.Run`
  needed — which is the property the planned sweeper actually requires.
  It's also safe to call on an already-cleaned-up or never-existing
  `run_id`: every `DELETE` matches zero rows and the transaction commits
  as a no-op, so a sweeper that reruns after a partial failure (crash
  mid-sweep) doesn't need its own idempotency tracking.
- Nothing calls `ListTerminalRuns` or `DeleteRun` yet — no sweeper loop
  exists in this increment, so terminal Runs keep accumulating exactly
  as before until that follow-up lands.
- `outbox` rows for a given Run are normally drained (sent + deleted) by
  `purgeOutbox` within one poll cycle of `Store`, so by the time a Run is
  old enough to be swept, its `outbox` rows have almost always already
  been deleted by the library itself; `DeleteRun`'s `outbox` delete is a
  belt-and-braces catch for the rare row still in flight (e.g. daemon
  killed between insert and the outbox consumer's delete), not the
  common case.
