package refactorsweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/luno/workflow"

	"github.com/andrewwormald/everflow/internal/git"
	"github.com/andrewwormald/everflow/internal/provider"
	"github.com/andrewwormald/everflow/internal/runner"
	"github.com/andrewwormald/everflow/internal/webhook"
)

// --- Test fake: provider.Provider ---

// fakeProvider records calls and returns canned values. Sufficient for
// exercising step bodies without hitting real GitLab / GitHub APIs.
type fakeProvider struct {
	mu sync.Mutex

	authedUser provider.User
	authedErr  error

	webhookID  string
	regErr     error
	registered registerCall

	deregisters []string

	createMRResult provider.MR
	createMRErr    error
	createMRCalls  []provider.MRDraft

	commentErr error
	comments   []postedComment

	closeErr error
	closes   []closedMR

	resolveErr error
	resolves   []resolvedDiscussion
}

type resolvedDiscussion struct {
	ProjectID    string
	MRIID        int
	DiscussionID string
}

type closedMR struct {
	ProjectID string
	IID       int
}

type postedComment struct {
	ProjectID string
	MRIID     int
	Body      string
}

type registerCall struct {
	ProjectID  string
	CallbackURL string
	Secret     string
	Events     []provider.EventKind
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) AuthenticatedUser(ctx context.Context) (provider.User, error) {
	return f.authedUser, f.authedErr
}

func (f *fakeProvider) RegisterWebhook(ctx context.Context, projectID, url, secret string, kinds []provider.EventKind) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.regErr != nil {
		return "", f.regErr
	}
	f.registered = registerCall{
		ProjectID:   projectID,
		CallbackURL: url,
		Secret:      secret,
		Events:      append([]provider.EventKind(nil), kinds...),
	}
	return f.webhookID, nil
}

func (f *fakeProvider) DeregisterWebhook(ctx context.Context, projectID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregisters = append(f.deregisters, id)
	return nil
}

func (f *fakeProvider) VerifySignature(_ http.Header, _ []byte, _ string) bool        { return true }
func (f *fakeProvider) NormaliseEvent(_ http.Header, _ []byte) (provider.Event, error) { return provider.Event{}, nil }
func (f *fakeProvider) CreateMR(_ context.Context, projectID string, draft provider.MRDraft) (provider.MR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createMRCalls = append(f.createMRCalls, draft)
	if f.createMRErr != nil {
		return provider.MR{}, f.createMRErr
	}
	if f.createMRResult.IID == 0 {
		// Sensible default: use the project ID and a fake IID.
		return provider.MR{ProjectID: projectID, IID: 1, URL: "https://fake/mr/1", Branch: draft.Branch}, nil
	}
	return f.createMRResult, nil
}
func (f *fakeProvider) PostComment(_ context.Context, projectID string, mrIID int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, postedComment{ProjectID: projectID, MRIID: mrIID, Body: body})
	return f.commentErr
}
func (f *fakeProvider) UpdateMRTitle(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeProvider) GetMRState(_ context.Context, _ string, _ int) (string, error)    { return "opened", nil }
func (f *fakeProvider) ListNotesSince(_ context.Context, _ string, _ int, _ int64) ([]provider.NotePoll, error) {
	return nil, nil
}
func (f *fakeProvider) ResolveDiscussion(_ context.Context, projectID string, mrIID int, discussionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves = append(f.resolves, resolvedDiscussion{ProjectID: projectID, MRIID: mrIID, DiscussionID: discussionID})
	return f.resolveErr
}
func (f *fakeProvider) CloseMR(_ context.Context, projectID string, iid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, closedMR{ProjectID: projectID, IID: iid})
	return f.closeErr
}
func (f *fakeProvider) RetryPipelineJob(_ context.Context, _ string, _ int64) error      { return nil }
func (f *fakeProvider) IsBot(u provider.User) bool                                       { return u.Bot }

// --- Test fake: runner.Runner ---

// fakeRunner records calls and returns canned responses.
type fakeRunner struct {
	mu sync.Mutex

	resp  runner.Response
	err   error
	calls []runner.Request

	// If non-nil, called after Run() returns. Useful for tests that want
	// to simulate the runner having modified files in the worktree.
	onRun func(req runner.Request)
}

func (f *fakeRunner) Name() string { return "fake-runner" }
func (f *fakeRunner) Run(_ context.Context, req runner.Request) (runner.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.onRun != nil {
		f.onRun(req)
	}
	return f.resp, f.err
}

// --- Test fake: git.Git ---

// fakeGit records calls and returns canned results. Default: dirty=true on
// HasChanges (so tests don't have to remember to opt in). Override with
// the explicit fields when a test needs different behaviour.
type fakeGit struct {
	mu sync.Mutex

	ensureErr  error
	resetErr   error
	commitErr  error
	pushErr    error
	hasChanges *bool // nil → default true; set to a bool pointer for explicit
	hasChErr   error

	ensures []ensureCall
	resets  []string
	commits []string
	pushes  []string
	removes []string
}

type ensureCall struct {
	Dir, BaseRepo, BaseBranch, Branch string
}

func (g *fakeGit) EnsureBranch(_ context.Context, dir, baseRepo, baseBranch, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensures = append(g.ensures, ensureCall{dir, baseRepo, baseBranch, branch})
	return g.ensureErr
}

func (g *fakeGit) HardReset(_ context.Context, dir, baseBranch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resets = append(g.resets, dir+"@"+baseBranch)
	return g.resetErr
}

func (g *fakeGit) HasChanges(_ context.Context, _ string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hasChErr != nil {
		return false, g.hasChErr
	}
	if g.hasChanges != nil {
		return *g.hasChanges, nil
	}
	return true, nil // default: assume runner did make changes
}

