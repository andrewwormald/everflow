package filter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLPhrases_MissingFilesIsFine(t *testing.T) {
	dir := t.TempDir()
	ps, err := LoadYAMLPhrases(
		filepath.Join(dir, "absent-perrun.yaml"),
		filepath.Join(dir, "absent-global.yaml"),
	)
	if err != nil {
		t.Fatalf("missing files should not error: %v", err)
	}
	if ps.Contains("lgtm") {
		t.Errorf("empty PhraseSet should not contain anything")
	}
	if len(ps.All()) != 0 {
		t.Errorf("All() should be empty; got %v", ps.All())
	}
}

func TestPhraseSet_AddAndPersist(t *testing.T) {
	dir := t.TempDir()
	perRun := filepath.Join(dir, "phrases.yaml")
	global := filepath.Join(dir, "phrases.global.yaml")

	ps1, err := LoadYAMLPhrases(perRun, global)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	added, err := ps1.Add([]string{"lgtm", "looks good", "👍"}, "subagent", 42)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added != 3 {
		t.Errorf("added: want 3, got %d", added)
	}
	if !ps1.Contains("LGTM") {
		t.Errorf("Contains should be case-insensitive")
	}
	if !ps1.Contains("  lgtm  ") {
		t.Errorf("Contains should trim whitespace")
	}

	// Reload from disk; everything should still be there.
	ps2, err := LoadYAMLPhrases(perRun, global)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !ps2.Contains("lgtm") || !ps2.Contains("looks good") {
		t.Errorf("phrases not persisted across reload; got %v", ps2.All())
	}

	// Add the same phrases again — should be a no-op (dedupe).
	added, _ = ps2.Add([]string{"lgtm", "LGTM", " looks good "}, "subagent", 43)
	if added != 0 {
		t.Errorf("re-adding existing phrases should not count; got added=%d", added)
	}
}

func TestPhraseSet_GlobalContributesButIsNotWrittenByAdd(t *testing.T) {
	// Pre-populate the global file by hand; verify per-Run Add doesn't
	// touch it.
	dir := t.TempDir()
	perRun := filepath.Join(dir, "phrases.yaml")
	global := filepath.Join(dir, "phrases.global.yaml")
	if err := os.WriteFile(global, []byte("version: 1\nphrases:\n  - text: thanks\n"), 0o644); err != nil {
		t.Fatalf("seed global: %v", err)
	}

	ps, _ := LoadYAMLPhrases(perRun, global)
	if !ps.Contains("thanks") {
		t.Errorf("global phrase should be reachable via Contains")
	}

	_, _ = ps.Add([]string{"lgtm"}, "subagent", 1)
	// Global file should be untouched.
	got, _ := os.ReadFile(global)
	if !contains(string(got), "thanks") || contains(string(got), "lgtm") {
		t.Errorf("Add should not modify global; got:\n%s", got)
	}
}

func TestPhraseSet_OverCap(t *testing.T) {
	dir := t.TempDir()
	ps, _ := LoadYAMLPhrases(filepath.Join(dir, "p.yaml"), "")
	if ps.OverCap() {
		t.Errorf("empty PhraseSet should not be over cap")
	}
	// Add MaxPerRunEntries + 1 distinct phrases.
	phrases := make([]string, MaxPerRunEntries+1)
	for i := range phrases {
		phrases[i] = "phrase-" + itoa(i)
	}
	if _, err := ps.Add(phrases, "subagent", 1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !ps.OverCap() {
		t.Errorf("should be over cap after %d adds", MaxPerRunEntries+1)
	}
}

// --- helpers ---

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
