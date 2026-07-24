# ADR-0072: Non-author review comments — auto-fix defects, ask on steering

**Status**: Accepted
**Date**: 2026-07-23

## Context

[ADR-0017](0017-author-privilege-model.md) draws the author-vs-reviewer
privilege line for **control commands**: only the Run's author can
`/syntropy pause|skip|abandon`. But for ordinary review comments the
runner treated every commenter the same — any substantive comment that
passed the filter was handed to the subagent as "reviewer feedback to
address," and the subagent implemented it.

That conflates two very different kinds of reviewer input:

- **Objective defect** — "this nil check is inverted," "this test doesn't
  cover the error path." Anyone spotting a correctness bug should get it
  fixed without ceremony.
- **Solution steering** — "I'd structure this as middleware," "why not
  library X?" This changes the direction the author chose. Auto-
  implementing it silently overrides the author with a reviewer's
  preference, on the author's MR, under the author's name.

The workflow already computes `ev.IsAuthor` for every event (it gates the
control-command path) and already has a pause-and-ask outcome:
`DecisionAsk`, which [ADR-0068](0068-retryci-decision-for-ci-triage.md)
similarly leans on for ambiguous CI fixes. What was missing is threading
the author bit into the runner request and telling the model what to do
with it.

## Decision

1. Add `CommenterIsAuthor bool` to `runner.Request`, set from
   `ev.IsAuthor` in `invokeForEvent`'s `EventNoteAdded` case. Meaningless
   (and left false) when `CommentBody` is empty.
2. In `BuildPrompt`, when `CommentBody` is set and `CommenterIsAuthor` is
   false, append triage guidance directly after the reviewer-feedback
   block: implement objective defects as usual; for solution steering,
   do NOT implement — finish with `Decision=Ask`, summarising the
   suggestion so the author can approve or decline it. When unsure,
   treat as steering and ask.

The existing `DecisionAsk` machinery does the rest: the Run pauses and
the question is relayed to the author as an MR comment. No new decision
value, no filter changes, and the ADR-0017 control-verb gate upstream is
untouched.

## Alternatives considered

- **Classify the comment in the workflow (Starlark filter or heuristics)
  before invoking the runner.** Rejected: defect-vs-steering is a
  semantic judgment, exactly what the model is good at and keyword
  filters are bad at. The filter stays a cheap relevance gate.
- **A new Decision value (e.g. `DecisionDeferred`).** Rejected:
  `DecisionAsk` already means "pause and ask the author via MR comment,"
  which is precisely the desired behaviour. A new value would duplicate
  its wiring for no observable difference.
- **Block non-author comments entirely.** Rejected: reviewers catching
  real bugs is the review process working; only *direction changes* need
  the author's sign-off.

## Consequences

- A reviewer's steering comment now pauses the Run with a question
  instead of being implemented; the author replies (and `/syntropy
  resume`s) to accept or decline. Slightly more ceremony for reviewers,
  in exchange for the author keeping authorship of direction.
- The classification lives in the prompt, so it is advisory — a runner
  can still misjudge a borderline comment. The "when unsure, ask"
  default biases misjudgments toward the safe side.
- Author comments are unaffected: `CommenterIsAuthor=true` renders no
  extra guidance, so authors steer their own Runs exactly as before.

## Tests

`internal/runner/claude/claude_test.go`:
- `TestBuildPrompt_NonAuthorComment_AppendsTriageGuidance` — guidance
  rendered for non-author comments, before the scope/decision boilerplate.
- `TestBuildPrompt_AuthorComment_NoTriageGuidance` — author comments get
  the plain feedback block.
- `TestBuildPrompt_NoComment_NoTriageGuidance` — planning/work/fix-CI
  requests (no `CommentBody`) never render it.

`internal/refactorsweep/workflow_test.go`:
- `TestResume_NoteAdded_CommenterIsAuthorPropagated` — `ev.IsAuthor`
  reaches the runner request for both author and reviewer comments.
- `TestResume_AuthorControlComment_GateUnaffected` — the ADR-0017
  control-verb gate still fires before any runner invocation.
