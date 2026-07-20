# ADR-0040: Adopt the "Flow Chevrons" brand mark — three-step green-tipped chevrons

**Status**: Superseded by [ADR-0058](0058-syntropy-brand-mark.md) (supersedes [ADR-0013](0013-orbit-brand-mark.md))
**Date**: 2026-07-03

## Context

[ADR-0013](0013-orbit-brand-mark.md) chose the "Orbit" brand mark — a coral
token circling a rounded-square event loop. That framing was tied to the
project's earlier framing ("agent loops that don't poll"). The message
has since shifted: everflow's headline pitch is now the *chain of small,
individually-reviewable MRs* — the "many small merges" story documented
in the current README and DESIGN.md.

The Orbit mark doesn't communicate the chain. It communicates a loop.
Reviewers looking at the README today parse the mark as "self-updating
thing" rather than "MR pipeline". Time to fit the mark to the message.

## Decision

Adopt the "Flow Chevrons" mark:

- **Three chevrons stepping forward** — visually parses as `>>>`, a shell
  prompt or a git `git-log --graph`-adjacent step-forward glyph.
- **Trailing two chevrons in grey** (merged / in-review) — decreasing
  opacity from front to back: 0.32 → 0.62 → 1.0.
- **Leading chevron in merge-green** (`#159a5c`, "in flight") — the MR
  currently open on the platform.
- **Ink** `#17191e` (near-black) for the trailing chevrons in the
  light-mode variant.

Construction rules:

- Chevrons sit on a 3-unit rhythm: equal stroke, equal gap.
- 30° apex per stroke (60° tip angle) — the same shape as the `>`
  character in a monospaced font.
- Never re-space or re-weight individual arrows relative to each other.
- Transparent background across all variants.

Light-mode and dark-mode assets are separate SVG files; the leading-green
stays `#159a5c` in both (single-hue brand identity, contrast-safe on both
backgrounds). Dark-mode uses white with matching opacities for the
trailing chevrons.

## Alternatives considered

- **Keep Orbit and add a chain glyph elsewhere.** The mark IS the brand
  identity in most places it's seen (README banner, GitHub social card,
  favicon). Adding a second glyph splits attention and doesn't fix the
  parse. Rejected.
- **Animated variant of Flow Chevrons.** Considered — chevrons could
  fade in from left to right on a slow loop, or the leading green could
  pulse. Rejected for the mark itself: SVG animations don't render in
  GitHub's markdown img tags and would need `.gif` fallback (the very
  thing we're removing here). A daemon-log ASCII animation could carry
  the "motion" story where a mark alone can't. Deferred.
- **Include a text lockup ("everflow" wordmark) alongside the mark.**
  The design brief has one (see the PDF); it's implemented as a text
  wordmark rather than baked into the mark SVG so it inherits the
  README's font stack cleanly. No SVG-embedded wordmark for now.

## Consequences

- `logo/mark.svg`, `logo/mark-dark.svg`, `logo/favicon.svg` all rewritten
  to the new design. `logo/mark.gif`, `logo/mark-dark.gif`, and
  `logo/mark-static.svg` deleted — the new SVGs are self-contained and
  render everywhere.
- `README.md`'s `<picture>` block now sources SVG. Lighter payload; no
  loss of quality; scales cleanly on high-DPI displays.
- ADR-0013 is marked Superseded. The Orbit mark's rationale remains
  historically valid (the coral-on-ink palette was thoughtful) but is
  no longer the current brand.
- Any external references to `logo/*.gif` in third-party docs / social
  cards break. This is a personal-project repo without external
  branding contracts; acceptable.
- Follow-up work (not required for this ADR):
  - App-icon variant (rounded square, filled green background, white
    chevrons) — used for GitHub Marketplace / OG cards. Design brief
    includes it; not implemented here.
  - "Stacked" lockup (mark above wordmark) for narrow contexts. Design
    brief includes it; not implemented here.
