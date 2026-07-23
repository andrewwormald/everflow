package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luno/workflow"
)

// storeRecord is a small helper: Store a record in the given RunState and
// return it, so tests don't repeat the same boilerplate.
func storeRecord(t *testing.T, rs *RecordStore, runID string, state workflow.RunState) *workflow.Record {
	t.Helper()
	rec := &workflow.Record{
		WorkflowName: "retention-test",
		ForeignID:    "fid-" + runID,
		RunID:        runID,
		RunState:     state,
		Status:       1,
		Object:       []byte(`{}`),
	}
	if err := rs.Store(t.Context(), rec); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := rs.Lookup(t.Context(), runID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return got
}

func TestListTerminalRuns_FiltersByRunState(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()

	storeRecord(t, rs, "run-running", workflow.RunStateRunning)
	storeRecord(t, rs, "run-cancelled", workflow.RunStateCancelled)
	storeRecord(t, rs, "run-completed", workflow.RunStateCompleted)
	storeRecord(t, rs, "run-paused", workflow.RunStatePaused)

	got, err := rs.ListTerminalRuns(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ListTerminalRuns: %v", err)
	}

	ids := make(map[string]bool)
	for _, tr := range got {
		ids[tr.RunID] = true
	}
	if !ids["run-cancelled"] || !ids["run-completed"] {
		t.Errorf("expected cancelled and completed runs in result, got %v", ids)
	}
	if ids["run-running"] || ids["run-paused"] {
		t.Errorf("running/paused runs must not be terminal, got %v", ids)
	}
	if len(got) != 2 {
		t.Errorf("want 2 terminal runs, got %d: %+v", len(got), got)
	}
}

func TestListTerminalRuns_FiltersByAge(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()

	rec := storeRecord(t, rs, "run-just-completed", workflow.RunStateCompleted)

	// A cutoff before the record's UpdatedAt excludes it: not old enough yet.
	got, err := rs.ListTerminalRuns(t.Context(), rec.UpdatedAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ListTerminalRuns: %v", err)
	}
	for _, tr := range got {
		if tr.RunID == rec.RunID {
			t.Errorf("run updated at %v should not match cutoff %v", rec.UpdatedAt, rec.UpdatedAt.Add(-time.Hour))
		}
	}

	// A cutoff after the record's UpdatedAt includes it: old enough.
	got, err = rs.ListTerminalRuns(t.Context(), rec.UpdatedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListTerminalRuns: %v", err)
	}
	found := false
	for _, tr := range got {
		if tr.RunID == rec.RunID {
			found = true
		}
	}
	if !found {
		t.Errorf("run updated at %v should match cutoff %v", rec.UpdatedAt, rec.UpdatedAt.Add(time.Hour))
	}
}

func TestDeleteRun_RemovesAcrossAllTables(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()
	ts := b.TimeoutStore()

	const targetRun = "run-to-delete"
	const otherRun = "run-to-keep"

	storeRecord(t, rs, targetRun, workflow.RunStateCompleted)
	storeRecord(t, rs, otherRun, workflow.RunStateCompleted)

	if err := ts.Create(t.Context(), "retention-test", "fid-"+targetRun, targetRun, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("timeout Create (target): %v", err)
	}
	if err := ts.Create(t.Context(), "retention-test", "fid-"+otherRun, otherRun, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("timeout Create (other): %v", err)
	}

	// event_log rows are written by the eventstream sender in production;
	// insert directly here since this test only cares about RecordStore's
	// deletion behaviour, not the streamer.
	insertEventLog := `INSERT INTO event_log (topic, foreign_id, type, headers, run_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := b.DB().ExecContext(t.Context(), insertEventLog, "topic", "fid-"+targetRun, 1, nil, targetRun, time.Now().UnixNano()); err != nil {
		t.Fatalf("insert event_log (target): %v", err)
	}
	if _, err := b.DB().ExecContext(t.Context(), insertEventLog, "topic", "fid-"+otherRun, 1, nil, otherRun, time.Now().UnixNano()); err != nil {
		t.Fatalf("insert event_log (other): %v", err)
	}

	if err := rs.DeleteRun(t.Context(), targetRun); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}

	if _, err := rs.Lookup(t.Context(), targetRun); err != workflow.ErrRecordNotFound {
		t.Errorf("records: want ErrRecordNotFound for deleted run, got %v", err)
	}
	if _, err := rs.Lookup(t.Context(), otherRun); err != nil {
		t.Errorf("records: other run should be untouched, got %v", err)
	}

	assertRowCount(t, b, "timeouts", targetRun, 0)
	assertRowCount(t, b, "timeouts", otherRun, 1)
	assertRowCount(t, b, "event_log", targetRun, 0)
	assertRowCount(t, b, "event_log", otherRun, 1)
}

func TestDeleteRun_UnknownRunIDIsNoop(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()

	if err := rs.DeleteRun(t.Context(), "does-not-exist"); err != nil {
		t.Fatalf("DeleteRun on unknown run_id should be a no-op, got: %v", err)
	}

	// Calling it twice on the same (now nonexistent) run must also be fine.
	storeRecord(t, rs, "run-x", workflow.RunStateCompleted)
	if err := rs.DeleteRun(t.Context(), "run-x"); err != nil {
		t.Fatalf("first DeleteRun: %v", err)
	}
	if err := rs.DeleteRun(t.Context(), "run-x"); err != nil {
		t.Fatalf("second DeleteRun should also be a no-op: %v", err)
	}
}

// TestOpenSqlite_BackfillsEventLogRunID simulates a daemon that has been
// running since before the run_id column existed: an event_log row with
// run_id left at its ALTER TABLE default of '', as every pre-migration row
// would be. Re-opening that same file must backfill run_id from the row's
// existing headers blob, so DeleteRun can find and clean up that Run's
// history once it goes terminal — without this, the row would be stuck
// with run_id = '' forever and DeleteRun would never match it.
func TestOpenSqlite_BackfillsEventLogRunID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")

	b, err := OpenSqlite(path)
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}

	const legacyRun = "run-from-before-migration"
	headers := []byte(`{"run_id":"` + legacyRun + `"}`)
	_, err = b.DB().ExecContext(t.Context(), `
INSERT INTO event_log (topic, foreign_id, type, headers, run_id, created_at) VALUES (?, ?, ?, ?, '', ?)`,
		"topic", "fid-"+legacyRun, 1, headers, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("insert legacy event_log row: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-opening the same file is what a restarted/upgraded daemon does;
	// this must run the backfill against the row inserted above.
	b2, err := OpenSqlite(path)
	if err != nil {
		t.Fatalf("re-open OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b2.Close() })

	assertRowCount(t, b2, "event_log", legacyRun, 1)

	rs := b2.RecordStore()
	storeRecord(t, rs, legacyRun, workflow.RunStateCompleted)
	if err := rs.DeleteRun(t.Context(), legacyRun); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	assertRowCount(t, b2, "event_log", legacyRun, 0)
}

func assertRowCount(t *testing.T, b *Backend, table, runID string, want int) {
	t.Helper()
	var got int
	err := b.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table+" WHERE run_id = ?", runID).Scan(&got)
	if err != nil {
		t.Fatalf("count %s for %s: %v", table, runID, err)
	}
	if got != want {
		t.Errorf("%s rows for run_id=%s: want %d, got %d", table, runID, want, got)
	}
}
