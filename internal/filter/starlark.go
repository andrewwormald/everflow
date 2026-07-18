package filter

import (
	_ "embed"
	"fmt"
	"os"
	"sync"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/andrewwormald/syntropy/internal/provider"
)

//go:embed default.star
var defaultStar []byte

// DefaultStarlark is the canonical filter content shipped with the daemon.
// setup() writes this to the per-Run filter file if no custom one exists,
// so the spike works out of the box without the user writing Starlark.
func DefaultStarlark() []byte {
	return append([]byte(nil), defaultStar...)
}

// StarlarkFilter loads a .star file from disk and evaluates its filter()
// function on each event. Implements the Filter interface.
//
// The filter is re-loaded from disk on every Eval() call — cheap for the
// file sizes we expect (<10KB), keeps "edit the .star and see the change
// next event" a useful workflow.
type StarlarkFilter struct {
	path string

	// thread is reused across evaluations to avoid setup cost; mu protects
	// it because starlark.Thread isn't safe for concurrent use.
	mu     sync.Mutex
	thread *starlark.Thread
}

// NewStarlarkFilter constructs a StarlarkFilter pointing at the given .star
// file. The file isn't loaded yet — Eval will load on first call.
func NewStarlarkFilter(path string) *StarlarkFilter {
	return &StarlarkFilter{
		path:   path,
		thread: &starlark.Thread{Name: "everflow-filter"},
	}
}

// Verify StarlarkFilter satisfies Filter at compile time.
var _ Filter = (*StarlarkFilter)(nil)

func (f *StarlarkFilter) Eval(event provider.Event, state any, phrases PhraseSet) (Outcome, error) {
	src, err := os.ReadFile(f.path)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("starlark: read %s: %w", f.path, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Reset the thread per call — Starlark globals don't carry between
	// distinct programs and we want each Eval to be independent.
	f.thread = &starlark.Thread{Name: "everflow-filter"}

	globals, err := starlark.ExecFile(f.thread, f.path, src, nil)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("starlark: exec %s: %w", f.path, err)
	}
	filterFn, ok := globals["filter"].(starlark.Callable)
	if !ok {
		return OutcomeUnknown, fmt.Errorf("starlark: %s does not define a filter() function", f.path)
	}

	evDict, err := eventToStarlark(event)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("starlark: marshal event: %w", err)
	}
	stDict, err := stateToStarlark(state)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("starlark: marshal state: %w", err)
	}
	phStruct := phrasesToStarlark(phrases)

	result, err := starlark.Call(f.thread, filterFn,
		starlark.Tuple{evDict, stDict, phStruct}, nil)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("starlark: filter(): %w", err)
	}
	out, err := outcomeFromStarlark(result)
	if err != nil {
		return OutcomeUnknown, err
	}
	return out, nil
}

// --- marshalling ---

func eventToStarlark(ev provider.Event) (*starlark.Dict, error) {
	d := starlark.NewDict(8)
	if err := d.SetKey(starlark.String("kind"), starlark.String(string(ev.Kind))); err != nil {
		return nil, err
	}
	if err := d.SetKey(starlark.String("is_author"), starlark.Bool(ev.IsAuthor)); err != nil {
		return nil, err
	}
	if err := d.SetKey(starlark.String("is_bot"), starlark.Bool(ev.IsBot)); err != nil {
		return nil, err
	}

	author := starlark.NewDict(3)
	_ = author.SetKey(starlark.String("handle"), starlark.String(ev.Author.Handle))
	_ = author.SetKey(starlark.String("email"), starlark.String(ev.Author.Email))
	_ = author.SetKey(starlark.String("is_bot"), starlark.Bool(ev.Author.Bot))
	_ = d.SetKey(starlark.String("author"), author)

	mr := starlark.NewDict(3)
	_ = mr.SetKey(starlark.String("project_id"), starlark.String(ev.MR.ProjectID))
	_ = mr.SetKey(starlark.String("iid"), starlark.MakeInt(ev.MR.IID))
	_ = mr.SetKey(starlark.String("url"), starlark.String(ev.MR.URL))
	_ = d.SetKey(starlark.String("mr"), mr)

	note := starlark.NewDict(2)
	_ = note.SetKey(starlark.String("id"), starlark.MakeInt64(ev.Note.ID))
	_ = note.SetKey(starlark.String("body"), starlark.String(ev.Note.Body))
	_ = d.SetKey(starlark.String("note"), note)

	pipe := starlark.NewDict(3)
	_ = pipe.SetKey(starlark.String("id"), starlark.MakeInt64(ev.Pipeline.ID))
	_ = pipe.SetKey(starlark.String("status"), starlark.String(ev.Pipeline.Status))
	jobs := starlark.NewList(nil)
	for _, j := range ev.Pipeline.FailedJobs {
		jd := starlark.NewDict(3)
		_ = jd.SetKey(starlark.String("name"), starlark.String(j.Name))
		_ = jd.SetKey(starlark.String("stage"), starlark.String(j.Stage))
		_ = jd.SetKey(starlark.String("status"), starlark.String(j.Status))
		_ = jobs.Append(jd)
	}
	_ = pipe.SetKey(starlark.String("failed_jobs"), jobs)
	_ = d.SetKey(starlark.String("pipeline"), pipe)

	return d, nil
}

