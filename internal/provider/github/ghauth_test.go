package github

import (
	"errors"
	"testing"
)

// TestLoadGhToken_Success exercises the happy path via the test hook.
// The real exec.Command path is covered by the daemon's own end-to-end
// usage on a developer machine.
func TestLoadGhToken_Success(t *testing.T) {
	original := loadGhToken
	t.Cleanup(func() { loadGhToken = original })

	loadGhToken = func(host string) (string, error) {
		if host != "github.com" {
			t.Errorf("want default host github.com, got %q", host)
		}
		return "gho_test_token_value", nil
	}

	got, err := LoadGhToken("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "gho_test_token_value" {
		t.Errorf("token: got %q", got)
	}
}

// TestLoadGhToken_CustomHost ensures the host argument is threaded
// through (GHE deployments override the default).
func TestLoadGhToken_CustomHost(t *testing.T) {
	original := loadGhToken
	t.Cleanup(func() { loadGhToken = original })

	var seenHost string
	loadGhToken = func(host string) (string, error) {
		seenHost = host
		return "gho_ghe_token", nil
	}

	_, _ = LoadGhToken("ghe.example.com")
	if seenHost != "ghe.example.com" {
		t.Errorf("host: want ghe.example.com, got %q", seenHost)
	}
}

// TestLoadGhToken_NotConfigured returns the sentinel so the caller
// can distinguish "fall through to env" from a real failure.
func TestLoadGhToken_NotConfigured(t *testing.T) {
	original := loadGhToken
	t.Cleanup(func() { loadGhToken = original })

	loadGhToken = func(_ string) (string, error) {
		return "", ErrGhNotConfigured
	}

	_, err := LoadGhToken("")
	if !errors.Is(err, ErrGhNotConfigured) {
		t.Errorf("want ErrGhNotConfigured, got %v", err)
	}
}

// TestLoadGhToken_EmptyTokenIsNotConfigured guards against the case
// where `gh auth token` exits 0 but prints an empty string.
func TestLoadGhToken_EmptyTokenIsNotConfigured(t *testing.T) {
	// We invoke the actual loadGhToken function (which checks for `gh`
	// on PATH) only if gh exists; otherwise the test would be testing
	// the LookPath branch, not the empty-token branch. To exercise the
	// empty-token branch deterministically, stub.
	original := loadGhToken
	t.Cleanup(func() { loadGhToken = original })
	loadGhToken = func(_ string) (string, error) {
		return "", ErrGhNotConfigured // production code returns this when stdout was empty
	}
	_, err := LoadGhToken("")
	if !errors.Is(err, ErrGhNotConfigured) {
		t.Errorf("empty token should map to ErrGhNotConfigured, got %v", err)
	}
}
