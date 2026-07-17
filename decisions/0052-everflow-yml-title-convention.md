# ADR-0052: `.everflow.yml` holds a per-repo free-text `title_convention`

**Status**: Accepted
**Date**: 2026-07-17

## Context

[ADR-0051](0051-setup-runner-model-config.md) covered the runner/model half
of the `everflow setup` spec goal — persisted per-user to
`~/.everflow/config.yaml`. That ADR explicitly left the third piece, this
repo's PR/MR title convention, for a follow-on increment, since it's a
per-*repo* setting (belongs alongside the code it titles MRs for) rather
than a per-*user* one, and needs its own file rather than overloading
`internal/config`.

A PR/MR title convention (Conventional Commits, a ticket-ID prefix like
`PROJ-123: ...`, no convention at all) is inherently repo-specific — the
whole point is that it should travel with the repo, be visible to anyone
reading it, and not depend on which user's `$HOME` a Run happens to
execute under.

Wiring a persisted convention into MR-title generation
(`internal/refactorsweep/workflow.go`) is a separate concern — this ADR
only covers the `setup` prompting and the on-disk shape. That wiring stays
out of scope, per the increment split, because it touches the runner
prompt-construction path rather than the setup command.

## Decision

**`.everflow.yml`** is a new per-repo config file, living at the root of
whatever directory `everflow setup` is run from (the repo's own working
copy — the same directory a spec's `base_repo` field would point at). Its
shape, defined in `internal/setup`:

```go
type RepoConfig struct {
    TitleConvention string `yaml:"title_convention"`
}
```

It's free text, not an enum — repos phrase their conventions too
differently ("Conventional Commits", "start with the Jira key", "no
convention, keep it short") to usefully constrain up front.

`internal/setup` gained the resolution + write logic, mirroring
`ResolveModel`/`ResolveRunner` from ADR-0051:

- `ResolveTitleConvention(flagConvention string, interactive bool, prompt
  func() (string, error)) (string, error)`: precedence is flag >
  interactive prompt > empty. Unlike `ResolveModel`, there's no "existing
  persisted value" fallback for a non-interactive rerun with no flag —
  `.everflow.yml` starts out not existing at all, so the meaningful
  non-interactive default is "make no claim," not "preserve a previous
  answer."
- `WriteRepoConfig(repoDir, convention string, force bool) (bool, error)`:
  a no-op when `convention` is empty (nothing to persist) or when the file
  already exists and `force` is false — mirroring `installSkill`'s
  file-presence-as-marker pattern (ADR-0048) so a user's hand-edited
  `.everflow.yml` is never silently overwritten by a later `setup` run.

`cmdSetup` in `main.go` wires these to a `--title-convention` flag and an
interactive stdin prompt (shown only when no flag is set and stdin is a
TTY), writing to `os.Getwd()` — `everflow setup` is expected to be run
from inside the repo it's configuring, the same assumption a spec's
`base_repo` makes.

## Alternatives considered

- **Fold `title_convention` into `~/.everflow/config.yaml`.** Rejected —
  that file is per-user (ADR-0051); a title convention is a property of
  the repo, not of whoever happens to run `everflow setup` in it. Two
  contributors on the same repo should see the same convention without
  each running `setup` themselves.
- **A closed enum of known conventions (`conventional-commits`,
  `ticket-prefix`, ...).** Rejected — real repos' conventions don't
  cleanly bucket, and an enum would need a new value shipped in a binary
  release every time a repo wants something slightly different. Free text
  keeps this a config file, not a feature-gated list.
- **Preserve an "existing" value on a flagless non-interactive rerun,
  matching `ResolveModel`'s precedence.** Rejected — `ResolveModel`
  preserves state that already lives in `~/.everflow/config.yaml` from a
  prior run. `.everflow.yml` has no such existing state to read at the
  point `ResolveTitleConvention` runs (reading the file back in would
  duplicate the file-presence check `WriteRepoConfig` already does, for no
  behavioural gain — an empty result from `ResolveTitleConvention` already
  makes `WriteRepoConfig` a no-op, leaving any existing file untouched).

## Consequences

- `everflow setup` run inside a repo, given a non-empty answer
  (interactively or via `--title-convention`), writes `.everflow.yml` at
  that repo's root with a `title_convention:` line.
- An empty answer, or a non-interactive run with no flag, leaves
  `.everflow.yml` absent — no file is created just to record "no
  convention."
- A pre-existing `.everflow.yml` (hand-written or from a prior `setup` run)
  survives a later `setup` run unless `--force` is passed, same guarantee
  the Skill-file install already gives (ADR-0048).
- The file isn't read anywhere yet — MR/PR title generation in
  `internal/refactorsweep/workflow.go` still ignores it. That wiring
  remains a separate follow-on increment, same split ADR-0051 made for the
  runner/model config.

## Tests

- `internal/setup/titleconvention_test.go` — `ResolveTitleConvention`'s
  flag/interactive/non-interactive precedence; `WriteRepoConfig`'s
  empty-convention no-op, first write, no-clobber-without-force, and
  force-overwrite behaviour.
- `main_test.go` — `cmdSetup` end-to-end against a temp cwd: no flag writes
  no file, `--title-convention` persists verbatim, and a pre-existing
  `.everflow.yml` survives a rerun without `--force`.
