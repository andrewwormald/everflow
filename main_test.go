package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/everflow/internal/refactorsweep"
	"github.com/andrewwormald/everflow/internal/runner"
	"github.com/andrewwormald/everflow/internal/store"
)

// captureStdout redirects os.Stdout to a pipe and returns a function that
// restores it and returns the captured output as a string.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	return func() string {
		w.Close()
		os.Stdout = old
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			t.Fatalf("io.Copy: %v", err)
		}
		return buf.String()
	}
}

// seedStore creates a temp sqlite store and inserts a single Record whose
// AgentState encodes the given state. Returns the store path.
func seedStore(t *testing.T, runID string, state refactorsweep.AgentState) string {
	t.Helper()
	dir := t.TempDir()
	sp := filepath.Join(dir, "store.db")

	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	obj, err := workflow.Marshal(&state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := &workflow.Record{
		WorkflowName: workflowName,
		ForeignID:    "test-foreign-id",
		RunID:        runID,
		RunState:     workflow.RunStateRunning,
		Status:       int(refactorsweep.StatusWorking),
		Object:       obj,
		UpdatedAt:    time.Now(),
	}
	if err := rs.Store(context.Background(), rec); err != nil {
		t.Fatalf("store.Store: %v", err)
	}
	return sp
}

func TestVersionString(t *testing.T) {
	orig := version
	origCommit := gitCommit
	origBuild := buildTime
	t.Cleanup(func() {
		version = orig
		gitCommit = origCommit
		buildTime = origBuild
	})

	version = "1.2.3"
	gitCommit = "abc1234"
	buildTime = "2026-07-03T12:00:00Z"

	got := versionString()
	want := "everflow 1.2.3 (commit: abc1234, built: 2026-07-03T12:00:00Z)"
	if got != want {
		t.Errorf("versionString() = %q, want %q", got, want)
	}
}

func TestDirectStatus_PrintsRunSummary(t *testing.T) {
	runID := "aaaaaaaa-0000-0000-0000-000000000001"
	state := refactorsweep.AgentState{
		Goal:         "migrate the acme/example service",
		ProviderName: "github",
		ProjectID:    "acme/example",
		TotalTokens:  42000,
		Budget:       runner.Budget{MaxTokens: 100000},
	}
	sp := seedStore(t, runID, state)

	flush := captureStdout(t)
	err := directStatus(context.Background(), sp, runID)
	out := flush()

	if err != nil {
		t.Fatalf("directStatus: %v", err)
	}
	for _, want := range []string{
		runID,
		"migrate the acme/example service",
		"github",
		"acme/example",
		"42000",
		"100000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n\nfull output:\n%s", want, out)
		}
	}
}

func TestDirectStatus_ListAllRuns(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "store.db")
	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	goals := []string{"migrate alpha service", "migrate beta service"}
	runIDs := []string{
		"bbbbbbbb-0001-0000-0000-000000000001",
		"bbbbbbbb-0002-0000-0000-000000000002",
	}
	for i, goal := range goals {
		state := refactorsweep.AgentState{Goal: goal}
		obj, mErr := workflow.Marshal(&state)
		if mErr != nil {
			t.Fatalf("marshal: %v", mErr)
		}
		rec := &workflow.Record{
			WorkflowName: workflowName,
			ForeignID:    "fid",
			RunID:        runIDs[i],
			RunState:     workflow.RunStateRunning,
			Status:       int(refactorsweep.StatusWorking),
			Object:       obj,
			UpdatedAt:    time.Now(),
		}
		if err := rs.Store(context.Background(), rec); err != nil {
			t.Fatalf("store.Store: %v", err)
		}
	}

	flush := captureStdout(t)
	err = directStatus(context.Background(), sp, "")
	out := flush()

	if err != nil {
		t.Fatalf("directStatus: %v", err)
	}
	if !strings.Contains(out, "RUN ID") {
		t.Errorf("missing table header in output:\n%s", out)
	}
	for _, want := range []string{"migrate alpha service", "migrate beta service"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing goal %q\n\nfull output:\n%s", want, out)
		}
	}
}
