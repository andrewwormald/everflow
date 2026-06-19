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

	"github.com/luno/workflow"

	"github.com/andrewwormald/everflow/internal/provider"
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

// discover refreshes the queue and picks the next unit. Stub for this
// commit — real impl runs the Starlark discovery rule + applies state-aware
// filtering. For now: pop the next unit if any, else complete.
func (d *Deps) discover(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if len(r.Object.Queue) == 0 && len(r.Object.InFlight) == 0 {
		return StatusCompleted, nil
	}
	if len(r.Object.Queue) == 0 {
		return StatusCompleted, nil
	}
	r.Object.CurrentUnit = r.Object.Queue[0]
	r.Object.Queue = r.Object.Queue[1:]
	return StatusWorking, nil
}

// work invokes the runner against the current unit and opens an MR. Stub
// for this commit — real impl invokes a Runner, runs git in the worktree,
// opens an MR via the provider, populates InFlight. Next commit.
func (d *Deps) work(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if r.Object.CurrentUnit == "" {
		return StatusFailed, fmt.Errorf("work: no CurrentUnit set")
	}
	return StatusAwaitingMerge, nil
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
