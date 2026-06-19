package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
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