func (g *fakeGit) Commit(_ context.Context, _, msg string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commits = append(g.commits, msg)
	return g.commitErr
}

func (g *fakeGit) Push(_ context.Context, _, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pushes = append(g.pushes, branch)
	return g.pushErr
}

func (g *fakeGit) RemoveWorktree(_ context.Context, _, dir string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removes = append(g.removes, dir)
	return nil
}

func (g *fakeGit) DiffShortstat(_ context.Context, _, _ string) (string, error) {
	return "1 file changed, 5 insertions(+)", nil
}

func boolPtr(b bool) *bool { return &b }

// --- Helpers ---

func newDeps(t *testing.T, p provider.Provider) *Deps {
	t.Helper()
	reg := runner.NewRegistry()
	return &Deps{
		Providers:     map[string]provider.Provider{p.Name(): p},
		Runners:       reg,
		Git:           &fakeGit{},
		Secrets:       webhook.NewSecretRegistry(),
		PublicBaseURL: "https://everflow.test",
		RunsRoot:      t.TempDir(),
	}
}

// withGit replaces the default fakeGit with a tailored one so tests can
// override individual behaviours (e.g. push fails, no changes detected).
func (d *Deps) withGit(g *fakeGit) *fakeGit {
	d.Git = g
	return g
}

// withRunner adds a runner to Deps.Runners and returns the typed fake so the
// test can inspect calls + override response.
func (d *Deps) withRunner(t *testing.T, fr *fakeRunner) *fakeRunner {
	t.Helper()
	d.Runners.Register(fr)
	return fr
}

func newRun(t *testing.T, state *AgentState) *workflow.Run[AgentState, AgentStatus] {
	t.Helper()
	// Run embeds TypedRecord which embeds Record. Step bodies only read
	// RunID and the Object — the controller (private) is not invoked here.
	return &workflow.Run[AgentState, AgentStatus]{
		TypedRecord: workflow.TypedRecord[AgentState, AgentStatus]{
			Record: workflow.Record{
				WorkflowName: "refactor-sweep-test",
				ForeignID:    "test-foreign",
				RunID:        "00000000-0000-0000-0000-deadbeefcafe",
			},
			Object: state,
		},
	}
}

// --- setup() tests ---

func TestSetup_HappyPath(t *testing.T) {
	fp := &fakeProvider{
		authedUser: provider.User{ID: "42", Handle: "andreww", Email: "a@example.com"},
		webhookID:  "wh-99",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		EventSource:  EventSourceWebhook, // explicit — default is now poll (ADR-0031)
	})

	next, err := d.setup(t.Context(), r)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("next status: want Discovering, got %v", next)
	}

	// Author was captured from AuthenticatedUser.
	if r.Object.Author.Handle != "andreww" {
		t.Errorf("Author.Handle: want andreww, got %q", r.Object.Author.Handle)
	}

	// Webhook was registered with the right shape.
	if fp.registered.ProjectID != "acme/example" {
		t.Errorf("RegisterWebhook ProjectID: want acme/example, got %q", fp.registered.ProjectID)
	}
	if !strings.HasPrefix(fp.registered.CallbackURL, "https://everflow.test/webhook/fake/") {
		t.Errorf("CallbackURL prefix wrong: got %q", fp.registered.CallbackURL)
	}
	if !strings.HasSuffix(fp.registered.CallbackURL, r.RunID) {
		t.Errorf("CallbackURL should end with runID: got %q", fp.registered.CallbackURL)
	}
	if len(fp.registered.Events) == 0 {
		t.Errorf("RegisterWebhook should subscribe to events")
	}
	if len(fp.registered.Secret) < 32 {
		t.Errorf("Secret too short to be hex-encoded 32 bytes: %d chars", len(fp.registered.Secret))
	}

	// AgentState captured the webhook identity.
	if r.Object.WebhookID != "wh-99" {
		t.Errorf("WebhookID: want wh-99, got %q", r.Object.WebhookID)
	}
	if r.Object.WebhookSecret != fp.registered.Secret {
		t.Errorf("WebhookSecret mismatch with registered secret")
	}

	// SecretRegistry was populated.
	got, ok := d.Secrets.Get("fake", r.RunID)
	if !ok {
		t.Errorf("Secret not in registry")
	}
	if got != r.Object.WebhookSecret {
		t.Errorf("Secret in registry doesn't match AgentState")
	}

	// Per-Run dir created.
	runDir := filepath.Join(d.RunsRoot, r.RunID)
	if info, err := os.Stat(runDir); err != nil {
		t.Errorf("run dir not created: %v", err)
	} else if !info.IsDir() {
		t.Errorf("run dir is not a directory")
	}

	// Defaults applied.
	if r.Object.Concurrency != 1 {
		t.Errorf("Concurrency default: want 1, got %d", r.Object.Concurrency)
	}
	if r.Object.InFlight == nil {
		t.Errorf("InFlight should be initialised")
	}
}

func TestSetup_UnknownProvider(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{ProviderName: "not-registered"})

	next, err := d.setup(t.Context(), r)
	if err == nil {
		t.Fatalf("want error for unknown provider")
	}
	if next != StatusFailed {
		t.Errorf("want StatusFailed, got %v", next)
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error message should mention unknown provider: %v", err)
	}
}

func TestSetup_AuthorOverride_Respected(t *testing.T) {
	// User pre-set the author via --author at trigger time. Setup must NOT
	// overwrite it from the token's authenticated user.
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "service-account"},
		webhookID:  "wh-1",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "x/y",
		Author:       provider.User{Handle: "andreww", Email: "a@example.com"},
	})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if r.Object.Author.Handle != "andreww" {
		t.Errorf("Author should be preserved when pre-set; got %q", r.Object.Author.Handle)
	}
}

