// Package config reads and writes everflow's per-user config file,
// ~/.everflow/config.yaml. It's the persisted counterpart to the
// interactive choices `everflow setup` walks a user through (ADR-0051):
// which runner and model to use by default when a spec doesn't pin its
// own.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk shape of ~/.everflow/config.yaml.
type Config struct {
	// Runner is the default coding-agent runner name (see runner.Registry),
	// e.g. "claude". Empty means no default has been set yet.
	Runner string `yaml:"runner"`
	// Model is the default model override passed to the runner, e.g.
	// "claude-sonnet-5". Empty means the runner's own default.
	Model string `yaml:"model"`
}

// Path returns the config file location under the given home directory.
func Path(home string) string {
	return filepath.Join(home, ".everflow", "config.yaml")
}

// Load reads the config file. A missing file is not an error — it
// returns the zero value, since a user who hasn't run `everflow setup`
// yet simply has no persisted defaults.
func Load(home string) (Config, error) {
	data, err := os.ReadFile(Path(home))
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", Path(home), err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", Path(home), err)
	}
	return cfg, nil
}

// Save writes cfg to the config file, creating ~/.everflow if needed and
// overwriting any existing file.
func Save(home string, cfg Config) error {
	dir := filepath.Join(home, ".everflow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(Path(home), data, 0o644); err != nil {
		return fmt.Errorf("config: write %s: %w", Path(home), err)
	}
	return nil
}
