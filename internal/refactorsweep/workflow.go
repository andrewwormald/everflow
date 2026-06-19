// State machine wiring for the bulk-refactor sweep. See DESIGN.md § "The
// state machine" and ADR-0015.
//
// Step bodies are methods on Deps so they can access the provider registry,
// secret registry, public URL, and runs root via closure. Build() wires the
// graph; the daemon constructs a Deps once at startup and passes it in.
package refactorsweep

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/everflow/internal/provider"
	"github.com/andrewwormald/everflow/internal/runner"
	"github.com/andrewwormald/everflow/internal/webhook"
)

// Deps is the set of collaborators a built workflow needs. The store /
// streamer / scheduler / clock fields are passed to workflow.Build at the
// bottom; the rest are captured by the step closures.
type Deps struct {
	// luno/workflow plumbing
	RecordStore   workflow.RecordStore
	TimeoutStore  workflow.TimeoutStore
	EventStreamer workflow.EventStreamer
	RoleScheduler workflow.RoleScheduler

	// everflow-specific dependencies for the step bodies
	Providers     map[string]provider.Provider // by name; populated at daemon start
	Runners       *runner.Registry             // by name; agents (claude, qwen, openhands)
	Secrets       *webhook.SecretRegistry      // per-(provider, runID) HMAC/token secrets
	PublicBaseURL string                       // e.g. https://everflow.example.com
	RunsRoot      string                       // e.g. ~/.everflow/runs
}

// Build wires the state machine described in DESIGN.md. Step bodies are
// closures over d so they have access to providers, secrets, etc.
func Build(name string, d Deps) *workflow.Workflow[AgentState, AgentStatus] {
	b := workflow.NewBuilder[AgentState, AgentStatus](name)

	b.AddStep(StatusInitiated, d.setup, StatusDiscovering, StatusFailed)

	b.AddStep(StatusDiscovering, d.discover,
		StatusWorking,
		StatusCompleted,
	)

	b.AddStep(StatusWorking, d.work,
		StatusAwaitingMerge,
		StatusFailed,
	)
	// Note: StatusPaused not yet a destination from Working. Once an MR
	// exists in InFlight, the resume callback owns pause/retry/skip; work()
	// can only fail before that point.

	b.AddCallback(StatusAwaitingMerge, d.resume,
		StatusAwaitingMerge,
		StatusDiscovering,
		StatusPaused,
		StatusFailed,
	)

	b.AddCallback(StatusPaused, d.resume,
		StatusAwaitingMerge,
		StatusDiscovering,
		StatusFailed,
	)

	return b.Build(
		d.EventStreamer,
		d.RecordStore,
		d.RoleScheduler,
		workflow.WithTimeoutStore(d.TimeoutStore),
	)
}

// setup runs once per Run when triggered. It:
//
//  1. Resolves the configured provider.
//  2. Captures the Author identity (whoever the token is for, unless
//     pre-set via the override at Trigger time).
//  3. Generates a per-Run webhook secret.
//  4. Registers a project-scoped webhook with the provider.
//  5. Stores webhook identity (ID, secret, URL) on AgentState so daemon
//     restart can resume + so teardown can deregister later.
//  6. Populates the in-memory SecretRegistry so the running daemon can
//     verify inbound webhooks.
//  7. Creates the per-Run filesystem layout (~/.everflow/runs/<runID>/).
//  8. Defaults Concurrency and InFlight if not set.
//
// Idempotency: if WebhookID is already set, we assume a prior partial
// success and skip the registration step. Other side effects (filesystem
// dir, defaults) are themselves idempotent.
//
// On unrecoverable error, returns StatusFailed; the worktree dir is kept
// for inspection.
func (d *Deps) setup(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	p, ok := d.Providers[r.Object.ProviderName]
	if !ok {
		return StatusFailed, fmt.Errorf("setup: unknown provider %q (registered: %v)",
			r.Object.ProviderName, providerNames(d.Providers))
	}

	// Author capture, only if not pre-set via --author override at trigger time.
	if r.Object.Author.Handle == "" {
		user, err := p.AuthenticatedUser(ctx)
		if err != nil {
			return StatusFailed, fmt.Errorf("setup: authenticated user: %w", err)
		}
		r.Object.Author = user
	}

	// Webhook registration. Skip if already done (retry / restart).
	if r.Object.WebhookID == "" {
		secret, err := randomHex(32)
		if err != nil {
			return StatusFailed, fmt.Errorf("setup: random secret: %w", err)
		}
		callbackURL := fmt.Sprintf("%s/webhook/%s/%s", d.PublicBaseURL, p.Name(), r.RunID)
		kinds := []provider.EventKind{
			provider.EventNoteAdded,
			provider.EventMRMerged,
			provider.EventMRClosed,
			provider.EventMRUpdated,
			provider.EventPipelineSucceeded,
			provider.EventPipelineFailed,
		}
		webhookID, err := p.RegisterWebhook(ctx, r.Object.ProjectID, callbackURL, secret, kinds)
		if err != nil {
			return StatusFailed, fmt.Errorf("setup: register webhook: %w", err)
		}
		r.Object.WebhookID = webhookID
		r.Object.WebhookSecret = secret
		r.Object.WebhookURL = callbackURL
	}

	// Populate the in-process secret registry so inbound POSTs verify.
	// Safe to call on every setup invocation; the map is overwritten.
	if d.Secrets != nil {
		d.Secrets.Set(p.Name(), r.RunID, r.Object.WebhookSecret)
	}

	// Per-Run filesystem. MkdirAll is idempotent.
	runDir := filepath.Join(d.RunsRoot, r.RunID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return StatusFailed, fmt.Errorf("setup: mkdir run dir: %w", err)
	}

	// Defaults.
	if r.Object.Concurrency <= 0 {
		r.Object.Concurrency = 1
	}
	if r.Object.InFlight == nil {
		r.Object.InFlight = map[string]provider.MR{}
	}

	return StatusDiscovering, nil
}

