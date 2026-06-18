// Everflow — bulk-refactor sweep CLI. See README.md, DESIGN.md, and the
// decisions/ log for the project's purpose and design.
//
// This file is the CLI surface; business logic lives under internal/.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/luno/workflow/adapters/memrolescheduler"
	"github.com/luno/workflow/adapters/memstreamer"

	"github.com/andrewwormald/everflow/internal/provider"
	"github.com/andrewwormald/everflow/internal/provider/gitlab"
	"github.com/andrewwormald/everflow/internal/refactorsweep"
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
		storePath     = fs.String("store", "", "path to sqlite store (default ~/.everflow/store.db)")
		listenAddr    = fs.String("listen", ":8080", "address for the webhook HTTP server")
		publicBaseURL = fs.String("public-base-url", "", "publicly reachable URL where webhooks land (e.g. https://everflow.example.com)")
		gitlabBaseURL = fs.String("gitlab-base-url", "", "GitLab base URL (defaults to https://gitlab.com)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *publicBaseURL == "" {
		return fmt.Errorf("--public-base-url is required (see DESIGN.md § Public-URL strategy)")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	providers, err := buildProviders(*gitlabBaseURL)
	if err != nil {
		return fmt.Errorf("provider setup: %w", err)
	}
	if len(providers) == 0 {
		logger.Warn("no providers configured — set GITLAB_TOKEN (or future GITHUB_TOKEN) to enable a provider")
	} else {
		for name := range providers {
			logger.Info("provider registered", "name", name)
		}
	}

	recordStore, timeoutStore, err := store.Open(*storePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	wf := refactorsweep.Build(workflowName, refactorsweep.Deps{
		RecordStore:   recordStore,
		TimeoutStore:  timeoutStore,
		EventStreamer: memstreamer.New(),
		RoleScheduler: memrolescheduler.New(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wf.Run(ctx)
	defer wf.Stop()

	secrets := webhook.NewSecretRegistry()
	dispatcher := func(_ context.Context, runID string, ev provider.Event) error {
		logger.Info("webhook received",
			"run_id", runID,
			"kind", ev.Kind,
			"mr_iid", ev.MR.IID,
			"author", ev.Author.Handle,
		)
		// Scaffold: real wiring goes through workflow.Callback once step bodies
		// can consume Events. For now, log and drop.
		return nil
	}
	srv := webhook.NewServer(providers, dispatcher, secrets)
	srvErrCh := make(chan error, 1)
	go func() {
		srvErrCh <- srv.Listen(ctx, *listenAddr)
	}()

	logger.Info("everflow daemon started",
		"version", version,
		"listen", *listenAddr,
		"public_base_url", *publicBaseURL,
		"workflow", workflowName,
	)
	logger.Warn("v1 scaffold — runners, step bodies, and CLI commands are stubs; see DESIGN.md for the build roadmap")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sigCh:
		logger.Info("shutdown signal received, stopping...")
	case err := <-srvErrCh:
		if err != nil {
			return fmt.Errorf("webhook server: %w", err)
		}
	}
	return nil
}

// buildProviders registers providers based on env-var credentials. Empty map
// is valid — the daemon still starts; it just can't accept webhooks until at
// least one provider is configured.
func buildProviders(gitlabBase string) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	if tok := os.Getenv("GITLAB_TOKEN"); tok != "" {
		p, err := gitlab.New(gitlab.Config{BaseURL: gitlabBase, Token: tok})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
	}
	return out, nil
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	_ = fs.String("goal", "", "one-sentence description of the refactor")
	_ = fs.String("provider", "gitlab", "provider name (gitlab | github)")
	_ = fs.String("project", "", "provider project ID, e.g. lunomoney/core")
	_ = fs.String("base-branch", "main", "branch to base each unit's MR off")
	_ = fs.Int("concurrency", 1, "max in-flight MRs at once (see ADR-0015)")
	_ = fs.String("skill", "", "path to the SKILL.md the per-unit subagent will run")
	_ = fs.String("filter", "", "path to a Starlark filter file (defaults to a sensible one)")
	_ = fs.String("discover", "", "path to a Starlark discovery rule (or omit to provide --units)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fmt.Errorf("everflow start: not implemented in scaffold; see DESIGN.md § What's not yet built (step 8)")
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
