// Package github implements provider.Provider for github.com and GitHub
// Enterprise Server. See ADR-0021 for the implementation choices that differ
// from the GitLab provider (HMAC-signed webhooks, owner/repo split, etc.).
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/andrewwormald/everflow/internal/provider"
)

// Provider is the GitHub implementation of provider.Provider.
type Provider struct {
	baseURL string // e.g. https://api.github.com (default) or GHE's API base
	token   string
	hc      *http.Client
}

// Config wires a Provider.
type Config struct {
	BaseURL string        // defaults to https://api.github.com
	Token   string        // required; classic PAT, fine-grained PAT, or App installation token
	Timeout time.Duration // defaults to 30s per request
}

// New constructs a Provider. Fails fast on missing token.
func New(cfg Config) (*Provider, error) {
	if cfg.Token == "" {
		return nil, errors.New("github: Token is required (set GITHUB_TOKEN)")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Provider{
		baseURL: base,
		token:   cfg.Token,
		hc:      &http.Client{Timeout: timeout},
	}, nil
}

func (p *Provider) Name() string { return "github" }

// AuthenticatedUser → GET /user. GitHub returns type=User|Bot|Organization
// and a login field which we treat as the canonical handle.
func (p *Provider) AuthenticatedUser(ctx context.Context) (provider.User, error) {
	var raw struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Email string `json:"email"`
		Type  string `json:"type"`
	}
	if err := p.doJSON(ctx, http.MethodGet, "/user", nil, &raw); err != nil {
		return provider.User{}, err
	}
	return provider.User{
		ID:     fmt.Sprintf("%d", raw.ID),
		Handle: raw.Login,
		Email:  raw.Email,
		Bot:    raw.Type == "Bot" || strings.HasSuffix(raw.Login, "[bot]"),
	}, nil
}

// RegisterWebhook → POST /repos/{owner}/{repo}/hooks. GitHub's `events`
// field takes a list of event names; we translate provider.EventKind into
// the union of GitHub event names that map onto each.
func (p *Provider) RegisterWebhook(ctx context.Context, projectID, callbackURL, secret string, kinds []provider.EventKind) (string, error) {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return "", err
	}
	events := mapEventKinds(kinds)
	body := map[string]any{
		"name":   "web",
		"active": true,
		"events": events,
		"config": map[string]any{
			"url":          callbackURL,
			"content_type": "json",
			"secret":       secret,
			"insecure_ssl": "0",
		},
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	path := fmt.Sprintf("/repos/%s/%s/hooks", owner, repo)
	if err := p.doJSON(ctx, http.MethodPost, path, body, &resp); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", resp.ID), nil
}

// DeregisterWebhook → DELETE /repos/{owner}/{repo}/hooks/{hook_id}. 404 is
// success (already gone).
func (p *Provider) DeregisterWebhook(ctx context.Context, projectID, webhookID string) error {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/hooks/%s", owner, repo, webhookID)
	resp, err := p.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 400 {
		return p.errFromResponse(resp)
	}
	return nil
}

// CreateMR → POST /repos/{owner}/{repo}/pulls. GitHub calls them PRs in the
// UI; the API uses both terms. Labels are applied as a follow-up call
// because the PR creation endpoint does not accept them.
func (p *Provider) CreateMR(ctx context.Context, projectID string, draft provider.MRDraft) (provider.MR, error) {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return provider.MR{}, err
	}
	body := map[string]any{
		"head":  draft.Branch,
		"base":  draft.TargetBranch,
		"title": draft.Title,
		"body":  draft.Description,
		"draft": draft.Draft, // GitHub honours this field on create
	}
	var resp struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	if err := p.doJSON(ctx, http.MethodPost, path, body, &resp); err != nil {
		return provider.MR{}, err
	}
	mr := provider.MR{
		ProjectID: projectID,
		IID:       resp.Number,
		URL:       resp.HTMLURL,
		Branch:    draft.Branch,
	}
	if len(draft.Labels) > 0 {
		labelPath := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, resp.Number)
		_ = p.doJSON(ctx, http.MethodPost, labelPath, map[string]any{"labels": draft.Labels}, nil)
		// Label-application failures are non-fatal; the MR is already open.
	}
	return mr, nil
}

