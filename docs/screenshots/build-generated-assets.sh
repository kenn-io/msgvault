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

echo "==> Capturing Web UI screenshots..."
# The capture builds the web client and daemon, which rewrites generated
# sources. Run it on a copy of the source files so the checkout stays as it
# was, and so host-platform web/node_modules never reaches the container.
repo_root="$(cd "$docs_root/.." && pwd)"
web_source="$(mktemp -d)"
trap 'rm -rf "$web_source"' EXIT
git -C "$repo_root" ls-files -z --cached --others --exclude-standard |
    (cd "$repo_root" && tar --null -T - -cf -) | tar -xf - -C "$web_source"
docker build -t msgvault-web-capture -f "$script_dir/web-capture.Dockerfile" "$script_dir"
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -e GOPATH=/tmp/go -e GOCACHE=/tmp/go-build -e BUN_INSTALL_CACHE_DIR=/tmp/bun-cache \
    -v "$web_source:/src" -w /src \
    msgvault-web-capture bash docs/screenshots/generate-web-fixture-screenshots.sh
mkdir -p "$generated/web"
cp "$web_source/docs/assets/generated/web/"*.png "$generated/web/"

mkdir -p "$output/assets"
cp -R "$generated/." "$output/assets/"
echo "Wrote $(find "$output/assets" -type f | wc -l) files to $output/assets"
