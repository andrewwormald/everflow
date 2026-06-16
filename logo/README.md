# Logo

Brand mark for everflow — the **Orbit**: a coral token forever circling an ink event loop. Designed in Claude Design (claude.ai/design); the original delivery sheet is preserved at `Everflow Logo.html` (not in this repo to keep size down; ask if you need it).

## Files

| File | Use |
|---|---|
| `mark.gif` | Animated GIF, paper background. Use in READMEs, social cards, anywhere GIF rendering is reliable. |
| `mark-dark.gif` | Same animation, ink background. Pair with the above in a `<picture>` for color-scheme switching. |
| `mark.svg` | Animated SVG, paper background. Smaller than the GIF, renders sharply at any size; use in HTML/web contexts where SVG SMIL is preserved. |
| `mark-dark.svg` | Animated SVG, ink background variant. |
| `mark-static.svg` | Single-frame SVG (comet frozen at the top of the loop). Use when animation isn't appropriate (PDF export, print, performance-sensitive contexts). |
| `favicon.svg` | 32px-optimised single-mote variant — comet simplifies to one dot at small sizes for legibility. |

## Palette

| Name | Hex | Use |
|---|---|---|
| Ink | `#16263d` | Loop stroke (light bg), background (dark bg) |
| Coral | `#ff5a3c` | Comet — the orbiting token |
| Amber | `#ffb13c` | Accent (sparingly) |
| Paper | `#fbf8f3` | Background (light bg), loop stroke (dark bg) |

## Animation

- Duration: **3.4s** per orbit
- Three coral motes ride the loop: head (radius 9.5), trail (radius 6, 58% opacity), tail (radius 4, 30% opacity)
- Trail motes are offset by `-3.18s` and `-3.04s` respectively (slightly *behind* the head as the animation loops)

## Regenerating the GIFs

A small script at `/tmp/gif-gen/` (puppeteer-core + gifenc) captured 82 frames at 24fps over the animation cycle for each variant. The script is not committed; if you need to regenerate (e.g. resize, change palette), the design colours and SVG paths are the source of truth — re-run with whatever pipeline fits.

Quick pipeline reference:

1. Launch headless Chrome on a page hosting the animated SVG.
2. Pause SVG SMIL clocks (`svg.pauseAnimations()`), then advance with `svg.setCurrentTime(t)` to capture deterministic frames.
3. Encode with `gifenc` (or ffmpeg, or ImageMagick — any GIF encoder with a decent palette quantiser).
