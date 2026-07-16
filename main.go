// Everflow — bulk-refactor sweep CLI. See README.md, DESIGN.md, and the
// decisions/ log for the project's purpose and design.
//
// This file is the CLI surface; business logic lives under internal/.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/luno/workflow"

	"github.com/luno/workflow/adapters/memrolescheduler"

	"github.com/andrewwormald/everflow/internal/eventstream"
	"github.com/andrewwormald/everflow/internal/git"
	"github.com/andrewwormald/everflow/internal/poller"
	"github.com/andrewwormald/everflow/internal/provider"
	"github.com/andrewwormald/everflow/internal/provider/github"
	"github.com/andrewwormald/everflow/internal/provider/gitlab"
	"github.com/andrewwormald/everflow/internal/refactorsweep"
	"github.com/andrewwormald/everflow/internal/runner"
	"github.com/andrewwormald/everflow/internal/runner/claude"
	"github.com/andrewwormald/everflow/internal/spec"
	"github.com/andrewwormald/everflow/internal/store"
	"github.com/andrewwormald/everflow/internal/webhook"
)

const (
	workflowName = "refactor-sweep"
)

var (
	version   = "0.0.1-scaffold"
	gitCommit = "none"
	buildTime = "unknown"
)

var commands = map[string]command{
	"daemon":  {usage: "run the long-lived daemon", run: cmdDaemon},
	"start":   {usage: "trigger a new refactor sweep Run", run: cmdStart},
	"status":  {usage: "show progress for a Run (or list all Runs)", run: cmdStatus},
	"list":    {usage: "list active and completed Runs", run: cmdList},
	"abandon": {usage: "request abandonment of a Run (two-tap confirmation)", run: cmdAbandon},
	"resume":  {usage: "resume a paused Run", run: cmdResume},
	"phrases": {usage: "manage the per-Run + global skip-phrase files", run: cmdPhrases},
	"version": {usage: "print the build version", run: cmdVersion},
}

