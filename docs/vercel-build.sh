#!/usr/bin/env bash
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
site_dir="$script_dir/site"

"$script_dir/assets/hydrate-assets.sh"

rm -rf "$site_dir"
MSGVAULT_DOCS_SITE_DIR="site/docs" "$script_dir/zensical-docs.sh" build

# The sitemap indexes every tier, so serve it from the standard root path
# crawlers probe, as the pre-tiered site did.
mv "$site_dir/docs/sitemap.xml" "$site_dir/sitemap.xml"

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

# Local credential/secret artifacts that must never enter the published site.
# Keep in sync with credential_globs in docs/zensical-docs.sh and
# FORBIDDEN_SITE_FILENAMES in docs/scripts/check_built_site.py.
credential_globs=(
  'client_secret*.json'
  'oauth_client*.json'
  'credentials*.json'
  'service_account*.json'
  'service-account*.json'
  'token.json'
  'tokens.json'
  'token-*.json'
  '*.pem'
  '*.key'
  '*.crt'
  '*.cer'
  '*.der'
  '*.p12'
  '*.pfx'
  '*.p8'
  '*.jks'
  '*.keystore'
  '*.ppk'
  'id_rsa*'
  'id_dsa*'
  'id_ecdsa*'
  'id_ed25519*'
  '*.tfstate'
  '*.tfstate.backup'
  '*.tfvars'
)

# The website copy is recursive over the working tree, so prune dotfiles and
# credential-pattern files at any depth before publishing.
prune_expr=('(' -name '.*')
for glob in "${credential_globs[@]}"; do
  prune_expr+=(-o -iname "$glob")
done
prune_expr+=(')')
find "$site_dir" -depth "${prune_expr[@]}" -exec rm -rf {} +

# Fail the build if anything slipped past the prune. This is the same
# inventory gate check-docs.sh runs; here it guards every deployment.
if command -v python3 >/dev/null 2>&1; then
  python_bin="python3"
else
  python_bin="python"
fi
"$python_bin" -c "import sys; sys.path.insert(0, '$script_dir/scripts'); \
import check_built_site; check_built_site.check_public_site_file_inventory()"
