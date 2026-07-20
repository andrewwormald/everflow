# ADR-0058: Adopt the "Syntropy" brand mark — chaos converging into order

**Status**: Accepted (supersedes [ADR-0040](0040-flow-chevrons-brand-mark.md))
**Date**: 2026-07-20

## Context

[ADR-0055](0055-rename-everflow-to-syntropy.md) renamed the project from
everflow to syntropy. The "Flow Chevrons" mark from
[ADR-0040](0040-flow-chevrons-brand-mark.md) carried the everflow name in
its wordmark lockup and its `>>>` metaphor was tied to the "many small
merges" framing rather than the new name. A mark change was needed to
match the rename, and a chance to pick a metaphor that reads on its own
without leaning on the old name.

## Decision

Adopt the **Syntropy** mark: nine blocks, scattered, rotated, and
desaturated on the left, converging into a precise aligned row in cyan
on the right — chaos resolving into order, literalizing "syntropy"
(negative entropy) rather than referencing MR mechanics.

Construction:

- Nine rounded-square blocks on an implied grid; the leftmost blocks
  vary in rotation, size, corner radius, and a grey-to-blue-to-cyan
  colour ramp; the rightmost two are unrotated, identically sized, and
  flat cyan (`#00d2d3`).
- A soft Gaussian-blur glow duplicate of the final block sits behind it,
  reading as the point of arrival/resolution.
- Same construction across all three asset types (`mark.svg`,
  `wordmark.svg`, `favicon.svg`); `favicon.svg` reduces to three blocks
  (dark → mid → cyan) since nine don't survive 16px.
- Wordmark lockup sets "syntropy" in JetBrains Mono (monospace, matches
  the project's CLI-first identity) immediately after the mark.
- Transparent background across all variants, matching ADR-0040's rule.

Light-mode and dark-mode variants are separate SVG files, as in
ADR-0040; the cyan accent and colour ramp are identical in both — only
the wordmark ink flips (near-black on light, white/light on dark).

## Alternatives considered

- **Keep the Flow Chevrons `>>>` mark, just re-label the wordmark text.**
  Cheapest option — no new mark design. Rejected: the chevron metaphor
  ("MR pipeline, step-forward") was chosen for the old *everflow*
  framing; it doesn't carry over to "syntropy" and would read as an
  unexplained leftover once the name no longer matches the shape it was
  built to justify.
- **Animate the convergence (blocks sliding into place on load).**
  Considered, since "converging" is inherently a motion concept.
  Rejected for the same reason ADR-0040 rejected chevron animation:
  GitHub's markdown `<img>`/`<picture>` doesn't run SVG animations, and
  reintroducing a `.gif` fallback is the exact churn ADR-0040 removed.
  A single static frame — mid-convergence, already resolved on the right
  — carries the idea without needing motion.
- **Single-colour mark (no grey → cyan ramp).** Simpler, but loses the
  "chaos → order" read entirely; a flat mark would just look like nine
  identical squares. The ramp is load-bearing for the metaphor, kept.

## Consequences

- `logo/mark.svg`, `logo/mark-dark.svg`, `logo/wordmark.svg`,
  `logo/wordmark-dark.svg`, and `logo/favicon.svg` all rewritten to the
  new design (done across increments 1–3 of this run).
- `README.md`'s hero `<picture>` block already points at
  `logo/wordmark.svg` / `logo/wordmark-dark.svg`; no further README
  change needed beyond what those increments already made.
- ADR-0040 is marked Superseded. Its construction rules (equal
  stroke/gap, transparent background, separate light/dark files) remain
  valid project conventions; only the specific chevron shape and
  palette are replaced.
- `logo/README.md` still describes the old Flow Chevrons mark and the
  everflow name — it was not updated as part of this ADR's increments
  and is now stale. Flagged as follow-up work, not fixed here.
- Any external references to the old chevron SVGs (social cards, past
  screenshots) go stale. Personal-project repo, no external branding
  contracts; acceptable, matching ADR-0040's own precedent.
