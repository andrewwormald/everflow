# ADR-0068: RetryCI decision for automatic CI-failure triage

**Status**: Accepted
**Date**: 2026-07-22

## Context

`internal/refactorsweep/workflow.go` already invokes the runner with
`req.CIFailure` populated when a unit's pipeline fails (ADR-0027's
decision-marker protocol; see `formatCIFailure`). Today the runner has no
way to signal "this failure isn't a code problem" — its only outcomes are
`continue`/`done` (treat it like any other unit of work and fix it),
`ask` (pause for ambiguous cases), `fail` (give up), or `nochange`. A
flaky test or an infra hiccup (network blip, runner timeout unrelated to
the diff) gets funneled into "fix it," which pushes the model toward
inventing a code change for a failure that would have passed on its own.

The full feature (tracked across several increments) is: retry
transient/infra CI failures up to 3 times, fix genuine code failures the
same way as an explicit human instruction, and ask before making an
ambiguous behavior change. This increment adds the vocabulary the model
needs to distinguish the first case from the other two; the retry-count
wiring in `invokeForEvent` is a follow-on increment.

## Decision

Add `runner.DecisionRetryCI` alongside the existing `Decision` values,
and a `retryci` verb in `ParseDecision` mirroring how `ask`/`fail` carry
an optional one-line reason after the colon:

```go
DecisionRetryCI  Decision = 6 // CI failure looks transient/infra; re-run without code changes
```

`ParseDecision` treats `<syntropy-decision>retryci: <reason></syntropy-decision>`
the same way it treats `fail:` — the reason (if present) is folded into
Summary — since both are terminal, no-question outcomes.

`decisionProtocol` (the prompt text appended to every runner invocation)
gains the `retryci` option plus explicit guidance on when to pick each of
the four CI-failure outcomes: retryci for transient/infra noise,
continue/done for a real bug fixed like any other instructed change, and
ask for a fix that requires choosing between more than one reasonable
user-visible behavior.

## Alternatives considered

- **Reuse `DecisionNoChange` for transient CI failures.** Rejected:
  `NoChange` means "there was nothing to do," which is a different signal
  than "there was a real failure, but re-running should clear it." A
  follow-on increment needs to distinguish these to decide whether to
  re-trigger CI or just move on.
- **Fold retry guidance into `unitScopeDiscipline` instead of
  `decisionProtocol`.** Rejected: the retry/fix/ask distinction is about
  which terminal marker to emit, not about scope discipline, so it
  belongs next to the marker options themselves.

## Consequences

- `runner.Decision` has a sixth value; any exhaustive `switch` on
  `Decision` outside `runner`/`claude` (there are none yet) will need a
  case for it once `invokeForEvent` starts producing it.
- `invokeForEvent` does not yet branch on `DecisionRetryCI` — a CI-failure
  invocation that returns it today falls through whatever default the
  caller applies to an unrecognized-in-context decision. Wiring that up
  (including the 3-retry cap) is left to a follow-on increment.

## Tests

`internal/runner/claude/claude_test.go`:
- `TestParseDecision_RetryCIWithReason` — asserts the `retryci:` marker
  parses to `runner.DecisionRetryCI` and the reason lands in Summary.