func TestSetup_Idempotent_SkipsWebhookRegistration(t *testing.T) {
	// Second invocation of setup (e.g. after retry, after daemon restart)
	// must not re-register the webhook. WebhookID already on AgentState =
	// already done.
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "andreww"},
		webhookID:  "wh-1",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{
		ProviderName:  "fake",
		ProjectID:     "x/y",
		WebhookID:     "previously-registered",
		WebhookSecret: "previously-set",
		WebhookURL:    "https://previous/webhook/...",
	})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if fp.registered.ProjectID != "" {
		t.Errorf("RegisterWebhook should not have been called on retry; was called with %+v", fp.registered)
	}
	if r.Object.WebhookID != "previously-registered" {
		t.Errorf("WebhookID was overwritten: got %q", r.Object.WebhookID)
	}
	if r.Object.WebhookSecret != "previously-set" {
		t.Errorf("WebhookSecret was overwritten: got %q", r.Object.WebhookSecret)
	}

	// Secret should still be (re-)populated in the registry — daemon
	// restart needs this even when registration is skipped.
	got, ok := d.Secrets.Get("fake", r.RunID)
	if !ok || got != "previously-set" {
		t.Errorf("Secret should be re-populated in registry; got %q, ok=%v", got, ok)
	}
}

func TestSetup_AuthenticatedUserFails(t *testing.T) {
	fp := &fakeProvider{authedErr: errors.New("401 unauthorized")}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{ProviderName: "fake", ProjectID: "x/y"})

	next, err := d.setup(t.Context(), r)
	if err == nil {
		t.Fatalf("want error from AuthenticatedUser failure")
	}
	if next != StatusFailed {
		t.Errorf("want StatusFailed, got %v", next)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should propagate the underlying message: %v", err)
	}
}

func TestSetup_RegisterWebhookFails(t *testing.T) {
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "andreww"},
		regErr:     errors.New("403 forbidden — token lacks admin:repo_hook"),
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{ProviderName: "fake", ProjectID: "x/y", EventSource: EventSourceWebhook})

	next, err := d.setup(t.Context(), r)
	if err == nil {
		t.Fatalf("want error from RegisterWebhook failure")
	}
	if next != StatusFailed {
		t.Errorf("want StatusFailed, got %v", next)
	}
	if r.Object.WebhookID != "" {
		t.Errorf("WebhookID should not be set on failure: got %q", r.Object.WebhookID)
	}
}

// --- discover() tests ---

func TestDiscover_PopsNextUnitFromQueue(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{
		Queue:    []string{"svc-a", "svc-b", "svc-c"},
		InFlight: map[string]provider.MR{},
	})

	next, err := d.discover(t.Context(), r)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if next != StatusWorking {
		t.Errorf("want Working, got %v", next)
	}
	if r.Object.CurrentUnit != "svc-a" {
		t.Errorf("CurrentUnit: want svc-a, got %q", r.Object.CurrentUnit)
	}
	if len(r.Object.Queue) != 2 || r.Object.Queue[0] != "svc-b" {
		t.Errorf("queue not advanced correctly: %v", r.Object.Queue)
	}
}

func TestDiscover_CompletesWhenQueueAndInFlightEmpty(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{InFlight: map[string]provider.MR{}})

	next, err := d.discover(t.Context(), r)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if next != StatusCompleted {
		t.Errorf("want Completed, got %v", next)
	}
}

func TestDiscover_DedupsAgainstCompletedAndBlacklisted(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{
		Queue:       []string{"svc-a", "svc-b", "svc-c"},
		Completed:   []CompletedUnit{{UnitID: "svc-a"}},
		Blacklisted: []BlacklistedUnit{{UnitID: "svc-b"}},
		InFlight:    map[string]provider.MR{},
	})

	next, err := d.discover(t.Context(), r)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if next != StatusWorking {
		t.Errorf("want Working, got %v", next)
	}
	if r.Object.CurrentUnit != "svc-c" {
		t.Errorf("CurrentUnit: want svc-c (dedup skipped a + b), got %q", r.Object.CurrentUnit)
	}
}

// --- work() tests ---

func TestWork_HappyPath(t *testing.T) {
	fp := &fakeProvider{webhookID: "wh-1"}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "Migrated logrus calls to slog in services/payments",
		Tokens:   1234,
	}})
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Migrate to slog",
		CurrentUnit:  "svc-payments",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
	})

	// Have the fake provider return a real-shaped MR.
	fp.createMRResult = provider.MR{
		ProjectID: "acme/example", IID: 42,
		URL: "https://gitlab/x/merge_requests/42", Branch: "everflow/deadbeef/svc-payments",
	}

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}

	if len(fr.calls) != 1 {
		t.Fatalf("want 1 runner call, got %d", len(fr.calls))
	}
	call := fr.calls[0]
	if call.UnitID != "svc-payments" {
		t.Errorf("Request.UnitID: got %q", call.UnitID)
	}
	if call.Goal != "Migrate to slog" {
		t.Errorf("Request.Goal: got %q", call.Goal)
	}

	mr, ok := r.Object.InFlight["svc-payments"]
	if !ok {
		t.Fatalf("InFlight should contain svc-payments; got %v", r.Object.InFlight)
	}
	if mr.IID != 42 {
		t.Errorf("InFlight MR.IID: want 42, got %d", mr.IID)
	}

	if r.Object.SubagentInvocations != 1 {
		t.Errorf("SubagentInvocations: want 1, got %d", r.Object.SubagentInvocations)
	}
	if len(r.Object.History) != 1 {
		t.Fatalf("History: want 1 turn, got %d", len(r.Object.History))
	}
	if r.Object.History[0].UnitID != "svc-payments" {
		t.Errorf("Turn.UnitID: got %q", r.Object.History[0].UnitID)
	}
	if r.Object.History[0].Tokens != 1234 {
		t.Errorf("Turn.Tokens: want 1234, got %d", r.Object.History[0].Tokens)
	}

	if len(fp.comments) != 1 {
		t.Errorf("expected an initial status comment; got %d", len(fp.comments))
	}
}

