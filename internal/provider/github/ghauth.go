package github

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// LoadGhToken returns the OAuth token the `gh` CLI is currently using
// for the given host — the same token `gh auth status` shows. Useful
// for spike / personal-laptop deployments where the user has already
// done `gh auth login` and doesn't want to mint a separate PAT.
// Production deployments should still use a service-account PAT via
// GITHUB_TOKEN.
//
// Unlike glab (which stores its token in a plain YAML config we can
// read directly), gh on macOS stores the token in the system keychain
// and exposes it only via the `gh auth token` subcommand. We shell out
// rather than poke at the keychain ourselves — it's the supported
// surface and is portable to Linux/Windows where gh uses different
// backends. Same approach as the rest of the daemon: shell out to host
// tools (git, claude) rather than reimplement them.
//
// Returns ErrGhNotConfigured if `gh` isn't on $PATH, isn't logged in
// for the given host, or returned an empty token. Callers can fall
// through to a GITHUB_TOKEN env var.
func LoadGhToken(host string) (token string, err error) {
	if host == "" {
		host = "github.com"
	}
	return loadGhToken(host)
}

// loadGhToken is the package-level variable form so tests can stub
// out the exec without needing gh installed on the test machine.
var loadGhToken = func(host string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", ErrGhNotConfigured
	}
	cmd := exec.Command("gh", "auth", "token", "--hostname", host)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// gh exits non-zero when not logged in for that host. Treat as
		// "not configured" rather than a hard error so the caller can
		// fall through to env-var PAT.
		if strings.Contains(stderr.String(), "not logged into") ||
			strings.Contains(stderr.String(), "no oauth token") {
			return "", ErrGhNotConfigured
		}
		return "", fmt.Errorf("gh auth token: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	tok := strings.TrimSpace(stdout.String())
	if tok == "" {
		return "", ErrGhNotConfigured
	}
	return tok, nil
}

// ErrGhNotConfigured signals that `gh` is unavailable or not logged
// into the requested host. Caller can fall through to a GITHUB_TOKEN
// env var.
var ErrGhNotConfigured = errors.New("gh: no token configured for host")
