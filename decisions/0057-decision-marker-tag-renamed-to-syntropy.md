# ADR-0057: Decision marker tag renamed to `<syntropy-decision>`, both names accepted

**Status**: Accepted
**Date**: 2026-07-20

## Context

[ADR-0055](0055-rename-everflow-to-syntropy.md) deliberately left the
`<everflow-decision>` prompt-marker protocol tag (`ParseDecision`/
`decisionProtocol` in `internal/runner/claude/claude.go`) untouched,
reasoning that renaming it needs the prompt text and the parsing regex
updated atomically, and only takes effect once the daemon that's
already mid-Run is rebuilt — a real risk of an in-flight invocation
being prompted with one tag name and parsed against another.

That reasoning still holds for a single-tag design, but the atomicity
problem disappears entirely if the parser accepts *either* tag name.
This ADR does that.

## Decision

- `decisionProtocol`'s instructions now ask for `<syntropy-decision>`.
- `decisionRE` accepts both `<syntropy-decision>` and the legacy
  `<everflow-decision>`:
  ```go
  regexp.MustCompile(`(?m)^[ \t]*<(?:syntropy|everflow)-decision>\s*(.*?)\s*</(?:syntropy|everflow)-decision>[ \t]*$`)
  ```
- Go's `regexp` package (RE2) has no backreferences, so this can't
  enforce that a matched pair's open and close tag names are the same
  word. In practice a model never produces a mismatched pair (it copies
  the tag verbatim from the instructions), so this is a pragmatic
  tradeoff, not a real correctness gap.
- The internal `SkillCommand` label strings in
  `internal/refactorsweep/workflow.go` (`/everflow-plan`,
  `/everflow-unit`, `/everflow-address-comment`, `/everflow-fix-ci`) —
  missed by ADR-0055's rename sweep since they're distinct from both the
  human-facing `/everflow` control verb and this decision tag — are
  renamed to `/syntropy-*` in the same change. These are prompt labels
  only (never parsed back by a regex), so there's no equivalent
  atomicity concern for them.

## Alternatives considered

- **Wait for a quiet window with zero active Runs, then do a hard
  cutover.** Rejected: unnecessarily blocks the rename on operational
  timing, and this repo's own dogfooding rarely has a truly quiet
  window. Dual-acceptance removes the need to wait at all.
- **Keep `<everflow-decision>` permanently, treat the tag name as
  independent of the project's branding.** Rejected: the whole point of
  a full rename is not leaving a load-bearing "everflow" reference
  behind for cosmetic reasons; the dual-accept approach gets full
  consistency without the atomicity risk, so there's no reason to settle
  for a permanent legacy name.

## Consequences

- Any in-flight invocation prompted before this change (asking for
  `<everflow-decision>`) still parses correctly after the daemon
  rebuilds — no race window.
- New invocations, prompted after this merges and the daemon rebuilds,
  use `<syntropy-decision>` exclusively going forward.
- The legacy tag name in `decisionRE` can be removed later, once
  there's no operational reason to think an old-tag response might
  still be in flight (in practice: any time after the next daemon
  restart with no genuinely long-running invocation in progress) — not
  urgent, since keeping it costs nothing.
- Covered by `TestParseDecision_AcceptsBothTagNames` in
  `internal/runner/claude/claude_test.go`.
