package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/andrewwormald/everflow/internal/provider"
)

// VerifySignature checks the X-Hub-Signature-256 header against an
// HMAC-SHA256 of the body, keyed by the registered secret. Constant-time
// comparison. Unlike GitLab, this is real cryptographic signing.
func (p *Provider) VerifySignature(headers http.Header, body []byte, secret string) bool {
	sig := headers.Get("X-Hub-Signature-256")
	if sig == "" || secret == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return false
	}
	got, err := hex.DecodeString(sig[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(got, want)
}

// NormaliseEvent decodes a GitHub webhook POST into provider.Event. Routes
// by the X-GitHub-Event header. Returns provider.ErrIgnore for event kinds
// we did not subscribe to or sub-actions we don't care about.
//
// GitHub's three comment events (issue_comment, pull_request_review,
// pull_request_review_comment) all collapse onto provider.EventNoteAdded.
// We only surface body-bearing reviews; "approved" reviews with no body
// are skipped (no actionable content for a subagent).
func (p *Provider) NormaliseEvent(headers http.Header, body []byte) (provider.Event, error) {
	now := time.Now().UnixNano()

	switch headers.Get("X-GitHub-Event") {
	case "issue_comment":
		return parseIssueComment(body, now)
	case "pull_request_review":
		return parsePRReview(body, now)
	case "pull_request_review_comment":
		return parsePRReviewComment(body, now)
	case "check_suite":
		return parseCheckSuite(body, now)
	case "pull_request":
		return parsePullRequest(body, now)
	case "ping":
		return provider.Event{}, provider.ErrIgnore{Reason: "GitHub ping (webhook handshake)"}
	default:
		return provider.Event{}, provider.ErrIgnore{Reason: "unsubscribed event kind"}
	}
}

// --- issue_comment (MR-level comments) ---

type issueCommentPayload struct {
	Action  string `json:"action"` // "created" | "edited" | "deleted"
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User ghUser `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number      int `json:"number"`
		HTMLURL     string `json:"html_url"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request,omitempty"`
	} `json:"issue"`
	Repository ghRepo `json:"repository"`
}

func parseIssueComment(body []byte, now int64) (provider.Event, error) {
	var p issueCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("decode issue_comment: %w", err)
	}
	if p.Action != "created" {
		return provider.Event{}, provider.ErrIgnore{Reason: "issue_comment action " + p.Action}
	}
	// issue_comment fires for issues AND PR-level comments; the pull_request
	// pointer is only set for PR-level. We only care about PRs.
	if p.Issue.PullRequest == nil {
		return provider.Event{}, provider.ErrIgnore{Reason: "comment on issue, not PR"}
	}
	return provider.Event{
		Kind:      provider.EventNoteAdded,
		ProjectID: p.Repository.FullName,
		MR: provider.MR{
			ProjectID: p.Repository.FullName,
			IID:       p.Issue.Number,
			URL:       p.Issue.HTMLURL,
		},
		Author: p.Comment.User.toProviderUser(),
		IsBot:  p.Comment.User.isBot(),
		Note: provider.Note{
			ID:   p.Comment.ID,
			Body: p.Comment.Body,
		},
		Raw:        body,
		ReceivedAt: now,
	}, nil
}

// --- pull_request_review (review-level body) ---

type prReviewPayload struct {
	Action string `json:"action"` // "submitted" | "edited" | "dismissed"
	Review struct {
		ID    int64  `json:"id"`
		Body  string `json:"body"`
		State string `json:"state"` // "approved" | "changes_requested" | "commented"
		User  ghUser `json:"user"`
	} `json:"review"`
	PullRequest struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository ghRepo `json:"repository"`
}

func parsePRReview(body []byte, now int64) (provider.Event, error) {
	var p prReviewPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("decode pull_request_review: %w", err)
	}
	if p.Action != "submitted" {
		return provider.Event{}, provider.ErrIgnore{Reason: "pull_request_review action " + p.Action}
	}
	// Body-less "approved" reviews have no actionable content; skip.
	if p.Review.Body == "" && p.Review.State == "approved" {
		return provider.Event{}, provider.ErrIgnore{Reason: "body-less approval"}
	}
	return provider.Event{
		Kind:      provider.EventNoteAdded,
		ProjectID: p.Repository.FullName,
		MR: provider.MR{
			ProjectID: p.Repository.FullName,
			IID:       p.PullRequest.Number,
			URL:       p.PullRequest.HTMLURL,
			Branch:    p.PullRequest.Head.Ref,
		},
		Author: p.Review.User.toProviderUser(),
		IsBot:  p.Review.User.isBot(),
		Note: provider.Note{
			ID:   p.Review.ID,
			Body: p.Review.Body,
		},
		Raw:        body,
		ReceivedAt: now,
	}, nil
}

// --- pull_request_review_comment (inline line comments) ---

type prReviewCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User ghUser `json:"user"`
	} `json:"comment"`
	PullRequest struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository ghRepo `json:"repository"`
}