type command struct {
	usage string
	run   func(args []string) error
}

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	verb := os.Args[1]
	if verb == "-h" || verb == "--help" || verb == "help" {
		printUsage(os.Stdout)
		return
	}
	cmd, ok := commands[verb]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", verb)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if err := cmd.run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "everflow — bulk-refactor sweep daemon\n\nusage: everflow <command> [flags]\n\ncommands:\n")
	for _, name := range []string{"daemon", "start", "status", "list", "abandon", "resume", "phrases", "version"} {
		fmt.Fprintf(w, "  %-9s %s\n", name, commands[name].usage)
	}
	fmt.Fprintf(w, "\nrun `everflow <command> -h` for command-specific flags.\n")
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	var (
		storePath     = fs.String("store", "", "path to sqlite store (default ~/.everflow/store.db; pass ':memory:' for volatile)")
		listenAddr    = fs.String("listen", ":8080", "address for the webhook HTTP server")
		publicBaseURL = fs.String("public-base-url", "", "publicly reachable URL where webhooks land (e.g. https://everflow.example.com)")
		gitlabBaseURL = fs.String("gitlab-base-url", "", "GitLab base URL (defaults to https://gitlab.com)")
		githubBaseURL = fs.String("github-base-url", "", "GitHub API base URL (defaults to https://api.github.com; GHE users set this to https://<your-ghe>/api/v3)")
		triggerAddr   = fs.String("trigger-listen", "127.0.0.1:8081", "address for the localhost-only trigger HTTP server (used by `everflow start`)")
		commitAuthor  = fs.String("commit-author", "", "git commit author name (default: host .gitconfig)")
		commitEmail   = fs.String("commit-email", "", "git commit author email (default: host .gitconfig)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("home dir: %w", err)
		}
		*storePath = home + "/.everflow/store.db"
	}
	// --public-base-url is only required for webhook-mode Runs. Poll mode
	// (the default since ADR-0031) doesn't need a public URL. If a webhook
	// Run is triggered and this is empty, setup() will fail with a clear
	// error referencing this flag.

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	providers, err := buildProviders(logger, *gitlabBaseURL, *githubBaseURL)
	if err != nil {
		return fmt.Errorf("provider setup: %w", err)
	}
	if len(providers) == 0 {
		logger.Warn("no providers configured — set GITLAB_TOKEN / GITHUB_TOKEN or run `glab auth login`")
	}

	recordStore, timeoutStore, err := store.Open(*storePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// Per-Run filesystem layout sits next to the store file. If --store is
	// /tmp/x/store.db, runs root is /tmp/x/runs/. Both happily live under
	// ~/.everflow/ when --store takes its default.
	runsRoot := filepath.Join(filepath.Dir(*storePath), "runs")

	secrets := webhook.NewSecretRegistry()
	if err := rehydrateSecrets(context.Background(), recordStore, secrets, logger); err != nil {
		logger.Warn("secret rehydration encountered errors; some Runs may have empty registry entries", "err", err)
	}
	runners := runner.NewRegistry()
	runners.Register(claude.NewRunner("")) // "claude" on $PATH; ADR-0004 + ADR-0027

	// Commit author falls back to the host's git config when blank — which is
	// what we want when pushing to a shared repo where the platform expects
	// verified email addresses. Override via --commit-author / --commit-email
	// (rare; useful for shared-bot deployments).
	gitClient := git.NewExec(*commitAuthor, *commitEmail)

	wf := refactorsweep.Build(workflowName, refactorsweep.Deps{
		RecordStore:   recordStore,
		TimeoutStore:  timeoutStore,
		EventStreamer: eventstream.New(),
		RoleScheduler: memrolescheduler.New(),
		Providers:     providers,
		Runners:       runners,
		Git:           gitClient,
		Secrets:       secrets,
		PublicBaseURL: *publicBaseURL,
		RunsRoot:      runsRoot,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wf.Run(ctx)
	defer wf.Stop()

	dispatcher := func(ctx context.Context, runID string, ev provider.Event) error {
		logger.Info("webhook received",
			"run_id", runID,
			"kind", ev.Kind,
			"mr_iid", ev.MR.IID,
			"author", ev.Author.Handle,
		)
		// Look up the Run's foreignID + current status so we can route the
		// callback through the workflow library, which loads the Run by
		// (workflowName, foreignID) and only fires the callback if status
		// matches.
		rec, err := recordStore.Lookup(ctx, runID)
		if err != nil {
			return fmt.Errorf("dispatcher: lookup run %s: %w", runID, err)
		}
		buf, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("dispatcher: marshal event: %w", err)
		}
		status := refactorsweep.AgentStatus(rec.Status)
		if err := wf.Callback(ctx, rec.ForeignID, status, bytes.NewReader(buf)); err != nil {
			return fmt.Errorf("dispatcher: workflow.Callback: %w", err)
		}
		return nil
	}
	// TODO: secret rehydration on daemon restart. Currently if the daemon
	// restarts, the in-memory SecretRegistry is empty until each Run's
	// setup() runs again — which it won't, because Runs past Initiated
	// don't re-enter setup. Inbound webhooks will be rejected as
	// "unknown runID" until a follow-up commit iterates the store at
	// startup and re-populates the registry from AgentState.WebhookSecret.
	// The webhook server reuses the same SecretRegistry the workflow
	// populates from setup() — single source of truth.
	whSrv := webhook.NewServer(providers, dispatcher, secrets)

	// Two HTTP listeners (ADR-0028): the public one carries the webhook +
	// health routes; the localhost-only one carries /trigger so the public
	// URL doesn't expose a way for outside parties to start Runs.
	publicMux := http.NewServeMux()
	whSrv.Mount(publicMux)
	publicSrv := &http.Server{
		Addr:              *listenAddr,
		Handler:           publicMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	localMux := http.NewServeMux()
	localMux.HandleFunc("/trigger", triggerHandler(wf, logger))
	localMux.HandleFunc("/status", statusHandler(recordStore, logger))
	localMux.HandleFunc("/control", controlHandler(wf, recordStore, logger))
	localSrv := &http.Server{
		Addr:              *triggerAddr,
		Handler:           localMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	srvErrCh := make(chan error, 2)
	go func() {
		if err := publicSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErrCh <- fmt.Errorf("public listener: %w", err)
		}
	}()
	go func() {
		if err := localSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErrCh <- fmt.Errorf("trigger listener: %w", err)
		}
	}()

	// Start the polling loop — ingests events for Runs whose EventSource
	// is "poll" (the default; ADR-0031). Web-hook-mode Runs co-exist;
	// the poller's RunSource just iterates active Runs and the poller
	// only acts on the ones with InFlight MRs.
	pollerLoop := &poller.Loop{
		Interval:   30 * time.Second,
		Providers:  providers,
		Source:     &poller.StoreSource{Store: recordStore, WorkflowName: workflowName, Decode: decodeActiveRun},
		Dispatcher: dispatcher,
		Logger:     logger,
	}
	go pollerLoop.Run(ctx)

	fmt.Fprintf(os.Stderr, "%s\n", daemonBannerLine())
	logger.Info("everflow daemon started",
		"version", version,
		"listen", *listenAddr,
		"trigger_listen", *triggerAddr,
		"public_base_url", *publicBaseURL,
		"workflow", workflowName,
		"store", *storePath,
		"runs_root", runsRoot,
	)
	logger.Warn("v1 scaffold — runners, step bodies, and CLI commands are stubs; see DESIGN.md for the build roadmap")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sigCh:
		logger.Info("shutdown signal received, stopping...")
	case err := <-srvErrCh:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = publicSrv.Shutdown(shutdownCtx)
	_ = localSrv.Shutdown(shutdownCtx)
	return nil
}

// decodeActiveRun turns a workflow.Record into a poller.ActiveRun by
// unmarshalling AgentState and projecting the fields the poller needs.
// Returns false for inactive / undecodable / non-poll Runs so they're
// silently skipped.
func decodeActiveRun(object []byte) (poller.ActiveRun, bool) {
	var s refactorsweep.AgentState
	if err := workflow.Unmarshal(object, &s); err != nil {
		return poller.ActiveRun{}, false
	}
	if !s.IsPollMode() {
		return poller.ActiveRun{}, false // webhook-mode Runs handled by the HTTP server
	}
	if len(s.InFlight) == 0 {
		return poller.ActiveRun{}, false // nothing to poll
	}
	return poller.ActiveRun{
		Provider:            s.ProviderName,
		ProjectID:           s.ProjectID,
		Author:              s.Author,
		InFlight:            s.InFlight,
		LastSeenNoteIDs:     s.LastSeenNoteIDs,
		LastSeenNoteCursors: s.LastSeenNoteIDsByStream,
		LastMRStates:        s.LastMRStates,
	}, true
}

// rehydrateSecrets iterates active Runs in the record store and re-populates
// the in-memory SecretRegistry from AgentState.WebhookSecret. Required on
// daemon restart — the registry lives in memory, so without this every
// inbound webhook for an existing Run would 401 after a restart.
//
// "Active" means: not terminal (Completed/Failed/Cancelled) AND has a
// non-empty WebhookSecret. Runs whose secret somehow ended up empty are
// logged and skipped (rather than failing the whole startup).
//
// See ADR-0029.
func rehydrateSecrets(ctx context.Context, store workflow.RecordStore, secrets *webhook.SecretRegistry, logger *slog.Logger) error {
	const pageSize = 200
	var offset int64
	rehydrated := 0
	for {
		records, err := store.List(ctx, workflowName, offset, pageSize, workflow.OrderTypeAscending)
		if err != nil {
			return fmt.Errorf("list records at offset %d: %w", offset, err)
		}
		if len(records) == 0 {
			break
		}
		for _, rec := range records {
			if rec.RunState.Finished() {
				continue
			}
			status := refactorsweep.AgentStatus(rec.Status)
			if !isActiveStatus(status) {
				continue
			}
			var state refactorsweep.AgentState
			if err := workflow.Unmarshal(rec.Object, &state); err != nil {
				logger.Warn("rehydrate: unmarshal AgentState failed; skipping",
					"run_id", rec.RunID, "err", err)
				continue
			}
			if state.WebhookSecret == "" || state.ProviderName == "" {
				continue
			}
			secrets.Set(state.ProviderName, rec.RunID, state.WebhookSecret)
			rehydrated++
		}
		if int64(len(records)) < pageSize {
			break
		}
		offset += int64(len(records))
	}
	if rehydrated > 0 {
		logger.Info("rehydrated webhook secrets from store", "count", rehydrated)
	}
	return nil
}

// isActiveStatus reports whether a Run in this state is expected to receive
// further webhook callbacks. Used by rehydrateSecrets to skip terminals.
func isActiveStatus(s refactorsweep.AgentStatus) bool {
	switch s {
	case refactorsweep.StatusCompleted,
		refactorsweep.StatusFailed,
		refactorsweep.StatusCancelled:
		return false
	}
	return true
}

// buildProviders registers providers from credentials. For GitLab we try
// GITLAB_TOKEN env first (PAT, sent as PRIVATE-TOKEN); if absent, fall
// back to the user's `glab auth login` token from the glab config
// (OAuth, sent as Bearer). The fallback is what lets a personal-laptop
// spike work without minting a separate PAT — ADR-0031.
func buildProviders(logger *slog.Logger, gitlabBase, githubBase string) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}

	if tok := os.Getenv("GITLAB_TOKEN"); tok != "" {
		p, err := gitlab.New(gitlab.Config{BaseURL: gitlabBase, Token: tok, AuthMode: gitlab.AuthPAT})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
		logger.Info("provider registered", "name", "gitlab", "auth", "private-token")
	} else if tok, err := gitlab.LoadGlabToken(""); err == nil {
		p, err := gitlab.New(gitlab.Config{BaseURL: gitlabBase, Token: tok, AuthMode: gitlab.AuthBearer})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
		logger.Info("provider registered", "name", "gitlab", "auth", "glab-oauth")
	} else if !errors.Is(err, gitlab.ErrGlabNotConfigured) {
		return nil, fmt.Errorf("gitlab: load glab token: %w", err)
	}

	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		p, err := github.New(github.Config{BaseURL: githubBase, Token: tok})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
		logger.Info("provider registered", "name", "github", "auth", "env-token")
	} else if tok, err := github.LoadGhToken(""); err == nil {
		p, err := github.New(github.Config{BaseURL: githubBase, Token: tok})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
		logger.Info("provider registered", "name", "github", "auth", "gh-oauth")
	} else if !errors.Is(err, github.ErrGhNotConfigured) {
		return nil, fmt.Errorf("github: load gh token: %w", err)
	}
	return out, nil
}

