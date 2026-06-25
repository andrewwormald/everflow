package store

import (
	"path/filepath"
	"testing"

	"github.com/luno/workflow"
	"github.com/luno/workflow/adapters/adaptertest"
)

// freshBackend returns a Backend pointed at a unique sqlite file inside
// t.TempDir(). Using a file (not :memory:) catches WAL-mode + on-disk
// behaviour that pure :memory: would mask.
func freshBackend(t *testing.T) *Backend {
	t.Helper()
	dir := t.TempDir()
	b, err := OpenSqlite(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestSqliteRecordStore_Conformance(t *testing.T) {
	adaptertest.RunRecordStoreTest(t, func() workflow.RecordStore {
		return freshBackend(t).RecordStore()
	})
}

func TestSqliteTimeoutStore_Conformance(t *testing.T) {
	adaptertest.RunTimeoutStoreTest(t, func() workflow.TimeoutStore {
		return freshBackend(t).TimeoutStore()
	})
}

// TestSqliteRestart proves the headline claim: a Run's state survives a
// process restart. We open a backend, write a record, close it, re-open
// at the same path, and read the record back.
func TestSqliteRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")

	b1, err := OpenSqlite(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	rs1 := b1.RecordStore()
	rec := &workflow.Record{
		WorkflowName: "refactor-sweep",
		ForeignID:    "fid-1",
		RunID:        "00000000-0000-0000-0000-000000000001",
		RunState:     workflow.RunStateInitiated,
		Status:       1,
		Object:       []byte(`{"goal":"test"}`),
	}
	if err := rs1.Store(t.Context(), rec); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Restart.
	b2, err := OpenSqlite(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	t.Cleanup(func() { _ = b2.Close() })
	rs2 := b2.RecordStore()
	got, err := rs2.Lookup(t.Context(), rec.RunID)
	if err != nil {
		t.Fatalf("Lookup after restart: %v", err)
	}
	if got.RunID != rec.RunID {
		t.Errorf("RunID: want %q, got %q", rec.RunID, got.RunID)
	}
	if got.WorkflowName != rec.WorkflowName {
		t.Errorf("WorkflowName: want %q, got %q", rec.WorkflowName, got.WorkflowName)
	}
	if string(got.Object) != string(rec.Object) {
		t.Errorf("Object: want %s, got %s", rec.Object, got.Object)
	}
}

// TestSqliteMetaRoundTrip regression-tests the bug that took the spike
// down: the workflow runtime bumps Record.Meta.Version before each Store
// and the consumer rejects events whose HeaderRecordVersion exceeds the
// stored value as "stale record lookup". If the store doesn't persist
// Meta.Version, every consumer fails on every event.
func TestSqliteMetaRoundTrip(t *testing.T) {
	rs := freshBackend(t).RecordStore()
	rec := &workflow.Record{
		WorkflowName: "refactor-sweep",
		ForeignID:    "fid-meta",
		RunID:        "00000000-0000-0000-0000-0000000000aa",
		RunState:     workflow.RunStateInitiated,
		Status:       1,
		Object:       []byte(`{}`),
		Meta: workflow.Meta{
			Version:           7,
			RunStateReason:    "paused for review",
			StatusDescription: "Initiated",
			TraceOrigin:       "main.go:42",
		},
	}
	if err := rs.Store(t.Context(), rec); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := rs.Lookup(t.Context(), rec.RunID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Meta.Version != 7 {
		t.Errorf("Meta.Version: want 7, got %d", got.Meta.Version)
	}
	if got.Meta.RunStateReason != "paused for review" {
		t.Errorf("Meta.RunStateReason: want %q, got %q", "paused for review", got.Meta.RunStateReason)
	}
	if got.Meta.StatusDescription != "Initiated" {
		t.Errorf("Meta.StatusDescription: want %q, got %q", "Initiated", got.Meta.StatusDescription)
	}
	if got.Meta.TraceOrigin != "main.go:42" {
		t.Errorf("Meta.TraceOrigin: want %q, got %q", "main.go:42", got.Meta.TraceOrigin)
	}
}
