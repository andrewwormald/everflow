package refactorsweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/luno/workflow"

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
func (f *fakeProvider) CloseMR(_ context.Context, _ string, _ int) error                 { return nil }
func (f *fakeProvider) RetryPipelineJob(_ context.Context, _ string, _ int64) error      { return nil }
func (f *fakeProvider) IsBot(u provider.User) bool                                       { return u.Bot }

// --- Test fake: runner.Runner ---

// fakeRunner records calls and returns canned responses.
type fakeRunner struct {
	mu sync.Mutex

	resp  runner.Response
	err   error
	calls []runner.Request
}

func (f *fakeRunner) Name() string { return "fake-runner" }
func (f *fakeRunner) Run(_ context.Context, req runner.Request) (runner.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return f.resp, f.err
}

// --- Helpers ---

func newDeps(t *testing.T, p provider.Provider) *Deps {
	t.Helper()
	reg := runner.NewRegistry()
	return &Deps{
		Providers:     map[string]provider.Provider{p.Name(): p},
		Runners:       reg,
		Secrets:       webhook.NewSecretRegistry(),
		PublicBaseURL: "https://everflow.test",
		RunsRoot:      t.TempDir(),
	}
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
		ProjectID:    "lunomoney/core",
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
	if fp.registered.ProjectID != "lunomoney/core" {
		t.Errorf("RegisterWebhook ProjectID: want lunomoney/core, got %q", fp.registered.ProjectID)
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
	r := newRun(t, &AgentState{ProviderName: "fake", ProjectID: "x/y"})

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
		ProjectID:    "lunomoney/core",
		RunnerName:   "fake-runner",
		Goal:         "Migrate to slog",
		CurrentUnit:  "svc-payments",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
	})

	// Have the fake provider return a real-shaped MR.
	fp.createMRResult = provider.MR{
		ProjectID: "lunomoney/core", IID: 42,
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
	if err == nil {
		t.Fatalf("want error from runner failure")
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
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

func TestWork_CreateMRFails(t *testing.T) {
	fp := &fakeProvider{createMRErr: errors.New("404 not found")}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err == nil {
		t.Fatalf("want error from CreateMR failure")
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
	if _, ok := r.Object.InFlight["svc-x"]; ok {
		t.Errorf("InFlight should not contain unit when CreateMR failed")
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
	mr := provider.MR{ProjectID: "lunomoney/core", IID: 42, URL: "https://x/42"}
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

func TestResume_ControlCommandFromAuthor_DeferredToNextCommit(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "andreww"}, // matches r.Object.Author
		Note:   provider.Note{Body: "/everflow pause"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	// For this commit we just route to a TODO. Either Status is acceptable
	// — the important thing is no subagent invocation and the event was
	// detected as a control command (no fallthrough to filter).
	if r.Object.SubagentInvocations != 0 {
		t.Errorf("control commands should not invoke subagent; got %d", r.Object.SubagentInvocations)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("expected to stay in current status; got %v", next)
	}
}

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

// --- restored: setup test that follows ---

func TestSetup_SubscribesToExpectedEvents(t *testing.T) {
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "andreww"},
		webhookID:  "wh-1",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{ProviderName: "fake", ProjectID: "x/y"})

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
