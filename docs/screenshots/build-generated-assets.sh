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
# Use the Playwright image whose Chromium matches the pinned test package.
playwright_version="$(sed -n 's/.*"@playwright\/test": *"\([0-9][^"]*\)".*/\1/p' "$repo_root/web/package.json")"
[[ -n "$playwright_version" ]] || { echo "cannot read @playwright/test version from web/package.json" >&2; exit 1; }
# Build before creating the capture directory, which sits in this build context.
docker build -t msgvault-web-capture --build-arg PLAYWRIGHT_VERSION="$playwright_version" \
    -f "$script_dir/web-capture.Dockerfile" "$script_dir"
# Keep the mounted directories inside the checkout, like the other Docker
# steps. A runner's Docker daemon may not see the runner's private /tmp, and
# then mounts an empty directory instead.
capture_root="$(mktemp -d "$script_dir/web-capture.XXXXXX")"
trap 'rm -rf "$capture_root"' EXIT
web_source="$capture_root/source"
capture_output="$capture_root/output"
mkdir "$web_source" "$capture_output"
git -C "$repo_root" ls-files -z --cached --others --exclude-standard |
    while IFS= read -r -d '' path; do
        # Skip tracked files deleted from the working tree.
        if [[ -e "$repo_root/$path" || -L "$repo_root/$path" ]]; then
            printf '%s\0' "$path"
        fi
    done |
    (cd "$repo_root" && tar --null -T - -cf -) | tar -xf - -C "$web_source"
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -e GOPATH=/tmp/go -e GOCACHE=/tmp/go-build -e BUN_INSTALL_CACHE_DIR=/tmp/bun-cache \
    -e MSGVAULT_DOCS_SCREENSHOT_OUTPUT_DIR=/capture \
    -v "$web_source:/src" -v "$capture_output:/capture" -w /src \
    msgvault-web-capture bash docs/screenshots/generate-web-fixture-screenshots.sh
# The publisher pushes these files to a public branch. Copy only regular PNG
# files, so a link the container left behind cannot pull in a host file.
mkdir -p "$generated/web"
for capture in "$capture_output"/*; do
    if [[ -L "$capture" || ! -f "$capture" ]] ||
        [[ "$(head -c 8 "$capture" | od -An -tx1 | tr -d ' \n')" != 89504e470d0a1a0a ]]; then
        printf 'Web UI capture is not a regular PNG file: %s\n' "${capture##*/}" >&2
        exit 1
    fi
    cp "$capture" "$generated/web/"
done

mkdir -p "$output/assets"
cp -R "$generated/." "$output/assets/"
echo "Wrote $(find "$output/assets" -type f | wc -l) files to $output/assets"
