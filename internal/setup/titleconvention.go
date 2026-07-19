package setup

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ReadRepoConfig reads `.syntropy.yml` from repoDir, returning a zero-value
// RepoConfig (no error) if the file doesn't exist — absence means "no
// convention set", not a failure. Mirrors WriteRepoConfig's file-presence
// convention (ADR-0052).
func ReadRepoConfig(repoDir string) (RepoConfig, error) {
	path := RepoConfigPath(repoDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RepoConfig{}, nil
		}
		return RepoConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg RepoConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return RepoConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// RepoConfig is the on-disk shape of `.syntropy.yml`, a per-repo (not
// per-user) config file living at the root of a spec's `base_repo`.
type RepoConfig struct {
	// TitleConvention is free-text guidance on how this repo likes its
	// PR/MR titles phrased, e.g. "Conventional Commits" or "ticket ID
	// prefix like PROJ-123: ...". Empty means no convention was set.
	TitleConvention string `yaml:"title_convention"`
}

// RepoConfigPath returns the `.syntropy.yml` path for the given repo root.
func RepoConfigPath(repoDir string) string {
	return filepath.Join(repoDir, ".syntropy.yml")
}

// ResolveTitleConvention picks the free-text title convention `syntropy
// setup` should write to `.syntropy.yml`. Precedence: --title-convention
// flag, then (if interactive) the prompt's answer, then empty — a
// non-interactive run with no flag makes no claim about a convention
// rather than guessing one.
//
// prompt is called only when flagConvention is empty and interactive is
// true; it returns the raw line the user typed, or an error reading stdin.
func ResolveTitleConvention(flagConvention string, interactive bool, prompt func() (string, error)) (string, error) {
	if flagConvention != "" {
		return flagConvention, nil
	}
	if !interactive {
		return "", nil
	}
	answer, err := prompt()
	if err != nil {
		return "", fmt.Errorf("read title convention: %w", err)
	}
	return answer, nil
}

// WriteRepoConfig writes `.syntropy.yml` into repoDir with the given title
// convention. It's a no-op (returns false, nil) when convention is empty —
// there's nothing to persist — or when the file already exists and force
// is false, so a user's local edits to it are never clobbered by a later
// `syntropy setup` run.
func WriteRepoConfig(repoDir, convention string, force bool) (bool, error) {
	if convention == "" {
		return false, nil
	}
	path := RepoConfigPath(repoDir)
	if !force {
		if _, err := os.Stat(path); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	data, err := yaml.Marshal(RepoConfig{TitleConvention: convention})
	if err != nil {
		return false, fmt.Errorf("marshal repo config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}
