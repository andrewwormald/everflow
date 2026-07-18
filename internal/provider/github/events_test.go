package github

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/andrewwormald/syntropy/internal/provider"
)

func header(name, value string) http.Header {
	h := http.Header{}
	h.Set(name, value)
	return h
}

func TestNormaliseEvent_Ping(t *testing.T) {
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-GitHub-Event", "ping"), []byte(`{"zen": "..."}`))
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for ping, got %T %v", err, err)
	}
}

func TestNormaliseEvent_UnknownHeader(t *testing.T) {
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-GitHub-Event", "push"), []byte(`{}`))
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for unsubscribed event, got %T %v", err, err)
	}
}

// --- issue_comment ---

func TestNormaliseEvent_IssueComment_OnPR(t *testing.T) {
	body := []byte(`{
		"action": "created",
		"comment": {
			"id": 9001,
			"body": "please rename this method",
			"user": {"id": 42, "login": "andreww", "type": "User"}
		},
		"issue": {
			"number": 1234,
			"html_url": "https://github.com/owner/repo/pull/1234",
			"pull_request": {"url": "https://api.github.com/repos/owner/repo/pulls/1234"}
		},
		"repository": {"id": 1, "full_name": "owner/repo"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-GitHub-Event", "issue_comment"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventNoteAdded {
		t.Errorf("Kind: want EventNoteAdded, got %v", ev.Kind)
	}
	if ev.ProjectID != "owner/repo" {
		t.Errorf("ProjectID: want owner/repo, got %q", ev.ProjectID)
	}
	if ev.MR.IID != 1234 {
		t.Errorf("MR.IID: want 1234, got %d", ev.MR.IID)
	}
	if ev.Author.Handle != "andreww" {
		t.Errorf("Author.Handle: want andreww, got %q", ev.Author.Handle)
	}
	if !strings.Contains(ev.Note.Body, "rename") {
		t.Errorf("Note.Body: missing comment text, got %q", ev.Note.Body)
	}
}

func TestNormaliseEvent_IssueComment_OnIssue_Ignored(t *testing.T) {
	body := []byte(`{
		"action": "created",
		"comment": {"id": 1, "body": "x", "user": {"id": 1, "login": "x", "type": "User"}},
		"issue": {"number": 5, "html_url": "x"},
		"repository": {"id": 1, "full_name": "owner/repo"}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-GitHub-Event", "issue_comment"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for issue (not PR) comment, got %T %v", err, err)
	}
}

