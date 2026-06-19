// Sqlite-backed implementations of workflow.RecordStore and
// workflow.TimeoutStore. See ADR-0022 for the library + schema choices.
//
// Backend owns the *sql.DB. RecordStore and TimeoutStore are facade types —
// both interfaces have a `List` method (with different signatures), and Go
// does not permit overloading, so we expose two distinct types backed by the
// same connection.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"k8s.io/utils/clock"

	"github.com/luno/workflow"

	_ "modernc.org/sqlite" // pure-Go driver; no cgo, cross-compiles trivially
)

// Backend wraps an open sqlite database. Use RecordStore() and TimeoutStore()
// to obtain the workflow-facing facades.
type Backend struct {
	db     *sql.DB
	clock  clock.Clock
	mu     sync.Mutex // sqlite handles concurrent reads but serialised writes via WAL; this mutex narrows the window during multi-step Store transactions
}

// OpenSqlite creates the file if it doesn't exist, applies the schema, and
// configures the connection for our access pattern (WAL, NORMAL sync, foreign
// keys on). path may be a filesystem path or ":memory:" for an in-memory DB.
// Parent directories are created on demand.
func OpenSqlite(path string) (*Backend, error) {
	if path != ":memory:" {
		if err := ensureDir(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("ensure parent dir: %w", err)
		}
	}
	// `_pragma` query args are honoured by modernc.org/sqlite at connection time.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One writer / many readers is the sqlite story. Cap to a small pool to
	// avoid contention; the workflow runtime is not a high-QPS consumer.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Backend{db: db, clock: clock.RealClock{}}, nil
}

// Close releases the database handle.
func (b *Backend) Close() error { return b.db.Close() }

// RecordStore returns the workflow.RecordStore facade.
func (b *Backend) RecordStore() *RecordStore { return &RecordStore{b: b} }

// TimeoutStore returns the workflow.TimeoutStore facade.
func (b *Backend) TimeoutStore() *TimeoutStore { return &TimeoutStore{b: b} }

const schemaSQL = `
CREATE TABLE IF NOT EXISTS records (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    workflow_name TEXT    NOT NULL,
    foreign_id    TEXT    NOT NULL,
    run_id        TEXT    NOT NULL UNIQUE,
    run_state     INTEGER NOT NULL,
    status        INTEGER NOT NULL,
    object        BLOB,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_records_workflow_foreign ON records(workflow_name, foreign_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_records_workflow_id ON records(workflow_name, id);

CREATE TABLE IF NOT EXISTS outbox (
    id            TEXT    NOT NULL PRIMARY KEY,
    workflow_name TEXT    NOT NULL,
    data          BLOB    NOT NULL,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbox_workflow_created ON outbox(workflow_name, created_at);

CREATE TABLE IF NOT EXISTS timeouts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    workflow_name TEXT    NOT NULL,
    foreign_id    TEXT    NOT NULL,
    run_id        TEXT    NOT NULL,
    status        INTEGER NOT NULL,
    completed     INTEGER NOT NULL DEFAULT 0,
    expire_at     INTEGER NOT NULL,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_timeouts_workflow_status_expire ON timeouts(workflow_name, status, expire_at, completed);
`

// --- RecordStore ---

// RecordStore implements workflow.RecordStore on top of a Backend.
type RecordStore struct {
	b *Backend
}

