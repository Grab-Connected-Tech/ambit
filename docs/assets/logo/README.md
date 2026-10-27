# Ambit logo mark: "Scope"

A bold indigo ring (the declared boundary) with an amber dot inside it,
offset toward the upper right (the agent, working within its scope).

## Colors
| Role          | Hex       |
|---------------|-----------|
| Indigo (ring) | `#23206B` |
| Amber (dot)   | `#F2A516` |
| Ring on dark  | `#E6E4FF` |

## Geometry (100 x 100 grid)
- Ring: center 50,50; radius 34; stroke 12 (outer edge 40, inner edge 28)
- Dot: center 58.5,41.5; radius 10

## Files
- `svg/ambit-mark.svg`: master mark for light backgrounds
- `svg/ambit-mark-dark.svg`: for dark backgrounds
- `svg/ambit-avatar.svg`: square indigo tile for GitHub, npm, Discord and similar (cropped to a circle cleanly)
- `svg/ambit-app-icon.svg`: white tile used for the touch and PWA icons
- `png/`: mark at 64 to 1024 px (light and dark), plus avatar at 500 and 1024 px
- `favicon/`: `favicon.ico` (16/32/48), adaptive `favicon.svg`, PNG favicons, `apple-touch-icon.png`, `icon-192.png`, `icon-512.png`, `site.webmanifest`

## HTML
```html
<link rel="icon" href="/favicon.ico" sizes="any">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<link rel="manifest" href="/site.webmanifest">
```

## Usage
- Keep clear space of at least the ring's stroke width on every side.
- Don't rotate the mark, recolor the dot, or move the dot outside the ring.
