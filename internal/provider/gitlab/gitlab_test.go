package gitlab

import (
	"net/http"
	"testing"
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