// Store upserts the record and appends an outbox event in one transaction —
// the transactional outbox pattern that guarantees exactly-once event emission
// once the record commit lands.
func (r *RecordStore) Store(ctx context.Context, record *workflow.Record) error {
	eventData, err := workflow.MakeOutboxEventData(*record)
	if err != nil {
		return fmt.Errorf("make outbox event: %w", err)
	}

	r.b.mu.Lock()
	defer r.b.mu.Unlock()

	tx, err := r.b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	now := r.b.clock.Now()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now

	// Upsert by run_id (UNIQUE). Created_at is preserved on conflict.
	const upsert = `
INSERT INTO records (workflow_name, foreign_id, run_id, run_state, status, object, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id) DO UPDATE SET
    workflow_name = excluded.workflow_name,
    foreign_id    = excluded.foreign_id,
    run_state     = excluded.run_state,
    status        = excluded.status,
    object        = excluded.object,
    updated_at    = excluded.updated_at;`
	if _, err := tx.ExecContext(ctx, upsert,
		record.WorkflowName, record.ForeignID, record.RunID,
		int(record.RunState), record.Status, record.Object,
		record.CreatedAt.UnixNano(), record.UpdatedAt.UnixNano(),
	); err != nil {
		return fmt.Errorf("upsert record: %w", err)
	}

	const insertOutbox = `INSERT OR IGNORE INTO outbox (id, workflow_name, data, created_at) VALUES (?, ?, ?, ?);`
	if _, err := tx.ExecContext(ctx, insertOutbox,
		eventData.ID, eventData.WorkflowName, eventData.Data, r.b.clock.Now().UnixNano(),
	); err != nil {
		return fmt.Errorf("insert outbox: %w", err)
	}

	return tx.Commit()
}

func (r *RecordStore) Lookup(ctx context.Context, runID string) (*workflow.Record, error) {
	row := r.b.db.QueryRowContext(ctx, `
SELECT workflow_name, foreign_id, run_id, run_state, status, object, created_at, updated_at
FROM records WHERE run_id = ?`, runID)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, workflow.ErrRecordNotFound
	}
	return rec, err
}

func (r *RecordStore) Latest(ctx context.Context, workflowName, foreignID string) (*workflow.Record, error) {
	row := r.b.db.QueryRowContext(ctx, `
SELECT workflow_name, foreign_id, run_id, run_state, status, object, created_at, updated_at
FROM records WHERE workflow_name = ? AND foreign_id = ?
ORDER BY id DESC LIMIT 1`, workflowName, foreignID)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, workflow.ErrRecordNotFound
	}
	return rec, err
}

