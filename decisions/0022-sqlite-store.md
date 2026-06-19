# ADR-0022: Sqlite-backed RecordStore + TimeoutStore (pure-Go driver)

**Status**: Accepted
**Date**: 2026-06-19

## Context

[ADR-0003](0003-single-long-lived-daemon.md) called out daemon-restart
survival as load-bearing: "Persistent state (sqlite, or similar) becomes
necessary for the daemon to survive restarts without losing in-flight
Runs." The v1 scaffold ([ADR-0019](0019-project-layout.md)) used the
in-memory adapters from `luno/workflow` so that nothing else blocked on
the store decision — that's now resolved.

The two interfaces we need to implement are:
- `workflow.RecordStore` — runs + transactional outbox
- `workflow.TimeoutStore` — scheduled timer triggers

`luno/workflow` ships an `adapters/sqlstore` implementation but it's
MySQL-only (depends on `go-sql-driver/mysql`); not usable for our
single-node daemon. We need our own.

## Decisions

### 1. `modernc.org/sqlite` (pure Go), not `mattn/go-sqlite3` (cgo)

The two main sqlite drivers for Go are:

| Driver | Approach | Pros | Cons |
|---|---|---|---|
| `github.com/mattn/go-sqlite3` | cgo binding to system sqlite | Most popular, slightly faster, mature | Requires cgo: needs build toolchain, cross-compilation is painful, Docker images need build deps, ARM/Mac handoff is awkward |
| `modernc.org/sqlite` | sqlite C code transpiled to pure Go | No cgo, cross-compiles trivially, single static binary | Slightly slower (~10-30% in benchmarks), larger binary (~5MB more), less battle-tested in production |

For everflow — a daemon meant to be deployed easily on whatever Linux box
the user has (laptop, VPS, EC2, ARM Graviton, the Mac mini in the
cupboard) — **cross-compile ease > raw performance**. We're not running
high-QPS workloads against the store; the steady-state load is a handful
of writes per minute per Run. The 10-30% perf gap is invisible.

`modernc.org/sqlite` it is.

### 2. WAL mode + NORMAL sync, configured at open time

```
journal_mode=WAL          # one writer, many concurrent readers
synchronous=NORMAL        # fsync at checkpoint boundaries, not every write
foreign_keys=1            # standard hygiene
busy_timeout=5000         # 5s wait before SQLITE_BUSY for transient locks
```

WAL is essential because the daemon has multiple goroutines hitting the
DB simultaneously (workflow runtime + webhook server). The default
rollback journal mode would serialise everything.

`synchronous=NORMAL` is the right trade-off: durability on power-cut
events is reduced from "every commit fsynced" to "checkpoints fsynced,"
which can cost the last few seconds of writes in a power-loss scenario.
For everflow that's acceptable — re-running a refactor sweep from "as
of 5 seconds ago" is fine; webhook replay covers the gap.

### 3. Single DB file housing all three tables

`records`, `outbox`, and `timeouts` live in one sqlite file at
`~/.everflow/store.db`. One file is simpler to back up, copy, inspect
(`sqlite3 store.db .schema`), or wipe.

Separating outbox into its own file was considered (some MySQL outbox
patterns use a dedicated DB for replication isolation) — irrelevant for
sqlite, so we don't.

### 4. Facade pattern for the two interfaces (Go doesn't allow method overloading)

Both `workflow.RecordStore` and `workflow.TimeoutStore` have a `List`
method with different signatures. A single Go type cannot satisfy both
because Go disallows method overloading.

Solution: one `Backend` type owns the `*sql.DB`. It exposes two facade
types via `Backend.RecordStore()` and `Backend.TimeoutStore()`. The
facades share the connection and the clock; each implements its
respective workflow interface.

```go
b, _ := store.OpenSqlite("...")
rs, ts := b.RecordStore(), b.TimeoutStore()
```

Trade-off accepted: the daemon's call sites talk to two values, not one.
Minor.

### 5. Transactional outbox via `BEGIN; UPSERT record; INSERT outbox; COMMIT;`

`Store()` writes the record and the outbox event in one transaction. If
the transaction commits, both are durable; if it rolls back, neither
appears. This guarantees exactly-once event emission once a state
transition lands — the standard transactional-outbox pattern.

The outbox event ID comes from `workflow.MakeOutboxEventData(record)`
and we `INSERT OR IGNORE` so a retried `Store()` for the same logical
write doesn't insert a duplicate event.

### 6. Time as `INTEGER` (unix nanoseconds), not `TIMESTAMP`

Sqlite has no native timestamp type — `TIMESTAMP` resolves to TEXT or
NUMERIC depending on the driver and the storage class chosen. We
sidestep the ambiguity by storing `time.Time` as `int64` unix nanos.
Roundtrips deterministically; sorts correctly; doesn't depend on the
driver's stringification of timestamps.

### 7. RecordFilter applied in Go, not translated to SQL

`workflow.MakeFilter(filters...)` returns a value of an unexported type
whose `By*().Matches()` methods are exported but not introspectable into
SQL WHERE clauses. We load by `workflow_name` (the cheap SQL filter),
then apply the workflow `Filter` predicates in Go.

Trade-off: for very large record sets this is slower than SQL filtering.
For refactor workflows (dozens to hundreds of records per Run, single-
digit Runs per daemon), it's irrelevant. Revisit if a workflow ever
accumulates >100k records.

## Alternatives considered

- **Use the MySQL adapter from luno/workflow** with a MySQL daemon
  running locally. Heavyweight: requires MySQL install, config, perhaps a
  Docker container. Defeats the "small VPS, just run this binary"
  story.
- **A KV store (BoltDB, BadgerDB) instead of sqlite**: simpler API, but
  we'd lose ad-hoc inspectability (`sqlite3 store.db 'SELECT * FROM ...'`
  is the killer feature for a daemon). Trade-off goes the other way.
- **Postgres**: same downside as MySQL plus needs a running server.
  Reasonable v2 work for multi-daemon deployments; absolutely overkill
  for v1.
- **Pure in-memory + JSON snapshot on shutdown**: brittle and complicates
  every restart code path. Sqlite is mature and dull.

## Consequences

- The everflow binary now embeds the sqlite engine (~5MB binary growth).
  Acceptable.
- `~/.everflow/store.db` is the canonical state. Backing up everflow is
  "copy that file." Restoring is "put it back." Inspecting is
  `sqlite3 store.db`.
- The schema migration story for future ADRs: if we add columns or
  tables, we run `IF NOT EXISTS` DDL at startup (we already do this for
  the initial schema). Destructive changes (column type changes, drops)
  will need explicit migration logic — record that approach as a future
  ADR when it first matters.
- Tests use the in-memory `:memory:` mode and `t.TempDir()` files;
  `adaptertest.RunRecordStoreTest` + `RunTimeoutStoreTest` validate the
  contract. Both pass.
- The `Open(path)` helper in `internal/store/store.go` defaults to
  in-memory when `path == ""` so existing tests that didn't care about
  durability keep working without flag changes. Production callers pass
  a real path.
- Future ADRs may switch a deployed instance from sqlite to Postgres for
  multi-machine fleets. The `workflow.RecordStore` interface abstracts
  the storage — adding a Postgres adapter would not require changes
  anywhere else in everflow.
