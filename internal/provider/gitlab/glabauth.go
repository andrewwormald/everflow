package gitlab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"gopkg.in/yaml.v3"
)

// LoadGlabToken reads the OAuth token from the `glab` CLI's config file —
// the same token `glab auth status` reports. Returns it as a Bearer token
// (AuthMode=AuthBearer) so callers can build a Provider that piggybacks
// on the user's interactive `glab auth login`. ErrGlabNotConfigured if
// the file or the gitlab.com section is missing.
//
// Useful for spike / personal-laptop deployments where the user has
// already done `glab auth login` and doesn't want to mint a separate PAT.
// Production deployments should still use a service-account PAT via env.
func LoadGlabToken(host string) (token string, err error) {
	if host == "" {
		host = "gitlab.com"
	}
	path, err := glabConfigPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrGlabNotConfigured
		}
		return "", fmt.Errorf("glab config: %w", err)
	}

	// The file looks like:
	//   hosts:
	//     gitlab.com:
	//       token: <value>
	//       ...
	var doc struct {
		Hosts map[string]struct {
			Token string `yaml:"token"`
		} `yaml:"hosts"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("glab config parse: %w", err)
	}
	h, ok := doc.Hosts[host]
	if !ok || h.Token == "" {
		return "", ErrGlabNotConfigured
	}
	return h.Token, nil
}

// glabRefreshPokeTimeout bounds RefreshGlabToken's call to the glab CLI —
// a real network round trip (to gitlab.com or a self-hosted instance), not
// a purely local check, so it needs a real timeout rather than blocking
// indefinitely if the network is unreachable.
const glabRefreshPokeTimeout = 10 * time.Second

// RefreshGlabToken forces `glab` to run its own internal access-token
// refresh (via the refresh token it manages, stored alongside the access
// token in its config file) before reading whatever token ends up on disk
// via LoadGlabToken.
//
// glab's OAuth access token is short-lived and only refreshed lazily, when
// something actually invokes glab — reading the config file directly
// (LoadGlabToken alone) can return a genuinely expired access token if
// nothing has triggered glab's own refresh recently, even though `glab
// auth status` would report a healthy login the moment it's run (ADR-0065).
// `glab api user` is used as the poke: a real authenticated API call,
// confirmed (by direct testing) to trigger glab's refresh-if-needed logic
// before it succeeds.
//
// The poke is best-effort: if it fails (glab not on PATH, a genuine
// re-login requirement, a transient network blip), this doesn't return
// early — LoadGlabToken still runs and its result (or lack of one) is
// what the caller ultimately sees, same as if RefreshGlabToken hadn't
// poked at all. A failed poke isn't proof the token is unusable; a
// successful poke doesn't guarantee LoadGlabToken succeeds either
// (e.g. the host section could still be missing) — this only maximises
// the chance the token on disk is current when read.
func RefreshGlabToken(host string) (token string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), glabRefreshPokeTimeout)
	defer cancel()
	_ = exec.CommandContext(ctx, "glab", "api", "user").Run()
	return LoadGlabToken(host)
}

// ErrGlabNotConfigured signals that the glab config file is missing the
// requested host, or doesn't exist at all. Caller can fall through to an
// env-var PAT.
var ErrGlabNotConfigured = errors.New("glab: no token configured for host")

// glabConfigPath returns the OS-specific path to glab's config.yml.
// macOS: ~/Library/Application Support/glab-cli/config.yml
// Linux / other: ~/.config/glab-cli/config.yml
func glabConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "glab-cli", "config.yml"), nil
	}
	return filepath.Join(home, ".config", "glab-cli", "config.yml"), nil
}
