package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andrewwormald/everflow/internal/provider"
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
