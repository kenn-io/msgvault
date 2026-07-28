#!/usr/bin/env bash
#
# Production-scale release gate for the Parquet-backed relationship index.
# The output is exactly one JSON object so benchmark runs remain comparable.

set -euo pipefail

readonly SCALE_MESSAGES=2500000
readonly SCALE_PARTICIPANTS=75000
readonly SCALE_EDGES=6000000
readonly BUILD_SECONDS_LIMIT=25
readonly BUILD_RSS_BYTES_LIMIT=3221225472
readonly COLD_QUERY_MS_LIMIT=250
readonly FILTERED_QUERY_MS_LIMIT=1000
readonly INTERACTIVE_RSS_DELTA_KB_LIMIT=1572864
readonly SETTLED_RSS_DELTA_KB_LIMIT=262144

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
benchmark_tmp_root="${TMPDIR:-/tmp}"
benchmark_tmp_root="${benchmark_tmp_root%/}"
scratch_root=""
benchmark_home=""
binary=""
daemon_pid=""
sampler_pid=""
result_emitted=0

cleanup() {
  if [[ -n "$sampler_pid" && "$sampler_pid" =~ ^[1-9][0-9]*$ ]] &&
    kill -0 "$sampler_pid" 2>/dev/null; then
    kill "$sampler_pid" 2>/dev/null || true
    wait "$sampler_pid" 2>/dev/null || true
  fi
  if [[ -n "$daemon_pid" && "$daemon_pid" =~ ^[1-9][0-9]*$ ]] &&
    kill -0 "$daemon_pid" 2>/dev/null; then
    daemon_command="$(ps -p "$daemon_pid" -o command= 2>/dev/null || true)"
    if [[ -n "$binary" && "$daemon_command" == "$binary --home $benchmark_home serve"* ]]; then
      kill "$daemon_pid" 2>/dev/null || true
      wait "$daemon_pid" 2>/dev/null || true
    fi
  fi
  if [[ -n "$scratch_root" && -d "$scratch_root" ]]; then
    case "$scratch_root" in
      "$benchmark_tmp_root"/msgvault-relationships-benchmark.*)
        rm -rf -- "$scratch_root"
        ;;
    esac
  fi
  if [[ "$result_emitted" -eq 0 ]]; then
    printf '%s\n' \
      '{"status":"error","error":"benchmark terminated before producing measurements"}'
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ "$(uname -s)" != "Darwin" || "$(uname -m)" != "arm64" ]]; then
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"reference gate requires Darwin arm64"}'
  exit 2
fi

scratch_root="$(mktemp -d "$benchmark_tmp_root/msgvault-relationships-benchmark.XXXXXX")"
benchmark_home="$scratch_root/home"
binary="$scratch_root/msgvault"
mkdir -p "$benchmark_home"

cd "$repo_root"
CGO_ENABLED=1 go build -tags "fts5 sqlite_vec" -o "$binary" ./cmd/msgvault \
  >"$scratch_root/build-binary.log" 2>&1

"$binary" fake-vault \
  --output "$benchmark_home" \
  --messages "$SCALE_MESSAGES" \
  --participants "$SCALE_PARTICIPANTS" \
  --participant-edges "$SCALE_EDGES" \
  --attachment-bytes 0 \
  --seed 1 \
  --quiet \
  >"$scratch_root/generate.log" 2>&1

cat >"$benchmark_home/config.toml" <<'EOF'
[server]
api_port = 0
bind_addr = "127.0.0.1"

[analytics]
engine = "duckdb"
auto_build_cache = false

[vector]
enabled = false
EOF

build_time_file="$scratch_root/build-cache.time"
# The child shell intentionally expands its own PPID so the timed msgvault
# process recognizes the direct daemon-child build mode.
# shellcheck disable=SC2016
if ! /usr/bin/time -l -o "$build_time_file" \
  /bin/bash -c '
    export MSGVAULT_DAEMON_BUILD_CACHE_PARENT_PID="$PPID"
    exec "$@"
  ' benchmark-build "$binary" --home "$benchmark_home" \
    build-cache --full-rebuild \
  >"$scratch_root/build-cache.log" 2>&1; then
  tail -100 "$scratch_root/build-cache.log" >&2
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"full cache build failed"}'
  exit 1
fi
build_seconds="$(awk 'NR == 1 { print $1 }' "$build_time_file")"
build_peak_rss_bytes="$(
  awk '/maximum resident set size/ { print $1; exit }' "$build_time_file"
)"

query_profile_file="$scratch_root/query-profile.json"
if ! MSGVAULT_RELATIONSHIPS_SCALE_BENCH=1 \
  MSGVAULT_RELATIONSHIPS_BENCH_HOME="$benchmark_home" \
  MSGVAULT_RELATIONSHIPS_BENCH_PROFILE="$query_profile_file" \
  go test -tags "fts5 sqlite_vec" ./internal/query \
    -run '^$' -bench '^BenchmarkRelationshipIndexCold$' -benchtime=1x \
    >"$scratch_root/query-benchmark.log" 2>&1; then
  tail -100 "$scratch_root/query-benchmark.log" >&2
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"cold query benchmark failed"}'
  exit 1