func TestNormaliseEvent_IssueComment_Edited_Ignored(t *testing.T) {
	body := []byte(`{
		"action": "edited",
		"comment": {"id": 1, "body": "x", "user": {"id": 1, "login": "x"}},
		"issue": {"number": 5, "html_url": "x", "pull_request": {"url": "x"}},
		"repository": {"full_name": "owner/repo"}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-GitHub-Event", "issue_comment"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for edited comment (we only react to created), got %T %v", err, err)
	}
}

// --- pull_request_review ---

func TestNormaliseEvent_PRReview_WithBody(t *testing.T) {
	body := []byte(`{
		"action": "submitted",
		"review": {
			"id": 555,
			"body": "Looks fine but rename foo to bar.",
			"state": "changes_requested",
			"user": {"id": 7, "login": "reviewer", "type": "User"}
		},
		"pull_request": {
			"number": 88,
			"html_url": "https://github.com/owner/repo/pull/88",
			"head": {"ref": "feature/branch"}
		},
		"repository": {"full_name": "owner/repo"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-GitHub-Event", "pull_request_review"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventNoteAdded {
		t.Errorf("Kind: want EventNoteAdded, got %v", ev.Kind)
	}
	if ev.MR.Branch != "feature/branch" {
		t.Errorf("MR.Branch: want feature/branch, got %q", ev.MR.Branch)
	}
	if ev.Note.ID != 555 {
		t.Errorf("Note.ID: want 555, got %d", ev.Note.ID)
	}
}

func TestNormaliseEvent_PRReview_BodylessApproval_Ignored(t *testing.T) {
	body := []byte(`{
		"action": "submitted",
		"review": {
			"id": 1, "body": "", "state": "approved",
			"user": {"id": 1, "login": "x", "type": "User"}
		},
		"pull_request": {"number": 88, "html_url": "x", "head": {"ref": "x"}},
		"repository": {"full_name": "owner/repo"}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-GitHub-Event", "pull_request_review"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for body-less approval, got %T %v", err, err)
	}
}

// --- pull_request_review_comment ---

func TestNormaliseEvent_PRReviewComment(t *testing.T) {
	body := []byte(`{
		"action": "created",
		"comment": {
			"id": 7777,
			"node_id": "PRRC_kwDOABCDEF4Aabcdef",
			"body": "could you handle the nil case here?",
			"user": {"id": 7, "login": "reviewer", "type": "User"}
		},
		"pull_request": {
			"number": 88,
			"html_url": "https://github.com/owner/repo/pull/88",
			"head": {"ref": "feature/x"}
		},
		"repository": {"full_name": "owner/repo"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-GitHub-Event", "pull_request_review_comment"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventNoteAdded {
		t.Errorf("Kind: want EventNoteAdded, got %v", ev.Kind)
	}
	if ev.Note.ID != 7777 {
		t.Errorf("Note.ID: want 7777, got %d", ev.Note.ID)
	}
	// node_id is preserved as DiscussionID so the (eventual) GraphQL
	// resolveReviewThread call has something to map onto a thread node.
	if ev.Note.DiscussionID != "PRRC_kwDOABCDEF4Aabcdef" {
		t.Errorf("Note.DiscussionID: want the comment's node_id, got %q", ev.Note.DiscussionID)
	}
}

// issue_comment is on the PR conversation tab — it does NOT live on a
// review thread, so there's nothing for ResolveDiscussion to act on.
// DiscussionID must stay empty (a no-op for the caller).
func TestNormaliseEvent_IssueComment_NoDiscussionID(t *testing.T) {
	body := []byte(`{
		"action": "created",
		"comment": {
			"id": 555,
			"node_id": "IC_kwDOABCDEF55",
			"body": "general PR comment",
			"user": {"id": 7, "login": "reviewer", "type": "User"}
		},
		"issue": {
			"number": 88,
			"html_url": "https://github.com/owner/repo/pull/88",
			"pull_request": {"url": "https://api.github.com/repos/owner/repo/pulls/88"}
		},
		"repository": {"full_name": "owner/repo"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-GitHub-Event", "issue_comment"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Note.DiscussionID != "" {
		t.Errorf("issue_comment has no review thread; DiscussionID must be empty, got %q", ev.Note.DiscussionID)
	}
}

// --- check_suite ---

func TestNormaliseEvent_CheckSuite_Success(t *testing.T) {
	body := []byte(`{
		"action": "completed",
		"check_suite": {
			"id": 9999,
			"status": "completed",
			"conclusion": "success",
			"pull_requests": [{"number": 88, "url": "x", "head": {"ref": "feature/x"}}]
		},
		"repository": {"full_name": "owner/repo"},
		"sender": {"id": 1, "login": "ci-runner", "type": "User"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-GitHub-Event", "check_suite"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventPipelineSucceeded {
		t.Errorf("Kind: want EventPipelineSucceeded, got %v", ev.Kind)
	}
	if ev.Pipeline.ID != 9999 {
		t.Errorf("Pipeline.ID: want 9999, got %d", ev.Pipeline.ID)
	}
}

func TestNormaliseEvent_CheckSuite_Failure(t *testing.T) {
	body := []byte(`{
		"action": "completed",
		"check_suite": {
			"id": 9999, "status": "completed", "conclusion": "failure",
			"pull_requests": [{"number": 88, "url": "x", "head": {"ref": "x"}}]
		},
		"repository": {"full_name": "owner/repo"},
		"sender": {"id": 1, "login": "x"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-GitHub-Event", "check_suite"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventPipelineFailed {
		t.Errorf("Kind: want EventPipelineFailed, got %v", ev.Kind)
	}
}

func TestNormaliseEvent_CheckSuite_NotCompleted_Ignored(t *testing.T) {
	body := []byte(`{
		"action": "requested",
		"check_suite": {"id": 1, "status": "queued", "pull_requests": []},
		"repository": {"full_name": "owner/repo"}, "sender": {}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-GitHub-Event", "check_suite"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for non-completed check_suite, got %T %v", err, err)
	}
}

func TestNormaliseEvent_CheckSuite_NoPR_Ignored(t *testing.T) {
	body := []byte(`{
		"action": "completed",
		"check_suite": {"id": 1, "status": "completed", "conclusion": "success", "pull_requests": []},
		"repository": {"full_name": "owner/repo"}, "sender": {}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-GitHub-Event", "check_suite"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for PR-less suite, got %T %v", err, err)
	}
}

// --- pull_request ---

func TestNormaliseEvent_PR_Merged(t *testing.T) {
	body := []byte(`{
		"action": "closed",
		"pull_request": {
			"number": 88,
			"html_url": "https://github.com/owner/repo/pull/88",
			"merged": true,
			"head": {"ref": "feature/x"}
		},
		"repository": {"full_name": "owner/repo"},
		"sender": {"id": 1, "login": "andreww", "type": "User"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-GitHub-Event", "pull_request"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventMRMerged {
		t.Errorf("Kind: want EventMRMerged, got %v", ev.Kind)
	}
}

func TestNormaliseEvent_PR_ClosedNotMerged(t *testing.T) {
	body := []byte(`{
		"action": "closed",
		"pull_request": {"number": 88, "html_url": "x", "merged": false, "head": {"ref": "x"}},
		"repository": {"full_name": "owner/repo"},
		"sender": {"login": "x"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-GitHub-Event", "pull_request"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventMRClosed {
		t.Errorf("Kind: want EventMRClosed, got %v", ev.Kind)
	}
}

func TestNormaliseEvent_PR_Opened_Ignored(t *testing.T) {
	body := []byte(`{
		"action": "opened",
		"pull_request": {"number": 88, "html_url": "x", "head": {"ref": "x"}},
		"repository": {"full_name": "owner/repo"},
		"sender": {"login": "x"}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-GitHub-Event", "pull_request"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for self-emitted open, got %T %v", err, err)
	}
}

// --- Bot detection ---

func TestBotDetection(t *testing.T) {
	cases := []struct {
		user ghUser
		want bool
	}{
		{ghUser{Login: "dependabot[bot]", Type: "Bot"}, true},
		{ghUser{Login: "renovate[bot]", Type: "Bot"}, true},
		{ghUser{Login: "andreww", Type: "User"}, false},
		{ghUser{Login: "weird-bot", Type: "User"}, false},                 // type matters more than name
		{ghUser{Login: "looks-like[bot]", Type: "User"}, true},            // trailing [bot] wins even if Type=User
	}
	for _, tc := range cases {
		if got := tc.user.isBot(); got != tc.want {
			t.Errorf("isBot(%+v): want %v, got %v", tc.user, tc.want, got)
		}
	}
}
