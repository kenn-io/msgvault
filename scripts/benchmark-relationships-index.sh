#!/usr/bin/env bash
#
# Production-scale release gate for the Relationships sidebar. It exercises
# the real binary, cache builder, daemon, and HTTP routes, then prints one JSON
# result for easy comparison between runs.

set -euo pipefail

readonly SCALE_MESSAGES=2500000
readonly SCALE_PARTICIPANTS=75000
readonly SCALE_EDGES=6000000
readonly GROUP_CHAT_MEMBERS=3
readonly FULL_BUILD_SECONDS_LIMIT=60
readonly FULL_BUILD_RSS_BYTES_LIMIT=4294967296
readonly INDEX_BUILD_SECONDS_LIMIT=45
readonly INDEX_BUILD_RSS_BYTES_LIMIT=4294967296
readonly COLD_QUERY_MS_LIMIT=250
readonly FILTERED_QUERY_MS_LIMIT=2500
readonly INTERACTIVE_RSS_DELTA_KB_LIMIT=1572864
readonly SETTLED_RSS_DELTA_KB_LIMIT=1048576

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_parent="${TMPDIR:-/tmp}"
tmp_parent="${tmp_parent%/}"
scratch=""
benchmark_home=""
binary=""
daemon_pid=""
sampler_pid=""
result_emitted=0

cleanup() {
  if [[ -n "$sampler_pid" ]] && kill -0 "$sampler_pid" 2>/dev/null; then
    kill "$sampler_pid" 2>/dev/null || true
    wait "$sampler_pid" 2>/dev/null || true
  fi
  if [[ -n "$daemon_pid" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  if [[ -n "$scratch" && -d "$scratch" ]]; then
    case "$scratch" in
      "$tmp_parent"/msgvault-relationships-benchmark.*)
        rm -rf -- "$scratch"
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
  printf '%s\n' '{"status":"error","error":"reference gate requires Darwin arm64"}'
  exit 2
fi

scratch="$(mktemp -d "$tmp_parent/msgvault-relationships-benchmark.XXXXXX")"
benchmark_home="$scratch/home"
binary="$scratch/msgvault"
mkdir -p "$benchmark_home"

cd "$repo_root"
CGO_ENABLED=1 go build -tags "fts5 sqlite_vec" -o "$binary" ./cmd/msgvault \
  >"$scratch/build-binary.log" 2>&1

"$binary" fake-vault \
  --output "$benchmark_home" \
  --messages "$SCALE_MESSAGES" \
  --participants "$SCALE_PARTICIPANTS" \
  --participant-edges "$SCALE_EDGES" \
  --group-chat-members "$GROUP_CHAT_MEMBERS" \
  --attachment-bytes 0 \
  --seed 1 \
  --quiet \
  >"$scratch/generate.log" 2>&1

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

build_time_file="$scratch/build-cache.time"
# The child shell expands PPID so build-cache recognizes the bounded daemon
# child mode used in production.
# shellcheck disable=SC2016
/usr/bin/time -l -o "$build_time_file" \
  /bin/bash -c '
    export MSGVAULT_DAEMON_BUILD_CACHE_PARENT_PID="$PPID"
    exec "$@"
  ' benchmark-build "$binary" --home "$benchmark_home" \
    build-cache --full-rebuild \
  >"$scratch/build-cache.log" 2>&1

build_seconds="$(awk 'NR == 1 { print $1 }' "$build_time_file")"
build_peak_rss_bytes="$(
  awk '/maximum resident set size/ { print $1; exit }' "$build_time_file"
)"

# A derived-only refresh with no identity or membership change is a no-op by
# design, so inject real conversation-participant drift first: the timed run
# below must exercise the production drift-heal path, not process startup.
sqlite3 "$benchmark_home/msgvault.db" "
  INSERT INTO conversation_participants (conversation_id, participant_id)
  SELECT (SELECT min(id) FROM conversations),
         (SELECT min(p.id) FROM participants p
          WHERE NOT EXISTS (
            SELECT 1 FROM conversation_participants cp
            WHERE cp.conversation_id = (SELECT min(id) FROM conversations)
              AND cp.participant_id = p.id));
"

index_time_file="$scratch/build-index.time"
# shellcheck disable=SC2016
/usr/bin/time -l -o "$index_time_file" \
  /bin/bash -c '
    export MSGVAULT_DAEMON_BUILD_CACHE_PARENT_PID="$PPID"
    exec "$@"
  ' benchmark-index "$binary" --home "$benchmark_home" \
    build-cache --derived-only \
  >"$scratch/build-index.log" 2>&1
index_seconds="$(awk 'NR == 1 { print $1 }' "$index_time_file")"
index_peak_rss_bytes="$(
  awk '/maximum resident set size/ { print $1; exit }' "$index_time_file"
)"

