# Logo

Brand mark for everflow — the **Flow Chevrons**: three chevrons stepping forward, trailing two grey (merged / in-review), leading one merge-green (in flight). Reads as a shell prompt (`>`). Designed via Claude Design (claude.ai/design). See [ADR-0040](../decisions/0040-flow-chevrons-brand-mark.md) for the rationale (supersedes the earlier "Orbit" mark from ADR-0013).

## Files

| File | Use |
|---|---|
| `wordmark.svg` | **Primary lockup** — chevrons + "everflow" wordmark side by side. Use as the README banner and anywhere the mark appears with the name. Light-mode variant. |
| `wordmark-dark.svg` | Primary lockup, dark-mode variant. Pair with `wordmark.svg` in a `<picture>` for `prefers-color-scheme` switching. |
| `mark.svg` | Standalone mark — chevrons only, no wordmark. Use where the "everflow" name is already visible in surrounding text or UI chrome. Light-mode. |
| `mark-dark.svg` | Standalone mark, dark-mode variant. |
| `favicon.svg` | 16-px-optimised single-chevron variant — the three-chevron rhythm doesn't survive at favicon scale, so this reduces to the leading green chevron alone. |

All variants have **transparent backgrounds** so they lay cleanly over any surface.

## Palette

| Name | Hex | Use |
|---|---|---|
| Ink | `#17191e` | Trailing chevrons (light-mode) |
| Merge-green | `#159a5c` | Leading chevron (both variants) — the accent |
| — | `#ffffff` | Trailing chevrons (dark-mode) |

Trailing chevrons use opacity `0.32` (rearmost) and `0.62` (middle) against the ink or white base to convey "faded, already-merged/in-review". Leading chevron is full-opacity green.

## Construction

- Three chevrons on a 3-unit rhythm: **equal stroke, equal gap**
- 30° apex per stroke (60° tip angle)
- Same shape as the `>` character in a monospaced font
- **Never** re-space or re-weight individual arrows relative to each other

## Don'ts

- No gradients
- No skew / rotation of individual chevrons
- Keep contrast — don't drop the trailing-chevron opacity below `0.30` (readability falls off) or raise it above `0.70` (loses the fade-back-to-merged story)

## Suggested lockups (not implemented as SVGs)

- **Primary**: mark alongside the word "everflow" set in a monospaced typeface at the same cap-height as the chevron.
- **Stacked**: mark centred, "everflow" wordmark below, "refactor chains" subline underneath in a smaller weight.
- **App icon**: rounded-square (radius ≈ 20% of side), filled `#159a5c`, three white chevrons centred. Not yet in the repo.
