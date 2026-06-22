// Package spec parses everflow spec documents — markdown files with YAML
// frontmatter. A spec is one Run's worth of work: the frontmatter carries
// the structured config (provider, project, runner, base branch, status);
// the markdown body is what the planner reads each iteration to decide the
// next increment.
//
// See ADR-0024 for the rationale (spec = Run; sweep + spec modes coexist).
package spec

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is the parsed shape of a spec file. Frontmatter fields are exported
// for unmarshalling; Body and Path are populated by Parse/ParseBytes.
type Spec struct {
	// Required:
	Goal     string `yaml:"goal"`
	Provider string `yaml:"provider"` // "gitlab" | "github"
	Project  string `yaml:"project"`  // "owner/repo" or path-with-namespace
	Runner   string `yaml:"runner"`   // "claude" | "qwen" | ... — see runner.Registry

	// Optional:
	BaseRepo    string `yaml:"base_repo"`
	BaseBranch  string `yaml:"base_branch"`
	Concurrency int    `yaml:"concurrency"`
	Status      string `yaml:"status"` // "draft" | "ready" | "in_progress" | "compressed"; everflow only acts on "ready"

	// Populated by the parser:
	Body string `yaml:"-"` // markdown body after the frontmatter
	Path string `yaml:"-"` // filesystem path Parse loaded from (empty for ParseBytes)
}

// ErrNoFrontmatter is returned when a spec file doesn't start with the
// "---" delimiter. Frontmatter is required — the planner needs the
// structured config to drive a Run.
var ErrNoFrontmatter = errors.New("spec: missing YAML frontmatter")

// Parse reads a spec from disk.
func Parse(path string) (*Spec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("spec: open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("spec: read %s: %w", path, err)
	}
	s, err := ParseBytes(data)
	if err != nil {
		return nil, err
	}
	s.Path = path
	return s, nil
}

// ParseBytes parses an in-memory spec. Useful for tests + future watch-
// directory ingestion that has the content already.
func ParseBytes(data []byte) (*Spec, error) {
	frontmatter, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := yaml.Unmarshal(frontmatter, &s); err != nil {
		return nil, fmt.Errorf("spec: parse YAML frontmatter: %w", err)
	}
	s.Body = strings.TrimSpace(string(body))
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate checks that the required fields are present. Returns a single
// error joining any missing ones so the user can fix all at once.
func (s *Spec) Validate() error {
	var missing []string
	if strings.TrimSpace(s.Goal) == "" {
		missing = append(missing, "goal")
	}
	if strings.TrimSpace(s.Provider) == "" {
		missing = append(missing, "provider")
	}
	if strings.TrimSpace(s.Project) == "" {
		missing = append(missing, "project")
	}
	if strings.TrimSpace(s.Runner) == "" {
		missing = append(missing, "runner")
	}
	if len(missing) > 0 {
		return fmt.Errorf("spec: missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// IsReady is true when the spec's `status:` is "ready" (or empty —
// treated as ready since the field is optional). Future spec-ingestion
// will use this to decide which specs to Trigger.
func (s *Spec) IsReady() bool {
	st := strings.TrimSpace(strings.ToLower(s.Status))
	return st == "" || st == "ready"
}

// splitFrontmatter extracts the YAML frontmatter from a markdown file.
// The frontmatter is bracketed by "---" on its own line, at the very start
// of the file. Returns ErrNoFrontmatter if the opening delimiter isn't on
// line 1; returns an error if the closing delimiter is missing.
func splitFrontmatter(data []byte) (frontmatter, body []byte, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Spec body can have long lines (long URLs, prompts) — bump the scanner
	// buffer above the 64K default to accommodate.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	if !scanner.Scan() {
		return nil, nil, ErrNoFrontmatter
	}
	if strings.TrimSpace(scanner.Text()) != "---" {
		return nil, nil, ErrNoFrontmatter
	}

	var fm bytes.Buffer
	closed := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		fm.WriteString(line)
		fm.WriteByte('\n')
	}
	if !closed {
		return nil, nil, errors.New("spec: missing closing --- delimiter for frontmatter")
	}

	var bod bytes.Buffer
	for scanner.Scan() {
		bod.WriteString(scanner.Text())
		bod.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("spec: scan: %w", err)
	}
	return fm.Bytes(), bod.Bytes(), nil
}