fanout_line="$(grep 'Relationship fan-out:' "$scratch/build-index.log" | tail -1)"
activity_direct="$(sed -E 's/.*direct=([0-9]+).*/\1/' <<<"$fanout_line")"
activity_conversation="$(sed -E 's/.*conversation=([0-9]+).*/\1/' <<<"$fanout_line")"
activity_final="$(sed -E 's/.*final=([0-9]+).*/\1/' <<<"$fanout_line")"
activity_expansion="$(sed -E 's/.*expansion=([0-9.]+)x.*/\1/' <<<"$fanout_line")"
for fanout_value in "$activity_direct" "$activity_conversation" \
  "$activity_final" "$activity_expansion"; do
  if [[ ! "$fanout_value" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    printf '%s\n' "derived-only run reported no relationship fan-out;" \
      "the index rebuild did not happen:" >&2
    tail -20 "$scratch/build-index.log" >&2
    exit 1
  fi
done

"$binary" --home "$benchmark_home" serve >"$scratch/serve.log" 2>&1 &
daemon_pid=$!

base_url=""
ready=0
for _ in $(seq 1 600); do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    tail -100 "$scratch/serve.log" >&2
    exit 1
  fi
  base_url="$(sed -n 's/^  API server: //p' "$scratch/serve.log" | tail -1)"
  if [[ -n "$base_url" ]] &&
    curl --fail --silent "$base_url/health" -o "$scratch/health.json" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.1
done
if [[ "$ready" -ne 1 ]]; then
  printf '%s\n' "daemon readiness timed out" >&2
  exit 1
fi

baseline_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
sampler_stop="$scratch/stop-sampler"
sampler_result="$scratch/sampler-result"
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
    if [[ "$current_spill_kb" =~ ^[0-9]+$ ]] &&
      ((current_spill_kb * 1024 > peak_spill_bytes)); then
      peak_spill_bytes=$((current_spill_kb * 1024))
    fi
    sleep 0.1
  done
  printf '%s %s\n' "$peak_rss_kb" "$peak_spill_bytes" >"$sampler_result"
) &
sampler_pid=$!

request_milliseconds() {
  local name="$1"
  local path="$2"
  local body="$3"
  local result
  local elapsed
  local status
  result="$(
    curl --silent --show-error \
      -H "Content-Type: application/json" \
      --data "$body" \
      --output "$scratch/$name.json" \
      --write-out '%{time_total} %{http_code}' \
      "$base_url$path"
  )"
  read -r elapsed status <<<"$result"
  if [[ ! "$status" =~ ^2[0-9][0-9]$ ]]; then
    printf '%s returned HTTP %s: ' "$name" "$status" >&2
    tr -d '\r\n' <"$scratch/$name.json" >&2
    printf '\n' >&2
    return 1
  fi
  awk -v seconds="$elapsed" 'BEGIN { printf "%.3f", seconds * 1000 }'
}

relationships_ms="$(
  request_milliseconds relationships /api/v1/relationships \
    '{"show_all":true,"limit":100}'
)"
people_ms="$(
  request_milliseconds people /api/v1/people/search \
    '{"predicate":{},"sort":{"field":"activity_count","direction":"desc"},"limit":100}'
)"
domains_ms="$(
  request_milliseconds domains /api/v1/domains/search \
    '{"predicate":{},"sort":{"field":"activity_count","direction":"desc"},"limit":100}'
)"
person_search_ms="$(
  request_milliseconds person-search /api/v1/people/search \
    '{"identity_query":"person101","predicate":{},"sort":{"field":"activity_count","direction":"desc"},"limit":100}'
)"
if ! jq -e '.rows | length > 0' "$scratch/person-search.json" >/dev/null; then
  printf '%s\n' "person search returned no fixture-backed rows" >&2
  exit 1
