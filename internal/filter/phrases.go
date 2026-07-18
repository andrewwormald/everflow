package filter

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// YAMLPhraseSet is a Filter PhraseSet backed by two YAML files:
//   1. Per-Run phrases at ~/.syntropy/runs/<runID>/phrases.yaml — learned
//      via the runner's Learnings.AddPhrases, capped at MaxPerRunEntries
//      (50 — ADR-0018 §4.2).
//   2. Global phrases at ~/.syntropy/phrases.global.yaml — human-curated
//      only; never auto-written.
//
// Contains() checks BOTH sources case-insensitively. Add() appends to the
// per-Run file only (global stays curated).
type YAMLPhraseSet struct {
	perRunPath string
	globalPath string

	mu       sync.RWMutex
	perRun   PhraseFile
	global   PhraseFile
	combined map[string]struct{} // lower-cased phrase → membership
}

// PhraseFile is the YAML shape on disk. Schema version is on file to allow
// future migrations without breaking older daemons.
type PhraseFile struct {
	Version int             `yaml:"version"`
	Phrases []PhraseEntry   `yaml:"phrases"`
}

type PhraseEntry struct {
	Text     string    `yaml:"text"`
	AddedBy  string    `yaml:"added_by,omitempty"`            // "subagent" | "human"
	AddedAt  time.Time `yaml:"added_at,omitempty"`
	AfterMR  int       `yaml:"after_mr,omitempty"`            // MR IID this was learned from
}

// MaxPerRunEntries is the soft cap before everflow surfaces a warning that
// the per-Run list has grown beyond what's likely useful. Hard-coded for
// v1 (ADR-0018 §4.2).
const MaxPerRunEntries = 50

// LoadYAMLPhrases reads both files (missing files are fine — treated as
// empty) and returns a PhraseSet usable by the Filter.
func LoadYAMLPhrases(perRunPath, globalPath string) (*YAMLPhraseSet, error) {
	ps := &YAMLPhraseSet{perRunPath: perRunPath, globalPath: globalPath}
	if err := ps.reload(); err != nil {
		return nil, err
	}
	return ps, nil
}

func (p *YAMLPhraseSet) reload() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, err := readPhraseFile(p.perRunPath)
	if err != nil {
		return fmt.Errorf("phrases: read per-Run %s: %w", p.perRunPath, err)
	}
	gl, err := readPhraseFile(p.globalPath)
	if err != nil {
		return fmt.Errorf("phrases: read global %s: %w", p.globalPath, err)
	}
	p.perRun = pr
	p.global = gl
	p.rebuildIndex()
	return nil
}

func (p *YAMLPhraseSet) rebuildIndex() {
	combined := make(map[string]struct{}, len(p.perRun.Phrases)+len(p.global.Phrases))
	for _, e := range p.perRun.Phrases {
		combined[normalisePhrase(e.Text)] = struct{}{}
	}
	for _, e := range p.global.Phrases {
		combined[normalisePhrase(e.Text)] = struct{}{}
	}
	p.combined = combined
}

func normalisePhrase(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}

// Contains reports whether the given text matches any known phrase
// (per-Run or global), case-insensitively and trim-tolerant.
func (p *YAMLPhraseSet) Contains(text string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.combined[normalisePhrase(text)]
	return ok
}

// All returns the union of per-Run + global phrases. Order is arbitrary.
// Useful for the Starlark `phrases.all()` call and for tests.
func (p *YAMLPhraseSet) All() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.combined))
	for k := range p.combined {
		out = append(out, k)
	}
	return out
}

// Add appends new phrases to the per-Run file (deduplicated against the
// combined view) and persists. Idempotent: phrases already known are
// dropped silently. Returns true if any new phrases were actually added.
//
// Add is the only write path on YAMLPhraseSet — global stays human-
// curated (promote via a future `everflow phrases promote` command).
func (p *YAMLPhraseSet) Add(phrases []string, addedBy string, afterMR int) (added int, err error) {
	if len(phrases) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().UTC()
	for _, raw := range phrases {
		norm := normalisePhrase(raw)
		if norm == "" {
			continue
		}
		if _, dup := p.combined[norm]; dup {
			continue
		}
		p.perRun.Phrases = append(p.perRun.Phrases, PhraseEntry{
			Text:    norm,
			AddedBy: addedBy,
			AddedAt: now,
			AfterMR: afterMR,
		})
		p.combined[norm] = struct{}{}
		added++
	}
	if added == 0 {
		return 0, nil
	}
	if p.perRun.Version == 0 {
		p.perRun.Version = 1
	}
	if err := writePhraseFile(p.perRunPath, p.perRun); err != nil {
		return 0, fmt.Errorf("phrases: write per-Run %s: %w", p.perRunPath, err)
	}
	return added, nil
}

// OverCap reports whether the per-Run list has grown past
// MaxPerRunEntries. Used to surface a one-time warning in the daemon's
// logs / on the MR thread.
func (p *YAMLPhraseSet) OverCap() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.perRun.Phrases) > MaxPerRunEntries
}

// --- file IO ---

func readPhraseFile(path string) (PhraseFile, error) {
	if path == "" {
		return PhraseFile{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PhraseFile{}, nil
		}
		return PhraseFile{}, err
	}
	var pf PhraseFile
	if err := yaml.Unmarshal(b, &pf); err != nil {
		return PhraseFile{}, fmt.Errorf("yaml: %w", err)
	}
	return pf, nil
}

func writePhraseFile(path string, pf PhraseFile) error {
	if path == "" {
		return fmt.Errorf("write: empty path")
	}
	b, err := yaml.Marshal(&pf)
	if err != nil {
		return err
	}
	// Write atomically via tmp + rename so a partial write doesn't corrupt
	// the file. Caller is responsible for the parent dir existing.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
