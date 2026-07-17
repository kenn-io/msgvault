#!/usr/bin/env bash
# Populate ignored docs asset directories from orphan asset branches.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$docs_root/.." && pwd)"
static_branch="${MSGVAULT_DOCS_ASSETS_BRANCH:-docs-assets}"
generated_branch="${MSGVAULT_DOCS_GENERATED_ASSETS_BRANCH:-docs-generated-assets}"
use_local_branches="${MSGVAULT_DOCS_USE_LOCAL_ASSET_BRANCHES:-false}"

static_target="$docs_root/assets/static"
generated_target="$docs_root/assets/generated"

static_assets=(
  "favicon-192.png"
  "favicon-512.png"
  "favicon.svg"
  "how-it-works.svg"
  "oauth-multi-account.svg"
  "og-image.png"
  "og-image.svg"
)

generated_assets=(
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

has_expected_assets() {
  local target="$1"
  shift

  local asset
  for asset in "$@"; do
    [[ -f "$target/$asset" ]] || return 1
  done
}

in_git_worktree() {
  git -C "$repo_root" rev-parse --is-inside-work-tree >/dev/null 2>&1
}

use_local_branches_enabled() {
  [[ "$use_local_branches" == "1" || "$use_local_branches" == "true" ]]
}

resolve_asset_ref() {
  local branch="$1"

  if use_local_branches_enabled; then
    if git -C "$repo_root" rev-parse --verify --quiet "$branch" >/dev/null; then
      printf '%s\n' "$branch"
      return 0
    fi
  fi

  if ! git -C "$repo_root" fetch --force --depth=1 origin \
    "+refs/heads/$branch:refs/remotes/origin/$branch" >/dev/null; then
    printf 'docs assets not hydrated: failed to fetch origin/%s\n' "$branch" >&2
    return 1
  fi

  if git -C "$repo_root" rev-parse --verify --quiet "origin/$branch" >/dev/null; then
    printf 'origin/%s\n' "$branch"
    return 0
  fi

  if use_local_branches_enabled; then
    if git -C "$repo_root" rev-parse --verify --quiet "$branch" >/dev/null; then
      printf '%s\n' "$branch"
      return 0
    fi
  fi

  return 1
}

hydrate_branch() {
  local branch="$1"
  local target="$2"
  shift 2

  if ! in_git_worktree; then
    if has_expected_assets "$target" "$@"; then
      return 0
    fi

    printf 'docs assets not hydrated: no git worktree found and expected assets are missing\n' >&2
    return 1
  fi

  local asset_ref
  if ! asset_ref="$(resolve_asset_ref "$branch")"; then
    printf 'docs assets not hydrated: %s branch unavailable\n' "$branch" >&2
    return 1
  fi

  rm -rf "$target"
  mkdir -p "$target"
  git -C "$repo_root" archive "$asset_ref" | tar -xf - -C "$target"

  if ! has_expected_assets "$target" "$@"; then
    printf 'docs assets not hydrated: %s is missing expected assets\n' "$branch" >&2
    return 1
  fi
}

hydrate_branch "$static_branch" "$static_target" "${static_assets[@]}"
hydrate_branch "$generated_branch" "$generated_target" "${generated_assets[@]}"
