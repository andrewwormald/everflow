# ADR-0069: Wire `DecisionRetryCI` into `invokeForEvent` with a 3-retry cap

**Status**: Accepted
**Date**: 2026-07-22

## Context

ADR-0068 added `runner.DecisionRetryCI` and the `retryci:` marker so the
runner has a way to say "this CI failure looks transient/infra, not a code
bug" — but explicitly left `invokeForEvent` not branching on it, falling
through to the unhandled-decision error at the bottom of its switch. This
increment is that follow-on: retry transient CI failures automatically, up
to a cap, before asking a human.

## Decision

`AgentState` gains `CIRetryCounts map[string]int`, keyed by unit ID —
tracking how many consecutive `DecisionRetryCI` outcomes a unit has seen.

`invokeForEvent`'s switch on `resp.Decision` gains a `DecisionRetryCI` case:

- Increment `CIRetryCounts[unitID]`.
- If the count exceeds `maxCIRetries` (3), pause the Run (`PauseReason` +
  a bot comment) instead of retrying again — retrying alone isn't clearing
  it, so this needs a human, not another CI run.
- Otherwise, call `provider.RetryPipelineJob` for every job in
  `ev.Pipeline.FailedJobs`, post an informational "looks transient, retry
  N/3" comment, and stay `AwaitingMerge`. No git operations happen on this
  path — `DecisionRetryCI` means "no code change," so there's nothing to
  commit or push.
- If `RetryPipelineJob` itself errors, pause (mirrors the existing
  git-failure pause pattern elsewhere in this function).

`EventPipelineSucceeded` (already handled, previously a pure no-op) now
also deletes the unit's `CIRetryCounts` entry — a green pipeline means
whatever was flaky cleared, so the next failure should start counting from
zero rather than inherit a stale near-cap count from an unrelated earlier
run.

`Decision`'s re-exported alias block in `types.go` (`DecisionContinue`,
`DecisionAsk`, ...) gained `DecisionRetryCI = runner.DecisionRetryCI` — it
had not been updated when ADR-0068 added the value.

## Alternatives considered

- **Cap by wall-clock/time window instead of a per-unit counter.**
  Rejected: a consecutive-count cap is simpler, matches the "3 retries"
  language in the feature's stated goal, and self-resets on any green
  pipeline without needing a decay timer.
- **Reset the counter on any event other than `EventPipelineSucceeded`**
  (e.g. on every `DecisionDone`). Rejected: `DecisionDone`/`Continue` come
  from `NoteAdded` events too, which say nothing about whether CI is
  currently green — only an actual passing pipeline is a trustworthy
  signal that the flakiness cleared.
- **Retry via a fresh full pipeline re-run instead of per-job
  `RetryPipelineJob`.** Rejected: `RetryPipelineJob` already exists on the
  `Provider` interface (added ahead of this feature) and retrying only the
  failed job(s) is cheaper and matches what a human would do by hand.

## Consequences

- A transient CI failure now self-heals up to 3 times without ever
  invoking the runner's code-editing path or touching git.
- A unit stuck on a genuinely-failing-but-runner-thinks-transient job pauses
  after 3 retries instead of looping the CI/webhook cycle indefinitely.
- `work()`'s switch (the planned-work loop) does not set `req.CIFailure`
  and so never produces `DecisionRetryCI` in practice; it still falls
  into that switch's existing `default: unexpected decision` branch if a
  runner returned it there anyway, matching how it already treats
  `DecisionAsk`. No change needed there for this increment.

## Tests

`internal/refactorsweep/workflow_test.go`:
- `TestResume_PipelineFailed_DecisionRetryCI_RetriesFailedJobs` — asserts
  the failed job is retried via `RetryPipelineJob`, the counter increments,
  a "transient" comment is posted, and the Run stays `AwaitingMerge`.
- `TestResume_PipelineFailed_DecisionRetryCI_PausesAfterCap` — asserts that
  once `CIRetryCounts[unit] == maxCIRetries`, the next `DecisionRetryCI`
  pauses instead of retrying, and no job retry is issued.
- `TestResume_PipelineSucceeded_ResetsCIRetryCount` — asserts
  `EventPipelineSucceeded` deletes the unit's retry-count entry.
