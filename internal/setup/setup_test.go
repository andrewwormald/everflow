package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureClaudeSkill_NoClaudeDir(t *testing.T) {
	home := t.TempDir()
	installed, err := EnsureClaudeSkill(home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installed {
		t.Fatalf("expected no install without a .claude dir")
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

	installed, err := EnsureClaudeSkill(home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !installed {
		t.Fatalf("expected install to report true")
	}

	skillPath := filepath.Join(home, ".claude", "skills", "syntropy", "SKILL.md")
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
	skillDir := filepath.Join(home, ".claude", "skills", "syntropy")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	custom := []byte("user's own customized skill")
	if err := os.WriteFile(skillPath, custom, 0o644); err != nil {
		t.Fatal(err)
	}

	installed, err := EnsureClaudeSkill(home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installed {
		t.Fatalf("expected no install when the skill file already exists")
	}

	got, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(custom) {
		t.Fatalf("expected existing skill file to be left untouched, got %q", got)
	}
}

func TestInstallClaudeSkill_NoClaudeDirYet(t *testing.T) {
	home := t.TempDir()

	installed, err := InstallClaudeSkill(home, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !installed {
		t.Fatalf("expected explicit setup to install even without a pre-existing .claude dir")
	}
	if _, err := os.Stat(SkillPath(home)); err != nil {
		t.Fatalf("skill file not written: %v", err)
	}
}

func TestInstallClaudeSkill_ForceOverwrites(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".claude", "skills", "syntropy")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("stale custom content"), 0o644); err != nil {
		t.Fatal(err)
	}

	installed, err := InstallClaudeSkill(home, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installed {
		t.Fatalf("expected no overwrite without --force")
	}

	installed, err = InstallClaudeSkill(home, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !installed {
		t.Fatalf("expected --force to overwrite the existing skill file")
	}
	got, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(skillMD) {
		t.Fatalf("force install did not write current embedded SKILL.md")
	}
}
