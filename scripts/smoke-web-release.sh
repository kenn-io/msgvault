#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/msgvault-web-release.XXXXXX")"
daemon_pid=""
proxy_pid=""

cleanup() {
  local exit_code="${1:-$?}"
  local cleanup_status=0
  trap - EXIT INT TERM
  if [[ -n "$daemon_pid" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill -TERM "$daemon_pid" 2>/dev/null || cleanup_status=1
    wait "$daemon_pid" || cleanup_status=1
  fi
  if [[ -n "$proxy_pid" ]] && kill -0 "$proxy_pid" 2>/dev/null; then
    kill -TERM "$proxy_pid" 2>/dev/null || cleanup_status=1
    wait "$proxy_pid" || cleanup_status=1
  fi
  rm -rf -- "$scratch" || cleanup_status=1
  if [[ "$cleanup_status" -ne 0 ]]; then
    echo "release smoke cleanup failed" >&2
    if [[ "$exit_code" -eq 0 ]]; then
      exit "$cleanup_status"
    fi
  fi
  exit "$exit_code"
}
trap 'cleanup $?' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

for command in bun go node curl; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 1; }
done

home_dir="$scratch/archive"
os_home="$scratch/os-home"
binary="$scratch/msgvault"
mkdir -p "$home_dir" "$os_home"
# Keep immutable Go module downloads outside disposable HOME. Go marks module
# files read-only, which would make a strict scratch cleanup fail closed.
go_module_cache="$(go env GOMODCACHE)"
go_build_cache="$(go env GOCACHE)"
export GOMODCACHE="$go_module_cache"
export GOCACHE="$go_build_cache"
export HOME="$os_home"
export MSGVAULT_HOME="$home_dir"
# Kept explicit for harnesses that enforce a generic data-dir guard. Msgvault's
# supported override remains MSGVAULT_HOME.
export MSGVAULT_DATA_DIR="$home_dir"
export XDG_CONFIG_HOME="$scratch/xdg-config"
export XDG_CACHE_HOME="$scratch/xdg-cache"

cd "$repo_root"
make web-install web-embed
node scripts/check-web-assets.mjs

CGO_ENABLED=1 go build -tags "fts5 sqlite_vec" -trimpath -ldflags="-s -w" -o "$binary" ./cmd/msgvault
node scripts/check-web-assets.mjs --binary "$binary"

attachment_hash="$(go run -tags "fts5 sqlite_vec" ./scripts/smoke-fixture "$home_dir")"
[[ "$attachment_hash" =~ ^[0-9a-f]{64}$ ]] || { echo "fixture returned invalid attachment hash" >&2; exit 1; }

cat > "$home_dir/config.toml" <<'EOF'
[server]
api_port = 0
bind_addr = "127.0.0.1"
api_key = "release-smoke-api-key"

[analytics]
engine = "duckdb"
auto_build_cache = false

[vector]
enabled = false
EOF

if ! MSGVAULT_DAEMON_BUILD_CACHE_PARENT_PID="$$" \
  "$binary" --home "$home_dir" --local build-cache --full-rebuild > "$scratch/cache.log" 2>&1; then
  cat "$scratch/cache.log" >&2
  exit 1
fi
"$binary" --home "$home_dir" serve > "$scratch/serve.log" 2>&1 &
daemon_pid=$!

base_url=""
for _ in $(seq 1 120); do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    cat "$scratch/serve.log" >&2
    echo "release daemon exited before startup completed" >&2
    exit 1
  fi
  base_url="$(sed -n 's/^  API server: //p' "$scratch/serve.log" | tail -1)"
  if [[ -n "$base_url" ]] && curl --fail --silent --show-error "$base_url/" -o "$scratch/index.html"; then
    break
  fi
  sleep 0.1
done
[[ -n "$base_url" && -s "$scratch/index.html" ]] || { cat "$scratch/serve.log" >&2; echo "release daemon did not become ready" >&2; exit 1; }
cmp -s "$scratch/index.html" "$repo_root/web/dist/index.html" || {
  echo "served index differs from the built distribution" >&2
  exit 1
}

auth_header="Authorization: Bearer release-smoke-api-key"
while IFS= read -r asset; do
  [[ "$asset" == "index.html" || "$asset" == ".vite/manifest.json" ]] && continue
  curl --fail --silent --show-error "$base_url/$asset" -o "$scratch/asset"
  cmp -s "$scratch/asset" "$repo_root/web/dist/$asset" || {
    echo "served asset differs from the built distribution: $asset" >&2
    exit 1
  }
done < <(node scripts/check-web-assets.mjs --list)

curl --fail --silent --show-error "$base_url/api/session" -o "$scratch/session.json"
node -e 'const x=JSON.parse(require("fs").readFileSync(process.argv[1]));if(!x.auth_mode)process.exit(1)' "$scratch/session.json"
curl --fail --silent --show-error "$base_url/openapi.json" -o "$scratch/openapi.json"
node -e 'const x=JSON.parse(require("fs").readFileSync(process.argv[1]));if(x.openapi!=="3.1.0")process.exit(1)' "$scratch/openapi.json"
curl --fail --silent --show-error -H "$auth_header" -H 'Content-Type: application/json' \
  --data '{}' "$base_url/api/v1/explore" -o "$scratch/explore.json"
node -e 'const x=JSON.parse(require("fs").readFileSync(process.argv[1]));if(!Array.isArray(x.rows)||x.rows.length<1)process.exit(1)' "$scratch/explore.json"
curl --fail --silent --show-error -H "$auth_header" \
  "$base_url/api/v1/attachments/$attachment_hash/content" -o "$scratch/attachment.txt"
cmp -s "$scratch/attachment.txt" <(printf 'msgvault release smoke attachment\n')

# Existing bearer clients remain accepted on the same daemon. This is an
# authenticated API operation, not a loopback-trust-only health probe.
curl --fail --silent --show-error -H "$auth_header" "$base_url/api/v1/stats" -o "$scratch/bearer-stats.json"
node -e 'JSON.parse(require("fs").readFileSync(process.argv[1]))' "$scratch/bearer-stats.json"

# Drive the built CLI through the runtime record published by this daemon.
# This exercises the same daemonclient path used by existing local CLI users.
"$binary" --home "$home_dir" stats > "$scratch/local-cli-stats.txt"
node -e 'const text=require("fs").readFileSync(process.argv[1],"utf8");if(!text.includes("Messages:    1")||!text.includes("Database:"))process.exit(1)' \
  "$scratch/local-cli-stats.txt"

# Re-run the built CLI through configured-remote resolution. The proxy rejects
# every request without the expected X-Api-Key, then forwards accepted requests
# to the same isolated daemon using its separately verified bearer path.
node scripts/smoke-api-key-proxy.mjs "$base_url" release-smoke-api-key \
  > "$scratch/proxy-url.txt" 2> "$scratch/proxy.log" &
proxy_pid=$!
proxy_url=""
for _ in $(seq 1 100); do
  if ! kill -0 "$proxy_pid" 2>/dev/null; then
    cat "$scratch/proxy.log" >&2
    echo "API-key verification proxy exited before startup completed" >&2
    exit 1
  fi
  proxy_url="$(head -1 "$scratch/proxy-url.txt" 2>/dev/null || true)"
  [[ -n "$proxy_url" ]] && break
  sleep 0.05
done
[[ -n "$proxy_url" ]] || { echo "API-key verification proxy did not become ready" >&2; exit 1; }
cat >> "$home_dir/config.toml" <<EOF

[remote]
url = "$proxy_url"
api_key = "release-smoke-api-key"
allow_insecure = true
EOF
"$binary" --home "$home_dir" stats > "$scratch/remote-cli-stats.txt"
node -e 'const text=require("fs").readFileSync(process.argv[1],"utf8");const url=process.argv[2];if(!text.includes(`Remote: ${url}`)||!text.includes("Messages:    1"))process.exit(1)' \
  "$scratch/remote-cli-stats.txt" "$proxy_url"

echo "release web smoke passed"