// List returns the slice of records matching (workflowName, filters...),
// offset/limit applied AFTER filtering — same semantics as memrecordstore.
// We load by workflow_name and apply RecordFilter in Go because the filter
// internals aren't exported in a SQL-translatable shape.
func (r *RecordStore) List(
	ctx context.Context,
	workflowName string,
	offset int64,
	limit int,
	order workflow.OrderType,
	filters ...workflow.RecordFilter,
) ([]workflow.Record, error) {
	if limit == 0 {
		limit = defaultListLimit
	}

	rows, err := r.b.db.QueryContext(ctx, `
SELECT workflow_name, foreign_id, run_id, run_state, status, object, created_at, updated_at
FROM records WHERE workflow_name = ? OR ? = ''
ORDER BY id ASC`, workflowName, workflowName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	filter := workflow.MakeFilter(filters...)
	byFID := filter.ByForeignID()
	bySt := filter.ByStatus()
	byRS := filter.ByRunState()
	byAft := filter.ByCreatedAtAfter()
	byBef := filter.ByCreatedAtBefore()

	var all []workflow.Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		if byFID.Enabled && !byFID.Matches(rec.ForeignID) {
			continue
		}
		if bySt.Enabled && !bySt.Matches(strconv.FormatInt(int64(rec.Status), 10)) {
			continue
		}
		if byRS.Enabled && !byRS.Matches(strconv.FormatInt(int64(rec.RunState), 10)) {
			continue
		}
		if byAft.Enabled && !byAft.Matches(rec.CreatedAt) {
			continue
		}
		if byBef.Enabled && !byBef.Matches(rec.CreatedAt) {
			continue
		}
		all = append(all, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Apply offset (1-based, like memrecordstore) then limit.
	start := offset
	if start < 0 {
		start = 0
	}
	if start >= int64(len(all)) {
		return nil, nil
	}
	end := start + int64(limit)
	if end > int64(len(all)) {
		end = int64(len(all))
	}
	page := all[start:end]

	if order == workflow.OrderTypeDescending {
		out := make([]workflow.Record, len(page))
		for i, rec := range page {
			out[len(page)-1-i] = rec
		}
		return out, nil
	}
	return page, nil
}

func (r *RecordStore) ListOutboxEvents(ctx context.Context, workflowName string, limit int64) ([]workflow.OutboxEvent, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := r.b.db.QueryContext(ctx, `
SELECT id, workflow_name, data, created_at
FROM outbox WHERE workflow_name = ?
ORDER BY created_at ASC LIMIT ?`, workflowName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []workflow.OutboxEvent
	for rows.Next() {
		var (
			ev      workflow.OutboxEvent
			created int64
		)
		if err := rows.Scan(&ev.ID, &ev.WorkflowName, &ev.Data, &created); err != nil {
			return nil, err
		}
		ev.CreatedAt = time.Unix(0, created)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (r *RecordStore) DeleteOutboxEvent(ctx context.Context, id string) error {
	_, err := r.b.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id)
	return err
}

// --- TimeoutStore ---

// TimeoutStore implements workflow.TimeoutStore on top of a Backend.
type TimeoutStore struct {
	b *Backend
}

func (t *TimeoutStore) Create(ctx context.Context, workflowName, foreignID, runID string, status int, expireAt time.Time) error {
	_, err := t.b.db.ExecContext(ctx, `
INSERT INTO timeouts (workflow_name, foreign_id, run_id, status, completed, expire_at, created_at)
VALUES (?, ?, ?, ?, 0, ?, ?)`,
		workflowName, foreignID, runID, status, expireAt.UnixNano(), t.b.clock.Now().UnixNano())
	return err
}

func (t *TimeoutStore) Complete(ctx context.Context, id int64) error {
	_, err := t.b.db.ExecContext(ctx, `UPDATE timeouts SET completed = 1 WHERE id = ?`, id)
	return err
}

func (t *TimeoutStore) Cancel(ctx context.Context, id int64) error {
	_, err := t.b.db.ExecContext(ctx, `DELETE FROM timeouts WHERE id = ?`, id)
	return err
}

func (t *TimeoutStore) List(ctx context.Context, workflowName string) ([]workflow.TimeoutRecord, error) {
	rows, err := t.b.db.QueryContext(ctx, `
SELECT id, workflow_name, foreign_id, run_id, status, completed, expire_at, created_at
FROM timeouts WHERE workflow_name = ? ORDER BY id ASC`, workflowName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTimeouts(rows)
}

func (t *TimeoutStore) ListValid(ctx context.Context, workflowName string, status int, now time.Time) ([]workflow.TimeoutRecord, error) {
	rows, err := t.b.db.QueryContext(ctx, `
SELECT id, workflow_name, foreign_id, run_id, status, completed, expire_at, created_at
FROM timeouts WHERE workflow_name = ? AND status = ? AND completed = 0 AND expire_at <= ?
ORDER BY expire_at ASC`, workflowName, status, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTimeouts(rows)
}

// --- internal helpers ---

const defaultListLimit = 25

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(s rowScanner) (*workflow.Record, error) {
	var (
		rec               workflow.Record
		runState          int
		created, updated  int64
	)
	if err := s.Scan(
		&rec.WorkflowName, &rec.ForeignID, &rec.RunID,
		&runState, &rec.Status, &rec.Object,
		&created, &updated,
	); err != nil {
		return nil, err
	}
	rec.RunState = workflow.RunState(runState)
	rec.CreatedAt = time.Unix(0, created)
	rec.UpdatedAt = time.Unix(0, updated)
	return &rec, nil
}

func scanTimeouts(rows *sql.Rows) ([]workflow.TimeoutRecord, error) {
	var out []workflow.TimeoutRecord
	for rows.Next() {
		var (
			tr               workflow.TimeoutRecord
			completed        int
			expire, created  int64
		)
		if err := rows.Scan(&tr.ID, &tr.WorkflowName, &tr.ForeignID, &tr.RunID, &tr.Status, &completed, &expire, &created); err != nil {
			return nil, err
		}
		tr.Completed = completed != 0
		tr.ExpireAt = time.Unix(0, expire)
		tr.CreatedAt = time.Unix(0, created)
		out = append(out, tr)
	}
	return out, rows.Err()
}

func ensureDir(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
