#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd "$script_dir/../.." && pwd -P)"
fixture_lock="$repo_root/docs/fixtures/fixture.lock.json"
output_dir="${MSGVAULT_DOCS_SCREENSHOT_OUTPUT_DIR:-$repo_root/docs/assets/static}"
original_home="${HOME:-}"
platform="${MSGVAULT_DOCS_SCREENSHOT_PLATFORM:-darwin}"

case "$platform" in
  darwin|linux) ;;
  *) printf 'unsupported screenshot platform: %s\n' "$platform" >&2; exit 2 ;;
esac

for command in bun go gzip python3; do
  command -v "$command" >/dev/null || { printf 'required command unavailable: %s\n' "$command" >&2; exit 1; }
done
[[ -f "$fixture_lock" ]] || { printf 'fixture lock is missing: %s\n' "$fixture_lock" >&2; exit 1; }

umask 077
scratch="$(mktemp -d /tmp/msgvault-docs-capture.XXXXXX)"
go_modcache="$(go env GOMODCACHE)"
go_cache="$(go env GOCACHE)"
daemon_pid=""
cleanup() {
  local exit_code="$?"
  trap - EXIT INT TERM
  if [[ -n "$daemon_pid" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill -TERM "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  chmod -R u+rwX "$scratch" 2>/dev/null || true
  rm -rf "$scratch"
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

bash "$repo_root/docs/fixtures/hydrate-fixture.sh" --output-dir "$scratch/fixture"
fixture_dir="$scratch/fixture"
home_dir="$scratch/home"
os_home="$scratch/os-home"
mkdir -p "$home_dir" "$os_home" "$scratch/xdg-config" "$scratch/xdg-cache" "$scratch/output"
if [[ -z "${PLAYWRIGHT_BROWSERS_PATH:-}" ]]; then
  if [[ -d "$original_home/Library/Caches/ms-playwright" ]]; then
    export PLAYWRIGHT_BROWSERS_PATH="$original_home/Library/Caches/ms-playwright"
  elif [[ -d /ms-playwright ]]; then
    export PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
  fi
fi
export HOME="$os_home"
export MSGVAULT_HOME="$home_dir"
export MSGVAULT_DATA_DIR="$home_dir"
export XDG_CONFIG_HOME="$scratch/xdg-config"
export XDG_CACHE_HOME="$scratch/xdg-cache"
export GOMODCACHE="$go_modcache"
export GOCACHE="$go_cache"

cat > "$home_dir/config.toml" <<'EOF'
[server]
api_port = 0
bind_addr = "127.0.0.1"

[analytics]
engine = "duckdb"
auto_build_cache = false

[vector]
enabled = false
EOF

(cd "$repo_root" && make web-embed >/dev/null)
binary="$scratch/msgvault"
CGO_ENABLED=1 go build -buildvcs=false -tags "fts5 sqlite_vec" -trimpath -o "$binary" "$repo_root/cmd/msgvault"
plain_mbox="$scratch/enron-web-fixture.mbox"
gzip -dc "$fixture_dir/enron-web-fixture.mbox.gz" > "$plain_mbox"
owner_identifier="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["fixture"]["owner_identifier"])' "$fixture_dir/manifest.json")"
MSGVAULT_DAEMON_CLI_PARENT_PID="$$" "$binary" --home "$home_dir" --local import-mbox "$owner_identifier" "$plain_mbox" --no-attachments >/dev/null
MSGVAULT_DAEMON_BUILD_CACHE_PARENT_PID="$$" "$binary" --home "$home_dir" --local build-cache --full-rebuild >/dev/null
"$binary" --home "$home_dir" serve > "$scratch/serve.log" 2>&1 &
daemon_pid=$!

base_url=""
for _ in $(seq 1 180); do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    sed -n '1,240p' "$scratch/serve.log" >&2
    exit 1
  fi
  base_url="$(sed -n 's/^  API server: //p' "$scratch/serve.log" | tail -1)"
  if [[ -n "$base_url" ]] && curl --fail --silent --show-error "$base_url/api/session" -o "$scratch/session.json"; then break; fi
  sleep 0.1
done
[[ -n "$base_url" && -s "$scratch/session.json" ]] || { sed -n '1,240p' "$scratch/serve.log" >&2; exit 1; }

# The capture itself only needs the running web app. Maintainers review and
# publish each platform's rasterization through the docs asset branch.
(cd "$repo_root/web" && MSGVAULT_DOCS_BASE_URL="$base_url" MSGVAULT_DOCS_SCREENSHOT_OUTPUT="$scratch/output" MSGVAULT_DOCS_SCREENSHOT_PLATFORM="$platform" bunx playwright test tests/docs-fixture-screenshots.spec.ts --config "$repo_root/docs/screenshots/playwright-fixture.config.ts")

expected=(
  "analytical-dark-comfortable-$platform.png"
  "analytical-dark-compact-$platform.png"
  "analytical-light-comfortable-$platform.png"
  "analytical-light-compact-$platform.png"
)
if [[ "$platform" == darwin ]]; then
  expected+=(relationships-dark-comfortable-darwin.png relationships-light-compact-darwin.png)
fi
mkdir -p "$output_dir"
for asset in "${expected[@]}"; do
  [[ -s "$scratch/output/$asset" ]] || { printf 'capture missing or empty: %s\n' "$asset" >&2; exit 1; }
  cp "$scratch/output/$asset" "$output_dir/$asset"
done
printf 'generated %s documentation screenshots in %s\n' "${#expected[@]}" "$output_dir"
