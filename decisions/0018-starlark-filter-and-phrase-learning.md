# ADR-0018: Starlark filter on every event, with bounded phrase learning

**Status**: Accepted
**Date**: 2026-06-16

## Context

The mandate in [ADR-0014](0014-refactor-sweep-mandate.md) hinges on
**amortising LLM cost**: most events the workflow sees (most CI runs, most
review comments, most webhook payloads) don't actually need reasoning to
handle. A reviewer leaving "👍" should not cost a token. A pipeline
succeeding should not cost a token. Only the events that need judgment
should reach the subagent.

The workflow needs a way to express "is this event interesting?" as
**cheap code** that runs without an LLM call. The author writes this
expression once at Trigger; the workflow runs it many times.

## Decision

**Starlark is the filter language** ([go.starlark.net](https://github.com/google/starlark-go)
in-process). The author writes a Starlark function at Trigger time;
everflow evaluates it on every inbound event.

### Filter signature

```python
def filter(event, state, phrases):
    """
    event:    immutable dict of the inbound webhook payload + normalised fields
              (event.kind, event.author, event.is_author, event.is_bot,
               event.note.body, event.mr.iid, ...)
    state:    read-only view of the Run's AgentState (units_completed, queue, ...)
    phrases:  the known-skip phrase set for this Run

    return:   one of SKIP | INVOKE_SUBAGENT | CONTROL_COMMAND | PAUSE
    """
```

### The default filter for `note_added` events

```python
def filter(event, state, phrases):
    if event.is_author and event.note.body.startswith("/everflow "):
        return CONTROL_COMMAND
    if event.is_bot:
        return bot_handler(event)         # provider-specific deterministic paths
    text = event.note.body.strip().lower()
    if is_emoji_only(text):               return SKIP
    if word_count(text) <= 3 and all_known(text, phrases): return SKIP
    return INVOKE_SUBAGENT
```

### Phrase learning

Phrase storage:

```
~/.everflow/runs/<runID>/phrases.yaml     # per-Run, project-flavoured
~/.everflow/phrases.global.yaml           # cross-Run defaults, human-curated only
```

When the filter returns `INVOKE_SUBAGENT`, the subagent's response
includes an optional `learnings.add_phrases: [...]` field. The workflow
**appends those phrases to the per-Run file only**. The global file is not
auto-modified.

### Bounds and guardrails

Three explicit limits on the learning loop:

1. **Per-Run scope by default.** Phrases learned by one Run do not affect
   any other Run. This prevents one bad learning from poisoning all
   future workflows.
2. **Cap with review.** If a Run's phrase list grows past **50 entries**,
   the workflow posts a comment on the current MR: "I've learned 50
   phrases on this refactor. Want to review them before I keep adding?"
   The author can `/everflow accept-phrases` to silence the warning or
   inspect the file directly.
3. **Promotion is manual only.** Promoting a Run's learnings to the global
   list (so future Runs benefit) is a CLI action: `everflow phrases
   promote <runID>`. The user sees the diff before committing.

## Alternatives considered

- **Plain Go for filters, compiled per workflow**: powerful but requires
  a build step. The author can't author the filter at Trigger time;
  changing the filter mid-Run requires redeploy. Rejected for v1.
- **Declarative JSON/YAML rules**: simple but limited. Refactor-comment
  classification is too nuanced for "match regex X on field Y." Rejected.
- **A cheap LLM (Haiku) as the filter**: not free, still tokens, still
  latency. Defeats the cost-amortisation pitch. Rejected.
- **Embedded Lua, JavaScript (QuickJS), CEL**: all evaluated. Starlark
  won because it's (a) deterministic by design (no `now()`, no random),
  (b) sandboxed from the start (no I/O, no `import` of arbitrary modules),
  (c) Python-ish so readable by non-specialists, (d) has a battle-tested
  Go embedding from Google.
- **Promote learnings to global automatically when N Runs agree**: too
  much complexity for v1. Manual promotion is a small UX cost for a real
  safety win.

## Consequences

- Adds one dependency: `go.starlark.net`. Pure Go, no cgo, MIT-licensed.
- The author needs to *learn the filter language* — Starlark is close
  enough to Python that a developer reads it without instruction, but
  writing one from scratch is a small learning curve. Mitigation: ship
  good *example filters* in the repo (`examples/filters/note_added.star`,
  `examples/filters/ci_failed.star`) that the author can copy-and-edit.
- Filters are **read-only** with respect to the workflow's state — they
  inspect `state` but can't mutate it. Mutations happen only in the Go
  handlers after the filter returns a `DECISION`. This keeps the filter
  pure and easy to test.
- A `everflow filter-test <filter.star> <fixture.json>` CLI subcommand
  lets the author dry-run the filter against canned event payloads
  before going live. Important for catching "this filter would have
  invoked the subagent on every emoji" bugs cheaply.
- The phrase-file format is YAML, deliberately editable by humans.
  Schema:

  ```yaml
  version: 1
  phrases:
    - text: "lgtm"
      added_by: subagent
      added_at: 2026-06-16T14:32:00Z
      after_mr: 47
    - text: "looks good to me"
      added_by: human                     # via `everflow phrases promote`
  ```