func TestWork_RunnerFails(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{err: errors.New("rate limited")})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state, not retried), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "rate limited") {
		t.Errorf("LastError should propagate runner error: %q", r.Object.LastError)
	}
	if len(fp.createMRCalls) != 0 {
		t.Errorf("CreateMR should not be called when runner errors; got %d calls", len(fp.createMRCalls))
	}
}

func TestWork_RunnerDeclines(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionFail,
		Summary:  "I can't figure out the right slog level mapping for this codebase",
	}})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, _ := d.work(t.Context(), r)
	if next != StatusFailed {
		t.Errorf("want Failed on DecisionFail, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "slog level") {
		t.Errorf("LastError should carry the runner's reason: %q", r.Object.LastError)
	}
}

// Regression: when the runner returns DecisionNoChange in the work phase,
// the unit must be blacklisted and the Run must return to Discovering —
// NOT terminate as StatusFailed. The first cross-MR-chain dogfood spike
// against github.com/andrewwormald/everflow ended the whole Run when the
// planner picked a third increment, the runner correctly evaluated it as
// no-op, and work() routed NoChange through the catch-all "unexpected
// decision" → StatusFailed branch.
func TestWork_RunnerNoChange_BlacklistsAndContinues(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionNoChange,
		Summary:  "Spec already satisfied; nothing to change for increment-3",
	}})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "increment-3", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err, got %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("NoChange must route to Discovering (let planner re-evaluate), got %v", next)
	}
	if r.Object.CurrentUnit != "" {
		t.Errorf("CurrentUnit should be cleared after NoChange; got %q", r.Object.CurrentUnit)
	}
	if len(r.Object.Blacklisted) != 1 || r.Object.Blacklisted[0].UnitID != "increment-3" {
		t.Errorf("increment-3 should be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if !strings.Contains(r.Object.Blacklisted[0].Reason, "NoChange") {
		t.Errorf("Blacklist reason should name the decision: %q", r.Object.Blacklisted[0].Reason)
	}
	if r.Object.LastError != "" {
		t.Errorf("NoChange should not set LastError (it's not a failure): %q", r.Object.LastError)
	}
}

func TestWork_CreateMRFails(t *testing.T) {
	fp := &fakeProvider{createMRErr: errors.New("404 not found")}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "404 not found") {
		t.Errorf("LastError should propagate CreateMR error: %q", r.Object.LastError)
	}
	if _, ok := r.Object.InFlight["svc-x"]; ok {
		t.Errorf("InFlight should not contain unit when CreateMR failed")
	}
}

func TestWork_RunnerDoneButCleanWorktree_BlacklistsAndMovesOn(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	d.withGit(&fakeGit{hasChanges: boolPtr(false)})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("clean-worktree Done should go to Discovering, got %v", next)
	}
	if len(fp.createMRCalls) != 0 {
		t.Errorf("no MR should be opened when nothing changed; got %d", len(fp.createMRCalls))
	}
	if len(r.Object.Blacklisted) != 1 || r.Object.Blacklisted[0].UnitID != "svc-x" {
		t.Errorf("svc-x should be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if !strings.Contains(r.Object.Blacklisted[0].Reason, "no changes") {
		t.Errorf("Blacklisted.Reason should mention no changes: %q", r.Object.Blacklisted[0].Reason)
	}
}

func TestWork_PushFails(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	d.withGit(&fakeGit{pushErr: errors.New("remote rejected: branch protection")})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed on push fail, got %v", next)
	}
	if len(fp.createMRCalls) != 0 {
		t.Errorf("no MR should be opened when push failed; got %d", len(fp.createMRCalls))
	}
	if !strings.Contains(r.Object.LastError, "branch protection") {
		t.Errorf("LastError should propagate push error: %q", r.Object.LastError)
	}
}

func TestWork_EnsureBranchFails(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone}})
	d.withGit(&fakeGit{ensureErr: errors.New("base branch 'release/v9' does not exist")})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "release/v9") {
		t.Errorf("LastError should propagate EnsureBranch error: %q", r.Object.LastError)
	}
	if len(fr.calls) != 0 {
		t.Errorf("runner should NOT be invoked when worktree setup fails; got %d calls", len(fr.calls))
	}
}

