# ADR-0050: `everflow setup` picks and persists a default runner + model

**Status**: Accepted
**Date**: 2026-07-17

## Context

The full `everflow setup` spec goal is to let a user choose their coding
agent, its default model, and this repo's PR/MR title convention.
[ADR-0048](0048-file-presence-as-first-run-marker.md) covered the Skill
bundle install mechanism; runner/model selection and title-convention
prompting were left as separate follow-on increments to keep each landable
change to one concern.

`runner.Registry` (ADR-0007) and the spec-level `runner:`/`model:` fields
(ADR-0044) already exist, but nothing lets a user set a *default* — every
spec must currently spell out `runner:`/`model:` itself, or fall back to
whatever `everflow start --runner`/`--model` passes (which defaults to the
zero value: no runner-name-resolution fallback, no model override). This
ADR only adds the persistence + the `setup` prompting; wiring the persisted
default into spec resolution (so an unset spec field falls back to the
config file instead of the runner's own default) is intentionally left for
a later increment — it touches `internal/spec` and the trigger path, a
different concern than "ask the user once and remember the answer."

## Decision

**`internal/config`** is a new package holding the on-disk shape of
`~/.everflow/config.yaml`:

```go
type Config struct {
    Runner string `yaml:"runner"`
    Model  string `yaml:"model"`
}
```

`Load`/`Save` mirror the read-tolerant, write-creates-the-dir pattern
`internal/spec` and the sqlite store already use elsewhere in this
codebase. A missing file loads as the zero value — no error — since a user
who hasn't run `setup` yet simply has no persisted defaults.

**`internal/setup`** gained two pure resolution functions, `ResolveRunner`
and `ResolveModel`, so the choice logic is unit-testable without stdin/TTY
plumbing:

- `ResolveRunner(flagRunner string)`: with `KnownRunners` holding exactly
  one entry (`"claude"`), an unset `--runner` auto-selects it. A set flag is
  validated against `KnownRunners` — an unrecognised name errors instead of
  silently persisting a runner that will fail to resolve later.
- `ResolveModel(flagModel, existing string, interactive bool, prompt func(existing string) (string, error))`:
  precedence is flag > interactive prompt > existing persisted value. The
  "existing" fallback (not "empty") is the load-bearing choice — see
  Consequences.

`cmdSetup` in `main.go` wires these to real I/O: `--runner`/`--model`
flags, `isatty.IsTerminal(os.Stdin.Fd())` for the interactive check, and a
`bufio.Scanner`-based prompt over `os.Stdin`/`os.Stdout` that shows the
existing value as what a blank answer keeps. `go-isatty` was already an
indirect dependency (pulled in transitively); this promotes it to direct.

## Alternatives considered

- **Always prompt for runner too, even with one option.** Rejected —
  prompting when there's nothing to choose between is friction with no
  payoff. `ResolveRunner` auto-selects instead; the function signature
  already accommodates a future second runner without changing callers
  (just add to `KnownRunners` and drop the length-1 shortcut).
- **Reset the model to empty on a non-interactive re-run with no flag.**
  Rejected — `everflow setup` might reasonably be re-run non-interactively
  (a script re-asserting the Skill install, CI, etc.) and blowing away a
  previously chosen model on every such run would be a nasty surprise.
  `ResolveModel`'s "existing" fallback avoids this: only an explicit
  `--model` or an explicit prompt answer changes the persisted value.
- **Introspect a live `runner.Registry` for `KnownRunners` instead of a
  hardcoded slice.** Rejected for now — constructing the registry means
  constructing runners (subprocess-shelling adapters) just to read their
  names. A hardcoded list in `internal/setup` mirrors the "Claude-only for
  now" framing already in that package's doc comment; when a second runner
  ships, its name gets added here alongside its own integration bundle.

## Consequences

- `everflow setup` now always ends with `~/.everflow/config.yaml` holding
  `runner: claude` (today) and whatever model was chosen (or the empty
  string if none was ever set, interactively or via flag).
- Non-interactive `setup` runs (no TTY, e.g. cron or a first-run script
  hook) degrade gracefully: no hang waiting on stdin, and no accidental
  reset of a previously configured model.
- The persisted config isn't consumed anywhere yet — spec resolution still
  ignores it. That wiring, and the title-convention prompt +
  `.everflow.yml` write, remain separate follow-on increments per the
  original scope split.

## Tests

- `internal/config/config_test.go` — round-trip Save/Load, missing-file
  zero value, overwrite-on-resave.
- `internal/setup/runner_test.go` — `ResolveRunner` auto-select + unknown-
  flag rejection; `ResolveModel`'s flag/interactive/non-interactive/blank-
  answer precedence.
- `main_test.go` — `cmdSetup` end-to-end against a temp `$HOME`: default
  non-interactive run persists `claude`/empty model, `--model` persists
  verbatim, a flagless rerun keeps a previously persisted model, and an
  unknown `--runner` errors.