fi
readonly filtered='[{"dimension":"source","values":["1"]},{"dimension":"participant","values":["101"]},{"dimension":"message_type","values":["email"]},{"dimension":"deletion","values":["active"]}]'
filtered_people_ms="$(
  request_milliseconds filtered-people /api/v1/people/search \
    "{\"predicate\":{\"filters\":$filtered},\"sort\":{\"field\":\"activity_count\",\"direction\":\"desc\"},\"limit\":100}"
)"
filtered_relationships_ms="$(
  request_milliseconds filtered-relationships /api/v1/relationships \
    "{\"filters\":$filtered,\"show_all\":true,\"limit\":100}"
)"
readonly date_window='[{"dimension":"after","values":["2024-01-01T00:00:00Z"]},{"dimension":"before","values":["2025-01-01T00:00:00Z"]}]'
date_relationships_ms="$(
  request_milliseconds date-relationships /api/v1/relationships \
    "{\"filters\":$date_window,\"show_all\":true,\"limit\":100}"
)"

sleep 2
settled_rss_kb="$(ps -p "$daemon_pid" -o rss= | awk '{ print $1 }')"
touch "$sampler_stop"
wait "$sampler_pid"
sampler_pid=""
read -r peak_rss_kb peak_spill_bytes <"$sampler_result"

peak_rss_delta_kb=$((peak_rss_kb - baseline_rss_kb))
settled_rss_delta_kb=$((settled_rss_kb - baseline_rss_kb))
((peak_rss_delta_kb < 0)) && peak_rss_delta_kb=0
((settled_rss_delta_kb < 0)) && settled_rss_delta_kb=0

gate_failed=0
float_exceeds() {
  awk -v value="$1" -v limit="$2" 'BEGIN { exit !(value > limit) }'
}
for latency in \
  "$relationships_ms" "$people_ms" "$domains_ms" "$person_search_ms"; do
  if float_exceeds "$latency" "$COLD_QUERY_MS_LIMIT"; then
    gate_failed=1
  fi
done
for latency in \
  "$filtered_people_ms" "$filtered_relationships_ms" "$date_relationships_ms"; do
  if float_exceeds "$latency" "$FILTERED_QUERY_MS_LIMIT"; then
    gate_failed=1
  fi
done
if float_exceeds "$build_seconds" "$FULL_BUILD_SECONDS_LIMIT" ||
  ((build_peak_rss_bytes > FULL_BUILD_RSS_BYTES_LIMIT)) ||
  float_exceeds "$index_seconds" "$INDEX_BUILD_SECONDS_LIMIT" ||
  ((index_peak_rss_bytes > INDEX_BUILD_RSS_BYTES_LIMIT)) ||
  ((peak_rss_delta_kb > INTERACTIVE_RSS_DELTA_KB_LIMIT)) ||
  ((settled_rss_delta_kb > SETTLED_RSS_DELTA_KB_LIMIT)); then
  gate_failed=1
fi

gate_status="pass"
if [[ "$gate_failed" -ne 0 ]]; then
  gate_status="fail"
fi
result_emitted=1
printf '%s\n' \
  "{\"status\":\"$gate_status\",\"messages\":$SCALE_MESSAGES,\"participants\":$SCALE_PARTICIPANTS,\"participant_edges\":$SCALE_EDGES,\"group_chat_members\":$GROUP_CHAT_MEMBERS,\"activity_direct_rows\":$activity_direct,\"activity_conversation_rows\":$activity_conversation,\"activity_final_rows\":$activity_final,\"activity_expansion_ratio\":$activity_expansion,\"full_build_seconds\":$build_seconds,\"full_build_peak_rss_bytes\":$build_peak_rss_bytes,\"index_build_seconds\":$index_seconds,\"index_build_peak_rss_bytes\":$index_peak_rss_bytes,\"relationships_ms\":$relationships_ms,\"people_ms\":$people_ms,\"domains_ms\":$domains_ms,\"person_search_ms\":$person_search_ms,\"filtered_people_ms\":$filtered_people_ms,\"filtered_relationships_ms\":$filtered_relationships_ms,\"date_relationships_ms\":$date_relationships_ms,\"baseline_rss_kb\":$baseline_rss_kb,\"peak_rss_delta_kb\":$peak_rss_delta_kb,\"settled_rss_delta_kb\":$settled_rss_delta_kb,\"peak_spill_bytes\":$peak_spill_bytes}"

if [[ "$gate_failed" -ne 0 ]]; then
  exit 1
fi
