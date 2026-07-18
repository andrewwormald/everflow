package filter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewwormald/syntropy/internal/provider"
)

// writeFilter creates a .star file in t.TempDir() with the given content
// and returns its path.
func writeFilter(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "filter.star")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write filter: %v", err)
	}
	return path
}

// noPhrases is an empty PhraseSet for cases where the filter shouldn't be
// consulting phrases anyway.
type emptyPhraseSet struct{}

func (emptyPhraseSet) Contains(string) bool { return false }
func (emptyPhraseSet) All() []string        { return nil }

func TestStarlarkFilter_DefaultBuiltIn_SkipsLgtm(t *testing.T) {
	// Write the embedded default filter to a temp file and evaluate it
	// against an "lgtm" comment. With "lgtm" in the phrase list, it skips.
	path := writeFilter(t, string(DefaultStarlark()))
	f := NewStarlarkFilter(path)

	// Phrases preloaded with "lgtm".
	pdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pdir, "p.yaml"),
		[]byte("version: 1\nphrases:\n  - text: lgtm\n"), 0o644); err != nil {
		t.Fatalf("write phrases: %v", err)
	}
	ps, _ := LoadYAMLPhrases(filepath.Join(pdir, "p.yaml"), "")

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		Note:   provider.Note{Body: "lgtm"},
		Author: provider.User{Handle: "reviewer"},
	}
	out, err := f.Eval(ev, nil, ps)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if out != OutcomeSkip {
		t.Errorf("want OutcomeSkip on known phrase 'lgtm'; got %v", out)
	}
}

func TestStarlarkFilter_DefaultBuiltIn_InvokesSubagentOnRealComment(t *testing.T) {
	path := writeFilter(t, string(DefaultStarlark()))
	f := NewStarlarkFilter(path)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		Note:   provider.Note{Body: "please rename Foo to Bar and add a deprecation note"},
		Author: provider.User{Handle: "reviewer"},
	}
	out, _ := f.Eval(ev, nil, emptyPhraseSet{})
	if out != OutcomeInvokeSubagent {
		t.Errorf("substantive comment should InvokeSubagent; got %v", out)
	}
}

func TestStarlarkFilter_DefaultBuiltIn_SkipsBots(t *testing.T) {
	path := writeFilter(t, string(DefaultStarlark()))
	f := NewStarlarkFilter(path)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		Note:   provider.Note{Body: "Dependabot has opened a PR ..."},
		Author: provider.User{Handle: "dependabot[bot]", Bot: true},
		IsBot:  true,
	}
	out, _ := f.Eval(ev, nil, emptyPhraseSet{})
	if out != OutcomeSkip {
		t.Errorf("bot comments should skip in default filter; got %v", out)
	}
}

func TestStarlarkFilter_DefaultBuiltIn_PipelineFailedInvokes(t *testing.T) {
	path := writeFilter(t, string(DefaultStarlark()))
	f := NewStarlarkFilter(path)

	ev := provider.Event{
		Kind:     provider.EventPipelineFailed,
		Pipeline: provider.Pipeline{ID: 1, Status: "failed"},
	}
	out, _ := f.Eval(ev, nil, emptyPhraseSet{})
	if out != OutcomeInvokeSubagent {
		t.Errorf("pipeline_failed should InvokeSubagent in default; got %v", out)
	}
}

func TestStarlarkFilter_DefaultBuiltIn_SkipsShortAscii(t *testing.T) {
	// "ok" (2 bytes) falls under the length-3 short-circuit. (Multi-byte
	// emoji like 👍 have len >= 4 — those need to land in the phrase list
	// to be skipped. Known limitation.)
	path := writeFilter(t, string(DefaultStarlark()))
	f := NewStarlarkFilter(path)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		Note:   provider.Note{Body: "ok"},
		Author: provider.User{Handle: "reviewer"},
	}
	out, _ := f.Eval(ev, nil, emptyPhraseSet{})
	if out != OutcomeSkip {
		t.Errorf("short ASCII should skip; got %v", out)
	}
}

