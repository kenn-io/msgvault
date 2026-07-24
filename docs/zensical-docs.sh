#!/usr/bin/env bash
set -euo pipefail

command_name="${1:-}"
if [[ "$command_name" != "build" && "$command_name" != "serve" ]]; then
  printf 'usage: %s {build|serve} [zensical args...]\n' "$0" >&2
  exit 2
fi
shift || true

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$script_dir"
site_dir="${MSGVAULT_DOCS_SITE_DIR:-site}"

if [[ -n "${VIRTUAL_ENV:-}" && -x "$VIRTUAL_ENV/bin/zensical" ]]; then
  zensical_bin="$VIRTUAL_ENV/bin/zensical"
elif [[ -x "$docs_root/.venv/bin/zensical" ]]; then
  zensical_bin="$docs_root/.venv/bin/zensical"
elif command -v zensical >/dev/null 2>&1; then
  zensical_bin="zensical"
else
  printf 'zensical not found; install with: cd docs && uv sync --frozen --no-dev\n' >&2
  exit 127
fi

tmp_docs=""
tmp_config_base=""
tmp_config=""

cleanup() {
  if [[ -n "$tmp_docs" ]]; then
    rm -rf "$tmp_docs"
  fi
  if [[ -n "$tmp_config" ]]; then
    rm -f "$tmp_config"
  fi
  if [[ -n "$tmp_config_base" ]]; then
    rm -f "$tmp_config_base"
  fi
}
trap cleanup EXIT INT TERM

tmp_docs_name="$(cd "$docs_root" && mktemp -d zensical-public-docs.XXXXXX)"
tmp_docs="$docs_root/$tmp_docs_name"
tmp_config_base_name="$(cd "$docs_root" && mktemp .zensical-build.XXXXXX)"
tmp_config_base="$docs_root/$tmp_config_base_name"
tmp_config="$tmp_config_base.toml"
tmp_config_name="$tmp_config_base_name.toml"
if [[ -e "$tmp_config" ]]; then
  printf 'temporary config path already exists: %s\n' "$tmp_config" >&2
  exit 1
fi
mv "$tmp_config_base" "$tmp_config"
tmp_config_base=""

# Local credential/secret artifacts that must never enter the public docs tree.
# Keep in sync with FORBIDDEN_SITE_FILENAMES in docs/scripts/check_built_site.py.
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

tar_credential_excludes=()
for glob in "${credential_globs[@]}"; do
  tar_credential_excludes+=(--exclude "./$glob")
done

(
  cd "$docs_root"
  tar \
    --exclude './.venv' \
    --exclude './.vercel' \
    --exclude './.env' \
    --exclude './.env.*' \
    --exclude './.env*.local' \
    "${tar_credential_excludes[@]}" \
    --exclude './site' \
    --exclude './zensical-public-docs.*' \
    --exclude './.zensical-build.*' \
    --exclude './superpowers' \
    --exclude './internal' \
    --exclude './overrides' \
    --exclude './scripts' \
    --exclude './screenshots' \
    --exclude './diagrams' \
    --exclude './README.md' \
    --exclude './pyproject.toml' \
    --exclude './uv.lock' \
    --exclude './vercel.json' \
    --exclude './vercel-build.sh' \
    --exclude './zensical-docs.sh' \
    --exclude './zensical.toml' \
    --exclude './assets/*.sh' \
    -cf - .
) | (cd "$tmp_docs" && tar -xf -)

# The temporary docs tree is the public publishing boundary. Keep ignored local
# credentials and dotfiles out even if a future tar implementation misses a glob.
# This backstop matches at any depth (tar excludes above only anchor the root).
find_prune_expr=('(' -name '.*')
for glob in "${credential_globs[@]}"; do
  find_prune_expr+=(-o -iname "$glob")
done
find_prune_expr+=(')')
find "$tmp_docs" -depth "${find_prune_expr[@]}" -exec rm -rf {} +

awk -v docs_dir="$tmp_docs_name" -v site_dir="$site_dir" '
  $0 == "docs_dir = \"docs\"" {
    print "docs_dir = \"" docs_dir "\""
    next
  }
  $0 == "site_dir = \"site\"" {
    print "site_dir = \"" site_dir "\""
    next
  }
  { print }
' "$docs_root/zensical.toml" > "$tmp_config"

case "$command_name" in
  build)
    (cd "$docs_root" && "$zensical_bin" build --strict --config-file "$tmp_config_name" "$@")
    ;;
  serve)
    (cd "$docs_root" && "$zensical_bin" serve --config-file "$tmp_config_name" "$@")
    ;;
esac
