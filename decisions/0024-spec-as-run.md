# ADR-0024: Spec as Run; sweep + spec modes coexist on one workflow definition

**Status**: Accepted
**Date**: 2026-06-22

## Context

[ADR-0014](0014-refactor-sweep-mandate.md) named bulk-refactor sweeps as
everflow's primary use case — a Run takes a list of units (services to
update, files to migrate) enumerated up front and processes them
throttled-sequentially. That model works for "the units are knowable
before we start."

There's a second shape that doesn't fit that model: **drive a single spec
end-to-end, with the agent planning each MR until the spec is implemented
or the author abandons the run**. The unit isn't knowable up front — it's
discovered iteratively. Examples:

- "Build feature X" where each MR is one slice of the implementation
- "Migrate to library Y" where each MR resolves a coherent chunk of the
  blast radius the agent finds while working
- Any human-authored spec doc that needs incremental delivery

The customer need surfaced in conversation on 2026-06-22: one spec ⇒ one
Run; loop until done; abandon-via-comment with confirmation. Same
review-and-CI machinery as sweep mode; different brains inside `discover()`.

## Decisions

### 1. Spec is the Run, not a unit inside a Run

When a spec triggers a Run, the spec *is* the Run's reason for existing.
The Run's lifecycle ends when the spec is implemented (planner declares
done) or the author cancels (`/everflow stop` / `/everflow abandon`).

The alternative — "one Run owns N specs as units" — was rejected because:
- Specs have independent lifecycles (one can finish, one can be abandoned)
- Per-spec config (provider, project, runner, base branch) is naturally per-Run
- Concurrency control at the spec level is incoherent if specs share a Run

This is also bletchley's model. Picking it up is consistent.

### 2. One workflow definition serves both modes

We add an `AgentState.Mode` field ("sweep" or "spec"; empty = "sweep" for
backwards compatibility). `discover()` branches on Mode:

| Mode | `discover()` behaviour |
|---|---|
| `sweep` | Pop next from static `Queue`. Today's mechanical-sweep behaviour, unchanged. |
| `spec` | Invoke `runner.Run` with planning context (spec body + plan history + merged increments) — the runner returns the next increment or "spec implemented." See [ADR-0025](0025-planner-driven-discover.md). |

Both modes:
- Use the same `setup`/`work`/`resume` step bodies
- Use the same MR-comment communication channel ([ADR-0016](0016-mr-comments-only-channel.md))
- Use the same author-privilege model ([ADR-0017](0017-author-privilege-model.md))
- Cycle the same way: `Discovering → Working → AwaitingMerge → Discovering → ...`

What differs is *only* the contents of `discover()` (queue.Pop vs LLM call).
This keeps the state-machine surface, audit shape, and operational story
identical across modes.

### 3. Sweep mode stays the default (and unchanged)

Existing v1 Runs (and any code path that hasn't been updated for spec
mode) continue to work without changes:

- `AgentState.Mode == ""` is treated as sweep mode
- `--units a,b,c` still populates `AgentState.Queue` and triggers a sweep
- All 43 existing refactorsweep tests pass without modification

[ADR-0014](0014-refactor-sweep-mandate.md) is *not* superseded. Sweep
remains a primary use case; spec joins it as a co-equal sibling.

### 4. Spec format: YAML frontmatter + markdown body

```markdown
---
goal: Migrate logrus to log/slog across services
provider: gitlab
project: lunomoney/core
runner: claude
base_branch: main
base_repo: /home/everflow/repos/core
status: ready
---
# Migration plan

Replace `github.com/sirupsen/logrus` imports with `log/slog` across
all Go services. Preserve log levels and structured fields.

## Constraints

- One service per MR
- Tests must pass
- Don't change logging behaviour, only the library
```

Frontmatter carries structured config (consumed at Trigger to populate
AgentState); body is what the planner reads each iteration to decide the
next increment. The format borrows from Jekyll / Hugo / Obsidian — every
markdown-editing tool the author might use already supports it.

Required fields: `goal`, `provider`, `project`, `runner`. Optional:
`base_repo`, `base_branch`, `concurrency`, `status`. The parser fails
fast on missing required fields, listing all missing ones in one error.

A `status` of empty or `ready` means everflow will pick this spec up; any
other value (e.g. `draft`, `in_progress`, `compressed`) means leave it
alone. This matches bletchley's lifecycle and lets a future watch-
directory ingestion driver dedup against in-flight Runs.

## Alternatives considered

- **Plain text Goal field, no structured spec** — works for trivial cases,
  but loses the per-spec config (provider/project/runner/base) that
  makes Trigger ergonomic. Also closes the door on future safety
  primitives like allowlists.
- **JSON specs** — no human writes JSON by hand pleasantly. YAML
  frontmatter + markdown body is the format developers already use for
  notes and ADRs.
- **TOML frontmatter** — fewer editors support it, and we'd add a new
  parser dependency. YAML is already in our transitive deps via
  testify/luno-workflow.
- **Two workflow definitions** (one per mode) — duplicated state-machine
  wiring, doubled operational surface, no shared audit. The Mode field
  keeps everything in one graph.

## Consequences

- `AgentState` grows by three fields (`Mode`, `SpecPath`, `SpecBody`) and
  one new collection (`Plan []PlannedIncrement`). Sweep-mode Runs leave
  them empty.
- The spec format becomes part of everflow's user-facing surface. We
  own its evolution; field additions need to be backwards-compatible
  (new optional fields only).
- The Trigger surface gains a `--spec <path>` flag (built when
  `everflow start` is fleshed out). Existing `--units a,b,c` still works.
- `discover()`'s "spec" branch is a load-bearing follow-up — see
  [ADR-0025](0025-planner-driven-discover.md) for the planning-call
  design. Until that lands, spec-mode Runs would Complete immediately
  (queue empty, plan empty); the parser + state plumbing in this commit
  is the precondition, not the activation.
- A `internal/spec/` package houses the parser. ~200 LOC including
  validation + tests. No new runtime deps (we already depend on
  `gopkg.in/yaml.v3` transitively).
- Future ADRs may add: spec-watch-directory ingestion driver, spec
  status-projection back to disk, ADR-style compression on completion.
  All additive — none require interface changes here.