func TestStarlarkFilter_CustomFilter_PauseOutcome(t *testing.T) {
	// A custom filter that returns "pause" on any pipeline failure.
	path := writeFilter(t, `
def filter(event, state, phrases):
    if event["kind"] == "pipeline_failed":
        return "pause"
    return "invoke_subagent"
`)
	f := NewStarlarkFilter(path)
	ev := provider.Event{Kind: provider.EventPipelineFailed}
	out, _ := f.Eval(ev, nil, emptyPhraseSet{})
	if out != OutcomePause {
		t.Errorf("custom filter should return OutcomePause; got %v", out)
	}
}

func TestStarlarkFilter_MissingFile_Errors(t *testing.T) {
	f := NewStarlarkFilter("/no/such/file.star")
	_, err := f.Eval(provider.Event{}, nil, emptyPhraseSet{})
	if err == nil {
		t.Fatalf("want error for missing file")
	}
}

func TestStarlarkFilter_MissingFilterFunction_Errors(t *testing.T) {
	path := writeFilter(t, "# no filter() defined\nx = 1\n")
	f := NewStarlarkFilter(path)
	_, err := f.Eval(provider.Event{Kind: provider.EventNoteAdded}, nil, emptyPhraseSet{})
	if err == nil {
		t.Fatalf("want error when filter() is missing")
	}
}

func TestStarlarkFilter_SyntaxError(t *testing.T) {
	path := writeFilter(t, "def filter(:\n")
	f := NewStarlarkFilter(path)
	_, err := f.Eval(provider.Event{}, nil, emptyPhraseSet{})
	if err == nil {
		t.Fatalf("want error on syntax error")
	}
}

func TestStarlarkFilter_UnknownOutcomeString(t *testing.T) {
	path := writeFilter(t, `
def filter(event, state, phrases):
    return "burrito"
`)
	f := NewStarlarkFilter(path)
	_, err := f.Eval(provider.Event{Kind: provider.EventNoteAdded}, nil, emptyPhraseSet{})
	if err == nil {
		t.Fatalf("want error on unknown outcome string")
	}
}

func TestStarlarkFilter_StateAccessible(t *testing.T) {
	// Custom filter that reads state and routes based on it.
	path := writeFilter(t, `
def filter(event, state, phrases):
    if state["completed_count"] > 5:
        return "pause"
    return "invoke_subagent"
`)
	f := NewStarlarkFilter(path)
	ev := provider.Event{Kind: provider.EventNoteAdded, Note: provider.Note{Body: "real comment"}}

	out, _ := f.Eval(ev, map[string]any{"completed_count": int64(3)}, emptyPhraseSet{})
	if out != OutcomeInvokeSubagent {
		t.Errorf("under threshold: want InvokeSubagent, got %v", out)
	}

	out, _ = f.Eval(ev, map[string]any{"completed_count": int64(10)}, emptyPhraseSet{})
	if out != OutcomePause {
		t.Errorf("over threshold: want Pause, got %v", out)
	}
}

func TestStarlarkFilter_PhrasesContains(t *testing.T) {
	path := writeFilter(t, `
def filter(event, state, phrases):
    if phrases.contains(event["note"]["body"]):
        return "skip"
    return "invoke_subagent"
`)
	f := NewStarlarkFilter(path)
	pdir := t.TempDir()
	_ = os.WriteFile(filepath.Join(pdir, "p.yaml"),
		[]byte("version: 1\nphrases:\n  - text: ship it\n"), 0o644)
	ps, _ := LoadYAMLPhrases(filepath.Join(pdir, "p.yaml"), "")

	ev := provider.Event{Kind: provider.EventNoteAdded, Note: provider.Note{Body: "ship it"}}
	out, err := f.Eval(ev, nil, ps)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if out != OutcomeSkip {
		t.Errorf("want skip on known phrase; got %v", out)
	}
}