// triggerRequest is the JSON body of POST /trigger. The CLI's cmdStart
// builds one of these (from --spec or --units flags) and posts it to the
// localhost-only trigger listener. The daemon turns it into an AgentState
// and calls wf.Trigger.
type triggerRequest struct {
	Mode         string   `json:"mode"`         // "spec" | "sweep" — see ADR-0024
	Goal         string   `json:"goal"`
	ProviderName string   `json:"provider"`
	ProjectID    string   `json:"project"`
	RunnerName   string   `json:"runner"`
	RunnerModel  string   `json:"runner_model,omitempty"` // spec's `model:` override; empty means the runner's default
	BaseRepo     string   `json:"base_repo"`
	BaseBranch   string   `json:"base_branch"`
	Concurrency  int      `json:"concurrency"`
	Units        []string `json:"units,omitempty"`     // sweep mode
	SpecPath     string   `json:"spec_path,omitempty"` // spec mode
	SpecBody     string   `json:"spec_body,omitempty"` // spec mode
	DraftMRs     bool     `json:"draft_mrs,omitempty"` // open MRs as Draft / WIP
	ForeignID    string   `json:"foreign_id,omitempty"`
}

type triggerResponse struct {
	RunID     string `json:"run_id"`
	ForeignID string `json:"foreign_id"`
}

