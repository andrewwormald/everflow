package gitlab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/andrewwormald/syntropy/internal/provider"
)

// NormaliseEvent decodes a GitLab webhook POST into provider.Event. Routes
// by the X-Gitlab-Event header; returns provider.ErrIgnore for event kinds
// we didn't subscribe to (a project hook fires for everything the project
// enables, not just our subscription).
func (p *Provider) NormaliseEvent(headers http.Header, body []byte) (provider.Event, error) {
	now := time.Now().UnixNano()

	switch headers.Get("X-Gitlab-Event") {
	case "Note Hook":
		return parseNote(body, now)
	case "Pipeline Hook":
		return parsePipeline(body, now)
	case "Merge Request Hook":
		return parseMR(body, now)
	default:
		return provider.Event{}, provider.ErrIgnore{Reason: "unsubscribed event kind"}
	}
}

// --- Note Hook (comments on MRs) ---

type notePayload struct {
	ObjectKind       string `json:"object_kind"`
	User             gitlabUser `json:"user"`
	Project          gitlabProject `json:"project"`
	ObjectAttributes struct {
		ID           int64  `json:"id"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		DiscussionID string `json:"discussion_id"`
	} `json:"object_attributes"`
	MergeRequest *struct {
		IID          int    `json:"iid"`
		SourceBranch string `json:"source_branch"`
		WebURL       string `json:"url"`
	} `json:"merge_request,omitempty"`
}

func parseNote(body []byte, now int64) (provider.Event, error) {
	var p notePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("decode note hook: %w", err)
	}
	if p.ObjectAttributes.NoteableType != "MergeRequest" {
		return provider.Event{}, provider.ErrIgnore{Reason: "non-MR note (issue/commit/snippet)"}
	}
	if p.MergeRequest == nil {
		return provider.Event{}, provider.ErrIgnore{Reason: "note without merge_request envelope"}
	}
	return provider.Event{
		Kind:      provider.EventNoteAdded,
		ProjectID: p.Project.idAsString(),
		MR: provider.MR{
			ProjectID: p.Project.idAsString(),
			IID:       p.MergeRequest.IID,
			URL:       p.MergeRequest.WebURL,
			Branch:    p.MergeRequest.SourceBranch,
		},
		Author: p.User.toProviderUser(),
		IsBot:  p.User.Bot,
		Note: provider.Note{
			ID:           p.ObjectAttributes.ID,
			Body:         p.ObjectAttributes.Note,
			DiscussionID: p.ObjectAttributes.DiscussionID,
			Stream:       streamNote,
		},
		Raw:        body,
		ReceivedAt: now,
	}, nil
}

// --- Pipeline Hook ---

type pipelinePayload struct {
	ObjectKind       string `json:"object_kind"`
	User             gitlabUser `json:"user"`
	Project          gitlabProject `json:"project"`
	ObjectAttributes struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Ref    string `json:"ref"`
	} `json:"object_attributes"`
	MergeRequest *struct {
		IID          int    `json:"iid"`
		SourceBranch string `json:"source_branch"`
		WebURL       string `json:"url"`
	} `json:"merge_request,omitempty"`
	Builds []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Stage  string `json:"stage"`
		Status string `json:"status"`
	} `json:"builds"`
}

func parsePipeline(body []byte, now int64) (provider.Event, error) {
	var p pipelinePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("decode pipeline hook: %w", err)
	}
	var kind provider.EventKind
	switch p.ObjectAttributes.Status {
	case "success":
		kind = provider.EventPipelineSucceeded
	case "failed":
		kind = provider.EventPipelineFailed
	default:
		return provider.Event{}, provider.ErrIgnore{Reason: "pipeline status " + p.ObjectAttributes.Status}
	}
	// We only track pipelines associated with an MR — pipelines on `main` after
	// a merge are not actionable by everflow.
	if p.MergeRequest == nil {
		return provider.Event{}, provider.ErrIgnore{Reason: "pipeline not tied to an MR"}
	}

	var failed []provider.Job
	for _, b := range p.Builds {
		if b.Status != "failed" {
			continue
		}
		failed = append(failed, provider.Job{
			ID:     b.ID,
			Name:   b.Name,
			Stage:  b.Stage,
			Status: b.Status,
			// LogTail is left empty; the workflow fetches the trace lazily via
			// the API only when a subagent needs to classify the failure.
		})
	}

	return provider.Event{
		Kind:      kind,
		ProjectID: p.Project.idAsString(),
		MR: provider.MR{
			ProjectID: p.Project.idAsString(),
			IID:       p.MergeRequest.IID,
			URL:       p.MergeRequest.WebURL,
			Branch:    p.MergeRequest.SourceBranch,
		},
		Author: p.User.toProviderUser(),
		IsBot:  p.User.Bot,
		Pipeline: provider.Pipeline{
			ID:         p.ObjectAttributes.ID,
			Status:     p.ObjectAttributes.Status,
			FailedJobs: failed,
		},
		Raw:        body,
		ReceivedAt: now,
	}, nil
}

// --- Merge Request Hook ---

type mrPayload struct {
	ObjectKind       string `json:"object_kind"`
	User             gitlabUser `json:"user"`
	Project          gitlabProject `json:"project"`
	ObjectAttributes struct {
		IID          int    `json:"iid"`
		Action       string `json:"action"`        // "open" | "close" | "reopen" | "update" | "approved" | "merge"
		State        string `json:"state"`         // "opened" | "closed" | "merged"
		SourceBranch string `json:"source_branch"`
		URL          string `json:"url"`
	} `json:"object_attributes"`
}

func parseMR(body []byte, now int64) (provider.Event, error) {
	var p mrPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("decode merge_request hook: %w", err)
	}
	var kind provider.EventKind
	switch p.ObjectAttributes.Action {
	case "merge":
		kind = provider.EventMRMerged
	case "close":
		kind = provider.EventMRClosed
	case "update", "reopen", "approved":
		kind = provider.EventMRUpdated
	case "open":
		// We don't react to our own opens — we just created the MR. The MR
		// state machine is already in StatusAwaitingMerge by the time this
		// fires. Ignore.
		return provider.Event{}, provider.ErrIgnore{Reason: "self-emitted MR open"}
	default:
		return provider.Event{}, provider.ErrIgnore{Reason: "MR action " + p.ObjectAttributes.Action}
	}

	return provider.Event{
		Kind:      kind,
		ProjectID: p.Project.idAsString(),
		MR: provider.MR{
			ProjectID: p.Project.idAsString(),
			IID:       p.ObjectAttributes.IID,
			URL:       p.ObjectAttributes.URL,
			Branch:    p.ObjectAttributes.SourceBranch,
		},
		Author:     p.User.toProviderUser(),
		IsBot:      p.User.Bot,
		Raw:        body,
		ReceivedAt: now,
	}, nil
}

// --- shared shapes ---

type gitlabUser struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Bot      bool   `json:"bot"`
}

func (u gitlabUser) toProviderUser() provider.User {
	id := ""
	if u.ID != 0 {
		id = fmt.Sprintf("%d", u.ID)
	}
	return provider.User{
		ID:     id,
		Handle: u.Username,
		Email:  u.Email,
		Bot:    u.Bot,
	}
}

type gitlabProject struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

// idAsString returns the project identifier the GitLab API accepts —
// either the numeric ID or the URL-encoded path_with_namespace. We prefer
// the path because it's stable across project moves and human-readable in
// logs.
func (g gitlabProject) idAsString() string {
	if g.PathWithNamespace != "" {
		return g.PathWithNamespace
	}
	return fmt.Sprintf("%d", g.ID)
}