func TestResume_NoteAdded_DoneButCleanWorktree_PostsInfoComment(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Already correct"}})
	d.withGit(&fakeGit{hasChanges: boolPtr(false)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "are you sure about Foo?", DiscussionID: "disc-abc"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("clean-worktree Done in comment phase should stay AwaitingMerge, got %v", next)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "No code changes") {
		t.Errorf("expected an info-only comment; got %+v", fp.comments)
	}
	// Even when no code change was needed, the discussion should be
	// resolved — the question was answered, even if verbally.
	if len(fp.resolves) != 1 || fp.resolves[0].DiscussionID != "disc-abc" {
		t.Errorf("expected ResolveDiscussion(disc-abc) on clean-worktree path; got %+v", fp.resolves)
	}
}

// Regression: invokeForEvent must NOT pause when Commit returns ErrNoChanges
// (e.g. the runner ran `go build`, producing only a binary artefact that
// our staging filter excluded, so HasChanges=true but nothing was staged).
// Previously this paused the Run with "git Commit failed during ..."; now
// it stays AwaitingMerge with an info comment.
func TestResume_NoteAdded_CommitReturnsNoChanges_StaysAwaitingMerge(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Considered but no code change needed"}})
	d.withGit(&fakeGit{
		hasChanges: boolPtr(true),       // worktree dirty (e.g. compiled binary present)
		commitErr:  git.ErrNoChanges,    // but Commit's filter saw nothing stageable
	})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "what about edge case Z?", DiscussionID: "disc-xyz"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("Commit ErrNoChanges in comment phase should stay AwaitingMerge, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("Run should not be paused on ErrNoChanges; PauseReason=%q", r.Object.PauseReason)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "No code changes") {
		t.Errorf("expected an info-only comment; got %+v", fp.comments)
	}
	if len(fp.resolves) != 1 || fp.resolves[0].DiscussionID != "disc-xyz" {
		t.Errorf("expected ResolveDiscussion(disc-xyz); got %+v", fp.resolves)
	}
}

// Push succeeded but ResolveDiscussion errored → the Run must still
// stay AwaitingMerge (the change is pushed; thread resolution is
// best-effort). A "couldn't resolve" info comment is posted so the
// reviewer knows to close manually.
func TestResume_NoteAdded_ResolveDiscussionFails_StaysAwaitingMerge(t *testing.T) {
	fp := &fakeProvider{resolveErr: errors.New("403 forbidden")}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Renamed."}})
	d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "rename Foo → Bar", DiscussionID: "disc-1"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: want nil err even when resolve fails, got %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge after push+failed-resolve, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("Run must not pause on resolve failure; PauseReason=%q", r.Object.PauseReason)
	}
	// ResolveDiscussion was attempted exactly once.
	if len(fp.resolves) != 1 {
		t.Errorf("ResolveDiscussion should be attempted once; got %d", len(fp.resolves))
	}
	// And an info comment was posted explaining the failure.
	foundInfo := false
	for _, c := range fp.comments {
		if strings.Contains(c.Body, "couldn't resolve") || strings.Contains(c.Body, "403 forbidden") {
			foundInfo = true
			break
		}
	}
	if !foundInfo {
		t.Errorf("expected an info comment surfacing the resolve failure; got %+v", fp.comments)
	}
}

// Push succeeded → resolve the originating discussion thread so the
// reviewer sees their comment marked resolved.
func TestResume_NoteAdded_PushSucceeded_ResolvesDiscussion(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Renamed."}})
	d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "rename Foo → Bar", DiscussionID: "disc-rename"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge after successful push, got %v", next)
	}
	if len(fp.resolves) != 1 || fp.resolves[0].DiscussionID != "disc-rename" {
		t.Errorf("ResolveDiscussion(disc-rename) should be called after push; got %+v", fp.resolves)
	}
}

func TestResume_NoteAdded_PushFails_Pauses(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "fixed"}})
	d.withGit(&fakeGit{pushErr: errors.New("auth required")})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("push failure in comment phase should Pause (MR exists for recovery), got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "Push") {
		t.Errorf("PauseReason should mention push: %q", r.Object.PauseReason)
	}
}

func TestResume_MRMerged_CleansUpWorktree(t *testing.T) {
	g := &fakeGit{}
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	d.withGit(g)
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "svc-a", mr)
	r.Object.BaseRepo = "/some/repo"

	ev := provider.Event{Kind: provider.EventMRMerged, MR: mr}
	if _, err := d.resume(t.Context(), r, payloadOf(t, ev)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(g.removes) != 1 {
		t.Errorf("expected one RemoveWorktree call on merge, got %d", len(g.removes))
	}
	if !strings.Contains(g.removes[0], "svc-a") {
		t.Errorf("removed worktree should be svc-a's, got %q", g.removes[0])
	}
}

func TestWork_UnknownRunner(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "not-registered",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err == nil || !strings.Contains(err.Error(), "unknown runner") {
		t.Fatalf("want unknown-runner error, got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
}

// --- resume() tests ---

// payloadOf JSON-encodes the event in the same shape the daemon's dispatcher
// will produce.
func payloadOf(t *testing.T, ev provider.Event) *bytes.Reader {
	t.Helper()
	buf, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return bytes.NewReader(buf)
}

// awaitingRun returns a Run already in StatusAwaitingMerge with one unit
// in flight against the given MR. Mirrors the state work() leaves behind.
func awaitingRun(t *testing.T, unitID string, mr provider.MR) *workflow.Run[AgentState, AgentStatus] {
	t.Helper()
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    mr.ProjectID,
		RunnerName:   "fake-runner",
		Goal:         "test goal",
		Author:       provider.User{Handle: "andreww"},
		CurrentUnit:  unitID,
		InFlight:     map[string]provider.MR{unitID: mr},
	})
	r.Status = StatusAwaitingMerge
	return r
}

func TestResume_MRMerged_MovesToCompleted(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "acme/example", IID: 42, URL: "https://x/42"}
	r := awaitingRun(t, "svc-a", mr)

	ev := provider.Event{
		Kind: provider.EventMRMerged,
		MR:   mr,
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("want Discovering, got %v", next)
	}
	if _, still := r.Object.InFlight["svc-a"]; still {
		t.Errorf("unit should be removed from InFlight after merge")
	}
	if len(r.Object.Completed) != 1 || r.Object.Completed[0].UnitID != "svc-a" {
		t.Errorf("svc-a should be in Completed; got %+v", r.Object.Completed)
	}
}

