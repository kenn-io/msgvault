#!/usr/bin/env bash
# Hydrate published docs assets, verify the docs build, and deploy to Vercel.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
dry_run=false

usage() {
  cat <<EOF
Usage: $(basename "$0") [--dry-run]

Hydrate the docs-assets and docs-generated-assets branches, build and check
the docs, then deploy the current committed workspace to production Vercel.
The automated screenshot runner publishes docs-generated-assets; this script
does not regenerate it.

Run this after docs source changes have already been committed.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)
      dry_run=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'ERROR: %s is required for docs deployment\n' "$1" >&2
    exit 1
  fi
}

run() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  if [[ "$dry_run" == false ]]; then
    "$@"
  fi
}

require_clean_tracked_tree() {
  local status
  status="$(git status --porcelain --untracked-files=all)"
  if [[ -n "$status" ]]; then
    printf 'ERROR: uncommitted, non-ignored changes are present. Commit or stash docs source changes before deploying.\n' >&2
    printf '%s\n' "$status" >&2
    exit 1
  fi
}

run_clean_tree_check() {
  printf '+ check clean working tree\n'
  if [[ "$dry_run" == false ]]; then
    require_clean_tracked_tree
  fi
}

require_cmd bash
require_cmd git
require_cmd make
require_cmd vercel

cd "$repo_root"
run_clean_tree_check

run make docs-install
run rm -rf docs/assets/static docs/assets/generated
run bash docs/assets/hydrate-assets.sh
run make docs-build
run make docs-check
run_clean_tree_check
run make docs-deploy
