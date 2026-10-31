#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd "$script_dir/../.." && pwd -P)"
fixture_dir="${1:-}"
if [[ -z "$fixture_dir" ]]; then
  printf 'Usage: %s FIXTURE_DIR\n' "$(basename "$0")" >&2
  exit 2
fi
fixture_dir="$(cd "$fixture_dir" && pwd -P)"

for command in go curl node make gzip python3; do
  command -v "$command" >/dev/null || { printf 'required command unavailable: %s\n' "$command" >&2; exit 1; }
done

umask 077
scratch="$(mktemp -d /tmp/msgvault-docs-smoke.XXXXXX)"
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

home_dir="$scratch/home"
os_home="$scratch/os-home"
mkdir -p "$home_dir" "$os_home" "$scratch/xdg-config" "$scratch/xdg-cache"
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

binary="$scratch/msgvault"
(cd "$repo_root" && make web-embed >/dev/null)
CGO_ENABLED=1 go build -buildvcs=false -tags "fts5 sqlite_vec" -trimpath -o "$binary" "$repo_root/cmd/msgvault"
plain_mbox="$scratch/enron-web-fixture.mbox"
gzip -dc "$fixture_dir/enron-web-fixture.mbox.gz" > "$plain_mbox"
owner_identifier="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["fixture"]["owner_identifier"])' "$fixture_dir/manifest.json")"
MSGVAULT_DAEMON_CLI_PARENT_PID="$$" "$binary" --home "$home_dir" --local import-mbox "$owner_identifier" "$plain_mbox" --no-attachments
MSGVAULT_DAEMON_BUILD_CACHE_PARENT_PID="$$" "$binary" --home "$home_dir" --local build-cache --full-rebuild

"$binary" --home "$home_dir" serve > "$scratch/serve.log" 2>&1 &
daemon_pid=$!
base_url=""
analytics_ready=0
for _ in $(seq 1 180); do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    sed -n '1,240p' "$scratch/serve.log" >&2
    exit 1
  fi
  base_url="$(sed -n 's/^  API server: //p' "$scratch/serve.log" | tail -1)"
  if [[ -n "$base_url" ]] &&
    curl --fail --silent --show-error "$base_url/api/session" -o "$scratch/session.json" &&
    curl --fail --silent --show-error "$base_url/health" -o "$scratch/health.json" &&
    python3 -c 'import json,sys; raise SystemExit(json.load(open(sys.argv[1], encoding="utf-8")).get("analytics_engine") != "duckdb")' "$scratch/health.json"; then
    analytics_ready=1
    break
  fi
  sleep 0.1
done
[[ "$analytics_ready" == "1" ]] || { sed -n '1,240p' "$scratch/serve.log" >&2; exit 1; }
node "$script_dir/smoke_fixture.mjs" "$base_url" "$fixture_dir/manifest.json"
printf 'docs fixture smoke passed\n'
