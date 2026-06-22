# ADR-0027: Claude runner adapter — prompt-marker protocol for Decision

**Status**: Accepted
**Date**: 2026-06-22

## Context

[ADR-0004](0004-shell-out-to-claude-p.md) records the decision to shell
out to `claude -p` rather than the Anthropic SDK. [ADR-0007](0007-pluggable-runner-interface.md)
and [ADR-0008](0008-native-structured-output.md) lock the cross-runner
contract: each runner produces a `runner.Response` with a `Decision`
(Continue / Done / Ask / Fail / NoChange), `Summary`, and optional
`Question`.

Claude's "native structured output" is `--output-format json`, which
emits the full message stream as JSON — useful for token accounting,
not directly useful for signalling a domain Decision. We need a contract
between everflow and claude that lets the model say "I'm done" vs
"I need help" vs "this is unfixable."

## Decisions

### 1. Prompt-marker protocol (instead of JSON output mode)

Every prompt the adapter sends to `claude -p` ends with:

```
## How to finish

After completing your work (or deciding you can't), end your response
with EXACTLY ONE of these tags on its own line:

- <everflow-decision>continue</everflow-decision>
- <everflow-decision>done</everflow-decision>
- <everflow-decision>ask: <one-line question></everflow-decision>
- <everflow-decision>fail: <one-line reason></everflow-decision>
- <everflow-decision>nochange</everflow-decision>
```

The adapter parses the LAST occurrence of `<everflow-decision>...</everflow-decision>`
in the response. Text before the marker becomes `Response.Summary`; the
marker itself is stripped. For `ask`, the post-colon text becomes
`Response.Question`.

Why prompt-marker over tool-use or JSON output mode:

- **Tool-use** (registering an `everflow_complete(decision, summary, question)`
  tool) would be cleaner but requires the adapter to drive claude's
  tool-call API rather than the simple `-p <prompt>` shell-out. That's
  a bigger refactor of the adapter — possible v2.
- **`--output-format json`** surfaces the message stream, not a domain
  Decision. We'd still need to define a marker to extract from
  message bodies. Doing it without JSON keeps the adapter readable.
- **Sentinel exit codes** can't carry a Question payload.

The downside: a model that doesn't follow the protocol returns
`ErrNoDecisionMarker`, which becomes a runner-level failure. Acceptable
for v1 — claude-class models follow protocol instructions reliably. We
fall back gracefully (the step body sees an error and routes to
StatusFailed or StatusPaused per its rules).

### 2. Last-marker-wins for parsing

A model occasionally echoes the protocol in its reasoning before
producing the real decision:

> "I could finish with `<everflow-decision>continue</everflow-decision>`,
> but actually I'm done. `<everflow-decision>done</everflow-decision>`"

`ParseDecision` picks the *last* matching tag in the response, treating
earlier mentions as conversational. Verified with a unit test.

### 3. The adapter is dumb about SkillCommand

`runner.Request.SkillCommand` (e.g. `/everflow-plan`,
`/everflow-address-comment`) is *not* interpreted by the adapter. It's
included verbatim in the prompt header so claude has it for context,
but the actual task instructions live in `req.Goal`. The step body owns
prompt construction; the adapter owns:

- Composing per-field headers (Skill, Unit, Worktree)
- Appending the decision protocol
- Running claude
- Parsing the response

This keeps the adapter swappable for runners with different mental
models (qwen, openhands) — those adapters compose prompts their own
way, but consume the same `runner.Request` fields.

### 4. `--dangerously-skip-permissions` is unconditional

The adapter always passes `--dangerously-skip-permissions` because
[ADR-0006](0006-worktree-per-run.md) makes the worktree the blast-
radius boundary. Inside the worktree, yolo mode is safe; outside, the
runner cannot reach.

### 5. Env inheritance from the daemon

By default, the subprocess inherits the daemon's `os.Environ()`. This
gets `ANTHROPIC_API_KEY` and any other auth env vars through to claude.
The `Env` field on the adapter lets tests override; production should
leave it nil.

## Alternatives considered

- **JSON output mode + parse last assistant message for marker** —
  works, more code; deferred to a future ADR if we need token accounting.
- **Use a tool-use registration via the API** — would require dropping
  `claude -p` for direct API calls; conflicts with ADR-0004.
- **Have the step body append the protocol** — would couple every step
  body to claude's marker. Step bodies are runtime-agnostic; the
  adapter owns its runtime's protocol.
- **Multiple smaller markers (one per field)** — wider parsing surface
  for no real benefit; one marker carrying verb + optional payload is
  sufficient.

## Consequences

- `runner.Response.Tokens` stays 0 from this adapter for now (we don't
  parse JSON output). Budget enforcement based on token count is
  effectively disabled for the claude runner until v2 surfaces it.
- Models that ignore the marker break this adapter. The error is
  observable (`ErrNoDecisionMarker`) and routes to StatusFailed via
  step bodies, so the failure isn't silent.
- 13 unit tests cover the parser + prompt builder (no `exec.Command`
  in tests — those need a real claude install and are spike-time
  validation).
- main.go registers `claude.NewRunner("")` so `--runner claude` (the
  default in everflow start) works once the CLI lands.
- A future ADR will add `internal/runner/qwen/` and
  `internal/runner/openhands/`, each with its own per-runner protocol
  for Decision signalling.
