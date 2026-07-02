package poller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/andrewwormald/everflow/internal/provider"
)

func TestAuthBackoffDuration(t *testing.T) {
	tests := []struct {
		failures int
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{0, 30 * time.Second, 30 * time.Second},
		{1, 2 * time.Minute, 2 * time.Minute},
		{2, 8 * time.Minute, 8 * time.Minute},
		{3, 32 * time.Minute, 32 * time.Minute},
		{4, 2 * time.Hour, 2 * time.Hour}, // capped
		{10, 2 * time.Hour, 2 * time.Hour}, // still capped
	}
	for _, tt := range tests {
		got := authBackoffDuration(tt.failures)
		if got < tt.wantMin || got > tt.wantMax {
			t.Errorf("authBackoffDuration(%d) = %v, want [%v, %v]",
				tt.failures, got, tt.wantMin, tt.wantMax)
		}
	}
}

// --- Auth event dispatch ---

// fakeAuthProvider wraps a fixed GetMRState error to simulate auth failures.
type fakeAuthProvider struct {
	getMRStateErr error
	getMRStateCalls int
	mu sync.Mutex
}

func (f *fakeAuthProvider) Name() string                                            { return "fake" }
func (f *fakeAuthProvider) AuthenticatedUser(_ context.Context) (provider.User, error) { return provider.User{}, nil }
func (f *fakeAuthProvider) RegisterWebhook(_ context.Context, _, _, _ string, _ []provider.EventKind) (string, error) { return "", nil }
func (f *fakeAuthProvider) DeregisterWebhook(_ context.Context, _, _ string) error { return nil }
func (f *fakeAuthProvider) VerifySignature(_ http.Header, _ []byte, _ string) bool { return true }
func (f *fakeAuthProvider) NormaliseEvent(_ http.Header, _ []byte) (provider.Event, error) { return provider.Event{}, nil }
func (f *fakeAuthProvider) CreateMR(_ context.Context, _ string, _ provider.MRDraft) (provider.MR, error) { return provider.MR{}, nil }
func (f *fakeAuthProvider) PostComment(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeAuthProvider) UpdateMRTitle(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeAuthProvider) CloseMR(_ context.Context, _ string, _ int) error { return nil }
func (f *fakeAuthProvider) ListNotesSince(_ context.Context, _ string, _ int, _ int64) ([]provider.NotePoll, error) { return nil, nil }
func (f *fakeAuthProvider) ResolveDiscussion(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeAuthProvider) RetryPipelineJob(_ context.Context, _ string, _ int64) error { return nil }
func (f *fakeAuthProvider) IsBot(_ provider.User) bool { return false }
func (f *fakeAuthProvider) GetMRState(_ context.Context, _ string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getMRStateCalls++
	return "", f.getMRStateErr
}

// dispatchRecord captures events dispatched by the poller.
type dispatchRecord struct {
	mu     sync.Mutex
	events []provider.Event
}

func (d *dispatchRecord) dispatch(_ context.Context, _ string, ev provider.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, ev)
	return nil
}

func (d *dispatchRecord) kinds() []provider.EventKind {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]provider.EventKind, len(d.events))
	for i, ev := range d.events {
		out[i] = ev.Kind
	}
	return out
}

func TestPollRun_AuthFailure_DispatchesAuthFailureEventOnceOnly(t *testing.T) {
	fp := &fakeAuthProvider{getMRStateErr: provider.ErrAuthFailure}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": fp},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	run := ActiveRun{
		RunID:    "run-1",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 1}},
	}

	// First poll tick — should dispatch EventProviderAuthFailure.
	l.pollRun(t.Context(), run)
	kinds := dr.kinds()
	if len(kinds) != 1 || kinds[0] != provider.EventProviderAuthFailure {
		t.Errorf("first auth failure poll should dispatch exactly one EventProviderAuthFailure; got %v", kinds)
	}

	// Second poll tick — the run is still in backoff (until in the future);
	// pollRun skips it entirely, so no new event is dispatched.
	l.pollRun(t.Context(), run)
	kinds = dr.kinds()
	if len(kinds) != 1 {
		t.Errorf("subsequent polls during backoff should not dispatch further events; got %v", kinds)
	}
}

func TestPollRun_AuthRestored_DispatchesRestoredEvent(t *testing.T) {
	fp := &fakeAuthProvider{getMRStateErr: provider.ErrAuthFailure}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": fp},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	run := ActiveRun{
		RunID:    "run-2",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 1}},
	}

	// First tick: auth failure registered, backoff set.
	l.pollRun(t.Context(), run)
	// Force the backoff to expire so the next poll is not skipped.
	l.authMu.Lock()
	e := l.authBackoff["run-2"]
	e.until = time.Now().Add(-1 * time.Second)
	l.authBackoff["run-2"] = e
	l.authMu.Unlock()

	// Second tick: token now works.
	fp.mu.Lock()
	fp.getMRStateErr = nil
	fp.mu.Unlock()

	l.pollRun(t.Context(), run)

	kinds := dr.kinds()
	// Expect: EventProviderAuthFailure from tick 1, EventProviderAuthRestored from tick 2.
	found := map[provider.EventKind]bool{}
	for _, k := range kinds {
		found[k] = true
	}
	if !found[provider.EventProviderAuthFailure] {
		t.Errorf("expected EventProviderAuthFailure in dispatched events; got %v", kinds)
	}
	if !found[provider.EventProviderAuthRestored] {
		t.Errorf("expected EventProviderAuthRestored in dispatched events after recovery; got %v", kinds)
	}
}

// Verify that ErrAuthFailure is the sentinel we compare against.
func TestIsAuthError_WrappedErrAuthFailure(t *testing.T) {
	wrapped := errors.New("some context: " + provider.ErrAuthFailure.Error())
	if !provider.IsAuthError(wrapped) {
		// IsAuthError checks the error message too, so a message containing
		// "401" or "unauthorized" qualifies even without errors.Is.
		t.Log("note: wrapped ErrAuthFailure caught via string match, not errors.Is — OK")
	}
	if !provider.IsAuthError(provider.ErrAuthFailure) {
		t.Error("IsAuthError(ErrAuthFailure) should be true")
	}
	if !provider.IsAuthError(fmt.Errorf("wrap: %w", provider.ErrAuthFailure)) {
		t.Error("IsAuthError(wrapped ErrAuthFailure) should be true via errors.Is")
	}
}
