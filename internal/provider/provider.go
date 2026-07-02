// Package provider defines the abstraction between everflow and an MR-hosting
// platform (GitLab, GitHub). Each platform supplies an implementation; the
// rest of everflow programs against this interface.
//
// See ../../DESIGN.md § "Provider abstraction" and ADR-0014, ADR-0016, ADR-0017.
package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrAuthFailure is returned (or wrapped) by provider methods when the
// platform responds with an authentication or authorisation failure (HTTP
// 401/403). The poller uses it to back off rather than hammering an expired
// token on every tick.
var ErrAuthFailure = errors.New("provider: authentication failure")

// IsAuthError reports whether err is (or wraps) ErrAuthFailure, or whether
// the error message indicates a 401/403 from the platform. The string check
// is a fallback for provider implementations that haven't yet wrapped
// ErrAuthFailure explicitly.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAuthFailure) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden")
}

// Provider is the platform abstraction. v1 ships gitlab.Provider; v2 adds
// github.Provider. All other everflow code programs against this interface.
type Provider interface {
	// Name returns the provider identifier ("gitlab", "github") used in URLs,
	// config, and logs.
	Name() string

	// AuthenticatedUser returns the user the configured credentials belong to.
	// Called once at Run trigger time to capture the Author (see ADR-0017).
	AuthenticatedUser(ctx context.Context) (User, error)

	// RegisterWebhook subscribes to project-scoped events. Returns the platform's
	// webhook ID so we can deregister later. Webhooks are project-scoped, not
	// MR-scoped — events for the whole project arrive and the daemon dispatches
	// to Runs by payload (see DESIGN.md "Provider abstraction").
	RegisterWebhook(ctx context.Context, projectID, callbackURL, secret string, events []EventKind) (webhookID string, err error)
	DeregisterWebhook(ctx context.Context, projectID, webhookID string) error

	// VerifySignature returns true iff the inbound request's HMAC matches the
	// secret we registered with. GitLab puts the bare token in X-Gitlab-Token;
	// GitHub uses X-Hub-Signature-256 with a sha256 HMAC. The provider knows.
	VerifySignature(headers http.Header, body []byte, secret string) bool

	// NormaliseEvent parses a webhook POST into our internal Event shape.
	// Returns ErrIgnore for event kinds we don't care about (e.g. push events
	// when we only subscribed to merge_requests).
	NormaliseEvent(headers http.Header, body []byte) (Event, error)

	// MR lifecycle.
	CreateMR(ctx context.Context, projectID string, mr MRDraft) (MR, error)
	PostComment(ctx context.Context, projectID string, mrIID int, body string) error
	UpdateMRTitle(ctx context.Context, projectID string, mrIID int, title string) error
	CloseMR(ctx context.Context, projectID string, mrIID int) error

	// Polling support (used when EventSource=poll instead of webhook).
	GetMRState(ctx context.Context, projectID string, mrIID int) (state string, err error)
	ListNotesSince(ctx context.Context, projectID string, mrIID int, sinceNoteID int64) ([]NotePoll, error)

	// ResolveDiscussion marks a comment thread as resolved on the platform.
	// Called by invokeForEvent after a runner-driven change has been pushed
	// in response to a reviewer comment, so the reviewer sees the thread
	// closed automatically. discussionID is the platform-specific identifier
	// surfaced in Note.DiscussionID; passing an empty string is a no-op.
	ResolveDiscussion(ctx context.Context, projectID string, mrIID int, discussionID string) error

	// CI/job control.
	RetryPipelineJob(ctx context.Context, projectID string, jobID int64) error

	// User classification.
	IsBot(u User) bool
}

// EventKind names a normalised event we may subscribe to. Provider-specific
// event names map onto these.
type EventKind string

const (
	EventNoteAdded         EventKind = "note_added"
	EventPipelineSucceeded EventKind = "pipeline_succeeded"
	EventPipelineFailed    EventKind = "pipeline_failed"
	EventMRMerged          EventKind = "mr_merged"
	EventMRClosed          EventKind = "mr_closed"
	EventMRUpdated         EventKind = "mr_updated"

	// EventProviderAuthFailure is a synthetic event the poller emits when
	// it receives a 401/403 from the provider. It is never received from a
	// webhook — it signals the state machine that the token has expired so
	// it can park the Run and post a comment. See ADR-0038.
	EventProviderAuthFailure EventKind = "provider_auth_failure"

	// EventProviderAuthRestored is a synthetic event emitted by the poller
	// on the first successful API call after a prior auth failure. It clears
	// the auth-pause state and returns the Run to normal watching.
	EventProviderAuthRestored EventKind = "provider_auth_restored"
)

// User is the normalised shape of a platform user. Author classification
// (ADR-0017) uses Handle to match against the Run's recorded author.
type User struct {
	ID     string
	Handle string
	Email  string
	Bot    bool
}

// MRDraft is what we hand the provider when opening a new MR.
type MRDraft struct {
	Branch       string
	TargetBranch string
	Title        string
	Description  string
	Labels       []string
	// Draft, when true, signals the platform to open the MR as Draft /
	// Work-in-Progress so it isn't accidentally reviewed or merged. GitLab
	// uses a "Draft: " title prefix; GitHub uses the draft field on create.
	Draft bool
}

// MR is a created MR's identity. Stored on AgentState alongside the unit it
// represents so inbound events can be dispatched to the right Run.
type MR struct {
	ProjectID string
	IID       int
	URL       string
	Branch    string
}

// Event is the normalised inbound event everflow's state machine consumes.
// Provider implementations parse their wire format into this shape.
type Event struct {
	Kind        EventKind
	ProjectID   string
	MR          MR
	Author      User    // the event's commenter / pusher; not the Run's author
	IsAuthor    bool    // set by everflow after normalisation, not the provider
	IsBot       bool    // mirror of Author.Bot for ergonomics
	Note        Note    // populated when Kind == EventNoteAdded
	Pipeline    Pipeline // populated for pipeline events
	Raw         []byte  // original payload for filter access; immutable
	ReceivedAt  int64   // unix nanos
}

// Note is the comment payload on a note_added event.
type Note struct {
	ID            int64
	Body          string
	DiscussionID  string // platform-specific thread identifier; pass to Provider.ResolveDiscussion
}

// NotePoll is the per-comment shape returned by ListNotesSince — used by
// the poller to synthesise note_added events. Includes the author so we
// can populate Event.Author / IsAuthor / IsBot.
type NotePoll struct {
	ID            int64
	Body          string
	Author        User
	DiscussionID  string
}

// Pipeline is the CI payload on pipeline events.
type Pipeline struct {
	ID       int64
	Status   string // "success" | "failed" | ...
	FailedJobs []Job
}

// Job is a single CI job.
type Job struct {
	ID    int64
	Name  string
	Stage string
	Status string
	LogTail string // last ~2KB of the job log, populated for failed jobs
}

// ErrIgnore signals NormaliseEvent that this payload is not one we care about
// (e.g. a push event when we only subscribed to MR events). The caller treats
// this as a no-op, not a real error.
type ErrIgnore struct{ Reason string }

func (e ErrIgnore) Error() string { return "event ignored: " + e.Reason }
