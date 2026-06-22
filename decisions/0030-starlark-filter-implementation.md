# ADR-0030: Starlark filter implementation — embedded default, per-Run override, YAML phrases

**Status**: Accepted
**Date**: 2026-06-22

## Context

[ADR-0018](0018-starlark-filter-and-phrase-learning.md) decided that
events go through a cheap, agent-authored Starlark filter before
reaching the (expensive) subagent. The implementation was deferred so
the rest of v1 could land first; this ADR records what the implementation
actually does.

## Decisions

### 1. Default filter ships embedded in the binary

The everflow binary embeds `internal/filter/default.star` via `//go:embed`.
`setup()` writes this file to `<RunsRoot>/<runID>/note_added.star` for
each new Run if the path doesn't already exist. The user can `everflow
start --spec ...` and immediately get sensible filtering without writing
any Starlark — the default catches `lgtm`/short comments/bots.

`filter.DefaultStarlark()` returns the embedded bytes so tests can use
the same source the daemon ships with.

### 2. The default filter encodes the conservative skip rules

```python
def filter(event, state, phrases):
    if event["kind"] == "pipeline_failed":
        return "invoke_subagent"
    body = event["note"]["body"].strip()
    body_lc = body.lower()
    if event["author"]["is_bot"]:
        return "skip"
    if len(body) <= 3:
        return "skip"
    if phrases.contains(body_lc):
        return "skip"
    words = body_lc.split()
    if len(words) <= 3 and phrases.contains(" ".join(words)):
        return "skip"
    return "invoke_subagent"
```

Three rules: pipeline failures always invoke; bots and very-short
comments skip; known phrases skip; everything else invokes. Conservative
on purpose — false-skips are worse than false-invocations because the
former hides reviewer feedback.

Known limitation surfaced in tests: `len(body)` in Starlark counts
bytes, not characters. Single emoji like 👍 are 4 bytes and slip past
the length-3 check. They get caught once added to the phrase list. The
test `TestStarlarkFilter_DefaultBuiltIn_SkipsShortAscii` documents this.

### 3. Filter is re-read from disk on every Eval()

`StarlarkFilter.Eval` reads the .star file each time. Cheap (typical
file is < 1 KB) and means editing the .star while the daemon is running
takes effect on the next event — useful for the spike's iterative
prompt-tuning. Cache later if it ever matters.

`starlark.Thread` is re-created per Eval. Starlark globals don't carry
between distinct programs anyway; the fresh thread keeps each Eval
independent.

### 4. State is exposed via a curated dict, not full AgentState

`stateToMap(s *AgentState)` returns a 10-key map of fields a filter
author plausibly needs:

```
goal, mode, provider, project,
completed_count, blacklisted_count, in_flight_count,
queue_count, plan_count,
events_seen, subagent_invocations
```

Why not pass AgentState directly:
- Many AgentState fields (WebhookSecret, Author identity, PromptInjection)
  are sensitive or implementation detail
- A curated dict gives us room to evolve AgentState without breaking
  every user's filter
- The dict's schema becomes part of everflow's user-facing contract;
  adding keys is safe, removing them is not

### 5. Phrases are YAML, per-Run + global, capped + warned

`internal/filter/phrases.go` houses `YAMLPhraseSet`:

- **Per-Run** at `<RunsRoot>/<runID>/phrases.yaml` — appended by the
  runner via `Learnings.AddPhrases`. Auto-creation of an empty file
  happens in `setup()` so `Add()` doesn't have to MkdirAll.
- **Global** at `<parent(RunsRoot)>/phrases.global.yaml` — human-curated.
  Read by every Run; never auto-written. v2 will add `everflow phrases
  promote <runID>` to copy per-Run entries up.

`Contains(text)` checks both sources case-insensitively and trim-tolerantly
(`"  LGTM  "` matches `"lgtm"`). The combined index is rebuilt on every
load. `All()` returns the union.

`Add(phrases, addedBy, afterMR)`:
- Deduplicates against the combined view
- Appends new entries with a timestamp + provenance
- Atomically writes the per-Run file (tmp + rename)
- Returns the count actually added (excluding dupes)

`OverCap()` flips after `MaxPerRunEntries` (50). When invokeForEvent
hits this, it posts a one-time comment suggesting the author either
trim the list or promote useful entries to global.

Schema versioning: the YAML root has `version: 1`. Future migrations
can branch on this without breaking older files.

### 6. Outcome strings, not exported Go constants

`filter()` returns a string:

| Returned | Outcome |
|---|---|
| `"skip"` | `OutcomeSkip` |
| `"invoke_subagent"` | `OutcomeInvokeSubagent` |
| `"control_command"` | `OutcomeControlCommand` (resume() handles this before the filter; the filter typically won't need to return it) |
| `"pause"` | `OutcomePause` |
| anything else | error |

Strings instead of magic ints because:
- Filter authors copy-paste from examples; magic ints look opaque
- The strings appear verbatim in `default.star` and any user-written
  filter — same vocabulary in code and config

## Alternatives considered

- **Eval'd per-call vs cached** — chose per-call for live edit-ability.
  Performance is well within budget for v1 traffic levels.
- **Single phrase file, no per-Run / global split** — would conflate
  trusted human curation with auto-appended subagent learnings, making
  it hard to trim experimental entries without losing trusted ones.
  ADR-0018 §4 specifically called this split out; we honour it.
- **Embed phrase metadata in the .star** — confuses code with data, and
  makes "I want to clear learned phrases" require editing the script.
  YAML keeps the data layer separate.
- **More state fields in stateToMap** — temptation is real; resisted to
  keep the user-facing contract small.

## Consequences

- **New dependency**: `go.starlark.net`. ~2 MB compiled, no cgo, MIT-
  licensed. Adds ~30s to a clean build's `go mod download`. Acceptable.
- The previous `StubFilter` is kept as the test default and is the
  fallback if `AgentState.FilterPath` is empty (i.e. the Run was
  triggered before setup() wrote the default — defensive, shouldn't
  happen in practice).
- `setup()` now writes two files in the Run directory (`note_added.star`
  + `phrases.yaml`). Both idempotent — re-running setup leaves existing
  files alone.
- `invokeForEvent()` calls `Phrases.Add` on `resp.Learnings.AddPhrases`
  after each subagent turn. Over-cap triggers a one-time MR comment.
- 15 new tests in `internal/filter`: 4 phrases, 11 starlark.
- Future: `everflow phrases promote` to copy per-Run → global. Out of
  scope for this ADR.
