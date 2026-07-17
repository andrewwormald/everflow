// Package setup installs the Claude Code Skill bundle that ADR-0002 decided
// on, so that Claude Code knows when and how to invoke the everflow binary.
package setup

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed SKILL.md
var skillMD []byte

// EnsureClaudeSkill installs ~/.claude/skills/everflow/SKILL.md the first
// time it's called on a host that has Claude Code set up (~/.claude/
// present). It's a no-op if Claude Code isn't installed, or if the skill
// file already exists — the file's own presence is the marker, so a user's
// local edits to it are never overwritten by a later invocation.
func EnsureClaudeSkill(home string) error {
	if _, err := os.Stat(filepath.Join(home, ".claude")); err != nil {
		return nil
	}

	skillDir := filepath.Join(home, ".claude", "skills", "everflow")
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if _, err := os.Stat(skillPath); err == nil || !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(skillPath, skillMD, 0o644)
}
