# ADR-0008: Native structured output per runner

**Status**: Accepted
**Date**: 2026-06-15

## Context

The workflow's step graph needs the runner to communicate a `Decision`
(continue / ask / done / fail) plus a `Summary` and an updated `Scratchpad`.
Two ways to get that out of an LLM:

- **JSON-in-prompt contract** — prompt instructs the model to end its
  response with `<workflow-decision>{...}</workflow-decision>` (or
  similar). We parse it. Works for any runner that follows instructions.
- **Native structured output** — use each runner's built-in mechanism:
  `claude -p --output-format json`, OpenAI's `response_format`, OpenHands'
  on-disk session-state file. Robust per runner; more total code.

## Decision

Use **native structured output** for each runner, with per-runner adapter
code that parses whatever shape that runner emits. The `Runner` interface
returns a single normalized `RunResponse`; each adapter's job is the
denormalization.

## Alternatives considered

- **JSON-in-prompt for all runners** — universal but fragile. Models
  occasionally ignore the format directive (especially with long context
  or strong tool-use prompts), and the failure mode is "we get a 4KB
  string back with no parseable decision."
- **Hybrid** — native where available, JSON-in-prompt elsewhere. Killed:
  if we have to maintain JSON-in-prompt as a fallback, we've kept its
  fragility forever for almost no portability win (Qwen Code is the only
  near-term candidate without confirmed native structured output, and
  that's resolvable).

## Consequences

- Each Runner adapter is larger — ~100 LOC including parsing, not ~30.
  Acceptable, since the per-runner surface is small and infrequently
  changing.
- New runners require structured-output capability. If a future runner
  doesn't have it natively, we add JSON-in-prompt as that runner's adapter
  internal detail — not as a universal fallback.
- Tests of the adapter can use canned native output (claude's JSON, etc.)
  without invoking the real CLI.