fi
if [[ ! -s "$query_profile_file" ]]; then
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"cold query benchmark produced no profile evidence"}'
  exit 1
fi
query_profile="$(tr -d '\r\n' <"$query_profile_file")"
if [[ "$query_profile" != '{"version":1,"operations":['*'}' ]]; then
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"cold query benchmark profile evidence is invalid"}'
  exit 1
fi

"$binary" --home "$benchmark_home" serve \
  >"$scratch_root/serve.log" 2>&1 &
daemon_pid=$!

base_url=""
daemon_ready=0
for _ in $(seq 1 600); do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    result_emitted=1
    printf '%s\n' \
      '{"status":"error","error":"daemon exited before becoming ready"}'
    exit 1
  fi
  base_url="$(sed -n 's/^  API server: //p' "$scratch_root/serve.log" | tail -1)"
  if [[ -n "$base_url" ]] &&
    curl --fail --silent "$base_url/health" \
      -o "$scratch_root/health.json" 2>/dev/null; then
    daemon_ready=1
    break
  fi
  sleep 0.1
done
if [[ "$daemon_ready" -ne 1 ]]; then
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"daemon readiness timed out"}'
  exit 1
fi

baseline_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
if [[ ! "$baseline_rss_kb" =~ ^[0-9]+$ ]]; then
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"could not read daemon baseline RSS"}'
  exit 1
fi

sampler_stop="$scratch_root/stop-sampler"
sampler_result="$scratch_root/sampler-result"
spill_directory="$benchmark_home/tmp/duckdb-query-$daemon_pid"
(
  peak_rss_kb="$baseline_rss_kb"
  peak_spill_bytes=0
  while [[ ! -e "$sampler_stop" ]]; do
    current_rss_kb="$(ps -p "$daemon_pid" -o rss= 2>/dev/null | awk '{ print $1 }')"
    if [[ "$current_rss_kb" =~ ^[0-9]+$ ]] &&
      ((current_rss_kb > peak_rss_kb)); then
      peak_rss_kb="$current_rss_kb"
    fi
    current_spill_kb="$(
      du -sk "$spill_directory" 2>/dev/null | awk '{ print $1 }' || true
    )"
    if [[ "$current_spill_kb" =~ ^[0-9]+$ ]]; then
      current_spill_bytes=$((current_spill_kb * 1024))
      if ((current_spill_bytes > peak_spill_bytes)); then
        peak_spill_bytes="$current_spill_bytes"
      fi
    fi
    sleep 0.1
  done
  printf '%s %s\n' "$peak_rss_kb" "$peak_spill_bytes" >"$sampler_result"
) &
sampler_pid=$!

query_failed=0
request_milliseconds() {
  local name="$1"
  local path="$2"
  local body="$3"
  local elapsed_seconds
  if ! elapsed_seconds="$(
    curl --fail --silent --show-error \
      -H "Content-Type: application/json" \
      --data "$body" \
      --output "$scratch_root/$name.json" \
      --write-out '%{time_total}' \
      "$base_url$path"
  )"; then
    query_failed=1
    printf '%s' 999999
    return
  fi
  awk -v seconds="$elapsed_seconds" 'BEGIN { printf "%.3f", seconds * 1000 }'
}

relationships_ms="$(
  request_milliseconds relationships /api/v1/relationships \
    '{"show_all":true,"limit":100}'
)"
relationships_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
people_ms="$(
  request_milliseconds people /api/v1/people/search \
    '{"predicate":{},"sort":{"field":"activity_count","direction":"desc"},"limit":100}'
)"
people_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
domains_ms="$(
  request_milliseconds domains /api/v1/domains/search \
    '{"predicate":{},"sort":{"field":"activity_count","direction":"desc"},"limit":100}'
)"
domains_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
readonly source_only_filters='[{"dimension":"source","values":["1"]}]'
source_only_people_ms="$(
  request_milliseconds source-only-people /api/v1/people/search \
    "{\"predicate\":{\"filters\":$source_only_filters},\"sort\":{\"field\":\"activity_count\",\"direction\":\"desc\"},\"limit\":100}"
)"
source_only_people_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
readonly date_window_filters='[{"dimension":"after","values":["2024-01-01T00:00:00Z"]},{"dimension":"before","values":["2025-01-01T00:00:00Z"]}]'
date_window_relationships_ms="$(
  request_milliseconds date-window-relationships /api/v1/relationships \
    "{\"filters\":$date_window_filters,\"show_all\":true,\"limit\":100}"
)"
date_window_relationships_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
readonly filtered_filters='[{"dimension":"source","values":["1"]},{"dimension":"participant","values":["101"]},{"dimension":"message_type","values":["email"]},{"dimension":"deletion","values":["active"]}]'
filtered_people_ms="$(
  request_milliseconds filtered-people /api/v1/people/search \
    "{\"predicate\":{\"filters\":$filtered_filters},\"sort\":{\"field\":\"activity_count\",\"direction\":\"desc\"},\"limit\":100}"
)"
filtered_people_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
filtered_relationships_ms="$(
  request_milliseconds filtered-relationships /api/v1/relationships \
    "{\"filters\":$filtered_filters,\"show_all\":true,\"limit\":100}"
)"
filtered_relationships_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"

