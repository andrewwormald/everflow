package gitlab

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andrewwormald/syntropy/internal/provider"
)

func TestVerifySignature(t *testing.T) {
	p := &Provider{}
	body := []byte(`{"any":"thing"}`)

	tests := []struct {
		name   string
		header string
		secret string
		want   bool
	}{
		{"matching token", "secret-abc", "secret-abc", true},
		{"mismatched token", "wrong", "secret-abc", false},
		{"empty header", "", "secret-abc", false},
		{"empty secret", "secret-abc", "", false},
		{"both empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("X-Gitlab-Token", tc.header)
			}
			if got := p.VerifySignature(h, body, tc.secret); got != tc.want {
				t.Errorf("VerifySignature: want %v, got %v", tc.want, got)
			}
		})
	}
}

// TestVerifySignature_BodyIgnored documents the GitLab quirk: the body is
// not part of the signature input (unlike GitHub's HMAC). Same token, any
// body, same result. ADR-0020 §2 records this trade-off.
func TestVerifySignature_BodyIgnored(t *testing.T) {
	p := &Provider{}
	h := http.Header{}
	h.Set("X-Gitlab-Token", "secret")
	if !p.VerifySignature(h, []byte("body A"), "secret") {
		t.Errorf("body A should verify")
	}
	if !p.VerifySignature(h, []byte("entirely different body B"), "secret") {
		t.Errorf("body B should also verify — GitLab does not sign the body")
	}
}

func TestCreateMR_DraftPrefix(t *testing.T) {
	// We can't hit a real GitLab; assert the title-prefix logic via the
	// body assembly. Reuses the http.Client interception pattern by
	// pointing the Provider at a test server.
	var seenTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// httptest decodes URL paths; check the suffix only.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenTitle, _ = body["title"].(string)
		_, _ = w.Write([]byte(`{"iid":1,"web_url":"https://gitlab/x/-/merge_requests/1"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})

	// Without Draft → title unchanged
	_, _ = p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "Migrate logger",
	})
	if seenTitle != "Migrate logger" {
		t.Errorf("plain title: want %q, got %q", "Migrate logger", seenTitle)
	}

	// With Draft → prefix added
	_, _ = p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "Migrate logger", Draft: true,
	})
	if seenTitle != "Draft: Migrate logger" {
		t.Errorf("draft title: want %q, got %q", "Draft: Migrate logger", seenTitle)
	}

	// Already-prefixed → not double-prefixed
	_, _ = p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "Draft: already", Draft: true,
	})
	if seenTitle != "Draft: already" {
		t.Errorf("double-draft: want %q, got %q", "Draft: already", seenTitle)
	}
}

func TestReactToNote(t *testing.T) {
	var gotPath, gotMethod, gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotName, _ = body["name"].(string)
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ReactToNote(t.Context(), "owner/repo", 42, 99, streamNote, "eyes"); err != nil {
		t.Fatalf("ReactToNote: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	// httptest decodes URL paths, so the escaped "/" in the project ID
	// comes through unescaped here.
	if gotPath != "/api/v4/projects/owner/repo/merge_requests/42/notes/99/award_emoji" {
		t.Errorf("path: got %s", gotPath)
	}
	if gotName != "eyes" {
		t.Errorf("name: want eyes, got %s", gotName)
	}
}

func TestReplyToDiscussion(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotBody, _ = body["body"].(string)
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ReplyToDiscussion(t.Context(), "owner/repo", 42, "disc-abc", "fixed in latest push"); err != nil {
		t.Fatalf("ReplyToDiscussion: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	// httptest decodes URL paths, so the escaped "/" in the project ID
	// comes through unescaped here.
	if gotPath != "/api/v4/projects/owner/repo/merge_requests/42/discussions/disc-abc/notes" {
		t.Errorf("path: got %s", gotPath)
	}
	if gotBody != "fixed in latest push" {
		t.Errorf("body: want %q, got %q", "fixed in latest push", gotBody)
	}
}

// --- TokenSource tests (ADR-0063: don't cache a token that can go stale) ---

func TestNew_RequiresTokenOrTokenSource(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error when neither Token nor TokenSource is set")
	}
	if _, err := New(Config{Token: "t"}); err != nil {
		t.Errorf("Token alone should be sufficient: %v", err)
	}
	if _, err := New(Config{TokenSource: func() (string, error) { return "t", nil }}); err != nil {
		t.Errorf("TokenSource alone should be sufficient: %v", err)
	}
}

func TestDo_TokenSource_ResolvedFreshOnEveryRequest(t *testing.T) {
	var gotAuthHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeaders = append(gotAuthHeaders, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"id":1,"username":"andreww"}`))
	}))
	defer srv.Close()

	calls := 0
	tokens := []string{"first-token", "refreshed-token"}
	p, err := New(Config{
		BaseURL: srv.URL,
		TokenSource: func() (string, error) {
			tok := tokens[calls]
			calls++
			return tok, nil
		},
		AuthMode: AuthBearer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.AuthenticatedUser(t.Context()); err != nil {
		t.Fatalf("AuthenticatedUser (1st): %v", err)
	}
	if _, err := p.AuthenticatedUser(t.Context()); err != nil {
		t.Fatalf("AuthenticatedUser (2nd): %v", err)
	}

	if len(gotAuthHeaders) != 2 {
		t.Fatalf("want 2 requests, got %d", len(gotAuthHeaders))
	}
	if gotAuthHeaders[0] != "Bearer first-token" {
		t.Errorf("1st request: want %q, got %q", "Bearer first-token", gotAuthHeaders[0])
	}
	if gotAuthHeaders[1] != "Bearer refreshed-token" {
		t.Errorf("2nd request: want %q, got %q — TokenSource should be re-resolved every request, not cached", "Bearer refreshed-token", gotAuthHeaders[1])
	}
}

func TestDo_TokenSource_TakesPrecedenceOverStaticToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":1,"username":"andreww"}`))
	}))
	defer srv.Close()

	p, err := New(Config{
		BaseURL:     srv.URL,
		Token:       "stale-static-token",
		TokenSource: func() (string, error) { return "fresh-token", nil },
		AuthMode:    AuthBearer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.AuthenticatedUser(t.Context()); err != nil {
		t.Fatalf("AuthenticatedUser: %v", err)
	}
	if gotAuth != "Bearer fresh-token" {
		t.Errorf("want TokenSource to win over a stale static Token; got %q", gotAuth)
	}
}

func TestDo_TokenSource_ErrorPropagates(t *testing.T) {
	p, err := New(Config{
		BaseURL:     "http://unused.invalid",
		TokenSource: func() (string, error) { return "", errors.New("glab config: no such file") },
		AuthMode:    AuthBearer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.AuthenticatedUser(t.Context()); err == nil {
		t.Fatal("want error when TokenSource fails, got nil")
	}
}
