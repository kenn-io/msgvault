#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd "$script_dir/../.." && pwd -P)"
lock_path="${MSGVAULT_DOCS_FIXTURE_LOCK:-$script_dir/fixture.lock.json}"
output_dir=""
offline="${MSGVAULT_DOCS_FIXTURES_OFFLINE:-0}"
local_dir="${MSGVAULT_DOCS_FIXTURE_DIR:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output-dir) output_dir="$2"; shift 2 ;;
    --lock) lock_path="$2"; shift 2 ;;
    -h|--help) printf 'Usage: %s [--output-dir DIR] [--lock PATH]\n' "$(basename "$0")"; exit 0 ;;
    *) printf 'unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

if [[ -z "$output_dir" ]]; then
  output_dir="$(mktemp -d /tmp/msgvault-docs-fixture-hydrated.XXXXXX)"
else
  mkdir -p "$output_dir"
fi

if [[ -n "$local_dir" ]]; then
  local_dir="$(cd "$local_dir" && pwd -P)"
  cp "$local_dir/enron-web-fixture.mbox.gz" "$output_dir/"
  cp "$local_dir/manifest.json" "$output_dir/"
  cp "$local_dir/README.md" "$output_dir/"
elif [[ "$offline" == "1" || "$offline" == "true" ]]; then
  printf 'SKIP: docs-fixtures unavailable offline\n'
  exit 0
else
  locked_branch="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["branch"])' "$lock_path")"
  if [[ "$locked_branch" != "docs-fixtures" ]]; then
    printf 'unsupported fixture branch: %s\n' "$locked_branch" >&2
    exit 1
  fi
  locked_commit="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["commit"])' "$lock_path")"
  fixture_ref="refs/remotes/origin/docs-fixtures"
  git -C "$repo_root" fetch --no-tags origin "+refs/heads/docs-fixtures:$fixture_ref" >/dev/null
  git -C "$repo_root" cat-file -e "$locked_commit^{commit}"
  if ! git -C "$repo_root" merge-base --is-ancestor "$locked_commit" "$fixture_ref"; then
    printf 'locked fixture commit is not an ancestor of %s\n' "$fixture_ref" >&2
    exit 1
  fi
  git -C "$repo_root" archive "$locked_commit" | tar -xf - -C "$output_dir"
fi

python3 "$script_dir/validate-fixture.py" --fixture-dir "$output_dir"
locked_digest="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["sha256"])' "$lock_path")"
actual_digest="$(shasum -a 256 "$output_dir/enron-web-fixture.mbox.gz" | awk '{print $1}')"
[[ "$locked_digest" == "$actual_digest" ]] || { printf 'fixture lock digest mismatch\n' >&2; exit 1; }
locked_manifest_digest="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["manifest_sha256"])' "$lock_path")"
actual_manifest_digest="$(shasum -a 256 "$output_dir/manifest.json" | awk '{print $1}')"
[[ "$locked_manifest_digest" == "$actual_manifest_digest" ]] || { printf 'fixture lock manifest digest mismatch\n' >&2; exit 1; }
printf '%s\n' "$output_dir"