sleep 5
duckdb_memory_file="$scratch_root/duckdb-memory.json"
if ! curl --fail --silent --show-error \
  -H "Content-Type: application/json" \
  --data '{"sql":"SELECT tag, memory_usage_bytes, temporary_storage_bytes FROM duckdb_memory() ORDER BY tag"}' \
  --output "$duckdb_memory_file" \
  "$base_url/api/v1/query"; then
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"could not read settled DuckDB memory accounting"}'
  exit 1
fi
duckdb_memory="$(tr -d '\r\n' <"$duckdb_memory_file")"
if [[ "$duckdb_memory" != '{"columns":'* ]]; then
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"settled DuckDB memory accounting is invalid"}'
  exit 1
fi
settled_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
if [[ ! "$settled_rss_kb" =~ ^[0-9]+$ ]]; then
  result_emitted=1
  printf '%s\n' \
    '{"status":"error","error":"could not read daemon settled RSS"}'
  exit 1
fi
touch "$sampler_stop"
wait "$sampler_pid"
sampler_pid=""
read -r peak_rss_kb peak_spill_bytes <"$sampler_result"

peak_rss_delta_kb=$((peak_rss_kb - baseline_rss_kb))
settled_rss_delta_kb=$((settled_rss_kb - baseline_rss_kb))
((peak_rss_delta_kb < 0)) && peak_rss_delta_kb=0
((settled_rss_delta_kb < 0)) && settled_rss_delta_kb=0

gate_failed="$query_failed"
float_exceeds() {
  awk -v value="$1" -v limit="$2" 'BEGIN { exit !(value > limit) }'
}
for latency in "$relationships_ms" "$people_ms" "$domains_ms"; do
  if float_exceeds "$latency" "$COLD_QUERY_MS_LIMIT"; then
    gate_failed=1
  fi
done
for latency in \
  "$source_only_people_ms" \
  "$date_window_relationships_ms" \
  "$filtered_people_ms" \
  "$filtered_relationships_ms"; do
  if float_exceeds "$latency" "$FILTERED_QUERY_MS_LIMIT"; then
    gate_failed=1
  fi
done
if float_exceeds "$build_seconds" "$BUILD_SECONDS_LIMIT" ||
  ((build_peak_rss_bytes > BUILD_RSS_BYTES_LIMIT)) ||
  ((peak_rss_delta_kb > INTERACTIVE_RSS_DELTA_KB_LIMIT)) ||
  ((settled_rss_delta_kb > SETTLED_RSS_DELTA_KB_LIMIT)); then
  gate_failed=1
fi

if [[ "$gate_failed" -eq 0 ]]; then
  gate_status="pass"
else
  gate_status="fail"
  tail -100 "$scratch_root/build-cache.log" >&2
fi
result_emitted=1
printf '%s\n' \
  "{\"status\":\"$gate_status\",\"messages\":$SCALE_MESSAGES,\"participants\":$SCALE_PARTICIPANTS,\"participant_edges\":$SCALE_EDGES,\"build_seconds\":$build_seconds,\"build_peak_rss_bytes\":$build_peak_rss_bytes,\"relationships_ms\":$relationships_ms,\"relationships_rss_kb\":$relationships_rss_kb,\"people_ms\":$people_ms,\"people_rss_kb\":$people_rss_kb,\"domains_ms\":$domains_ms,\"domains_rss_kb\":$domains_rss_kb,\"source_only_people_ms\":$source_only_people_ms,\"source_only_people_rss_kb\":$source_only_people_rss_kb,\"date_window_relationships_ms\":$date_window_relationships_ms,\"date_window_relationships_rss_kb\":$date_window_relationships_rss_kb,\"filtered_people_ms\":$filtered_people_ms,\"filtered_people_rss_kb\":$filtered_people_rss_kb,\"filtered_relationships_ms\":$filtered_relationships_ms,\"filtered_relationships_rss_kb\":$filtered_relationships_rss_kb,\"baseline_rss_kb\":$baseline_rss_kb,\"peak_rss_kb\":$peak_rss_kb,\"peak_rss_delta_kb\":$peak_rss_delta_kb,\"settled_rss_kb\":$settled_rss_kb,\"settled_rss_delta_kb\":$settled_rss_delta_kb,\"peak_spill_bytes\":$peak_spill_bytes,\"query_profile\":$query_profile,\"duckdb_memory\":$duckdb_memory}"

if [[ "$gate_failed" -ne 0 ]]; then
  exit 1
fi
