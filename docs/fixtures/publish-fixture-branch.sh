#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd "$script_dir/../.." && pwd -P)"
fixture_dir=""
remote="origin"
branch="docs-fixtures"
push=false

usage() {
  printf 'Usage: %s --fixture-dir DIR [--remote NAME] [--push]\n' "$(basename "$0")"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --fixture-dir) fixture_dir="$2"; shift 2 ;;
    --remote) remote="$2"; shift 2 ;;
    --push) push=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

[[ -n "$fixture_dir" ]] || { usage >&2; exit 2; }
fixture_dir="$(cd "$fixture_dir" && pwd -P)"
python3 "$script_dir/validate-fixture.py" --fixture-dir "$fixture_dir"

tmp_root="$(mktemp -d /tmp/msgvault-docs-fixtures.XXXXXX)"
trap 'rm -rf "$tmp_root"' EXIT
umask 077
fixture_repo="$tmp_root/repo"
mkdir -p "$fixture_repo"

git -C "$fixture_repo" init --quiet --initial-branch="$branch"
git -C "$fixture_repo" remote add "$remote" "$(git -C "$repo_root" remote get-url "$remote")"
if git -C "$fixture_repo" fetch --no-tags "$remote" "+refs/heads/$branch:refs/remotes/$remote/$branch" >/dev/null 2>&1; then
  git -C "$fixture_repo" reset --quiet --hard "refs/remotes/$remote/$branch"
  git -C "$fixture_repo" rm --quiet -r --ignore-unmatch .
else
  git -C "$fixture_repo" rm --quiet --cached -r . 2>/dev/null || true
fi
cp "$fixture_dir/enron-web-fixture.mbox.gz" "$fixture_repo/"
cp "$fixture_dir/manifest.json" "$fixture_repo/"
cp "$script_dir/README.template.md" "$fixture_repo/README.md"
git -C "$fixture_repo" add enron-web-fixture.mbox.gz manifest.json README.md
if ! git -C "$fixture_repo" diff --cached --quiet; then
  git -C "$fixture_repo" -c user.name="msgvault docs bot" -c user.email="docs-bot@example.invalid" commit --quiet -m "docs fixture $(date -u +%Y-%m-%d)"
fi
fixture_commit="$(git -C "$fixture_repo" rev-parse HEAD)"

if [[ "$push" == true ]]; then
  git -C "$fixture_repo" push "$remote" "HEAD:refs/heads/$branch"
fi
printf 'docs-fixtures commit: %s\n' "$fixture_commit"
printf 'fixture branch pushed: %s\n' "$push"
