# ADR-0007: Pluggable `Runner` interface, per-Run `--runner` flag

**Status**: Accepted
**Date**: 2026-06-15

## Context

We want to support multiple coding agents — `claude`, `qwen`, `openhands`,
direct SDK runners, future entrants — without baking any one of them into
the workflow definition. They have different invocation shapes:

- **Single-turn CLIs** (`claude -p`, `qwen -p`) — one prompt in, structured
  text out, ~minutes per invocation
- **Full agent sessions** (OpenHands) — give it a goal, it runs its own
  inner loop for many turns, exits when *it* thinks it's done; ~tens of
  minutes per invocation

We also want users to mix runners (run a review-babysit skill via claude,
run a different skill via openhands) without registering a separate
workflow per runner.

## Decision

A single Go interface:

```go
type Runner interface {
    Name() string
    Run(ctx context.Context, req RunRequest) (RunResponse, error)
}
```

with a per-binary registry (`init()`-registered). The workflow definition is
shared; the runner is chosen at Trigger time via the `--runner` flag and
stored on the Run's `AgentState`. One daemon can have claude, qwen, and
openhands Runs in flight simultaneously.

## Alternatives considered

- **One workflow per runner** — `agent-loop-claude`, `agent-loop-qwen`, etc.
  registered as distinct `workflow.Workflow` definitions. Cleaner per-runner
  metrics, but you can't switch a paused Run from one runner to another and
  the operational surface (multiple workflows to register, monitor, ship)
  is larger.
- **Configure runner per workflow-definition, fix it at compile time** —
  forces a rebuild to add a runner. Doesn't fit the "support whatever the
  user has installed" story.

## Consequences

- All runners share one workflow definition and one event stream. Operational
  monitoring filters by runner name as needed.
- Adding a new runner is implementing one interface plus registering it.
- Each runner's adapter is responsible for: argv shape, env-var passthrough,
  output parsing (see [ADR-0008](0008-native-structured-output.md)),
  timeout enforcement.
- The `RunnerName` on `AgentState` is *immutable after Trigger*. Switching
  runners mid-Run is not supported; users must stop and re-trigger.