// discover picks the next unit from the queue, deduping against units
// already completed, blacklisted, or in-flight. When the queue is empty AND
// nothing is in-flight, the Run is done.
//
// v1: queue is populated at Trigger from the user's static --units list.
// Future: a Starlark discovery rule (DiscoveryPath, ADR-0018) re-runs each
// pass and appends newly-found units. The dedup logic below is forward-
// compatible — it ignores units we've already processed.
func (d *Deps) discover(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	// Build the set of units we no longer need to consider.
	seen := make(map[string]struct{}, len(r.Object.Completed)+len(r.Object.Blacklisted)+len(r.Object.InFlight))
	for _, c := range r.Object.Completed {
		seen[c.UnitID] = struct{}{}
	}
	for _, b := range r.Object.Blacklisted {
		seen[b.UnitID] = struct{}{}
	}
	for unitID := range r.Object.InFlight {
		seen[unitID] = struct{}{}
	}

	// Drop already-processed entries from the head of the queue.
	for len(r.Object.Queue) > 0 {
		if _, dup := seen[r.Object.Queue[0]]; !dup {
			break
		}
		r.Object.Queue = r.Object.Queue[1:]
	}

	// Terminal condition: nothing queued, nothing in-flight → done.
	if len(r.Object.Queue) == 0 && len(r.Object.InFlight) == 0 {
		return StatusCompleted, nil
	}

	// If nothing's queued but units are in-flight, we'd normally wait. In
	// v1 (concurrency=1) this state is unreachable: a unit going to
	// AwaitingMerge stays there until merged/closed, and only then do we
	// re-enter discover. Defensive return when concurrency > 1 lands.
	if len(r.Object.Queue) == 0 {
		return StatusCompleted, nil
	}

	r.Object.CurrentUnit = r.Object.Queue[0]
	r.Object.Queue = r.Object.Queue[1:]
	return StatusWorking, nil
}