// PostComment → POST /repos/{owner}/{repo}/issues/{number}/comments.
// Issue comments cover non-review-line MR comments — exactly what we want
// for status updates and the author's /everflow control conversation.
func (p *Provider) PostComment(ctx context.Context, projectID string, mrIID int, body string) error {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, mrIID)
	return p.doJSON(ctx, http.MethodPost, path, map[string]any{"body": body}, nil)
}

// UpdateMRTitle → PATCH /repos/{owner}/{repo}/pulls/{number}.
func (p *Provider) UpdateMRTitle(ctx context.Context, projectID string, mrIID int, title string) error {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, mrIID)
	return p.doJSON(ctx, http.MethodPatch, path, map[string]any{"title": title}, nil)
}

// CloseMR → PATCH /repos/{owner}/{repo}/pulls/{number} with state=closed.
func (p *Provider) CloseMR(ctx context.Context, projectID string, mrIID int) error {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, mrIID)
	return p.doJSON(ctx, http.MethodPatch, path, map[string]any{"state": "closed"}, nil)
}

// GetMRState reads the PR's state ("open" | "closed"; merged is "closed"
// with merged_at set). Returns the GitLab-style state vocabulary mapping
// for callers that don't care about the merged-vs-just-closed distinction.
// GetMRState → GET /repos/{owner}/{repo}/pulls/{number}. Returns one of
// "opened" | "closed" | "merged" to match the poller's state-event
// vocabulary (see internal/poller/poller.go mrStateEvent).
//
// GitHub's REST response has a `state` field ("open"|"closed") and a
// separate `merged` boolean; we collapse them into the same three
// strings GitLab returns so the poller can stay provider-agnostic.
func (p *Provider) GetMRState(ctx context.Context, projectID string, mrIID int) (string, error) {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return "", err
	}
	var pr struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, mrIID)
	if err := p.doJSON(ctx, http.MethodGet, path, nil, &pr); err != nil {
		return "", err
	}
	switch {
	case pr.Merged:
		return "merged", nil
	case pr.State == "closed":
		return "closed", nil
	default:
		return "opened", nil
	}
}

// GitHub's three comment endpoints (see ListNotesSince) each draw their
// `id` from an independent sequence — they are NOT a single globally
// monotonic counter shared across resource types. A review comment can
// easily have a lower id than an issue comment posted earlier in wall-clock
// time. These match the X-GitHub-Event webhook names so poll- and
// webhook-sourced notes bucket into the same AgentState cursor entries.
const (
	streamIssueComment  = "issue_comment"
	streamReviewComment = "pull_request_review_comment"
	streamReview        = "pull_request_review"
)

