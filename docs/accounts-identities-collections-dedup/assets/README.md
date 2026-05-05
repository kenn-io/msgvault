# Diagram assets

The PNGs in this directory are rendered from the matching HTML files. Each
HTML file is self-contained — no external CSS, no JavaScript, no fonts to
load. Re-rendering is a single headless-Chrome screenshot.

## Files

| File                                | What it shows                                                                                  |
| ----------------------------------- | ---------------------------------------------------------------------------------------------- |
| `account-collection-concept.html`   | The accounts vs. collections distinction: per-import sources and the named groups across them. |
| `deduplication-concept.html`        | Dedup before/after, with the safety-ladder strip showing this is rung 02 of 4.                 |
| `safety-ladder-concept.html`        | The full data safety ladder: rung 00 backup, then 4 explicit rungs.                            |
| `survivor-selection-concept.html`   | Survivor selection: the sent-message eligibility filter, then the priority list.               |

Each PNG is rendered at 1600 px wide. Heights vary because the panel
content drives layout — render with a tall viewport (1700 is enough for
all four) so nothing is clipped, then trim the bottom whitespace so the
PNG ends with the same padding it starts with.

## Re-rendering

Headless Chrome plus ImageMagick. The commands:

```bash
"$CHROME" --headless=new \
  --disable-gpu \
  --hide-scrollbars \
  --window-size=1600,1700 \
  --default-background-color=0a0a0aff \
  --screenshot=safety-ladder-concept.png \
  "file://$(pwd)/safety-ladder-concept.html"

# Trim vertical whitespace, restore the full 1600 px width, and pad
# 82 px of breathing room above and below.
magick safety-ladder-concept.png \
  -background "#0a0a0a" -fuzz 2% -trim +repage \
  -gravity center -extent 1600x \
  -gravity north -splice 0x82 \
  -gravity south -splice 0x82 \
  safety-ladder-concept.png
```

`$CHROME` is the path to the Chrome (or Chromium) binary. Common
locations:

| Platform | Path                                                            |
| -------- | --------------------------------------------------------------- |
| macOS    | `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`  |
| Linux    | `/usr/bin/google-chrome` or `/usr/bin/chromium-browser`         |
| Windows  | `C:\Program Files\Google\Chrome\Application\chrome.exe`         |

A small shell helper that picks one and re-renders all four:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

CHROME="${CHROME:-}"
if [ -z "$CHROME" ]; then
  for c in \
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    "/usr/bin/google-chrome" \
    "/usr/bin/chromium-browser" \
    "/usr/bin/chromium"; do
    if [ -x "$c" ]; then CHROME="$c"; break; fi
  done
fi
[ -n "$CHROME" ] || { echo "set CHROME to your Chrome/Chromium binary"; exit 1; }

for f in account-collection-concept deduplication-concept safety-ladder-concept survivor-selection-concept; do
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars \
    --window-size=1600,1700 --default-background-color=0a0a0aff \
    --screenshot="${f}.png" "file://$(pwd)/${f}.html"
  magick "${f}.png" \
    -background "#0a0a0a" -fuzz 2% -trim +repage \
    -gravity center -extent 1600x \
    -gravity north -splice 0x82 \
    -gravity south -splice 0x82 \
    "${f}.png"
  echo "rendered ${f}.png"
done
```

## Editing the diagrams

Edit the HTML directly — the styles are inline, the data is hand-written,
and there is no build step. The shared palette lives in the `:root` block
of each file and follows the `msgvault.io` site:

```css
--bg: #0a0a0a;          /* page background */
--surface-1: #161616;   /* panel surface */
--surface-2: #212121;   /* nested surface */
--hairline: #3a3a3a;    /* borders */
--text: #e8e8e8;        /* body text */
--text-2: #c0c0c0;      /* secondary text */
--muted: #a0a0a0;       /* hints, eyebrows */
--accent: #ffffff;      /* headings, key emphasis */
```

If you change palette tokens, change them across all four files so the
set stays visually coherent.

The design here should be refreshed periodically against `msgvault.io`
— the site is the source of truth for type, palette, and surface
treatments. If the site evolves, pull the updated tokens back into these
diagrams so they keep reading as part of the same product.