// work invokes the runner against the current unit and, on success,
// opens an MR via the provider. The MR is stored in InFlight so the
// resume callback can dispatch incoming webhook events to the right unit.
//
// What's done in this commit:
//   - Looks up the runner (by AgentState.RunnerName) and provider
//   - Builds a bounded RunRequest with the unit's scope
//   - Invokes the runner, appends a Turn to history
//   - On DecisionDone: calls Provider.CreateMR, records the MR in InFlight,
//     posts an initial status comment, transitions to AwaitingMerge
//   - On error / DecisionFail before MR creation: returns StatusFailed
//     (no MR exists yet, so there's no recovery surface for the author —
//     a future ADR may add a pre-MR pause+retry path)
//
// What's NOT done in this commit (TODO before production use):
//   - Setting up a git worktree at <RunsRoot>/<runID>/worktrees/<unitID>/
//   - Running git fetch / branch / commit / push on the runner's output
//   - Loading SkillPath + FilterPath into the runner's environment
//   The next commit lands these; until then Provider.CreateMR is called
//   with a branch name that doesn't exist on the remote — real GitLab/GitHub
//   would reject it, but the fake provider in tests accepts anything.
func (d *Deps) work(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if r.Object.CurrentUnit == "" {
		return StatusFailed, fmt.Errorf("work: no CurrentUnit set")
	}
	p, ok := d.Providers[r.Object.ProviderName]
	if !ok {
		return StatusFailed, fmt.Errorf("work: unknown provider %q", r.Object.ProviderName)
	}
	if d.Runners == nil {
		return StatusFailed, fmt.Errorf("work: no Runners configured")
	}
	rn, err := d.Runners.Get(r.Object.RunnerName)
	if err != nil {
		return StatusFailed, fmt.Errorf("work: runner: %w", err)
	}

	unitID := r.Object.CurrentUnit
	branch := branchName(r.RunID, unitID)
	worktree := filepath.Join(d.RunsRoot, r.RunID, "worktrees", unitID)

	req := runner.Request{
		Worktree:     worktree,
		SkillCommand: fmt.Sprintf("/everflow-unit %s", unitID), // overridden once SkillPath integration lands
		Goal:         r.Object.Goal,
		UnitID:       unitID,
		Budget:       r.Object.Budget,
	}

	resp, runErr := rn.Run(ctx, req)
	turn := Turn{
		Index:     len(r.Object.History),
		UnitID:    unitID,
		Runner:    rn.Name(),
		Phase:     "work",
		Summary:   resp.Summary,
		Tokens:    resp.Tokens,
		StartedAt: orNow(resp.StartedAt),
		EndedAt:   orNow(resp.EndedAt),
	}
	if runErr != nil {
		turn.Error = runErr.Error()
	}
	r.Object.History = append(r.Object.History, turn)
	r.Object.SubagentInvocations++

	if runErr != nil {
		r.Object.LastError = runErr.Error()
		return StatusFailed, fmt.Errorf("work: runner.Run: %w", runErr)
	}

	switch resp.Decision {
	case DecisionDone:
		// Real impl: at this point the runner's changes are committed in
		// the local worktree; we git-push, then open the MR. Until git ops
		// land in the next commit, CreateMR is called with a branch name
		// that doesn't yet exist remotely.
		mr, err := p.CreateMR(ctx, r.Object.ProjectID, provider.MRDraft{
			Branch:       branch,
			TargetBranch: defaultIfEmpty(r.Object.BaseBranch, "main"),
			Title:        fmt.Sprintf("%s: %s", r.Object.Goal, unitID),
			Description:  resp.Summary,
			Labels:       []string{"everflow", "everflow:" + r.RunID[:8]},
		})
		if err != nil {
			r.Object.LastError = err.Error()
			return StatusFailed, fmt.Errorf("work: CreateMR: %w", err)
		}
		r.Object.InFlight[unitID] = mr

		// Initial status comment so the author can see who's driving the MR.
		body := fmt.Sprintf("🤖 Opened by everflow run `%s` (unit `%s`). I'll babysit this MR through review and CI — reply `/everflow status` for progress, or `/everflow skip` to abandon.", r.RunID[:8], unitID)
		if err := p.PostComment(ctx, r.Object.ProjectID, mr.IID, body); err != nil {
			// Comment failure is non-fatal — the MR exists, the run continues.
			r.Object.LastError = fmt.Sprintf("post initial comment: %v", err)
		}

		return StatusAwaitingMerge, nil

	case DecisionFail:
		r.Object.LastError = fmt.Sprintf("runner declined unit %q: %s", unitID, resp.Summary)
		return StatusFailed, nil

	default:
		// Continue / Ask / NoChange are unexpected in the work phase — the
		// runner is supposed to produce a complete change set or give up.
		r.Object.LastError = fmt.Sprintf("unexpected decision %q in work phase for unit %q",
			resp.Decision, unitID)
		return StatusFailed, nil
	}
}

// resume handles webhook callbacks. Stub for this commit — real impl
// decodes Event from the payload, runs the Starlark filter, dispatches
// to comment-handling / CI-handling / MR-merge handlers. Commit after next.
func (d *Deps) resume(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], _ io.Reader) (AgentStatus, error) {
	return StatusAwaitingMerge, nil
}

// --- helpers ---

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func providerNames(m map[string]provider.Provider) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// branchName picks the source branch name for a unit's MR. Format:
// `everflow/<short-runID>/<unitID>`. Short runID keeps the branch readable
// while staying unique across concurrent refactors.
func branchName(runID, unitID string) string {
	short := runID
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("everflow/%s/%s", short, unitID)
}

func defaultIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