// ListNotesSince fetches new comments on a PR across GitHub's three
// comment streams and returns them merged + sorted by ID ascending:
//
//   - issue_comment              → /repos/.../issues/{n}/comments  (PR conversation)
//   - pull_request_review        → /repos/.../pulls/{n}/reviews    (top-level reviews)
//   - pull_request_review_comment → /repos/.../pulls/{n}/comments  (inline line comments)
//
// Each stream is filtered against its own watermark in `since`, falling
// back to since.Legacy for any stream not yet tracked individually. See
// provider.NoteCursor and ADR-0041 for why a single shared watermark is
// wrong here: mixing the three streams' ids into one scalar can cause a
// lower-id comment on one stream to be silently and permanently dropped
// after a higher-id comment arrives on a different stream.
//
// Only inline review comments carry a `node_id` we can hand to
// ResolveDiscussion; the other two come back with DiscussionID="".
// Body-less reviews ("approved" with no comment) are filtered out to
// match NormaliseEvent's webhook semantics — no actionable content for
// the subagent.
//
// Pagination cap: 100 per endpoint per tick. For dogfood / personal
// use this is fine; if a single 30s window ever sees >100 new comments
// across all streams we'll need to paginate.
func (p *Provider) ListNotesSince(ctx context.Context, projectID string, mrIID int, since provider.NoteCursor) ([]provider.NotePoll, error) {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return nil, err
	}

	threshold := func(stream string) int64 {
		if v, ok := since.ByStream[stream]; ok {
			return v
		}
		return since.Legacy
	}

	out := make([]provider.NotePoll, 0)

	// 1. Issue (PR conversation) comments.
	var issueComments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User ghUser `json:"user"`
	}
	if err := p.doJSON(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, mrIID),
		nil, &issueComments); err != nil {
		return nil, fmt.Errorf("ListNotesSince: issue comments: %w", err)
	}
	issueSince := threshold(streamIssueComment)
	for _, c := range issueComments {
		if c.ID <= issueSince {
			continue
		}
		out = append(out, provider.NotePoll{
			ID:     c.ID,
			Body:   c.Body,
			Author: c.User.toProviderUser(),
			Stream: streamIssueComment,
		})
	}

	// 2. Inline review comments (the ones with a thread node_id).
	var prReviewComments []struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Body   string `json:"body"`
		User   ghUser `json:"user"`
	}
	if err := p.doJSON(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100", owner, repo, mrIID),
		nil, &prReviewComments); err != nil {
		return nil, fmt.Errorf("ListNotesSince: pr review comments: %w", err)
	}
	reviewCommentSince := threshold(streamReviewComment)
	for _, c := range prReviewComments {
		if c.ID <= reviewCommentSince {
			continue
		}
		out = append(out, provider.NotePoll{
			ID:           c.ID,
			Body:         c.Body,
			DiscussionID: c.NodeID,
			Author:       c.User.toProviderUser(),
			Stream:       streamReviewComment,
		})
	}

	// 3. Top-level reviews (only body-bearing ones, matching the webhook semantics).
	var reviews []struct {
		ID    int64  `json:"id"`
		Body  string `json:"body"`
		State string `json:"state"`
		User  ghUser `json:"user"`
	}
	if err := p.doJSON(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", owner, repo, mrIID),
		nil, &reviews); err != nil {
		return nil, fmt.Errorf("ListNotesSince: reviews: %w", err)
	}
	reviewSince := threshold(streamReview)
	for _, r := range reviews {
		if r.ID <= reviewSince {
			continue
		}
		if r.Body == "" && r.State == "APPROVED" {
			continue
		}
		out = append(out, provider.NotePoll{
			ID:     r.ID,
			Body:   r.Body,
			Author: r.User.toProviderUser(),
			Stream: streamReview,
		})
	}

	// Chronological order so the resume() handler processes oldest first.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ResolveDiscussion marks a GitHub pull-request review thread as resolved
// via the GraphQL `resolveReviewThread` mutation.
//
// The discussionID we receive from the inbound webhook decoder is the
// pull_request_review_comment's GraphQL node_id (see events.go). The
// mutation operates on the parent PullRequestReviewThread node, so we
// first query for the comment's thread node ID, then call the mutation.
//
// Empty discussionID is a no-op — issue_comment and pull_request_review
// events don't live on a thread, so DiscussionID stays empty for those
// kinds (the only way the caller hits this method with a non-empty
// discussionID is via a pull_request_review_comment).
func (p *Provider) ResolveDiscussion(ctx context.Context, _ string, _ int, discussionID string) error {
	if discussionID == "" {
		return nil
	}

	// 1. Map comment node_id → parent review thread node_id.
	type threadLookup struct {
		Node struct {
			PullRequestReviewThread struct {
				ID string `json:"id"`
			} `json:"pullRequestReviewThread"`
		} `json:"node"`
	}
	const lookupQuery = `query($commentId: ID!) {
  node(id: $commentId) {
    ... on PullRequestReviewComment {
      pullRequestReviewThread { id }
    }
  }
}`
	var lookup threadLookup
	if err := p.doGraphQL(ctx, lookupQuery, map[string]any{"commentId": discussionID}, &lookup); err != nil {
		return fmt.Errorf("ResolveDiscussion: lookup thread for comment %s: %w", discussionID, err)
	}
	threadID := lookup.Node.PullRequestReviewThread.ID
	if threadID == "" {
		// Comment doesn't belong to a thread (e.g. it's a review-level
		// comment, not an inline comment). Nothing to resolve. Treat as
		// a no-op rather than an error so the caller's best-effort
		// resolve doesn't surface noise.
		return nil
	}

	// 2. Resolve the thread.
	const resolveMutation = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`
	type resolveResp struct {
		ResolveReviewThread struct {
			Thread struct {
				ID         string `json:"id"`
				IsResolved bool   `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	var resolved resolveResp
	if err := p.doGraphQL(ctx, resolveMutation, map[string]any{"threadId": threadID}, &resolved); err != nil {
		return fmt.Errorf("ResolveDiscussion: resolveReviewThread(%s): %w", threadID, err)
	}
	if !resolved.ResolveReviewThread.Thread.IsResolved {
		return fmt.Errorf("ResolveDiscussion: GitHub returned thread %s but isResolved=false", threadID)
	}
	return nil
}

