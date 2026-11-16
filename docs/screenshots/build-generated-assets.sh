#!/usr/bin/env bash
# Build every file on the docs-generated-assets branch into OUTPUT_DIR/assets.
# The host needs only Bash, Git and Docker, so automated screenshot runners can
# call this through `make docs-screenshots` with DOCS_SCREENSHOTS_OUTPUT set.
set -euo pipefail

output="${1:?usage: build-generated-assets.sh OUTPUT_DIR}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
generated="$docs_root/assets/generated"

rm -rf "$generated"
bash "$script_dir/generate-all.sh"

echo "==> Rendering concept diagrams..."
docker build -t msgvault-diagrams "$docs_root/diagrams"
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -e CHROME=/usr/bin/chromium -e CHROME_ARGS=--no-sandbox \
    -v "$docs_root:/docs" -w /docs/diagrams \
    msgvault-diagrams bash build.sh

mkdir -p "$output/assets"
cp -R "$generated/." "$output/assets/"
echo "Wrote $(find "$output/assets" -type f | wc -l) files to $output/assets"