func parsePRReviewComment(body []byte, now int64) (provider.Event, error) {
	var p prReviewCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("decode pull_request_review_comment: %w", err)
	}
	if p.Action != "created" {
		return provider.Event{}, provider.ErrIgnore{Reason: "pull_request_review_comment action " + p.Action}
	}
	return provider.Event{
		Kind:      provider.EventNoteAdded,
		ProjectID: p.Repository.FullName,
		MR: provider.MR{
			ProjectID: p.Repository.FullName,
			IID:       p.PullRequest.Number,
			URL:       p.PullRequest.HTMLURL,
			Branch:    p.PullRequest.Head.Ref,
		},
		Author: p.Comment.User.toProviderUser(),
		IsBot:  p.Comment.User.isBot(),
		Note: provider.Note{
			ID:   p.Comment.ID,
			Body: p.Comment.Body,
		},
		Raw:        body,
		ReceivedAt: now,
	}, nil
}

// --- check_suite (CI aggregate) ---

type checkSuitePayload struct {
	Action     string `json:"action"`
	CheckSuite struct {
		ID         int64    `json:"id"`
		Status     string   `json:"status"`     // "queued" | "in_progress" | "completed"
		Conclusion string   `json:"conclusion"` // "success" | "failure" | "cancelled" | ...
		PullRequests []struct {
			Number int `json:"number"`
			URL    string `json:"url"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
	Repository ghRepo `json:"repository"`
	Sender     ghUser `json:"sender"`
}

func parseCheckSuite(body []byte, now int64) (provider.Event, error) {
	var p checkSuitePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("decode check_suite: %w", err)
	}
	if p.Action != "completed" || p.CheckSuite.Status != "completed" {
		return provider.Event{}, provider.ErrIgnore{Reason: "check_suite not completed"}
	}
	var kind provider.EventKind
	switch p.CheckSuite.Conclusion {
	case "success":
		kind = provider.EventPipelineSucceeded
	case "failure", "timed_out":
		kind = provider.EventPipelineFailed
	default:
		return provider.Event{}, provider.ErrIgnore{Reason: "check_suite conclusion " + p.CheckSuite.Conclusion}
	}
	// A check_suite without an associated PR is for direct pushes to main —
	// not actionable by everflow.
	if len(p.CheckSuite.PullRequests) == 0 {
		return provider.Event{}, provider.ErrIgnore{Reason: "check_suite not tied to a PR"}
	}
	pr := p.CheckSuite.PullRequests[0]
	return provider.Event{
		Kind:      kind,
		ProjectID: p.Repository.FullName,
		MR: provider.MR{
			ProjectID: p.Repository.FullName,
			IID:       pr.Number,
			URL:       pr.URL,
			Branch:    pr.Head.Ref,
		},
		Author: p.Sender.toProviderUser(),
		IsBot:  p.Sender.isBot(),
		Pipeline: provider.Pipeline{
			ID:     p.CheckSuite.ID,
			Status: p.CheckSuite.Conclusion,
			// FailedJobs left empty — populated lazily by the workflow if it
			// needs to classify the failure. The check_suite payload doesn't
			// include individual job details; we'd need GET /repos/.../check-runs.
		},
		Raw:        body,
		ReceivedAt: now,
	}, nil
}

// --- pull_request (MR lifecycle) ---

type pullRequestPayload struct {
	Action      string `json:"action"`  // "opened" | "closed" | "reopened" | "edited" | "synchronize" | ...
	PullRequest struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Merged  bool   `json:"merged"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository ghRepo `json:"repository"`
	Sender     ghUser `json:"sender"`
}

func parsePullRequest(body []byte, now int64) (provider.Event, error) {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("decode pull_request: %w", err)
	}
	var kind provider.EventKind
	switch p.Action {
	case "closed":
		if p.PullRequest.Merged {
			kind = provider.EventMRMerged
		} else {
			kind = provider.EventMRClosed
		}
	case "reopened", "edited", "synchronize":
		kind = provider.EventMRUpdated
	case "opened":
		return provider.Event{}, provider.ErrIgnore{Reason: "self-emitted PR open"}
	default:
		return provider.Event{}, provider.ErrIgnore{Reason: "PR action " + p.Action}
	}
	return provider.Event{
		Kind:      kind,
		ProjectID: p.Repository.FullName,
		MR: provider.MR{
			ProjectID: p.Repository.FullName,
			IID:       p.PullRequest.Number,
			URL:       p.PullRequest.HTMLURL,
			Branch:    p.PullRequest.Head.Ref,
		},
		Author:     p.Sender.toProviderUser(),
		IsBot:      p.Sender.isBot(),
		Raw:        body,
		ReceivedAt: now,
	}, nil
}

// --- shared shapes ---

type ghUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"` // "User" | "Bot" | "Organization"
}

func (u ghUser) toProviderUser() provider.User {
	id := ""
	if u.ID != 0 {
		id = fmt.Sprintf("%d", u.ID)
	}
	return provider.User{
		ID:     id,
		Handle: u.Login,
		Bot:    u.isBot(),
	}
}

func (u ghUser) isBot() bool {
	return u.Type == "Bot" || strings.HasSuffix(u.Login, "[bot]")
}

type ghRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"` // "owner/repo"
}
