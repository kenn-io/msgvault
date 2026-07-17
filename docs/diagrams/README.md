# Concept diagrams

The PNGs under `docs/assets/generated/concepts/` are rendered from the HTML
files in this directory. Each HTML file is self-contained: inline CSS, no
JavaScript, no external fonts. Re-rendering is a single headless-Chrome
screenshot plus an ImageMagick trim/pad.

## Files

| Source                              | Rendered PNG                                                       | Used on                                  |
| ----------------------------------- | ------------------------------------------------------------------ | ---------------------------------------- |
| `account-collection-concept.html`   | `docs/assets/generated/concepts/account-collection-concept.png`    | Accounts, Identities, and Collections    |
| `deduplication-concept.html`        | `docs/assets/generated/concepts/deduplication-concept.png`         | Deduplication                            |
| `safety-ladder-concept.html`        | `docs/assets/generated/concepts/safety-ladder-concept.png`         | Deduplication (the five-rung safety ladder) |
| `survivor-selection-concept.html`   | `docs/assets/generated/concepts/survivor-selection-concept.png`    | Deduplication (survivor selection)       |
| `oauth-multi-account-concept.html`  | `docs/assets/generated/concepts/oauth-multi-account-concept.png`   | Accounts page and the OAuth Setup guide  |

## Building

```bash
./build.sh                 # render all diagrams to ../assets/generated/concepts/
CHROME=/path/to/chrome ./build.sh
```

Requirements:

- **Google Chrome or Chromium.** `build.sh` looks in the common macOS and Linux
  locations; override with the `CHROME` environment variable.
- **ImageMagick** (`magick`).

Each PNG is rendered at **2x (retina)**: 3200px wide. The script renders on a
tall canvas, trims the uniform `#0a0a0a` background, restores the full width,
and pads 82px of breathing room above and below so every diagram ends with the
same margin it starts with. Heights vary because the panel content drives
layout.

To render by hand (the safety ladder, for example):

```bash
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
"$CHROME" --headless=new --disable-gpu --hide-scrollbars \
  --force-device-scale-factor=2 --window-size=1600,2200 \
  --default-background-color=0a0a0aff \
  --screenshot=/tmp/out.png \
  "file://$(pwd)/safety-ladder-concept.html"

magick /tmp/out.png \
  -background "#0a0a0a" -fuzz 2% -trim +repage \
  -gravity center -extent 3200x \
  -gravity north -splice 0x164 -gravity south -splice 0x164 \
  ../assets/generated/concepts/safety-ladder-concept.png
```

## Editing

Edit the HTML directly: the styles are inline, the data is hand-written, and
there is no build step beyond `build.sh`. The shared palette lives in the
`:root` block of each file and follows the `msgvault.io` site:

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

If you change palette tokens, change them across all files so the set stays
visually coherent. After editing, run `./build.sh` from this directory. The HTML
sources live on the main docs branch; the rendered PNGs are published from the
orphan generated-assets branch through `docs/screenshots/update-generated-assets-branch.sh`.

The pages embed each PNG inside a `<figure data-lightbox>` so it can be clicked
to zoom; keep the `alt` text in sync with the diagram when you change it.
