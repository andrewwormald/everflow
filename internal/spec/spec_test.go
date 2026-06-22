package spec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBytes_Happy(t *testing.T) {
	in := []byte(`---
goal: Migrate logrus to log/slog across services
provider: gitlab
project: lunomoney/core
runner: claude
base_branch: main
base_repo: /home/everflow/repos/core
status: ready
---
# Migration plan

Replace ` + "`github.com/sirupsen/logrus`" + ` imports with the stdlib
` + "`log/slog`" + ` across all Go services. Preserve log levels.

## Constraints

- One service per MR
- Tests must pass
`)
	s, err := ParseBytes(in)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if s.Goal != "Migrate logrus to log/slog across services" {
		t.Errorf("Goal: got %q", s.Goal)
	}
	if s.Provider != "gitlab" {
		t.Errorf("Provider: got %q", s.Provider)
	}
	if s.Project != "lunomoney/core" {
		t.Errorf("Project: got %q", s.Project)
	}
	if s.Runner != "claude" {
		t.Errorf("Runner: got %q", s.Runner)
	}
	if s.BaseBranch != "main" {
		t.Errorf("BaseBranch: got %q", s.BaseBranch)
	}
	if s.BaseRepo != "/home/everflow/repos/core" {
		t.Errorf("BaseRepo: got %q", s.BaseRepo)
	}
	if s.Status != "ready" {
		t.Errorf("Status: got %q", s.Status)
	}
	if !strings.Contains(s.Body, "Migration plan") {
		t.Errorf("Body should contain markdown content; got:\n%s", s.Body)
	}
	if !strings.Contains(s.Body, "log/slog") {
		t.Errorf("Body should preserve inline content; got:\n%s", s.Body)
	}
	if !s.IsReady() {
		t.Errorf("IsReady should be true for status=ready")
	}
}

func TestParseBytes_NoFrontmatter(t *testing.T) {
	in := []byte("# Just a markdown file\n\nNo YAML at the top.\n")
	_, err := ParseBytes(in)
	if !errors.Is(err, ErrNoFrontmatter) {
		t.Fatalf("want ErrNoFrontmatter, got %v", err)
	}
}

func TestParseBytes_UnclosedFrontmatter(t *testing.T) {
	in := []byte("---\ngoal: x\nprovider: gitlab\n\nbody starts here without closing ---\n")
	_, err := ParseBytes(in)
	if err == nil {
		t.Fatalf("want error for missing closing delimiter")
	}
	if !strings.Contains(err.Error(), "closing") {
		t.Errorf("error should mention closing delimiter; got %v", err)
	}
}

func TestParseBytes_MalformedYAML(t *testing.T) {
	in := []byte("---\ngoal: x\n  : not a real key\n---\nbody\n")
	_, err := ParseBytes(in)
	if err == nil {
		t.Fatalf("want error for malformed YAML")
	}
}

func TestValidate_RequiredFields(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantField string
	}{
		{"missing goal", "provider: gitlab\nproject: x/y\nrunner: claude\n", "goal"},
		{"missing provider", "goal: x\nproject: x/y\nrunner: claude\n", "provider"},
		{"missing project", "goal: x\nprovider: gitlab\nrunner: claude\n", "project"},
		{"missing runner", "goal: x\nprovider: gitlab\nproject: x/y\n", "runner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte("---\n" + tc.yaml + "---\nbody\n")
			_, err := ParseBytes(in)
			if err == nil {
				t.Fatalf("want validation error")
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error should mention %q; got %v", tc.wantField, err)
			}
		})
	}
}

func TestValidate_MultipleMissing(t *testing.T) {
	in := []byte("---\ngoal: x\n---\nbody\n")
	_, err := ParseBytes(in)
	if err == nil {
		t.Fatalf("want validation error")
	}
	// Should list all three missing fields in one error.
	for _, f := range []string{"provider", "project", "runner"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error should list %q; got %v", f, err)
		}
	}
}

func TestIsReady(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"ready", true},
		{"READY", true},
		{"", true}, // empty = ready (status is optional)
		{"draft", false},
		{"in_progress", false},
		{"compressed", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			s := &Spec{Status: tc.status}
			if got := s.IsReady(); got != tc.want {
				t.Errorf("IsReady(%q) = %v; want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestParse_ReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "migrate.md")
	content := `---
goal: Migrate
provider: github
project: andrewwormald/everflow
runner: claude
---
Body content here.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Path != path {
		t.Errorf("Path: want %q, got %q", path, s.Path)
	}
	if s.Goal != "Migrate" {
		t.Errorf("Goal: got %q", s.Goal)
	}
}

func TestParse_FileNotFound(t *testing.T) {
	_, err := Parse("/no/such/spec.md")
	if err == nil {
		t.Fatalf("want error for missing file")
	}
}
