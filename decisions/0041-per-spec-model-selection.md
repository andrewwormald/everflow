# ADR-0041: Per-spec model selection for the runner

**Status**: Accepted
**Date**: 2026-07-16

## Context

Every increment of a spec-mode Run pays the same model cost regardless of
how hard the increment is. `internal/runner/claude/claude.go` already
supports a `--model` flag (added alongside `runner.Request.Model` in the
prior increment), but nothing in the workflow ever populated it: `Model`
was always the zero value, so every `claude` invocation used the CLI's
default model no matter what the spec asked for.

Some specs are cheap, mechanical sweeps (rename a symbol across a repo,
bump a dependency version everywhere) that a smaller/cheaper model can
drive reliably. Others require harder reasoning (multi-file refactors,
ambiguous scope calls) where a more expensive model earns its cost. The
spec author is in the best position to know which kind of work a given
spec is — they should be able to pin a model per spec rather than
everflow always defaulting to whatever the runner/CLI defaults to.

`internal/spec/spec.go` already had an optional `model:` frontmatter
field (added in the prior increment) but it was never threaded any
further than the parsed `Spec` struct.

## Decision

Thread the spec's `model:` field end-to-end:

1. `AgentState.RunnerModel string` (`internal/refactorsweep/types.go`) —
   new field, set once at Trigger and immutable after, mirroring how
   `RunnerName` already works.
2. All three `runner.Request{}` construction sites in
   `internal/refactorsweep/workflow.go` (`discoverSpec`'s planning
   request, `work`, `invokeForEvent`) now set `Model: r.Object.RunnerModel`.
   Every runner invocation for a Run — planning turns, unit work turns,
   and comment/CI-triggered turns alike — uses the same pinned model.
3. `main.go`:
   - `triggerRequest` gained `RunnerModel string` (JSON `runner_model`),
     mirrored into `AgentState.RunnerModel` in `triggerHandler`.
   - `cmdStart` gained a `--model` flag. Spec-mode mapping follows the
     same override-precedence pattern as the other spec fields: the
     flag wins if set, otherwise `sp.Model` from the parsed spec.

No new mode/behavior toggle: an empty `RunnerModel` means "no `--model`
flag passed to the runner," i.e. the runner's own default — fully
backwards compatible with every existing spec and sweep-mode Run.

## Alternatives considered

- **Per-increment model choice (planner picks the model each turn).**
  More flexible, but the planner protocol doesn't currently express a
  model choice, and it's unclear how the planner would judge "this
  increment is cheap enough for a smaller model" without a second LLM
  call to make that judgment — the same disproportionate-cost problem
  ADR-0039 rejected for scope-adherence grading. Per-spec pinning is the
  simplest thing that gets the stated goal (let cheap sweeps use cheap
  models) without adding runner calls.
- **Model as a CLI-only override (no spec field), i.e. skip step 3's
  spec-mode mapping.** Rejected — the goal is "let each spec choose,"
  which means the choice has to live in the spec file itself, not just
  be re-specified by whoever happens to run `everflow start`.

## Consequences

- Spec authors set `model: claude-haiku-4-5` (or leave it unset) in
  frontmatter and every runner turn for that Run honors it.
- `--model` flag lets an operator override a spec's model at trigger
  time without editing the spec file (same override precedence as
  `--runner`, `--provider`, etc.).
- `RunnerModel` is immutable for the life of a Run, same as `RunnerName`
  — no mechanism (yet) to change the model mid-Run. If that's needed
  later (e.g. escalate to a bigger model after repeated failures), it's
  a separate follow-up; out of scope here.
- Fully backwards compatible: Runs triggered before this field existed,
  or specs without `model:`, get `RunnerModel == ""` and behave exactly
  as before (runner's own default).

## Tests

`TestWork_ThreadsRunnerModelIntoRequest`
(`internal/refactorsweep/workflow_test.go`) — asserts that a Run with
`AgentState.RunnerModel` set invokes the fake runner with that value in
`runner.Request.Model`.