func TestResume_MRClosed_MovesToBlacklisted(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 7}
	r := awaitingRun(t, "svc-x", mr)

	ev := provider.Event{Kind: provider.EventMRClosed, MR: mr}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("want Discovering, got %v", next)
	}
	if len(r.Object.Blacklisted) != 1 {
		t.Fatalf("svc-x should be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if !strings.Contains(r.Object.Blacklisted[0].Reason, "closed without merge") {
		t.Errorf("Blacklisted.Reason should mention close: %q", r.Object.Blacklisted[0].Reason)
	}
}

func TestResume_PipelineSucceeded_NoOp(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{Kind: provider.EventPipelineSucceeded, MR: mr}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge (no-op), got %v", next)
	}
	if r.Object.SubagentInvocations != 0 {
		t.Errorf("no subagent should fire on pipeline success; got %d invocations", r.Object.SubagentInvocations)
	}
}

func TestResume_NoteAdded_InvokesSubagent_DecisionDone(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "Addressed: renamed Foo to Bar as requested",
		Tokens:   500,
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{ID: 100, Body: "please rename Foo to Bar"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner should be called once; got %d calls", len(fr.calls))
	}
	if !strings.Contains(fr.calls[0].CommentBody, "rename Foo to Bar") {
		t.Errorf("CommentBody not propagated to runner: %q", fr.calls[0].CommentBody)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "Addressed") {
		t.Errorf("status comment should be posted on DecisionDone; got %+v", fp.comments)
	}
}

func TestResume_NoteAdded_DecisionAsk_PausesWithQuestion(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionAsk,
		Question: "Should I delete the deprecated method or just mark it //Deprecated:?",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded,
		MR:   mr, Author: provider.User{Handle: "reviewer"},
		Note: provider.Note{Body: "what about the deprecated method?"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("want Paused on DecisionAsk, got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "deprecated method") {
		t.Errorf("PauseReason should carry the question: %q", r.Object.PauseReason)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "/everflow resume") {
		t.Errorf("pause comment should mention how to resume: %+v", fp.comments)
	}
}

func TestResume_PipelineFailed_InvokesSubagent(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone, Summary: "Fixed flaky test",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventPipelineFailed,
		MR:   mr,
		Pipeline: provider.Pipeline{
			ID: 99, Status: "failed",
			FailedJobs: []provider.Job{
				{ID: 1, Name: "test 3/5", Stage: "test", Status: "failed"},
			},
		},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner should be called; got %d calls", len(fr.calls))
	}
	if !strings.Contains(fr.calls[0].CIFailure, "test 3/5") {
		t.Errorf("CIFailure should mention failed job: %q", fr.calls[0].CIFailure)
	}
}

// Control-command dispatch is covered in controls_test.go.

func TestResume_PausedRun_DropsNonControlEvents(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused

	ev := provider.Event{
		Kind: provider.EventNoteAdded,
		MR:   mr, Author: provider.User{Handle: "reviewer"},
		Note: provider.Note{Body: "any chance you can also look at this"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("paused Run should stay paused on non-control event, got %v", next)
	}
	if r.Object.SubagentInvocations != 0 {
		t.Errorf("no subagent should fire while paused; got %d", r.Object.SubagentInvocations)
	}
}

// --- Self-comment echo suppression (RecentOutgoingHashes FIFO) ---

// TestPostBotComment_StoresHashOnState asserts the helper records a
// SHA-256 hash of the body on AgentState.RecentOutgoingHashes before
// invoking the provider. The write-before-network ordering is what
// prevents a race where the poller sees the comment before the state
// knows about it.
func TestPostBotComment_StoresHashOnState(t *testing.T) {
	fp := &fakeProvider{}
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	if err := postBotComment(t.Context(), r, fp, mr.ProjectID, mr.IID, "hello world"); err != nil {
		t.Fatalf("postBotComment: %v", err)
	}
	if got := len(r.Object.RecentOutgoingHashes); got != 1 {
		t.Fatalf("want 1 hash on state, got %d", got)
	}
	if r.Object.RecentOutgoingHashes[0] != hashBody("hello world") {
		t.Errorf("recorded hash doesn't match body hash")
	}
	if len(fp.comments) != 1 || fp.comments[0].Body != "hello world" {
		t.Errorf("provider should have received the raw body; got %+v", fp.comments)
	}
}

// TestPostBotComment_CapFIFO exercises the ring-buffer trim: N > cap
// posts and only the last `cap` hashes should survive, in FIFO order.
func TestPostBotComment_CapFIFO(t *testing.T) {
	fp := &fakeProvider{}
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	for i := 0; i < recentOutgoingHashCap+5; i++ {
		_ = postBotComment(t.Context(), r, fp, mr.ProjectID, mr.IID, fmt.Sprintf("msg-%d", i))
	}
	if got := len(r.Object.RecentOutgoingHashes); got != recentOutgoingHashCap {
		t.Errorf("want len==cap after overflow, got %d", got)
	}
	// The last cap messages should be msg-5 through msg-36 (drop msg-0..4).
	oldestKept := hashBody("msg-5")
	if r.Object.RecentOutgoingHashes[0] != oldestKept {
		t.Errorf("FIFO order broken: want first entry = hash(msg-5), got hash of a different body")
	}
	newestKept := hashBody(fmt.Sprintf("msg-%d", recentOutgoingHashCap+4))
	if r.Object.RecentOutgoingHashes[len(r.Object.RecentOutgoingHashes)-1] != newestKept {
		t.Errorf("FIFO order broken: last entry should be the newest post")
	}
}

// TestResume_SkipsOwnEchoedComment is the headline regression guard for
// the self-comment loop. A note whose body matches a recent outgoing
// hash must be silently dropped by resume() without running the filter,
// without invoking the runner, and without any state change beyond
// EventsSkippedByFilter++.
func TestResume_SkipsOwnEchoedComment(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	// Simulate the daemon having posted this exact body earlier in the Run.
	_ = postBotComment(t.Context(), r, fp, mr.ProjectID, mr.IID,
		"ℹ️ address_comment: No code changes were needed.")
	beforeSkipped := r.Object.EventsSkippedByFilter

	// Now the poller "sees" that same body coming back as a new note.
	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "andrewwormald"},
		Note:   provider.Note{ID: 200, Body: "ℹ️ address_comment: No code changes were needed."},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != r.Status {
		t.Errorf("echo skip must not change status; got %v", next)
	}
	if r.Object.EventsSkippedByFilter != beforeSkipped+1 {
		t.Errorf("EventsSkippedByFilter should have incremented; got %d (was %d)",
			r.Object.EventsSkippedByFilter, beforeSkipped)
	}
	if len(fr.calls) != 0 {
		t.Errorf("runner must not fire on an echo; got %d calls", len(fr.calls))
	}
}

