package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/everflow/internal/config"
	"github.com/andrewwormald/everflow/internal/eventstream"
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

// seedStoreMulti creates a temp sqlite store seeded with one Record per
// runID at the given status. Used by prefix-matching tests.
func seedStoreMulti(t *testing.T, runIDs []string, status refactorsweep.AgentStatus) string {
	t.Helper()
	sp := filepath.Join(t.TempDir(), "store.db")
	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	for i, rid := range runIDs {
		obj, err := workflow.Marshal(&refactorsweep.AgentState{Goal: "seed"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rec := &workflow.Record{
			WorkflowName: workflowName,
			ForeignID:    fmt.Sprintf("fid-%d", i),
			RunID:        rid,
			RunState:     workflow.RunStateRunning,
			Status:       int(status),
			Object:       obj,
			UpdatedAt:    time.Now(),
		}
		if err := rs.Store(context.Background(), rec); err != nil {
			t.Fatalf("store.Store: %v", err)
		}
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

func TestDirectList(t *testing.T) {
	type seedRun struct {
		runID string
		state refactorsweep.AgentState
	}
	tests := []struct {
		name     string
		seeds    []seedRun
		wantOut  []string
		wantNone bool
	}{
		{
			name:     "empty store",
			seeds:    nil,
			wantNone: true,
		},
		{
			name: "single run",
			seeds: []seedRun{
				{
					runID: "aaaaaaaa-1111-0000-0000-000000000001",
					state: refactorsweep.AgentState{
						Goal: "migrate the acme service",
						Mode: "spec",
					},
				},
			},
			wantOut: []string{"aaaaaaaa-1111...", "spec", "migrate the acme service"},
		},
		{
			name: "multi run",
			seeds: []seedRun{
				{
					runID: "bbbbbbbb-0001-0000-0000-000000000001",
					state: refactorsweep.AgentState{Goal: "fix the alpha bug", Mode: "sweep"},
				},
				{
					runID: "cccccccc-0002-0000-0000-000000000002",
					state: refactorsweep.AgentState{Goal: "add beta feature", Mode: "spec"},
				},
			},
			wantOut: []string{"fix the alpha bug", "add beta feature", "bbbbbbbb-0001...", "cccccccc-0002..."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sp := filepath.Join(dir, "store.db")

			if len(tc.seeds) > 0 {
				rs, _, err := store.Open(sp)
				if err != nil {
					t.Fatalf("store.Open: %v", err)
				}
				for _, s := range tc.seeds {
					obj, err := workflow.Marshal(&s.state)
					if err != nil {
						t.Fatalf("marshal: %v", err)
					}
					rec := &workflow.Record{
						WorkflowName: workflowName,
						ForeignID:    "fid",
						RunID:        s.runID,
						RunState:     workflow.RunStateRunning,
						Status:       int(refactorsweep.StatusWorking),
						Object:       obj,
						UpdatedAt:    time.Now(),
					}
					if err := rs.Store(context.Background(), rec); err != nil {
						t.Fatalf("store.Store: %v", err)
					}
				}
			}

			flush := captureStdout(t)
			err := directList(context.Background(), sp)
			out := flush()

			if err != nil {
				t.Fatalf("directList: %v", err)
			}
			if tc.wantNone {
				if !strings.Contains(out, "no runs found") {
					t.Errorf("expected 'no runs found', got:\n%s", out)
				}
				return
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\n\nfull output:\n%s", want, out)
				}
			}
		})
	}
}

func TestDaemonBannerLine(t *testing.T) {
	orig, origCommit := version, gitCommit
	t.Cleanup(func() { version, gitCommit = orig, origCommit })

	version = "2.3.4"
	gitCommit = "deadbeef"

	banner := daemonBannerLine()

	wants := []string{
		"everflow daemon starting",
		"version=2.3.4",
		"commit=deadbeef",
		fmt.Sprintf("pid=%d", os.Getpid()),
		fmt.Sprintf("go=%s", runtime.Version()),
		fmt.Sprintf("os=%s", runtime.GOOS),
		fmt.Sprintf("arch=%s", runtime.GOARCH),
	}
	for _, w := range wants {
		if !strings.Contains(banner, w) {
			t.Errorf("banner missing %q\n\nfull banner: %s", w, banner)
		}
	}
}

func TestBuildSweeper_WiredToDaemonDeps(t *testing.T) {
	dir := t.TempDir()
	backend, err := store.OpenSqlite(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	recordStore := backend.RecordStore()
	streamer := eventstream.New(backend.DB())
	threshold := 42 * time.Minute
	logger := discardLogger()

	sweeper := buildSweeper(recordStore, streamer, threshold, logger)

	if sweeper.Store != recordStore {
		t.Errorf("Store = %v, want the daemon's recordStore", sweeper.Store)
	}
	if sweeper.Streamer != streamer {
		t.Errorf("Streamer = %v, want the daemon's EventStreamer", sweeper.Streamer)
	}
	if sweeper.WorkflowName != workflowName {
		t.Errorf("WorkflowName = %q, want %q", sweeper.WorkflowName, workflowName)
	}
	if sweeper.Threshold != threshold {
		t.Errorf("Threshold = %v, want %v", sweeper.Threshold, threshold)
	}
	if sweeper.Logger != logger {
		t.Errorf("Logger = %v, want the daemon's logger", sweeper.Logger)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

// Full-UUID run IDs sharing a leading substring, for prefix-matching tests.
var prefixRunIDs = []string{
	"00000000-0000-0000-0000-000000000001",
	"00000000-0000-0000-0000-000000000002",
	"00000000-0000-0000-0000-000000000003",
}

// TestPrefixMatching covers the three subcommands (status/abandon/resume) for
// ambiguous, unique-full, and no-match prefixes. abandon/resume mutate state
// on success, so each subtest reseeds a fresh temp store.
func TestPrefixMatching(t *testing.T) {
	const ambiguous = "000000"
	const noMatch = "00000000-0000-0000-0000-000000000fff"
	full := prefixRunIDs[0]

	// resume requires Cancelled/Failed/Paused; the others accept Working.
	subcmds := []struct {
		name   string
		status refactorsweep.AgentStatus
		invoke func(ctx context.Context, storePath, runID string) error
	}{
		{"status", refactorsweep.StatusWorking, directStatus},
		{"abandon", refactorsweep.StatusWorking, func(ctx context.Context, sp, rid string) error {
			return directAbandon(ctx, sp, rid, "", "", "")
		}},
		{"resume", refactorsweep.StatusPaused, directResume},
	}
	for _, sc := range subcmds {
		t.Run(sc.name, func(t *testing.T) {
			seed := func() string { return seedStoreMulti(t, prefixRunIDs, sc.status) }

			err := sc.invoke(context.Background(), seed(), ambiguous)
			if err == nil {
				t.Fatal("ambiguous: expected error")
			}
			for _, id := range prefixRunIDs {
				if !strings.Contains(err.Error(), id) {
					t.Errorf("ambiguous error missing %q; got: %s", id, err)
				}
			}

			flush := captureStdout(t)
			if err := sc.invoke(context.Background(), seed(), full); err != nil {
				_ = flush()
				t.Fatalf("full uuid: %v", err)
			}
			_ = flush()

			err = sc.invoke(context.Background(), seed(), noMatch)
			if err == nil || !strings.Contains(err.Error(), "no run matches prefix") {
				t.Errorf("no match: want 'no run matches prefix' err, got: %v", err)
			}
		})
	}
}

// TestDaemonUnreachableHint asserts that when the daemon is unreachable AND
// no store fallback exists, all three subcommands surface a hint pointing at
// --store. HOME is redirected to an empty temp dir so ~/.everflow/store.db
// doesn't exist.
func TestDaemonUnreachableHint(t *testing.T) {
	const unreachable = "http://127.0.0.1:9" // reserved "discard" port
	t.Setenv("HOME", t.TempDir())

	subcmds := []struct {
		name string
		run  func(args []string) error
	}{
		{"status", cmdStatus},
		{"abandon", cmdAbandon},
		{"resume", cmdResume},
	}
	for _, sc := range subcmds {
		t.Run(sc.name, func(t *testing.T) {
			err := sc.run([]string{"--daemon", unreachable, prefixRunIDs[0]})
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "is unreachable") || !strings.Contains(msg, "--store") {
				t.Errorf("want 'is unreachable' and '--store' hint in error; got: %s", msg)
			}
		})
	}
}

// TestCmdSetup_NonInteractiveDefaultsToClaudeNoModel asserts that a
// non-interactive `everflow setup` (test binaries don't run with a stdin
// TTY) with no flags auto-selects the sole registered runner and leaves the
// model unset rather than hanging on a prompt.
func TestCmdSetup_NonInteractiveDefaultsToClaudeNoModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup(nil); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Runner != "claude" {
		t.Fatalf("got runner %q, want %q", cfg.Runner, "claude")
	}
	if cfg.Model != "" {
		t.Fatalf("got model %q, want empty (no TTY, no --model)", cfg.Model)
	}
}

// TestCmdSetup_ModelFlagPersists asserts --model is persisted verbatim.
func TestCmdSetup_ModelFlagPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup([]string{"--model", "claude-haiku-4-5"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Model != "claude-haiku-4-5" {
		t.Fatalf("got model %q, want %q", cfg.Model, "claude-haiku-4-5")
	}
}

// TestCmdSetup_RerunWithoutModelFlagKeepsExisting asserts that re-running
// setup non-interactively without --model doesn't clobber a previously
// persisted model back to empty.
func TestCmdSetup_RerunWithoutModelFlagKeepsExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup([]string{"--model", "claude-sonnet-5"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}
	if err := cmdSetup(nil); err != nil {
		t.Fatalf("cmdSetup (rerun): %v", err)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Model != "claude-sonnet-5" {
		t.Fatalf("got model %q, want previously persisted value kept", cfg.Model)
	}
}

// TestCmdSetup_UnknownRunnerFlagErrors asserts an unrecognised --runner
// fails loudly instead of silently persisting a bogus default.
func TestCmdSetup_UnknownRunnerFlagErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup([]string{"--runner", "not-a-real-runner"}); err == nil {
		t.Fatal("expected an error for an unknown runner")
	}
}

// TestCmdSetup_NoTitleConventionFlagWritesNothing asserts a non-interactive
// setup with no --title-convention leaves .everflow.yml absent rather than
// writing an empty convention.
func TestCmdSetup_NoTitleConventionFlagWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	if err := cmdSetup(nil); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	if _, err := os.Stat(".everflow.yml"); !os.IsNotExist(err) {
		t.Fatalf("expected no .everflow.yml, stat err = %v", err)
	}
}

// TestCmdSetup_TitleConventionFlagPersists asserts --title-convention is
// written verbatim to .everflow.yml in the current directory.
func TestCmdSetup_TitleConventionFlagPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	if err := cmdSetup([]string{"--title-convention", "Conventional Commits"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	data, err := os.ReadFile(".everflow.yml")
	if err != nil {
		t.Fatalf("read .everflow.yml: %v", err)
	}
	if !strings.Contains(string(data), "title_convention: Conventional Commits") {
		t.Fatalf("got %q, want it to contain the given title convention", string(data))
	}
}

// TestCmdSetup_TitleConventionDoesNotClobberExistingWithoutForce asserts a
// rerun without --force leaves a pre-existing .everflow.yml untouched, even
// when a new --title-convention is passed.
func TestCmdSetup_TitleConventionDoesNotClobberExistingWithoutForce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	if err := os.WriteFile(".everflow.yml", []byte("title_convention: existing\n"), 0o644); err != nil {
		t.Fatalf("seed existing .everflow.yml: %v", err)
	}

	if err := cmdSetup([]string{"--title-convention", "new convention"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	data, err := os.ReadFile(".everflow.yml")
	if err != nil {
		t.Fatalf("read .everflow.yml: %v", err)
	}
	if string(data) != "title_convention: existing\n" {
		t.Fatalf("existing .everflow.yml was clobbered: %q", string(data))
	}
}

// startTriggerCapture spins up a fake daemon that records the decoded
// triggerRequest of the last /trigger POST it receives.
func startTriggerCapture(t *testing.T) (url string, got *triggerRequest) {
	t.Helper()
	got = &triggerRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(got); err != nil {
			t.Errorf("decode trigger request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(triggerResponse{RunID: "run-1", ForeignID: "foreign-1"})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, got
}

// TestCmdStart_FallsBackToPersistedDefaultModel asserts that when neither
// --model nor the spec's `model:` set a runner model, cmdStart falls back
// to the default persisted by `everflow setup` (ADR-0051).
func TestCmdStart_FallsBackToPersistedDefaultModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(home, config.Config{Runner: "claude", Model: "claude-haiku-4-5"}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	daemonURL, got := startTriggerCapture(t)

	err := cmdStart([]string{
		"--units", "u1",
		"--provider", "gitlab",
		"--project", "acme/example",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
	})
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if got.RunnerModel != "claude-haiku-4-5" {
		t.Fatalf("got runner model %q, want persisted default %q", got.RunnerModel, "claude-haiku-4-5")
	}
}

// TestCmdStart_ModelFlagOverridesPersistedDefault asserts --model still
// wins over a persisted default.
func TestCmdStart_ModelFlagOverridesPersistedDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(home, config.Config{Runner: "claude", Model: "claude-haiku-4-5"}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	daemonURL, got := startTriggerCapture(t)

	err := cmdStart([]string{
		"--units", "u1",
		"--provider", "gitlab",
		"--project", "acme/example",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
		"--model", "claude-sonnet-5",
	})
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if got.RunnerModel != "claude-sonnet-5" {
		t.Fatalf("got runner model %q, want flag override %q", got.RunnerModel, "claude-sonnet-5")
	}
}

// TestCmdStart_NoConfigLeavesModelEmpty asserts that with no persisted
// config and no --model, the runner model stays empty (runner's own
// default applies).
func TestCmdStart_NoConfigLeavesModelEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	daemonURL, got := startTriggerCapture(t)

	err := cmdStart([]string{
		"--units", "u1",
		"--provider", "gitlab",
		"--project", "acme/example",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
	})
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if got.RunnerModel != "" {
		t.Fatalf("got runner model %q, want empty", got.RunnerModel)
	}
}