// triggerHandler validates the request, builds an AgentState, calls
// wf.Trigger, and responds with the assigned run ID. Used only on the
// localhost-only trigger listener (ADR-0028).
func triggerHandler(wf *workflow.Workflow[refactorsweep.AgentState, refactorsweep.AgentStatus], logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var req triggerRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.ProviderName == "" || req.ProjectID == "" || req.RunnerName == "" {
			http.Error(w, "provider, project, and runner are required", http.StatusBadRequest)
			return
		}
		if req.Mode == "" {
			if len(req.Units) > 0 {
				req.Mode = refactorsweep.ModeSweep
			} else if req.SpecBody != "" || req.SpecPath != "" {
				req.Mode = refactorsweep.ModeSpec
			} else {
				http.Error(w, "neither units nor spec provided", http.StatusBadRequest)
				return
			}
		}
		if req.Concurrency <= 0 {
			req.Concurrency = 1
		}
		if req.BaseBranch == "" {
			req.BaseBranch = "main"
		}
		foreignID := req.ForeignID
		if foreignID == "" {
			foreignID = uuid.NewString()
		}

		state := &refactorsweep.AgentState{
			Goal:         req.Goal,
			Mode:         req.Mode,
			ProviderName: req.ProviderName,
			ProjectID:    req.ProjectID,
			BaseRepo:     req.BaseRepo,
			BaseBranch:   req.BaseBranch,
			RunnerName:   req.RunnerName,
			RunnerModel:  req.RunnerModel,
			Concurrency:  req.Concurrency,
			Queue:        req.Units,
			SpecPath:     req.SpecPath,
			SpecBody:     req.SpecBody,
			DraftMRs:     req.DraftMRs,
			InFlight:     map[string]provider.MR{},
		}

		runID, err := wf.Trigger(r.Context(), foreignID,
			workflow.WithInitialValue[refactorsweep.AgentState, refactorsweep.AgentStatus](state))
		if err != nil {
			http.Error(w, "trigger: "+err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info("triggered run",
			"run_id", runID, "foreign_id", foreignID,
			"mode", req.Mode, "provider", req.ProviderName,
			"project", req.ProjectID,
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(triggerResponse{RunID: runID, ForeignID: foreignID})
	}
}

// --- /status and /control handlers ---

// runStatusResponse is the JSON shape returned by GET /status?run_id=xxx.
type runStatusResponse struct {
	RunID                string          `json:"run_id"`
	ForeignID            string          `json:"foreign_id"`
	Status               string          `json:"status"`
	Goal                 string          `json:"goal"`
	Mode                 string          `json:"mode"`
	Provider             string          `json:"provider"`
	Project              string          `json:"project"`
	Completed            int             `json:"completed"`
	Blacklisted          int             `json:"blacklisted"`
	InFlight             int             `json:"in_flight"`
	Queued               int             `json:"queued"`
	SubagentInvocations  int             `json:"subagent_invocations"`
	TotalTokens          int             `json:"total_tokens"`
	MaxTokens            int             `json:"max_tokens,omitempty"`
	MaxUnits             int             `json:"max_units,omitempty"`
	EventsSeen           int             `json:"events_seen"`
	EventsSkipped        int             `json:"events_skipped"`
	PauseReason          string          `json:"pause_reason,omitempty"`
	LastError            string          `json:"last_error,omitempty"`
	StartedAt            time.Time       `json:"started_at,omitempty"`
	UpdatedAt            time.Time       `json:"updated_at"`
	RecentTurns          []turnSummary   `json:"recent_turns,omitempty"`
}

// turnSummary is the compact representation of a Turn for the status command.
type turnSummary struct {
	Phase   string `json:"phase"`
	UnitID  string `json:"unit_id,omitempty"`
	Tokens  int    `json:"tokens"`
	Summary string `json:"summary"`
}

// statusHandler returns a JSON status summary. GET /status?run_id=xxx returns
// one run; GET /status returns all runs (compact listing).
func statusHandler(rs workflow.RecordStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()
		runID := r.URL.Query().Get("run_id")
		if runID != "" {
			rec, err := rs.Lookup(ctx, runID)
			if err != nil {
				http.Error(w, "run not found: "+err.Error(), http.StatusNotFound)
				return
			}
			resp, err := recordToStatus(rec)
			if err != nil {
				http.Error(w, "decode run: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// List all runs.
		records, err := rs.List(ctx, workflowName, 0, 500, workflow.OrderTypeDescending)
		if err != nil {
			http.Error(w, "list: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var out []runStatusResponse
		for _, rec := range records {
			resp, err := recordToStatus(&rec)
			if err != nil {
				logger.Warn("status: decode run", "run_id", rec.RunID, "err", err)
				continue
			}
			out = append(out, resp)
		}
		if out == nil {
			out = []runStatusResponse{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func recordToStatus(rec *workflow.Record) (runStatusResponse, error) {
	var state refactorsweep.AgentState
	if err := workflow.Unmarshal(rec.Object, &state); err != nil {
		return runStatusResponse{}, err
	}

	// Last 5 turns for the status display.
	const maxRecentTurns = 5
	turns := state.History
	if len(turns) > maxRecentTurns {
		turns = turns[len(turns)-maxRecentTurns:]
	}
	recent := make([]turnSummary, len(turns))
	for i, t := range turns {
		excerpt := t.Summary
		if len(excerpt) > 80 {
			excerpt = excerpt[:77] + "..."
		}
		recent[i] = turnSummary{Phase: t.Phase, UnitID: t.UnitID, Tokens: t.Tokens, Summary: excerpt}
	}

	return runStatusResponse{
		RunID:               rec.RunID,
		ForeignID:           rec.ForeignID,
		Status:              refactorsweep.AgentStatus(rec.Status).String(),
		Goal:                state.Goal,
		Mode:                state.Mode,
		Provider:            state.ProviderName,
		Project:             state.ProjectID,
		Completed:           len(state.Completed),
		Blacklisted:         len(state.Blacklisted),
		InFlight:            len(state.InFlight),
		Queued:              len(state.Queue),
		SubagentInvocations: state.SubagentInvocations,
		TotalTokens:         state.TotalTokens,
		MaxTokens:           state.Budget.MaxTokens,
		MaxUnits:            state.Budget.MaxUnits,
		EventsSeen:          state.EventsSeen,
		EventsSkipped:       state.EventsSkippedByFilter,
		PauseReason:         state.PauseReason,
		LastError:           state.LastError,
		StartedAt:           state.StartedAt,
		UpdatedAt:           rec.UpdatedAt,
		RecentTurns:         recent,
	}, nil
}

// controlRequest is the JSON body for POST /control.
type controlRequest struct {
	RunID string `json:"run_id"`
	Verb  string `json:"verb"` // abandon | resume | pause | stop | skip | retry | prompt | status
	Args  string `json:"args,omitempty"`
}

// controlHandler injects a synthetic /everflow control command into a Run by
// constructing a fake author NoteAdded event and calling wf.Callback. The
// event's Author is set to the Run's recorded author so the IsAuthor check in
// resume() passes. Only safe on the localhost-only trigger listener (ADR-0028).
func controlHandler(wf *workflow.Workflow[refactorsweep.AgentState, refactorsweep.AgentStatus], rs workflow.RecordStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var req controlRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.RunID == "" || req.Verb == "" {
			http.Error(w, "run_id and verb are required", http.StatusBadRequest)
			return
		}
		ctx := r.Context()

		rec, err := rs.Lookup(ctx, req.RunID)
		if err != nil {
			http.Error(w, "run not found: "+err.Error(), http.StatusNotFound)
			return
		}
		var state refactorsweep.AgentState
		if err := workflow.Unmarshal(rec.Object, &state); err != nil {
			http.Error(w, "decode run state: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Pick any in-flight MR to post the ack comment against. If none,
		// the control handlers will attempt to post to MR IID=0 which will
		// fail gracefully (ignored error).
		var mr provider.MR
		for _, m := range state.InFlight {
			mr = m
			break
		}

		noteBody := "/everflow " + req.Verb
		if req.Args != "" {
			noteBody += " " + req.Args
		}
		ev := provider.Event{
			Kind:      provider.EventNoteAdded,
			ProjectID: state.ProjectID,
			MR:        mr,
			Author:    state.Author,
			IsAuthor:  true,
			Note: provider.Note{
				Body: noteBody,
			},
			ReceivedAt: time.Now().UnixNano(),
		}
		buf, err := json.Marshal(ev)
		if err != nil {
			http.Error(w, "marshal event: "+err.Error(), http.StatusInternalServerError)
			return
		}

		status := refactorsweep.AgentStatus(rec.Status)
		if err := wf.Callback(ctx, rec.ForeignID, status, bytes.NewReader(buf)); err != nil {
			http.Error(w, "callback: "+err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info("injected control command", "run_id", req.RunID, "verb", req.Verb)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "verb": req.Verb, "run_id": req.RunID})
	}
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	var (
		specPath    = fs.String("spec", "", "path to a spec markdown file (spec mode; mutually exclusive with --units)")
		unitsCSV    = fs.String("units", "", "comma-separated unit IDs (sweep mode; mutually exclusive with --spec)")
		goal        = fs.String("goal", "", "one-sentence description (sweep mode; ignored in spec mode where the spec's `goal:` is used)")
		providerArg = fs.String("provider", "", "provider name (gitlab | github)")
		projectArg  = fs.String("project", "", "provider project ID, e.g. acme/example")
		runnerArg   = fs.String("runner", "claude", "runner name")
		modelArg    = fs.String("model", "", "runner model override (default: runner's default, or spec's `model:`)")
		baseBranch  = fs.String("base-branch", "", "base branch (default: main, or spec's `base_branch:`)")
		baseRepo    = fs.String("base-repo", "", "local path to a git checkout with origin remote (required)")
		concurrency = fs.Int("concurrency", 0, "max in-flight MRs (default 1, or spec's `concurrency:`)")
		draftMRs    = fs.Bool("draft-mrs", false, "open MRs as Draft / WIP (recommended for spikes against shared repos)")
		daemonURL   = fs.String("daemon", "http://127.0.0.1:8081", "daemon trigger endpoint")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*specPath == "" && *unitsCSV == "") || (*specPath != "" && *unitsCSV != "") {
		return errors.New("exactly one of --spec or --units must be provided")
	}

	req := triggerRequest{
		Goal:         *goal,
		ProviderName: *providerArg,
		ProjectID:    *projectArg,
		RunnerName:   *runnerArg,
		RunnerModel:  *modelArg,
		BaseRepo:     *baseRepo,
		BaseBranch:   *baseBranch,
		Concurrency:  *concurrency,
		DraftMRs:     *draftMRs,
	}

	if *specPath != "" {
		sp, err := spec.Parse(*specPath)
		if err != nil {
			return fmt.Errorf("parse spec: %w", err)
		}
		req.Mode = refactorsweep.ModeSpec
		req.SpecPath = sp.Path
		req.SpecBody = sp.Body
		req.DraftMRs = sp.DraftMRs
		// Spec frontmatter is authoritative unless explicitly overridden via flag.
		if req.Goal == "" {
			req.Goal = sp.Goal
		}
		if req.ProviderName == "" {
			req.ProviderName = sp.Provider
		}
		if req.ProjectID == "" {
			req.ProjectID = sp.Project
		}
		if req.RunnerName == "claude" && sp.Runner != "" {
			// "claude" is the flag default; defer to spec if it specified something.
			req.RunnerName = sp.Runner
		}
		if req.RunnerModel == "" {
			req.RunnerModel = sp.Model
		}
		if req.BaseRepo == "" {
			req.BaseRepo = sp.BaseRepo
		}
		if req.BaseBranch == "" {
			req.BaseBranch = sp.BaseBranch
		}
		if req.Concurrency == 0 {
			req.Concurrency = sp.Concurrency
		}
	} else {
		req.Mode = refactorsweep.ModeSweep
		req.Units = strings.Split(*unitsCSV, ",")
		for i, u := range req.Units {
			req.Units[i] = strings.TrimSpace(u)
		}
	}

	if req.ProviderName == "" || req.ProjectID == "" {
		return errors.New("provider and project are required (set --provider/--project or via spec frontmatter)")
	}
	if req.BaseRepo == "" {
		return errors.New("--base-repo is required (or set via spec's base_repo)")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal trigger request: %w", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, *daemonURL+"/trigger", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST %s/trigger: %w (is the daemon running?)", *daemonURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("trigger: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out triggerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	fmt.Printf("Triggered run %s (foreign id: %s, mode: %s)\n", out.RunID, out.ForeignID, req.Mode)
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	daemonURL := fs.String("daemon", "http://127.0.0.1:8081", "daemon address")
	storePath := fs.String("store", "", "path to sqlite store; when the daemon is unreachable the CLI falls back to the store (default: ~/.everflow/store.db if it exists)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runID := fs.Arg(0)

	url := *daemonURL + "/status"
	if runID != "" {
		full, err := resolveRunIDFromDaemon(*daemonURL, runID)
		if err != nil {
			// If the daemon is unreachable, fall back to the store when
			// possible; otherwise surface a hint pointing at --store.
			if isDaemonUnreachable(err) {
				if fallback, ok := tryStoreFallback(*storePath); ok {
					fmt.Fprintf(os.Stderr, "everflow: daemon unreachable (%v); reading store directly\n", err)
					return directStatus(context.Background(), fallback, runID)
				}
				return daemonUnreachableError(*daemonURL, err)
			}
			return err
		}
		runID = full
		url += "?run_id=" + runID
	}
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		// Daemon unreachable — fall back to a direct sqlite read when the
		// store exists; otherwise emit a hint pointing at --store.
		if fallback, ok := tryStoreFallback(*storePath); ok {
			fmt.Fprintf(os.Stderr, "everflow: daemon unreachable (%v); reading store directly\n", err)
			return directStatus(context.Background(), fallback, runID)
		}
		return daemonUnreachableError(*daemonURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	if runID != "" {
		var s runStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		printRunStatus(os.Stdout, s)
		return nil
	}

	var runs []runStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(runs) == 0 {
		fmt.Println("no runs found")
		return nil
	}
	fmt.Printf("%-10s  %-22s  %-9s  %s\n", "RUN ID", "UPDATED", "STATUS", "GOAL")
	for _, s := range runs {
		goal := s.Goal
		if len(goal) > 50 {
			goal = goal[:47] + "..."
		}
		fmt.Printf("%-10s  %-22s  %-9s  %s\n",
			s.RunID[:min(10, len(s.RunID))],
			s.UpdatedAt.Format("2006-01-02 15:04:05"),
			s.Status,
			goal,
		)
	}
	return nil
}

// directStatus prints a Run's state by reading directly from the sqlite store.
// It does not require the daemon to be running.
func directStatus(ctx context.Context, storePath, runID string) error {
	sp, err := defaultStorePath(storePath)
	if err != nil {
		return err
	}
	rs, _, err := store.Open(sp)
	if err != nil {
		return fmt.Errorf("open store %s: %w", sp, err)
	}

	if runID != "" {
		full, err := resolveRunIDFromStore(ctx, rs, runID)
		if err != nil {
			return err
		}
		rec, err := rs.Lookup(ctx, full)
		if err != nil {
			return fmt.Errorf("run %s not found: %w (hint: use 'everflow list' or query the store directly)", full, err)
		}
		s, err := recordToStatus(rec)
		if err != nil {
			return fmt.Errorf("decode run state: %w", err)
		}
		printRunStatus(os.Stdout, s)
		return nil
	}

	records, err := rs.List(ctx, workflowName, 0, 500, workflow.OrderTypeDescending)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if len(records) == 0 {
		fmt.Println("no runs found")
		return nil
	}
	fmt.Printf("%-10s  %-22s  %-9s  %s\n", "RUN ID", "UPDATED", "STATUS", "GOAL")
	for i := range records {
		s, err := recordToStatus(&records[i])
		if err != nil {
			continue
		}
		goal := s.Goal
		if len(goal) > 50 {
			goal = goal[:47] + "..."
		}
		fmt.Printf("%-10s  %-22s  %-9s  %s\n",
			s.RunID[:min(10, len(s.RunID))],
			s.UpdatedAt.Format("2006-01-02 15:04:05"),
			s.Status,
			goal,
		)
	}
	return nil
}

func printRunStatus(w io.Writer, s runStatusResponse) {
	fmt.Fprintf(w, "Run:      %s\n", s.RunID)
	fmt.Fprintf(w, "Status:   %s\n", s.Status)
	fmt.Fprintf(w, "Goal:     %s\n", s.Goal)
	fmt.Fprintf(w, "Provider: %s / %s\n", s.Provider, s.Project)
	fmt.Fprintf(w, "Mode:     %s\n", s.Mode)
	fmt.Fprintf(w, "Units:    %d completed, %d blacklisted, %d in-flight, %d queued\n",
		s.Completed, s.Blacklisted, s.InFlight, s.Queued)
	tokenStr := fmt.Sprintf("%d", s.TotalTokens)
	if s.MaxTokens > 0 {
		tokenStr = fmt.Sprintf("%d / %d", s.TotalTokens, s.MaxTokens)
	}
	fmt.Fprintf(w, "Tokens:   %s\n", tokenStr)
	fmt.Fprintf(w, "Invocations: %d (events: %d, skipped: %d)\n",
		s.SubagentInvocations, s.EventsSeen, s.EventsSkipped)
	if !s.StartedAt.IsZero() {
		fmt.Fprintf(w, "Started:  %s\n", s.StartedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(w, "Updated:  %s\n", s.UpdatedAt.Format(time.RFC3339))
	if s.PauseReason != "" {
		fmt.Fprintf(w, "Paused:   %s\n", s.PauseReason)
	}
	if s.LastError != "" {
		fmt.Fprintf(w, "Error:    %s\n", s.LastError)
	}
	if len(s.RecentTurns) > 0 {
		fmt.Fprintf(w, "\nRecent turns (last %d):\n", len(s.RecentTurns))
		for _, t := range s.RecentTurns {
			unit := t.UnitID
			if unit == "" {
				unit = "-"
			}
			fmt.Fprintf(w, "  [%-18s] %-16s tokens=%-6d %q\n", t.Phase, unit, t.Tokens, t.Summary)
		}
	}
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	storePath := fs.String("store", "", "path to sqlite store (default: ~/.everflow/store.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return directList(context.Background(), *storePath)
}

// directList prints all Runs from the sqlite store, one per line, sorted newest first.
func directList(ctx context.Context, storePath string) error {
	sp, err := defaultStorePath(storePath)
	if err != nil {
		return err
	}
	rs, _, err := store.Open(sp)
	if err != nil {
		return fmt.Errorf("open store %s: %w", sp, err)
	}

	records, err := rs.List(ctx, workflowName, 0, 500, workflow.OrderTypeDescending)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if len(records) == 0 {
		fmt.Println("no runs found")
		return nil
	}
	for i := range records {
		rec := &records[i]
		var state refactorsweep.AgentState
		if err := workflow.Unmarshal(rec.Object, &state); err != nil {
			continue
		}
		runID := rec.RunID
		if len(runID) > 13 {
			runID = runID[:13] + "..."
		}
		mode := state.Mode
		if mode == "" {
			mode = "sweep"
		}
		status := refactorsweep.AgentStatus(rec.Status).String()
		turns := len(state.History)
		goal := state.Goal
		if len(goal) > 40 {
			goal = `"` + goal[:37] + `..."`
		} else {
			goal = `"` + goal + `"`
		}
		fmt.Printf("%-16s  %-5s  %-9s  %2d turns  goal: %s\n",
			runID, mode, status, turns, goal)
	}
	return nil
}

// resolveRunIDPrefix returns the unique candidate matched by strings.HasPrefix.
// Full UUIDs fall through cleanly (they match only themselves). Zero or
// multiple matches return errors; the multi-match error lists every hit so
// the user can pick.
func resolveRunIDPrefix(candidates []string, prefix string) (string, error) {
	if prefix == "" {
		return "", errors.New("empty run-id")
	}
	var matches []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no run matches prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("prefix %q matches multiple runs — please be more specific:\n  %s", prefix, strings.Join(matches, "\n  "))
	}
}

// resolveRunIDFromStore resolves a prefix against the sqlite store.
func resolveRunIDFromStore(ctx context.Context, rs workflow.RecordStore, prefix string) (string, error) {
	records, err := rs.List(ctx, workflowName, 0, 500, workflow.OrderTypeDescending)
	if err != nil {
		return "", fmt.Errorf("list runs: %w", err)
	}
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.RunID)
	}
	return resolveRunIDPrefix(ids, prefix)
}

// resolveRunIDFromDaemon resolves a prefix by fetching /status from the daemon
// and filtering. Transport-level errors are returned unwrapped so callers can
// detect them via isDaemonUnreachable.
func resolveRunIDFromDaemon(daemonURL, prefix string) (string, error) {
	resp, err := http.Get(daemonURL + "/status") //nolint:noctx
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var runs []runStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.RunID)
	}
	return resolveRunIDPrefix(ids, prefix)
}

// isDaemonUnreachable reports transport-level HTTP failures (connection
// refused, timeout, DNS). http.Get returns errors only for transport failures
// — well-formed 4xx / 5xx come back via resp.StatusCode with a nil error — so
// a *url.Error in the chain is the reliable signal.
func isDaemonUnreachable(err error) bool {
	var urlErr *url.Error
	return err != nil && errors.As(err, &urlErr)
}

// daemonUnreachableError wraps a transport failure with a hint pointing users
// at --store as an alternative when the daemon isn't running.
func daemonUnreachableError(daemonURL string, err error) error {
	return fmt.Errorf("daemon at %s is unreachable: %w\nhint: pass --store /path/to/store.db to read the sqlite store directly (no daemon needed)", daemonURL, err)
}

// tryStoreFallback picks a store path for the offline-rescue path. --store
// wins; otherwise ~/.everflow/store.db is used only when it exists — we
// don't silently create an empty store and pretend nothing is wrong.
func tryStoreFallback(userProvided string) (string, bool) {
	if userProvided != "" {
		return userProvided, true
	}
	sp, err := defaultStorePath("")
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(sp); err != nil {
		return "", false
	}
	return sp, true
}

func cmdAbandon(args []string) error {
	fs := flag.NewFlagSet("abandon", flag.ExitOnError)
	daemonURL := fs.String("daemon", "http://127.0.0.1:8081", "daemon address")
	storePath := fs.String("store", "", "path to sqlite store; when the daemon is unreachable the CLI falls back to the store (default: ~/.everflow/store.db if it exists)")
	reasonFlag := fs.String("reason", "", "optional reason for abandonment")
	gitlabBaseURL := fs.String("gitlab-base-url", "", "GitLab base URL (defaults to https://gitlab.com)")
	githubBaseURL := fs.String("github-base-url", "", "GitHub API base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runID := fs.Arg(0)
	if runID == "" {
		return errors.New("usage: everflow abandon <run-id>")
	}

	// Try daemon first (preferred path: daemon handles two-tap confirmation
	// and provider-side MR cleanup gracefully). Resolve the prefix against
	// the daemon's view of the store so short IDs work.
	full, resolveErr := resolveRunIDFromDaemon(*daemonURL, runID)
	if resolveErr == nil {
		if err := sendControl(*daemonURL, full, "abandon", *reasonFlag); err == nil {
			return nil
		}
	} else if !isDaemonUnreachable(resolveErr) {
		return resolveErr
	}

	// Daemon not reachable — fall back to direct store manipulation. This is
	// the rescue path: no two-tap, immediate force-cancel. See ADR-0037.
	fallback, ok := tryStoreFallback(*storePath)
	if !ok {
		return daemonUnreachableError(*daemonURL, resolveErr)
	}
	fmt.Fprintln(os.Stderr, "everflow: daemon unreachable; falling back to direct store write")
	return directAbandon(context.Background(), fallback, runID, *reasonFlag, *gitlabBaseURL, *githubBaseURL)
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	daemonURL := fs.String("daemon", "http://127.0.0.1:8081", "daemon address")
	storePath := fs.String("store", "", "path to sqlite store; when the daemon is unreachable the CLI falls back to the store (default: ~/.everflow/store.db if it exists)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runID := fs.Arg(0)
	if runID == "" {
		return errors.New("usage: everflow resume <run-id>")
	}

	// Try daemon first (preferred: daemon can resume Paused → AwaitingMerge).
	// The daemon path handles the AwaitingMerge callback correctly; the direct
	// path is needed for Cancelled/Failed → Discovering revive.
	full, resolveErr := resolveRunIDFromDaemon(*daemonURL, runID)
	if resolveErr == nil {
		if err := sendControl(*daemonURL, full, "resume", ""); err == nil {
			return nil
		}
	} else if !isDaemonUnreachable(resolveErr) {
		return resolveErr
	}

	// Daemon not reachable — fall back to direct store write. The daemon must
	// be (re)started to process the outbox event that drives the Discovering
	// step. See ADR-0037.
	fallback, ok := tryStoreFallback(*storePath)
	if !ok {
		return daemonUnreachableError(*daemonURL, resolveErr)
	}
	fmt.Fprintln(os.Stderr, "everflow: daemon unreachable; falling back to direct store write")
	return directResume(context.Background(), fallback, runID)
}

// defaultStorePath returns ~/.everflow/store.db when path is blank.
func defaultStorePath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return home + "/.everflow/store.db", nil
}

// directAbandon force-cancels a Run by writing directly to the sqlite store.
// It does not require the daemon to be running. In-flight MRs are closed
// best-effort via the configured provider credentials.
func directAbandon(ctx context.Context, storePath, runID, reason, gitlabBaseURL, githubBaseURL string) error {
	sp, err := defaultStorePath(storePath)
	if err != nil {
		return err
	}
	rs, _, err := store.Open(sp)
	if err != nil {
		return fmt.Errorf("open store %s: %w", sp, err)
	}

	full, err := resolveRunIDFromStore(ctx, rs, runID)
	if err != nil {
		return err
	}
	runID = full
	rec, err := rs.Lookup(ctx, runID)
	if err != nil {
		return fmt.Errorf("run %s not found: %w", runID, err)
	}
	if rec.RunState.Finished() {
		return fmt.Errorf("run %s is already in a terminal state (%s); nothing to abandon", runID, rec.RunState)
	}

	var state refactorsweep.AgentState
	if err := workflow.Unmarshal(rec.Object, &state); err != nil {
		return fmt.Errorf("decode run state: %w", err)
	}

	// Close in-flight MRs best-effort via the provider.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	providers, _ := buildProviders(logger, gitlabBaseURL, githubBaseURL)
	if p, ok := providers[state.ProviderName]; ok {
		for _, mr := range state.InFlight {
			if cErr := p.CloseMR(ctx, mr.ProjectID, mr.IID); cErr != nil {
				fmt.Fprintf(os.Stderr, "everflow: close MR #%d (best-effort): %v\n", mr.IID, cErr)
			}
		}
	}

	// Remove per-unit worktrees best-effort.
	runsRoot := filepath.Join(filepath.Dir(sp), "runs")
	g := git.NewExec("", "")
	for unitID := range state.InFlight {
		wt := filepath.Join(runsRoot, runID, "worktrees", unitID)
		if rErr := g.RemoveWorktree(ctx, state.BaseRepo, wt); rErr != nil {
			fmt.Fprintf(os.Stderr, "everflow: remove worktree %s (best-effort): %v\n", wt, rErr)
		}
	}

	if reason == "" {
		reason = "abandoned via CLI"
	}
	state.LastError = reason

	obj, err := workflow.Marshal(&state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	rec.Object = obj
	rec.Status = int(refactorsweep.StatusCancelled)
	rec.RunState = workflow.RunStateCancelled
	rec.Meta.Version++

	if err := rs.Store(ctx, rec); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	fmt.Printf("✓ Run %s cancelled (direct store write; reason: %s)\n", runID[:min(10, len(runID))], reason)
	return nil
}

// directResume revives a Cancelled or Failed Run to Discovering by writing
// directly to the sqlite store. The daemon must be running (or restarted)
// to process the outbox event and drive the Discovering step. See ADR-0037.
func directResume(ctx context.Context, storePath, runID string) error {
	sp, err := defaultStorePath(storePath)
	if err != nil {
		return err
	}
	rs, _, err := store.Open(sp)
	if err != nil {
		return fmt.Errorf("open store %s: %w", sp, err)
	}

	full, err := resolveRunIDFromStore(ctx, rs, runID)
	if err != nil {
		return err
	}
	runID = full
	rec, err := rs.Lookup(ctx, runID)
	if err != nil {
		return fmt.Errorf("run %s not found: %w", runID, err)
	}

	status := refactorsweep.AgentStatus(rec.Status)
	if status != refactorsweep.StatusCancelled && status != refactorsweep.StatusFailed && status != refactorsweep.StatusPaused {
		return fmt.Errorf("run %s is in status %s; only Cancelled, Failed, or Paused runs can be resumed via direct store write", runID, status)
	}

	var state refactorsweep.AgentState
	if err := workflow.Unmarshal(rec.Object, &state); err != nil {
		return fmt.Errorf("decode run state: %w", err)
	}

	state.LastError = ""
	state.PauseReason = ""

	obj, err := workflow.Marshal(&state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	rec.Object = obj
	rec.Status = int(refactorsweep.StatusDiscovering)
	rec.RunState = workflow.RunStateRunning
	rec.Meta.Version++

	if err := rs.Store(ctx, rec); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	fmt.Printf("✓ Run %s revived to Discovering (direct store write)\n", runID[:min(10, len(runID))])
	fmt.Println("  The daemon must be running (or restarted) to pick up and process the outbox event.")
	return nil
}

// sendControl posts a control verb to the daemon's /control endpoint.
func sendControl(daemonURL, runID, verb, args string) error {
	req := controlRequest{RunID: runID, Verb: verb, Args: args}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := http.Post(daemonURL+"/control", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("POST /control: %w (is the daemon running?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Printf("✓ %s sent to run %s\n", verb, runID)
	return nil
}

func cmdPhrases(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println("usage: everflow phrases <list|promote> [args]")
		return nil
	}
	switch args[0] {
	case "list", "promote":
		return fmt.Errorf("everflow phrases %s: not implemented in scaffold", args[0])
	default:
		return fmt.Errorf("unknown subcommand %q (try list, promote)", args[0])
	}
}

func versionString() string {
	return fmt.Sprintf("everflow %s (commit: %s, built: %s)", strings.TrimSpace(version), gitCommit, buildTime)
}

func daemonBannerLine() string {
	return fmt.Sprintf("everflow daemon starting version=%s commit=%s pid=%d go=%s os=%s arch=%s",
		version, gitCommit, os.Getpid(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func cmdVersion(_ []string) error {
	fmt.Println(versionString())
	return nil
}
