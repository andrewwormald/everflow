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
// Polling not wired for GitHub in v1 — this is a parity stub that fails
// with a clear error.
func (p *Provider) GetMRState(_ context.Context, _ string, _ int) (string, error) {
	return "", fmt.Errorf("github: polling not yet implemented (v1 uses webhooks for GitHub; use GitLab for the polling path)")
}

// ListNotesSince — see GetMRState. Polling not wired for GitHub in v1.
func (p *Provider) ListNotesSince(_ context.Context, _ string, _ int, _ int64) ([]provider.NotePoll, error) {
	return nil, fmt.Errorf("github: polling not yet implemented (v1 uses webhooks for GitHub; use GitLab for the polling path)")
}

// ResolveDiscussion is a no-op stub for GitHub in v1. The real implementation
// would call GraphQL `resolveReviewThread` against the thread node ID surfaced
// via the pull-request review-thread API. For now invokeForEvent's auto-resolve
// after a successful push silently does nothing on GitHub — the reviewer
// resolves manually, same as before this feature shipped.
func (p *Provider) ResolveDiscussion(_ context.Context, _ string, _ int, _ string) error {
	return nil
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
