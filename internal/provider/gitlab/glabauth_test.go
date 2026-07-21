package gitlab

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeFakeGlabConfig writes a glab config.yml at the OS-specific path
// glabConfigPath() expects, rooted under a fake $HOME, so LoadGlabToken (and
// RefreshGlabToken, which calls it) can find it without touching the real
// user config.
func writeFakeGlabConfig(t *testing.T, home, host, token string) {
	t.Helper()
	var dir string
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(home, "Library", "Application Support", "glab-cli")
	} else {
		dir = filepath.Join(home, ".config", "glab-cli")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "hosts:\n  " + host + ":\n    token: " + token + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// writeFakeGlabBinary writes an executable shell script named "glab" into
// dir and returns dir, so tests can prepend it to $PATH — lets tests
// observe whether RefreshGlabToken actually invokes glab, without touching
// the real CLI or network.
func writeFakeGlabBinary(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "glab")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake glab: %v", err)
	}
	return dir
}

func TestRefreshGlabToken_InvokesGlabThenReadsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFakeGlabConfig(t, home, "gitlab.com", "token-on-disk")

	sentinel := filepath.Join(home, "glab-was-invoked")
	fakeDir := writeFakeGlabBinary(t, `
if [ "$1" = "api" ] && [ "$2" = "user" ]; then
  touch "`+sentinel+`"
fi
exit 0
`)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tok, err := RefreshGlabToken("")
	if err != nil {
		t.Fatalf("RefreshGlabToken: %v", err)
	}
	if tok != "token-on-disk" {
		t.Errorf("token: want %q, got %q", "token-on-disk", tok)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("want glab api user to have been invoked (sentinel missing): %v", err)
	}
}

func TestRefreshGlabToken_PokeFailureDoesNotBlockRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFakeGlabConfig(t, home, "gitlab.com", "still-readable")

	fakeDir := writeFakeGlabBinary(t, "exit 1\n") // poke always fails
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tok, err := RefreshGlabToken("")
	if err != nil {
		t.Fatalf("RefreshGlabToken: want no error even when the poke fails, got %v", err)
	}
	if tok != "still-readable" {
		t.Errorf("token: want %q, got %q", "still-readable", tok)
	}
}

func TestRefreshGlabToken_GlabNotOnPath_StillReadsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFakeGlabConfig(t, home, "gitlab.com", "readable-without-glab")
	t.Setenv("PATH", t.TempDir()) // empty dir — glab genuinely not found

	tok, err := RefreshGlabToken("")
	if err != nil {
		t.Fatalf("RefreshGlabToken: want no error when glab isn't on PATH, got %v", err)
	}
	if tok != "readable-without-glab" {
		t.Errorf("token: want %q, got %q", "readable-without-glab", tok)
	}
}

func TestRefreshGlabToken_NoConfigAtAll_ReturnsErrGlabNotConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeDir := writeFakeGlabBinary(t, "exit 0\n")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := RefreshGlabToken("")
	if err == nil {
		t.Fatal("want an error when no glab config exists at all")
	}
}
