# ADR-0055: Rename the project from everflow to syntropy

**Status**: Accepted
**Date**: 2026-07-19

## Context

The project shipped under the name "everflow" through ADR-0054: Go module
path `github.com/andrewwormald/everflow`, the `everflow` binary/CLI, the
`/everflow` control-verb prefix used in MR comments, the `~/.everflow`
config/data directory, and every forward-facing doc (README, AGENTS,
DESIGN, TROUBLESHOOTING). The name changed to "syntropy" — module path,
binary name, control-verb prefix, and config directory were updated across
the codebase over prior increments, with docs brought into line last
(commit 0637326, "docs: rename everflow to syntropy across forward-facing
docs").

## Decision

Rename everflow to syntropy everywhere a user or contributor encounters
the name going forward, and leave the historical record untouched.

Concretely:

- Go module path, binary name, `/everflow` control-verb prefix, and
  `~/.everflow` config/data directory conventions all became `syntropy`
  equivalents.
- Forward-facing docs (README.md, AGENTS.md, DESIGN.md,
  TROUBLESHOOTING.md) were updated to reference syntropy consistently.
- Historical ADRs (0001–0054) keep the word "everflow" as written — they
  describe decisions made under that name, and rewriting them would
  falsify the record. Only forward-facing docs and code were renamed;
  ADR history is not retroactively edited for branding.
- Commit messages and branch names from before the rename (e.g.
  `everflow/<runID>/increment-N`) are likewise left alone as historical
  artifacts.

## Alternatives considered

- **Rewrite historical ADRs to say "syntropy" throughout.** Rejected —
  ADRs are a record of what was decided and why, at the time it was
  decided. Editing them to match current branding would make cited
  context (e.g. "adopt the everflow branding" decisions) misleading and
  erase the fact that a rename happened at all.
- **Add a superseding ADR-per-renamed-decision.** Rejected as overkill —
  the rename is a single mechanical decision, not N separate decisions
  about N prior ADRs. One ADR documenting the policy is sufficient.

## Consequences

- Anyone reading ADRs 0001–0054 will see "everflow" and should understand
  that's the pre-rename name for the same project described from
  ADR-0055 onward — this ADR is the pointer that explains why the name
  differs.
- Any future search-and-replace tooling or agent sweep must not touch
  `decisions/0001-*.md` through `decisions/0054-*.md` for the word
  "everflow" — those files are intentionally frozen.
- New ADRs, docs, and code from this point on use "syntropy" exclusively.