// doGraphQL POSTs a query/mutation to GitHub's GraphQL endpoint and
// decodes the `data` field into `out`. GraphQL-level errors (returned
// in the `errors` array even when HTTP status is 200) surface as a
// non-nil error so the caller can decide.
func (p *Provider) doGraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	endpoint := p.graphQLEndpoint()
	reqBody := map[string]any{"query": query}
	if len(variables) > 0 {
		reqBody["variables"] = variables
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal graphql request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &apiError{Status: resp.StatusCode, Path: "/graphql", Body: string(body)}
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode graphql response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}
	if out != nil && len(envelope.Data) > 0 {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

// graphQLEndpoint converts the REST base URL into the matching GraphQL
// endpoint. github.com REST is `https://api.github.com`, GraphQL is
// `https://api.github.com/graphql`. GHE REST is
// `https://<host>/api/v3`, GraphQL is `https://<host>/api/graphql`.
func (p *Provider) graphQLEndpoint() string {
	if strings.HasSuffix(p.baseURL, "/api/v3") {
		return strings.TrimSuffix(p.baseURL, "/api/v3") + "/api/graphql"
	}
	return p.baseURL + "/graphql"
}

// RetryPipelineJob → POST /repos/{owner}/{repo}/actions/jobs/{job_id}/rerun.
// Works only for GitHub Actions workflow jobs; external CI (Jenkins, Circle)
// emits check_run events but does not support rerun via this endpoint. In
// that case the caller will get a 404 and should fall back to asking the
// author for help.
//
// We don't currently pass through projectID via the interface because
// jobID is globally unique within GitHub; we'd need to thread it through.
// Workaround for now: assume the daemon has one GH project per Run, which
// is true post-trigger because AgentState.ProjectID is set. Callers that
// want this method to work for GH must pass owner/repo via ProjectID.
func (p *Provider) RetryPipelineJob(ctx context.Context, projectID string, jobID int64) error {
	owner, repo, err := splitProjectID(projectID)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/rerun", owner, repo, jobID)
	return p.doJSON(ctx, http.MethodPost, path, nil, nil)
}

// IsBot covers GitHub Apps (Type=Bot) and the trailing `[bot]` username
// convention used by integrations like dependabot, renovate, and codecov.
func (p *Provider) IsBot(u provider.User) bool {
	return u.Bot || strings.HasSuffix(u.Handle, "[bot]")
}

// --- HTTP helpers ---

func (p *Provider) doJSON(ctx context.Context, method, path string, in, out any) error {
	resp, err := p.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return p.errFromResponse(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (p *Provider) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return p.hc.Do(req)
}

type apiError struct {
	Status int
	Path   string
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("github API %s: status=%d body=%s", e.Path, e.Status, e.Body)
}

func (p *Provider) errFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &apiError{
		Status: resp.StatusCode,
		Path:   resp.Request.URL.Path,
		Body:   strings.TrimSpace(string(body)),
	}
}

// splitProjectID accepts "owner/repo" and returns the parts. We deliberately
// keep projectID as a single opaque string at the interface boundary so the
// GitLab path-with-namespace shape and GitHub's owner/repo shape look the
// same to callers. The split happens here, in the provider that knows it
// has two components.
func splitProjectID(projectID string) (owner, repo string, err error) {
	parts := strings.SplitN(projectID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("github: project ID %q must be \"owner/repo\"", projectID)
	}
	return parts[0], parts[1], nil
}

// mapEventKinds translates our normalised EventKind set into the set of
// GitHub event names we need to subscribe to. Multiple provider.EventKind
// values can map onto one GitHub event (e.g. all MR actions ride the
// pull_request event), and vice versa (a single GitHub event may produce
// one of several normalised kinds depending on the action field).
func mapEventKinds(kinds []provider.EventKind) []string {
	set := map[string]bool{}
	for _, k := range kinds {
		switch k {
		case provider.EventNoteAdded:
			set["issue_comment"] = true
			set["pull_request_review"] = true
			set["pull_request_review_comment"] = true
		case provider.EventPipelineSucceeded, provider.EventPipelineFailed:
			set["check_suite"] = true
		case provider.EventMRMerged, provider.EventMRClosed, provider.EventMRUpdated:
			set["pull_request"] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}

// Verify Provider satisfies provider.Provider at compile time.
var _ provider.Provider = (*Provider)(nil)
