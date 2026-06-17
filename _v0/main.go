package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/luno/workflow"
)

const workflowName = "scheduled-skill"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "runners" {
		listRunners()
		return
	}
	if err := runDaemon(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func listRunners() {
	fmt.Println("Available runners:")
	for _, n := range runnerNames() {
		fmt.Printf("  %s\n", n)
	}
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("everflow", flag.ExitOnError)
	var (
		skill      = fs.String("skill", "", "slash command to invoke each pass, e.g. /mrs-babysit --slack-request-reviews")
		runnerName = fs.String("runner", "claude", "runner: "+strings.Join(runnerNames(), "|"))
		interval   = fs.Duration("interval", 30*time.Minute, "wall-clock interval between passes (e.g. 5m, 30m, 1h)")
		baseRepo   = fs.String("base-repo", "", "absolute path to the git repo to base the worktree off")
		baseBranch = fs.String("base-branch", "main", "branch to base the worktree off and rebase to each pass")
		root       = fs.String("root", "", "where to store worktrees (default ~/.everflow/wt)")
		foreignID  = fs.String("id", "", "foreign ID for the Run (default: auto)")
	)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: everflow --skill "/mrs-babysit ..." [flags]

Runs a Claude Code (or any registered runner) skill on a fixed interval, in a
durable workflow that survives terminal closure. The current process is the
"daemon" — it stays foreground; Ctrl-C stops gracefully. Designed for use
under launchd / systemd / a tmux pane.

Subcommands:
  runners      list registered runners and exit

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *skill == "" {
		fs.Usage()
		return fmt.Errorf("--skill is required")
	}
	if *baseRepo == "" {
		return fmt.Errorf("--base-repo is required (e.g. --base-repo ~/dev/core)")
	}
	if _, err := getRunner(*runnerName); err != nil {
		return err
	}

	absBaseRepo, err := filepath.Abs(expandHome(*baseRepo))
	if err != nil {
		return fmt.Errorf("resolve --base-repo: %w", err)
	}
	if _, err := os.Stat(filepath.Join(absBaseRepo, ".git")); err != nil {
		return fmt.Errorf("--base-repo %q is not a git repo: %w", absBaseRepo, err)
	}

	rootDir := *root
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("home dir: %w", err)
		}
		rootDir = filepath.Join(home, ".everflow", "wt")
	}

	fid := *foreignID
	if fid == "" {
		fid = fmt.Sprintf("%s-%d", sanitize(*skill), time.Now().Unix())
	}

	worktree := filepath.Join(rootDir, fid)
	branch := "wf-" + fid

	wf := BuildWorkflow(workflowName)

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wf.Run(rootCtx)
	defer wf.Stop()

	state := &AgentState{
		Goal:         fmt.Sprintf("Run %s every %s", *skill, *interval),
		RunnerName:   *runnerName,
		SkillCommand: *skill,
		Interval:     *interval,
		BaseRepo:     absBaseRepo,
		BaseBranch:   *baseBranch,
		Worktree:     worktree,
		Branch:       branch,
	}

	runID, err := wf.Trigger(rootCtx, fid, workflow.WithInitialValue[AgentState, AgentStatus](state))
	if err != nil {
		return fmt.Errorf("trigger: %w", err)
	}

	log.Printf("triggered run %s (foreign id: %s)", runID, fid)
	log.Printf("  skill:    %s", *skill)
	log.Printf("  runner:   %s", *runnerName)
	log.Printf("  interval: %s", *interval)
	log.Printf("  base:     %s @ %s", absBaseRepo, *baseBranch)
	log.Printf("  worktree: %s (branch %s)", worktree, branch)
	log.Printf("press Ctrl-C to stop")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Printf("shutdown signal received, stopping workflow gracefully...")
	cancel()
	// wf.Stop is deferred above; it waits for in-flight steps.
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// sanitize turns "/mrs-babysit --slack-request-reviews" into a safe-ish slug
// for a foreign ID. Lossy by design; foreign IDs need only be unique per
// workflow, not human-readable.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
		case r == '-' || r == '_':
			out = append(out, r)
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	s = strings.Trim(string(out), "-")
	if s == "" {
		return "skill"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
