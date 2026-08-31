#!/usr/bin/env bash
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
site_dir="$script_dir/site"

"$script_dir/assets/hydrate-assets.sh"

rm -rf "$site_dir"
MSGVAULT_DOCS_SITE_DIR="site/docs" "$script_dir/zensical-docs.sh" build

# The marketing and guide tiers ship as static files from website/; the
# zensical docs tier lives under /docs/.
website_dir="$repo_root/website"
for entry in index.html index.md guide guide.md llms.txt favicon.svg assets fonts scripts styles; do
  source_path="$website_dir/$entry"
  if [[ ! -e "$source_path" ]]; then
    printf 'missing website source: %s\n' "$entry" >&2
    exit 1
  fi
  cp -R "$source_path" "$site_dir/$entry"
done
