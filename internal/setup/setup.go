// Package setup installs the Claude Code Skill bundle that ADR-0002 decided
// on, so that Claude Code knows when and how to invoke the syntropy binary.
//
// This is deliberately Claude-only for now: ADR-0002 picked the Skill bundle
// as the first integration, not the only one. syntropy's Runner interface
// (ADR-0007) already anticipates other coding agents (Codex, Qwen,
// OpenHands, ...); adding one there needs a companion integration bundle in
// that agent's own distribution format, added alongside this package rather
// than folded into it.
package setup

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed SKILL.md
var skillMD []byte

// KnownRunners lists the coding-agent runners `everflow setup` can offer as
// a default. Mirrors the runners actually registered in main() (ADR-0007) —
// "claude" is the only one today. Kept here rather than introspecting a live
// runner.Registry so setup can validate --runner and auto-select a default
// without constructing runners (and their subprocess dependencies) just to
// list their names.
var KnownRunners = []string{"claude"}

// SkillPath returns the path where the Claude Code Skill bundle lives under
// the given home directory.
func SkillPath(home string) string {
	return filepath.Join(home, ".claude", "skills", "syntropy", "SKILL.md")
}

// EnsureClaudeSkill installs ~/.claude/skills/syntropy/SKILL.md the first
// time it's called on a host that has Claude Code set up (~/.claude/
// present). It's a no-op if Claude Code isn't installed, or if the skill
// file already exists — the file's own presence is the marker, so a user's
// local edits to it are never overwritten by a later invocation. It reports
// whether it installed the file, so callers can surface a one-time summary.
func EnsureClaudeSkill(home string) (bool, error) {
	if _, err := os.Stat(filepath.Join(home, ".claude")); err != nil {
		return false, nil
	}
	return installSkill(home, false)
}

// InstallClaudeSkill installs the Skill bundle for the explicit `syntropy
// setup` command. Unlike EnsureClaudeSkill it doesn't require ~/.claude to
// already exist — an explicit setup request creates it. When force is true,
// an existing Skill file is overwritten; otherwise install is skipped and
// the existing file is left untouched.
func InstallClaudeSkill(home string, force bool) (bool, error) {
	return installSkill(home, force)
}

func installSkill(home string, force bool) (bool, error) {
	skillDir := filepath.Join(home, ".claude", "skills", "syntropy")
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if !force {
		if _, err := os.Stat(skillPath); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(skillPath, skillMD, 0o644); err != nil {
		return false, err
	}
	return true, nil
}
