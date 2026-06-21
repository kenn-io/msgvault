#!/usr/bin/env bash
# Regenerate generated docs assets and update docs-generated-assets.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$docs_root/.." && pwd)"
assets_branch="${MSGVAULT_DOCS_GENERATED_ASSETS_BRANCH:-docs-generated-assets}"
source_dir=""
push=false
generate=true

expected_assets=(
  "concepts/account-collection-concept.png"
  "concepts/deduplication-concept.png"
  "concepts/oauth-multi-account-concept.png"
  "concepts/safety-ladder-concept.png"
  "concepts/survivor-selection-concept.png"
  "list-senders.svg"
  "stats.svg"
  "tui-all-messages.svg"
  "tui-deletion.svg"
  "tui-domains.svg"
  "tui-drilldown.svg"
  "tui-filter-modal.svg"
  "tui-labels.svg"
  "tui-message-detail.svg"
  "tui-search-drilldown.svg"
  "tui-search-sender.svg"
  "tui-search-subject.svg"
  "tui-selection.svg"
  "tui-senders.svg"
  "tui-subgroup-recipients.svg"
  "tui-subgroup-time.svg"
  "tui-thread.svg"
  "tui-time-daily.svg"
  "tui-time-monthly.svg"
  "tui-time-yearly.svg"
  "tui-time.svg"
)

usage() {
  cat <<EOF
Usage: $(basename "$0") [--source DIR] [--push]

Update the local $assets_branch branch to a single orphan commit containing
generated docs screenshots and concept assets. By default this runs
docs/screenshots/generate-all.sh and docs/diagrams/build.sh first.

Pass --source DIR to publish an already generated directory without running
generators. Pass --push to force-push the updated branch to origin.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source)
      [[ $# -ge 2 ]] || { printf 'ERROR: --source requires a directory\n' >&2; exit 2; }
      source_dir="$2"
      generate=false
      shift 2
      ;;
    --push)
      push=true
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

if [[ "$generate" == true ]]; then
  screenshots_script="$docs_root/screenshots/generate-all.sh"
  diagrams_script="$docs_root/diagrams/build.sh"

  if [[ ! -f "$screenshots_script" ]]; then
    printf 'generated docs assets cannot be regenerated: missing %s\n' "$screenshots_script" >&2
    exit 1
  fi
  if [[ ! -f "$diagrams_script" ]]; then
    printf 'generated docs assets cannot be regenerated: missing %s\n' "$diagrams_script" >&2
    exit 1
  fi

  rm -rf "$docs_root/assets/generated"
  bash "$screenshots_script"
  bash "$diagrams_script"
fi

if [[ -z "$source_dir" ]]; then
  source_dir="$docs_root/assets/generated"
fi

source_dir="$(cd "$source_dir" 2>/dev/null && pwd)" || {
  printf 'generated docs asset source does not exist: %s\n' "$source_dir" >&2
  exit 1
}

if ! find "$source_dir" -mindepth 1 \( -type f -o -type l \) -print -quit | grep -q .; then
  printf 'generated docs asset source is empty: %s\n' "$source_dir" >&2
  exit 1
fi

for asset in "${expected_assets[@]}"; do
  if [[ -L "$source_dir/$asset" ]]; then
    printf 'generated docs asset source must not be a symlink: %s\n' "$asset" >&2
    exit 1
  fi
  if [[ ! -f "$source_dir/$asset" ]]; then
    printf 'generated docs asset source is missing expected asset: %s\n' "$asset" >&2
    exit 1
  fi
done

is_expected_asset() {
  local path="$1"
  local asset
  for asset in "${expected_assets[@]}"; do
    [[ "$asset" == "$path" ]] && return 0
  done
  return 1
}

while IFS= read -r -d '' path; do
  rel="${path#"$source_dir"/}"
  case "$rel" in
    .DS_Store|*/.DS_Store)
      continue
      ;;
  esac
  if ! is_expected_asset "$rel"; then
    printf 'generated docs asset source has unexpected file: %s\n' "$rel" >&2
    exit 1
  fi
done < <(find "$source_dir" -mindepth 1 \( -type f -o -type l \) -print0)

tmp_root="$(mktemp -d)"
asset_repo="$tmp_root/assets-repo"

cleanup() {
  rm -rf "$tmp_root"
}
trap cleanup EXIT

mkdir -p "$asset_repo"
for asset in "${expected_assets[@]}"; do
  mkdir -p "$asset_repo/$(dirname "$asset")"
  cp "$source_dir/$asset" "$asset_repo/$asset"
done

git -C "$asset_repo" init --quiet
git -C "$asset_repo" add .
git -C "$asset_repo" \
  -c user.name="${GIT_AUTHOR_NAME:-msgvault docs bot}" \
  -c user.email="${GIT_AUTHOR_EMAIL:-docs-bot@example.invalid}" \
  commit -m "docs generated assets" >/dev/null
asset_commit="$(git -C "$asset_repo" rev-parse HEAD)"
git -C "$asset_repo" update-ref refs/heads/assets "$asset_commit"
git -C "$repo_root" fetch "$asset_repo" "+refs/heads/assets:refs/heads/$assets_branch" >/dev/null

printf 'Updated %s -> %s\n' "$assets_branch" "$asset_commit"

if [[ "$push" == true ]]; then
  git -C "$repo_root" push --force origin "$assets_branch"
fi