// TestResume_DoesNotSkipUnrelatedComment guards against a false positive:
// a note whose body has never been posted by the daemon must fall through
// to normal handling.
func TestResume_DoesNotSkipUnrelatedComment(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "done"}})
	d.withGit(&fakeGit{hasChanges: boolPtr(false)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	// Seed a completely unrelated hash on state.
	_ = postBotComment(t.Context(), r, fp, mr.ProjectID, mr.IID,
		"ℹ️ status: 0 completed, 1 in flight.")
	fp.comments = nil // ignore the seeding comment in subsequent assertions

	// User sends a genuine comment.
	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename Foo to Bar"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	// The runner should have fired for this genuine comment.
	if next != StatusAwaitingMerge {
		t.Errorf("real user comment should route through resume normally, want AwaitingMerge, got %v", next)
	}
}

// TestSelfCommentLoop_EndToEnd is the strong regression guard for ADR-0035.
// It runs the real work() code path — no direct call to postBotComment —
// captures the exact comment body work() posted via its provider, then
// feeds a synthesised note_added event containing that same body through
// resume(). The runner must NOT fire a second time.
//
// This test would fail if:
//   - work() bypassed postBotComment for any call site (regression to
//     direct p.PostComment)
//   - The hash write happened after the network call (race window)
//   - resume()'s isOwnEcho check moved to after the filter or after
//     invokeForEvent (would need a runner invocation to skip)
//
// The prior tests (TestResume_SkipsOwnEchoedComment etc.) prove the
// mechanism in isolation; this one proves the actual daemon loop is
// closed.
func TestSelfCommentLoop_EndToEnd(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "did the thing",
		Tokens:   100,
	}})
	fp.createMRResult = provider.MR{
		ProjectID: "acme/example", IID: 42,
		URL: "https://x/42", Branch: "everflow/deadbeef/svc-a",
	}

	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Migrate something",
		CurrentUnit:  "svc-a",
		BaseBranch:   "main",
		Author:       provider.User{Handle: "andrewwormald"},
		InFlight:     map[string]provider.MR{},
	})

	// --- Stage 1: real work() runs; posts the "🤖 Opened" comment.
	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Fatalf("work should land in AwaitingMerge, got %v", next)
	}
	if len(fp.comments) != 1 {
		t.Fatalf("work should have posted exactly 1 initial status comment; got %d", len(fp.comments))
	}
	if len(fr.calls) != 1 {
		t.Fatalf("work should have invoked the runner exactly once; got %d", len(fr.calls))
	}
	// The exact body work() decided to post — we take it back from what
	// the provider actually received, so this test is decoupled from the
	// specific "🤖 Opened by ..." string.
	echoedBody := fp.comments[0].Body

	// Sanity: the fix must have recorded a hash on state before returning.
	if len(r.Object.RecentOutgoingHashes) == 0 {
		t.Fatal("work() posted a comment without recording its hash on RecentOutgoingHashes — postBotComment was bypassed")
	}
	if r.Object.RecentOutgoingHashes[len(r.Object.RecentOutgoingHashes)-1] != hashBody(echoedBody) {
		t.Fatal("the most-recent recorded hash does not match the body work() actually posted — hash write is misordered")
	}

	// --- Stage 2: simulate the poller picking that same body back up 30s
	//     later as a "new" note_added event. Author is us (gh OAuth == the
	//     Run author), which is exactly the case that trips the bug.
	echo := provider.Event{
		Kind:      provider.EventNoteAdded,
		ProjectID: "acme/example",
		MR:        r.Object.InFlight["svc-a"],
		Author:    provider.User{Handle: "andrewwormald"},
		Note: provider.Note{
			ID:   9999, // any ID > previously-seen; poller believes it's new
			Body: echoedBody,
		},
	}
	skippedBefore := r.Object.EventsSkippedByFilter

	nextAfterEcho, err := d.resume(t.Context(), r, payloadOf(t, echo))
	if err != nil {
		t.Fatalf("resume on echoed comment: %v", err)
	}

	// --- Stage 3: assert the loop is closed.

	// The runner must not have been invoked a second time.
	if len(fr.calls) != 1 {
		t.Errorf(
			"self-comment loop NOT closed: runner was invoked %d times after echo (want 1 from work() only). "+
				"An echo of the daemon's own comment triggered another claude -p call — the very bug ADR-0035 fixes.",
			len(fr.calls),
		)
	}

	// The provider must not have received a second comment from this echo path.
	if len(fp.comments) != 1 {
		t.Errorf("echo path should not post additional comments; provider received %d total (want 1)", len(fp.comments))
	}

	// Status must be unchanged (still AwaitingMerge).
	if nextAfterEcho != r.Status {
		t.Errorf("echo should not transition status; got %v (was %v)", nextAfterEcho, r.Status)
	}

	// Counter increments to prove the skip was taken (rather than the note
	// being ignored for some unrelated reason).
	if r.Object.EventsSkippedByFilter != skippedBefore+1 {
		t.Errorf(
			"EventsSkippedByFilter must increment on the skip; got %d (was %d). "+
				"If this doesn't increment, the note was dropped somewhere else (e.g. unknown-MR filter) rather than by echo suppression, and the test doesn't actually prove the fix.",
			r.Object.EventsSkippedByFilter, skippedBefore,
		)
	}

	// LastSeenNoteIDs must advance PAST the echoed note — otherwise the
	// poller would re-fetch and re-dispatch it on every 30s tick. This
	// caught a real bug in ADR-0035's original implementation where the
	// watermark update was placed AFTER the echo-skip return, so echoes
	// looped forever (correctly skipped each time, but inflating
	// events_seen and generating log noise).
	if got := r.Object.LastSeenNoteIDs[echo.MR.IID]; got < echo.Note.ID {
		t.Errorf(
			"LastSeenNoteIDs[%d] must advance to %d after processing (including on the echo-skip path); got %d. "+
				"If the watermark doesn't advance for skipped echoes, the poller re-dispatches them forever.",
			echo.MR.IID, echo.Note.ID, got,
		)
	}
}

