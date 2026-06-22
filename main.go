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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/luno/workflow"

	"github.com/luno/workflow/adapters/memrolescheduler"
	"github.com/luno/workflow/adapters/memstreamer"

	"github.com/andrewwormald/everflow/internal/git"
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
	version      = "0.0.1-scaffold"
)

var commands = map[string]command{
	"daemon":  {usage: "run the long-lived daemon", run: cmdDaemon},
	"start":   {usage: "trigger a new refactor sweep Run", run: cmdStart},
	"status":  {usage: "show progress for a Run", run: cmdStatus},
	"list":    {usage: "list active and completed Runs", run: cmdList},
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
	for _, name := range []string{"daemon", "start", "status", "list", "phrases", "version"} {
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
	if *publicBaseURL == "" {
		return fmt.Errorf("--public-base-url is required (see DESIGN.md § Public-URL strategy)")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	providers, err := buildProviders(*gitlabBaseURL, *githubBaseURL)
	if err != nil {
		return fmt.Errorf("provider setup: %w", err)
	}
	if len(providers) == 0 {
		logger.Warn("no providers configured — set GITLAB_TOKEN or GITHUB_TOKEN to enable a provider")
	} else {
		for name := range providers {
			logger.Info("provider registered", "name", name)
		}
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
	runners := runner.NewRegistry()
	runners.Register(claude.NewRunner("")) // "claude" on $PATH; ADR-0004 + ADR-0027

	gitClient := git.NewExec(
		"everflow",                       // author name on commits; falls back to host .gitconfig if empty
		"everflow@noreply.invalid",       // author email
	)

	wf := refactorsweep.Build(workflowName, refactorsweep.Deps{
		RecordStore:   recordStore,
		TimeoutStore:  timeoutStore,
		EventStreamer: memstreamer.New(),
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

// buildProviders registers providers based on env-var credentials. Empty map
// is valid — the daemon still starts; it just can't accept webhooks until at
// least one provider is configured.
func buildProviders(gitlabBase, githubBase string) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	if tok := os.Getenv("GITLAB_TOKEN"); tok != "" {
		p, err := gitlab.New(gitlab.Config{BaseURL: gitlabBase, Token: tok})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		p, err := github.New(github.Config{BaseURL: githubBase, Token: tok})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
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
	BaseRepo     string   `json:"base_repo"`
	BaseBranch   string   `json:"base_branch"`
	Concurrency  int      `json:"concurrency"`
	Units        []string `json:"units,omitempty"`     // sweep mode
	SpecPath     string   `json:"spec_path,omitempty"` // spec mode
	SpecBody     string   `json:"spec_body,omitempty"` // spec mode
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
			Concurrency:  req.Concurrency,
			Queue:        req.Units,
			SpecPath:     req.SpecPath,
			SpecBody:     req.SpecBody,
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

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	var (
		specPath    = fs.String("spec", "", "path to a spec markdown file (spec mode; mutually exclusive with --units)")
		unitsCSV    = fs.String("units", "", "comma-separated unit IDs (sweep mode; mutually exclusive with --spec)")
		goal        = fs.String("goal", "", "one-sentence description (sweep mode; ignored in spec mode where the spec's `goal:` is used)")
		providerArg = fs.String("provider", "", "provider name (gitlab | github)")
		projectArg  = fs.String("project", "", "provider project ID, e.g. lunomoney/core")
		runnerArg   = fs.String("runner", "claude", "runner name")
		baseBranch  = fs.String("base-branch", "", "base branch (default: main, or spec's `base_branch:`)")
		baseRepo    = fs.String("base-repo", "", "local path to a git checkout with origin remote (required)")
		concurrency = fs.Int("concurrency", 0, "max in-flight MRs (default 1, or spec's `concurrency:`)")
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
		BaseRepo:     *baseRepo,
		BaseBranch:   *baseBranch,
		Concurrency:  *concurrency,
	}

	if *specPath != "" {
		sp, err := spec.Parse(*specPath)
		if err != nil {
			return fmt.Errorf("parse spec: %w", err)
		}
		req.Mode = refactorsweep.ModeSpec
		req.SpecPath = sp.Path
		req.SpecBody = sp.Body
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
	_ = args
	return fmt.Errorf("everflow status: not implemented in scaffold")
}

func cmdList(args []string) error {
	_ = args
	return fmt.Errorf("everflow list: not implemented in scaffold")
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

func cmdVersion(_ []string) error {
	fmt.Println(strings.TrimSpace(version))
	return nil
}
