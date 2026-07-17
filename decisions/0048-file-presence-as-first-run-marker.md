# ADR-0048: File-presence-as-marker for first-run Skill install

**Status**: Accepted
**Date**: 2026-07-17

## Context

[ADR-0002](0002-distribute-as-claude-skill.md) decided everflow ships a
Claude Code Skill bundle at `~/.claude/skills/everflow/SKILL.md`, installed
automatically so the user doesn't have to know the Skill exists before
everflow becomes useful. That ADR didn't say *how* "first invocation" gets
detected, or what an explicit re-install/override command looks like. Both
landed in `internal/setup` and `main.go`; this ADR records the mechanism
actually chosen, now that reviewers need to read the rationale without
reverse-engineering it from the diff.

## Decision

**The Skill file's own presence is the marker — there is no separate
`.setup-complete` sentinel.**

`internal/setup.installSkill` (backing both `EnsureClaudeSkill` and
`InstallClaudeSkill`) checks whether `~/.claude/skills/everflow/SKILL.md`
already exists:

- If it doesn't, write it (creating the `skills/everflow/` directory tree).
- If it does, and `force` is false, skip the write entirely.

A single detect/target/content triple drives this for the one integration
that exists today:

| detect (has-run marker)              | target (install path)                  | content (embedded)         |
|---------------------------------------|-----------------------------------------|-----------------------------|
| `~/.claude/skills/everflow/SKILL.md` exists | `~/.claude/skills/everflow/SKILL.md` | `SKILL.md`, via `go:embed` |

Detect and target are the same path on purpose — there's exactly one file
this integration ever writes, so a dedicated marker would just be a second
thing that can drift out of sync with the first. If a second integration
bundle is added (per-runner, per ADR-0002's "Consequences"), it gets its own
detect/target/content triple in its own package rather than a shared
sentinel file, for the same reason.

Two entry points share `installSkill`, differing only in when they run and
what "already installed" means:

- **`EnsureClaudeSkill(home)`** — called on every `everflow` invocation
  except `setup` itself (see `main.go`). Additionally requires `~/.claude`
  to exist (i.e. Claude Code is actually installed on this host); if not, it
  is a silent no-op — everflow must not create `~/.claude` as a side effect
  of an unrelated command. `force` is always `false` here: an automatic
  background hook must never clobber a file the user may have hand-edited.
- **`InstallClaudeSkill(home, force)`** — called only from `everflow setup`.
  Creates `~/.claude` if missing, since an explicit setup command is
  allowed to bootstrap the whole tree. Takes `force` from a CLI flag.

### `everflow setup --force`

`--force` is the only way to overwrite an existing `SKILL.md`. Without it,
`setup` on an already-installed host is a no-op that prints the existing
path — safe to run repeatedly (e.g. from a script) without clobbering local
edits. With it, the bundled `SKILL.md` always wins, which is the explicit
"give me the latest version" escape hatch for a user who wants to discard
local edits and pull the current bundled copy.

### Stderr summary on auto-install

When `EnsureClaudeSkill` performs the install (first run only), the caller
in `main.go` prints a one-line summary to stderr — not stdout, so it doesn't
pollute output a script might parse — telling the user what happened and
that `everflow setup` exists to reinstall or customize. This is the only
user-visible signal that first-run install happened at all; there is no
separate "welcome" flow.

## Alternatives considered

- **Separate `~/.claude/everflow/.setup-complete` sentinel file** — decouples
  "have we run setup" from "does the Skill file exist," which would allow
  detecting and reinstalling a deleted Skill file without re-triggering the
  whole first-run flow. Rejected for v1: it's a second piece of state that
  can desync from the first (sentinel present, Skill file deleted by hand →
  Claude Code silently loses the integration with no signal), for no
  present benefit — nothing today depends on distinguishing "never set up"
  from "set up, then the file went away." Revisit if a future integration
  needs multi-step setup state that can't be represented by a single file's
  existence.
- **Version-stamped marker** (e.g. embed a version comment in `SKILL.md` and
  diff it) — would let `EnsureClaudeSkill` auto-upgrade a stale bundled copy
  without `--force`. Rejected: conflicts with the "never overwrite local
  edits without an explicit ask" property that makes the silent auto-install
  safe to run unattended.

## Consequences

- A user who deletes `~/.claude/skills/everflow/SKILL.md` by hand will get
  it silently reinstalled on their very next everflow invocation — deleting
  the file is not a durable way to opt out of the Skill. There is currently
  no flag to disable auto-install; that would need its own opt-out marker
  distinct from the install target, which is exactly the complexity the
  "presence-is-marker" choice avoids until it's actually needed.
- Adding a second coding-agent integration (Codex, Qwen, ...) means adding a
  parallel detect/target/content triple in that agent's own package, per
  ADR-0002's consequences — not extending `internal/setup`'s marker logic to
  cover multiple files.