func TestResume_UnknownMR_Dropped(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	r := awaitingRun(t, "u", provider.MR{ProjectID: "x/y", IID: 1})

	// Event arrives for a completely different MR (cross-talk via shared
	// project webhook).
	ev := provider.Event{
		Kind: provider.EventNoteAdded,
		MR:   provider.MR{ProjectID: "x/y", IID: 99},
		Note: provider.Note{Body: "hello"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("unknown MR should be silently dropped; got %v", next)
	}
	if r.Object.SubagentInvocations != 0 {
		t.Errorf("no subagent for unknown MR")
	}
}

// --- Provider auth event handling (ADR-0038) ---

func TestResume_AuthFailure_ParksRunWithProviderAuthPrefix(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{Kind: provider.EventProviderAuthFailure}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusPaused {
		t.Errorf("auth failure should park Run as Paused, got %v", next)
	}
	if !strings.HasPrefix(r.Object.PauseReason, providerAuthPausePrefix) {
		t.Errorf("PauseReason should start with provider-auth prefix, got %q", r.Object.PauseReason)
	}
	// A comment should be posted on the in-flight MR.
	if len(fp.comments) != 1 {
		t.Errorf("expected one comment on auth failure; got %d", len(fp.comments))
	}
	if !strings.Contains(fp.comments[0].Body, "401") {
		t.Errorf("comment should mention 401, got: %q", fp.comments[0].Body)
	}
}

func TestResume_AuthFailure_IdempotentWhenAlreadyAuthPaused(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = providerAuthPausePrefix + "already set"

	ev := provider.Event{Kind: provider.EventProviderAuthFailure}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("second auth failure on already-paused Run should stay Paused, got %v", next)
	}
	// No duplicate comment.
	if len(fp.comments) != 0 {
		t.Errorf("should not post duplicate comment when already auth-paused; got %d", len(fp.comments))
	}
}

func TestResume_AuthRestored_ClearsAuthPauseAndResumesWatching(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = providerAuthPausePrefix + "token expired"

	ev := provider.Event{Kind: provider.EventProviderAuthRestored}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("auth restored on auth-paused Run should go to AwaitingMerge, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("PauseReason should be cleared after auth restored, got %q", r.Object.PauseReason)
	}
}

func TestResume_AuthRestored_NoopOnNonAuthPause(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = "paused by /everflow pause from @andreww"

	ev := provider.Event{Kind: provider.EventProviderAuthRestored}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("auth restored on non-auth-pause should stay Paused, got %v", next)
	}
	if r.Object.PauseReason == "" {
		t.Error("human-set PauseReason should not be cleared by auth restored event")
	}
}

// --- Diff shortstat in MR comments (item 4 hallucination guard, Approach A) ---

func TestResume_NoteAdded_DecisionDone_CommentContainsDiffShortstat(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Fixed it"}})
	// fakeGit.DiffShortstat returns "1 file changed, 5 insertions(+)" by default.

	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please address this"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fp.comments) != 1 {
		t.Fatalf("expected one comment; got %d", len(fp.comments))
	}
	if !strings.Contains(fp.comments[0].Body, "Diff:") {
		t.Errorf("addressed comment should contain diff shortstat; got: %q", fp.comments[0].Body)
	}
	if !strings.Contains(fp.comments[0].Body, "file changed") {
		t.Errorf("diff shortstat should mention file changes; got: %q", fp.comments[0].Body)
	}
}

// --- restored: setup test that follows ---

func TestSetup_SubscribesToExpectedEvents(t *testing.T) {
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "andreww"},
		webhookID:  "wh-1",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{ProviderName: "fake", ProjectID: "x/y", EventSource: EventSourceWebhook})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	want := map[provider.EventKind]bool{
		provider.EventNoteAdded:         true,
		provider.EventMRMerged:          true,
		provider.EventMRClosed:          true,
		provider.EventMRUpdated:         true,
		provider.EventPipelineSucceeded: true,
		provider.EventPipelineFailed:    true,
	}
	got := map[provider.EventKind]bool{}
	for _, k := range fp.registered.Events {
		got[k] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("setup should subscribe to %v; got %v", k, fp.registered.Events)
		}
	}
}
