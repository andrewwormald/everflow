# ADR-0056: Decision marker must be alone on its line

**Status**: Accepted
**Date**: 2026-07-19

## Context

[ADR-0027](0027-claude-runner-adapter.md) defined the prompt-marker
protocol: `ParseDecision` extracts the LAST
`<everflow-decision>...</everflow-decision>` tag from the model's
response, on the theory that the model sometimes echoes the protocol
in its reasoning before producing the real decision, and the real one
always comes last.

That assumption breaks when the response's prose, *after* the real
decision, incidentally mentions the tag again — for example the model
explaining what it changed ("I fixed the bug where a summary
mentioning `<everflow-decision>fail: ...</everflow-decision>` used to
hijack parsing") in trailing commentary. The old regex
(`(?s)<everflow-decision>\s*(.*?)\s*</everflow-decision>`) matches
this incidental mention just as readily as a genuine marker, and
"last wins" then picks the incidental one — silently discarding the
real decision and, in the worst case, misreading its verb.

## Decision

`ParseDecision`'s regex now requires the marker to occupy its own
line (only leading/trailing horizontal whitespace permitted):

```go
regexp.MustCompile(`(?m)^[ \t]*<everflow-decision>\s*(.*?)\s*</everflow-decision>[ \t]*$`)
```

This matches the protocol instruction verbatim — "end your response
with EXACTLY ONE of these tags **on its own line**" (see ADR-0027) —
so it tightens parsing to what was always specified, rather than
changing the contract. A tag mentioned mid-sentence, inside a quoted
example, or as part of other prose shares its line with other text
and no longer matches at all. Among genuine own-line markers,
last-wins is preserved (a model can still echo the protocol as a
standalone line while reasoning before landing on the real decision).

## Alternatives considered

- **Require the marker to be the last non-whitespace content in the
  entire response.** Simpler mental model, but more brittle: models
  sometimes add a trailing blank line or stray whitespace, and this
  would also break if a runner ever wants to allow a short trailing
  note after the marker. Own-line matching solves the actual bug
  (shared-line incidental mentions) without over-constraining trailing
  content.
- **Strip markdown code spans/fences before matching.** Would also
  catch backtick-quoted mentions, but doesn't handle plain-prose
  mentions (no backticks) sharing a line with other text, which is
  the more common failure mode. Own-line matching subsumes both cases
  more simply.

## Consequences

- No prompt changes needed — the protocol text already told the model
  to put the marker on its own line; only the parser was lenient.
- A model that concatenates the marker onto the same line as other
  text (e.g. "Done. <everflow-decision>done</everflow-decision>")
  will now get `ErrNoDecisionMarker` instead of a silently-accepted
  match. Acceptable: this was already off-protocol, and failing loudly
  is safer than the previous behaviour of sometimes being right and
  sometimes being hijacked.
- Covered by `TestParseDecision_IncidentalMentionDoesNotHijack` in
  `internal/runner/claude/claude_test.go`.
