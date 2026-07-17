#!/usr/bin/env bash
#
# Build the concept diagrams.
#
# Renders every diagrams/*-concept.html to a 2x (retina) PNG in
# ../assets/generated/concepts/, where the docs pages embed them. Each HTML file is
# self-contained (inline CSS, no JS, no external fonts), so a render is a
# single headless-Chrome screenshot plus an ImageMagick trim/pad.
#
# Requirements: Google Chrome (or Chromium) and ImageMagick (`magick`).
# Usage:        ./build.sh            # build all diagrams
#               CHROME=/path ./build.sh
#
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
command -v magick >/dev/null 2>&1 || { echo "ImageMagick (magick) is required"; exit 1; }

OUT="../assets/generated/concepts"
mkdir -p "$OUT"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

shopt -s nullglob
matched=0
for html in *-concept.html; do
  matched=1
  name="${html%.html}"
  tmp="$tmpdir/$name.png"

  # Render at 2x on a tall canvas so nothing is clipped. The page paints its
  # own #0a0a0a background; the tall viewport just leaves trimmable margin.
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars \
    --force-device-scale-factor=2 --window-size=1600,2200 \
    --default-background-color=0a0a0aff \
    --screenshot="$tmp" "file://$(pwd)/$html"

  # Trim the uniform background, restore the full 1600px (x2 = 3200) width,
  # then pad 82px (x2 = 164) of breathing room above and below so every
  # diagram ends with the same margin it starts with.
  magick "$tmp" \
    -background "#0a0a0a" -fuzz 2% -trim +repage \
    -gravity center -extent 3200x \
    -gravity north -splice 0x164 \
    -gravity south -splice 0x164 \
    "$OUT/$name.png"

  echo "built $OUT/$name.png"
done

[ "$matched" = 1 ] || { echo "no *-concept.html files found in $(pwd)"; exit 1; }