// stateToStarlark exposes a curated subset of AgentState fields to the
// filter — only what a filter author plausibly wants. We accept `any` here
// because the filter package can't import refactorsweep (cycle). Callers
// pass a map[string]any that the workflow has pre-extracted.
func stateToStarlark(state any) (*starlark.Dict, error) {
	d := starlark.NewDict(8)
	m, ok := state.(map[string]any)
	if !ok {
		// Nil or unrecognised — fine; filter just sees an empty dict.
		return d, nil
	}
	for k, v := range m {
		sv, err := starlark.NewBuiltin("noop", nil).Truth(), error(nil)
		_ = sv
		val, err := toStarlarkValue(v)
		if err != nil {
			return nil, fmt.Errorf("state[%q]: %w", k, err)
		}
		_ = d.SetKey(starlark.String(k), val)
	}
	return d, nil
}

func toStarlarkValue(v any) (starlark.Value, error) {
	switch v := v.(type) {
	case nil:
		return starlark.None, nil
	case string:
		return starlark.String(v), nil
	case bool:
		return starlark.Bool(v), nil
	case int:
		return starlark.MakeInt(v), nil
	case int64:
		return starlark.MakeInt64(v), nil
	case float64:
		return starlark.Float(v), nil
	default:
		return starlark.None, fmt.Errorf("unsupported type %T", v)
	}
}

// phrasesToStarlark builds a struct with `contains(text)` and `all()`
// methods backed by the Go PhraseSet.
func phrasesToStarlark(ps PhraseSet) *starlarkstruct.Struct {
	members := starlark.StringDict{}
	if ps != nil {
		members["contains"] = starlark.NewBuiltin("contains",
			func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
				var s string
				if err := starlark.UnpackPositionalArgs("contains", args, nil, 1, &s); err != nil {
					return nil, err
				}
				return starlark.Bool(ps.Contains(s)), nil
			})
		members["all"] = starlark.NewBuiltin("all",
			func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
				items := ps.All()
				out := starlark.NewList(nil)
				for _, s := range items {
					_ = out.Append(starlark.String(s))
				}
				return out, nil
			})
	} else {
		members["contains"] = starlark.NewBuiltin("contains",
			func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
				return starlark.False, nil
			})
		members["all"] = starlark.NewBuiltin("all",
			func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
				return starlark.NewList(nil), nil
			})
	}
	return starlarkstruct.FromStringDict(starlark.String("phrases"), members)
}

func outcomeFromStarlark(v starlark.Value) (Outcome, error) {
	s, ok := starlark.AsString(v)
	if !ok {
		return OutcomeUnknown, fmt.Errorf("filter() must return a string, got %s", v.Type())
	}
	switch s {
	case "skip":
		return OutcomeSkip, nil
	case "invoke_subagent":
		return OutcomeInvokeSubagent, nil
	case "control_command":
		return OutcomeControlCommand, nil
	case "pause":
		return OutcomePause, nil
	default:
		return OutcomeUnknown, fmt.Errorf("filter() returned unknown outcome %q", s)
	}
}
