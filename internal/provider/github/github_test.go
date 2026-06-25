package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewwormald/everflow/internal/provider"
)

// sign produces a valid X-Hub-Signature-256 value for body+secret.
func sign(body, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_ValidHMAC(t *testing.T) {
	p := &Provider{}
	body := []byte(`{"action":"opened"}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign(string(body), "secret-abc"))
	if !p.VerifySignature(h, body, "secret-abc") {
		t.Errorf("valid HMAC should verify")
	}
}

func TestVerifySignature_BodyTampered(t *testing.T) {
	p := &Provider{}
	original := []byte(`{"action":"opened"}`)
	tampered := []byte(`{"action":"closed"}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign(string(original), "secret-abc"))
	if p.VerifySignature(h, tampered, "secret-abc") {
		t.Errorf("tampered body should not verify (signed body was %q, request body is %q)",
			string(original), string(tampered))
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	p := &Provider{}
	body := []byte(`{}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign(string(body), "secret-abc"))
	if p.VerifySignature(h, body, "different-secret") {
		t.Errorf("different secret should not verify")
	}
}

func TestVerifySignature_EmptyOrMalformed(t *testing.T) {
	p := &Provider{}
	body := []byte(`{}`)

	cases := []struct {
		name   string
		header string
		secret string
	}{
		{"missing header", "", "secret"},
		{"empty secret", sign(string(body), "secret"), ""},
		{"wrong prefix", "md5=" + hex.EncodeToString([]byte("anything")), "secret"},
		{"no prefix", hex.EncodeToString([]byte("anything")), "secret"},
		{"invalid hex", "sha256=ZZZZZZ", "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("X-Hub-Signature-256", tc.header)
			}
			if p.VerifySignature(h, body, tc.secret) {
				t.Errorf("malformed/missing should not verify")
			}
		})
	}
}

func TestSplitProjectID(t *testing.T) {
	cases := []struct {
		in        string
		owner     string
		repo      string
		expectErr bool
	}{
		{"owner/repo", "owner", "repo", false},
		{"andrewwormald/everflow", "andrewwormald", "everflow", false},
		{"", "", "", true},
		{"only-one-part", "", "", true},
		{"owner/", "", "", true},
		{"/repo", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			o, r, err := splitProjectID(tc.in)
			if tc.expectErr {
				if err == nil {
					t.Errorf("want error for %q, got owner=%q repo=%q", tc.in, o, r)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if o != tc.owner || r != tc.repo {
				t.Errorf("split(%q) = (%q, %q); want (%q, %q)", tc.in, o, r, tc.owner, tc.repo)
			}
		})
	}
}

// EventNoteAdded subscription expands to all three GitHub comment events.
func TestMapEventKinds_NoteAddedExpandsToThreeEvents(t *testing.T) {
	got := mapEventKinds([]provider.EventKind{provider.EventNoteAdded})
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	for _, want := range []string{"issue_comment", "pull_request_review", "pull_request_review_comment"} {
		if !set[want] {
			t.Errorf("EventNoteAdded should subscribe to %q; got %v", want, got)
		}
	}
}

func TestMapEventKinds_PipelineKindsCollapseToCheckSuite(t *testing.T) {
	got := mapEventKinds([]provider.EventKind{
		provider.EventPipelineSucceeded,
		provider.EventPipelineFailed,
	})
	if len(got) != 1 || got[0] != "check_suite" {
		t.Errorf("Pipeline kinds should collapse to [check_suite]; got %v", got)
	}
}

func TestMapEventKinds_MRKindsCollapseToPullRequest(t *testing.T) {
	got := mapEventKinds([]provider.EventKind{
		provider.EventMRMerged,
		provider.EventMRClosed,
		provider.EventMRUpdated,
	})
	if len(got) != 1 || got[0] != "pull_request" {
		t.Errorf("MR kinds should collapse to [pull_request]; got %v", got)
	}
}

// TestResolveDiscussion_TwoStepGraphQL exercises the full happy path:
// commentNodeID → thread lookup → resolveReviewThread mutation. The
// fake GitHub responds with the expected GraphQL envelopes for each
// call.
func TestResolveDiscussion_TwoStepGraphQL(t *testing.T) {
	var (
		gotLookup   bool
		gotResolve  bool
		seenComment string
		seenThread  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			t.Errorf("want /graphql, got %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		switch {
		case strings.Contains(req.Query, "pullRequestReviewThread"):
			gotLookup = true
			seenComment, _ = req.Variables["commentId"].(string)
			_, _ = w.Write([]byte(`{"data":{"node":{"pullRequestReviewThread":{"id":"PRRT_thread_xyz"}}}}`))
		case strings.Contains(req.Query, "resolveReviewThread"):
			gotResolve = true
			seenThread, _ = req.Variables["threadId"].(string)
			_, _ = w.Write([]byte(`{"data":{"resolveReviewThread":{"thread":{"id":"PRRT_thread_xyz","isResolved":true}}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, "PRRC_comment_abc"); err != nil {
		t.Errorf("ResolveDiscussion: want nil err, got %v", err)
	}
	if !gotLookup {
		t.Errorf("expected the thread-lookup query to fire")
	}
	if !gotResolve {
		t.Errorf("expected the resolveReviewThread mutation to fire")
	}
	if seenComment != "PRRC_comment_abc" {
		t.Errorf("lookup commentId: want PRRC_comment_abc, got %q", seenComment)
	}
	if seenThread != "PRRT_thread_xyz" {
		t.Errorf("mutation threadId: want PRRT_thread_xyz, got %q", seenThread)
	}
}

// TestResolveDiscussion_EmptyID_NoOp guards the caller contract:
// passing an empty discussionID must not fire any HTTP call.
func TestResolveDiscussion_EmptyID_NoOp(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, ""); err != nil {
		t.Errorf("empty discussionID should be a no-op, got err: %v", err)
	}
	if called {
		t.Errorf("empty discussionID must not trigger an HTTP call")
	}
}

// TestResolveDiscussion_NotAThread covers the case where the lookup
// returns no parent thread (e.g. the comment is a review-level comment,
// not an inline one). Treat as no-op rather than error so the caller's
// best-effort resolve doesn't surface noise.
func TestResolveDiscussion_NotAThread(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":{"node":{"pullRequestReviewThread":null}}}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, "PRRC_xyz"); err != nil {
		t.Errorf("no-thread case: want nil err, got %v", err)
	}
	if calls != 1 {
		t.Errorf("only the lookup should fire when no thread is returned; got %d calls", calls)
	}
}

// TestResolveDiscussion_GraphQLError surfaces GraphQL-level errors as
// Go errors. GitHub returns 200 OK with an `errors` array on permission
// failures and similar — the envelope decoder must promote those.
func TestResolveDiscussion_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"Resource not accessible by integration"}]}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, "PRRC_xyz")
	if err == nil {
		t.Fatalf("want error on GraphQL errors, got nil")
	}
	if !strings.Contains(err.Error(), "Resource not accessible") {
		t.Errorf("err should surface the GraphQL message, got %v", err)
	}
}

// TestGraphQLEndpoint_GHE verifies the GHE path-rewrite logic.
func TestGraphQLEndpoint_GHE(t *testing.T) {
	p := &Provider{baseURL: "https://ghe.example.com/api/v3"}
	if got := p.graphQLEndpoint(); got != "https://ghe.example.com/api/graphql" {
		t.Errorf("GHE: want https://ghe.example.com/api/graphql, got %s", got)
	}
	p2 := &Provider{baseURL: "https://api.github.com"}
	if got := p2.graphQLEndpoint(); got != "https://api.github.com/graphql" {
		t.Errorf("github.com: want https://api.github.com/graphql, got %s", got)
	}
}
