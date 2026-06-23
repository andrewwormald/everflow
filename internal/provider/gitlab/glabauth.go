package gitlab

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
