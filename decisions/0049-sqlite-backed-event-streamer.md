# ADR-0049: Sqlite-backed EventStreamer event log

**Status**: Accepted
**Date**: 2026-07-17

## Context

ADR-0033 replaced `memstreamer` with `internal/eventstream`, a
`sync.Cond`-based `workflow.EventStreamer` that eliminated a CPU-burning
busy loop. That ADR explicitly kept the event log itself in-memory
(`[]*workflow.Event` + a `map[string]int` of cursors), reasoning that
durability already came from the RecordStore's transactional outbox
(ADR-0022) and that everflow is single-process.

That reasoning missed a gap: the outbox guarantees a `Store` call's event
eventually gets *appended* to the log, but says nothing about a receiver
that already consumed the event and is mid-step when the daemon dies. On
restart, `internal/eventstream.New()` starts with an empty log and zeroed
cursors — any event appended between the last flush and the crash is
gone, and any receiver's position is forgotten. `internal/reconciler`
(added to detect exactly this) papers over the symptom by polling for
Runs stuck in `StatusWorking` / `StatusDiscovering` past a threshold and
re-triggering them, but that's a timeout-driven workaround, not a fix —
it costs `threshold` of latency per lost event and depends on every
in-flight status being reconciler-visible.

## Decision

`internal/eventstream.Streamer` now persists its log and cursors in the
`event_log` / `event_cursors` sqlite tables (schema landed in a prior
increment; see `internal/store/sqlite.go`), sharing the same `*sql.DB`
handle as the daemon's `RecordStore` / `TimeoutStore` (`store.OpenSqlite`
returns a `*store.Backend`; `main.go`'s `cmdDaemon` passes `backend.DB()`
into `eventstream.New`).

Behaviourally:

- `Send` appends to `event_log` instead of a slice.
- Each receiver's cursor is a row in `event_cursors`, keyed by receiver
  name. `NewReceiver` only inserts a fresh row if one doesn't already
  exist for that name — on a restart, an existing row is left untouched,
  so the receiver resumes exactly where it left off rather than
  replaying from the start or skipping to latest.
- `Recv` still parks on `sync.Cond` — ADR-0033's fix stands unchanged.
  `Send`/`Ack` still `Broadcast()`. The cond and its mutex now guard
  sqlite reads/writes instead of a slice/map, so the single-process,
  cond-based signalling model is unchanged; only the storage underneath
  it moved to disk.
- `Ack` uses `context.WithoutCancel(ctx)` for its own write: a caller's
  ctx being cancelled concurrently with delivery must not stop the
  cursor advance, otherwise the just-processed event is redelivered
  after restart despite having been handled.

## Alternatives considered

- **Leave it in-memory, rely on the reconciler.** What we had. Silently
  bounds "how long can a Run be stuck" by the reconciler's poll
  threshold rather than closing the gap; every lost event still costs
  that latency and the reconciler needs to enumerate every in-flight
  status that can get stuck. Rejected — the reconciler is worth keeping
  as a defence-in-depth backstop for causes *other* than a crashed
  daemon (e.g. an operator's runner exiting) but shouldn't be the
  correctness argument for restart safety.
- **Separate sqlite file for the event log.** Isolates the streamer's
  schema from RecordStore/TimeoutStore's, but a daemon has exactly one
  storage durability requirement, and two files means two sets of
  connection/WAL settings to keep in sync and two failure points instead
  of one. Rejected — share the `*sql.DB` the RecordStore already opened.
- **Fold the event log into the outbox table (`ListOutboxEvents` /
  `DeleteOutboxEvent`).** The outbox is workflow-consumed-once-then-
  deleted, single-topic-implicit; the streamer's log is multi-topic,
  multi-cursor, retained (not deleted) so late-joining or slow receivers
  can still read old events. Different enough shapes that overloading
  one table would need conditional columns and confusing semantics.
  Rejected.

## Consequences

- A daemon restart can no longer lose or duplicate delivery of an
  in-flight event — this closes the gap `internal/reconciler`'s comment
  and DESIGN.md's "the state machine" section describe. The reconciler
  itself is untouched by this change (still valid as a backstop for
  non-restart stalls); a future increment can revisit whether its
  threshold should change now that the primary cause it targeted is
  fixed.
- `event_log` now grows unboundedly — nothing here deletes rows once
  every receiver's cursor has passed them. Fine for the durations
  everflow currently runs at; a retention/compaction pass (delete rows
  older than `MIN(cursor)` across `event_cursors`) is a reasonable
  follow-up if the table becomes large in practice.
- Every `Send`/`Recv`/`Ack` is now a sqlite round trip instead of a
  mutex-guarded slice append/read. Given the daemon's actual event rate
  (a handful of step transitions per Run, not a high-QPS stream), this
  is not expected to be a bottleneck; no benchmark was run to confirm,
  so revisit if profiling ever points here.
