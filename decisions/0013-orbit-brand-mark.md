# ADR-0013: Adopt the "Orbit" brand mark and warm-coral palette

**Status**: Accepted
**Date**: 2026-06-15

## Context

Everflow needs visual identity for the README, future docs, and any GitHub
social card. The brief: modern, clean, simple, and *fun*; not enterprise-grey;
must evoke "long-running agent loops that don't poll" without literal
clichés (no infinity symbol, no water-flow drops, no gears).

Five directions were explored in Claude Design (claude.ai/design); the
selected direction is "Orbit": a coral token forever circling an ink event
loop. Three motes ride the loop (head + two trailing) — fired, paused,
resumed, always coming back around. Monoline construction so the mark holds
from large down to 16px.

## Decision

Adopt the **Orbit** mark as the everflow brand. The palette is:

| Name | Hex | Use |
|---|---|---|
| Ink | `#16263d` | Loop stroke on light bg; background on dark bg |
| Coral | `#ff5a3c` | The orbiting token — always |
| Amber | `#ffb13c` | Accent (sparingly) |
| Paper | `#fbf8f3` | Background on light bg; loop stroke on dark bg |

Animation: 3.4s per orbit, three coral motes with offsets `-3.04s`, `-3.18s`,
`0s` (so the trail is *behind* the head as the loop repeats).

Assets live under [`logo/`](../logo/); see its README for the full file
inventory.

## Alternatives considered

- **Luno electric blue** — would give brand family resemblance to the parent.
  Killed: everflow is a personal project, not a Luno product, and the warm
  coral is more inviting than enterprise infrastructure-blue.
- **Static logo only** — simpler, smaller files. Killed: the animated comet
  *is* the metaphor (a token forever circling). Losing the animation in the
  hero loses half the meaning.
- **Animated SVG only, no GIF** — SVG is smaller and sharper, but GIFs
  render reliably everywhere (social cards, Slack previews, RSS readers).
  We ship both; README leads with the GIF for breadth, embeds reach for the
  SVG when they want sharpness.
- **Other four design directions explored in Claude Design** — Signal &
  callback (jump forward + return), Event/node graph, plus two more.
  Available in `Everflow Logo - Exploration.html` from the source bundle if
  ever needed.

## Consequences

- The `logo/` directory is the canonical source of brand assets. README,
  social cards, future docs all link back to it — no copies elsewhere in
  the repo.
- The original Fracktif font from the Claude Design bundle is *not*
  redistributed (it's a paid font); the README wordmark uses plain
  markdown headings instead. If a flattened-path SVG wordmark is needed
  later, it can be rendered once and committed without the font files.
- Changes to the mark (re-colour, simplification, new variants) should be
  reflected in `logo/README.md` and noted here as a follow-up ADR
  superseding this one.
- The "Never polls" tagline tracks the design language. Use it as the
  README hero strapline; don't dilute with alternative phrasings unless
  there's a reason.
