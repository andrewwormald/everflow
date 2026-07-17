package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureClaudeSkill_NoClaudeDir(t *testing.T) {
	home := t.TempDir()
	if err := EnsureClaudeSkill(home); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("expected no .claude dir to be created, got err=%v", err)
	}
}

func TestEnsureClaudeSkill_Installs(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := EnsureClaudeSkill(home); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	skillPath := filepath.Join(home, ".claude", "skills", "everflow", "SKILL.md")
	got, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("skill file not written: %v", err)
	}
	if string(got) != string(skillMD) {
		t.Fatalf("written content does not match embedded SKILL.md")
	}
}

func TestEnsureClaudeSkill_DoesNotOverwrite(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".claude", "skills", "everflow")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	custom := []byte("user's own customized skill")
	if err := os.WriteFile(skillPath, custom, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureClaudeSkill(home); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(custom) {
		t.Fatalf("expected existing skill file to be left untouched, got %q", got)
	}
}
