# ADR-0054: Wire `title_convention` into MR title generation via the runner

**Status**: Accepted
**Date**: 2026-07-18

## Context

[ADR-0052](0052-everflow-yml-title-convention.md) added `.everflow.yml`'s
free-text `title_convention` and, by the time of ADR-0053, the field was
being read once per Run (`setup()` in `internal/refactorsweep/workflow.go`)
and threaded into the runner prompt (`internal/runner/claude/claude.go`) as
context. But the actual MR title used by `work()` when calling `CreateMR`
stayed hardcoded as `fmt.Sprintf("%s: %s", Goal, unitID)` regardless of
what the convention said — ADR-0052 explicitly deferred that wiring as a
"separate concern."

`TitleConvention` is deliberately free text (ADR-0052's alternatives
considered rejected a closed enum), which means there's no structured way
for Go code in `workflow.go` to mechanically apply it — "start with the
Jira key", "Conventional Commits", "no convention, keep it short" all
require actually reading the unit's diff and goal and phrasing a title
accordingly. That's exactly the job the runner (Claude) already does each
invocation; `workflow.go` has no comparable capability of its own.

## Decision

The runner produces the MR title, not `workflow.go`. Concretely:

- `runner.Response` gained a `Title` field: the runner's suggested MR
  title for the unit, populated only when `Decision == Done`.
- `claude.BuildPrompt` — only when `req.TitleConvention != ""` and the
  invocation is unit-scoped — appends an instruction telling the model to
  phrase the MR title per the convention and put it after `"done: "` in
  the decision marker: `<everflow-decision>done: <title></everflow-decision>`.
  This reuses the existing marker-based protocol (ADR-0027) rather than
  adding a second output channel.
- `claude.ParseDecision` gained a fourth return value, `title`, populated
  from the text after `done:` (mirroring how `ask:` and `fail:` already
  carry text after the verb).
- `work()` in `internal/refactorsweep/workflow.go` now builds the
  `CreateMR` title as `resp.Title` if non-empty, falling back to the
  pre-existing `"Goal: unitID"` default otherwise.

The fallback keeps this backward compatible on two axes: repos with no
`title_convention` set never trigger the new prompt instruction, so
`resp.Title` stays empty and `work()` behaves exactly as before; and a
convention-having repo whose runner forgets to include a title (or an
older `claude` build not honoring the instruction) still gets a
functioning, if plain, MR title rather than an empty one.

## Alternatives considered

- **Have `workflow.go` derive the title itself from `TitleConvention` +
  `Goal` + `unitID` with string heuristics** (e.g. detect "Conventional
  Commits" and prepend `feat:`). Rejected — `TitleConvention` is
  intentionally unstructured (ADR-0052); heuristics would only handle the
  conventions someone thought to special-case, defeating the point of
  free text.
- **A dedicated title-only runner invocation** (call `claude` a second
  time just to phrase the title). Rejected — doubles cost and latency for
  a one-line string the same invocation that already did the work is
  well-placed to produce; the marker protocol already carries structured
  signals (Decision, Question) alongside free text (Summary), so a title
  is a natural addition rather than a reason for a new round-trip.
- **A separate output channel** (e.g. a second regex/tag just for the
  title, independent of the decision marker). Rejected — the `done:` /
  `ask:` / `fail:` verb-plus-rest shape already exists and is exercised by
  existing tests; reusing it means `title` falls out of the same parse
  instead of adding a second thing for the model to remember to emit.

## Consequences

- Repos with a `.everflow.yml` `title_convention` set get MR titles
  phrased per that convention (assuming the model complies); repos
  without one see no behavioural change.
- `runner.Response.Title` is currently only populated by the `claude`
  runner; other `Runner` implementations (Qwen Code, OpenHands — see
  ADR-0007/ADR-0008) that don't implement the `done: <title>` convention
  simply leave `Title` empty, and `work()`'s fallback covers them.
- This closes the wiring ADR-0052 deferred. Its "Consequences" section
  claiming the file "isn't read anywhere yet" for title generation is now
  stale for the read-and-thread-into-the-prompt half (done as of ADR-0053)
  and for the actual title-generation half (this ADR) — see the amendment
  in ADR-0052.

## Tests

- `internal/runner/claude/claude_test.go` —
  `TestParseDecision_DoneWithTitle` (title extracted from `done: <text>`);
  `TestParseDecision_Done` extended to assert `Title` stays empty when the
  model omits one.
- `internal/refactorsweep/workflow_test.go` —
  `TestWork_MRTitle_UsesRunnerSuggestion` (CreateMR gets `resp.Title`
  verbatim), `TestWork_MRTitle_FallsBackWhenRunnerOmitsOne` (CreateMR gets
  the pre-existing default when `resp.Title` is empty).
