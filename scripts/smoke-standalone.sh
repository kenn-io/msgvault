#!/bin/sh
# Exercise a built/downloaded executable without build tools or user state.
# Run inside a stock runtime image for a clean-host check; PATH alone cannot
# hide shared libraries installed on a developer's machine.
set -eu

if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
  echo "usage: sh scripts/smoke-standalone.sh /path/to/msgvault" >&2
  exit 2
fi

scratch="$(mktemp -d "${TMPDIR:-/tmp}/msgvault-standalone.XXXXXX")"
binary="$scratch/relocated bin/msgvault"
archive="$scratch/archive"

run() {
  env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
    HOME="$scratch/os-home" \
    XDG_CONFIG_HOME="$scratch/config" XDG_CACHE_HOME="$scratch/cache" \
    XDG_DATA_HOME="$scratch/data" \
    MSGVAULT_HOME="$archive" MSGVAULT_TELEMETRY_ENABLED=0 \
    "$binary" --home "$archive" --local --no-log-file "$@"
}

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$status" -ne 0 ] && [ -f "$archive/serve.log" ]; then
    cat "$archive/serve.log" >&2
  fi
  # init-db and import-eml can auto-start the daemon. Stop it before removing
  # the isolated home, even when a later import, cache build, or query fails.
  if [ -x "$binary" ] && ! run daemon stop; then
    echo "standalone smoke: daemon cleanup failed" >&2
    [ "$status" -ne 0 ] || status=1
  fi
  if ! rm -rf "$scratch"; then
    echo "standalone smoke: scratch cleanup failed" >&2
    [ "$status" -ne 0 ] || status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$scratch/relocated bin" "$scratch/os-home" "$archive" "$scratch/mail/Inbox.mailbox"
# Copy only the executable. A sidecar library or build-tree runpath must not
# accidentally satisfy the direct-download contract.
cp "$1" "$binary"
chmod 755 "$binary"
cat > "$archive/config.toml" << 'EOF'
[server]
api_port = 0
bind_addr = "127.0.0.1"

[analytics]
engine = "auto"
auto_build_cache = false

[vector]
enabled = false
EOF
cat > "$scratch/mail/Inbox.mailbox/message.eml" << 'EOF'
From: Synthetic Sender <sender@example.com>
To: Synthetic Recipient <recipient@example.org>
Date: Mon, 02 Nov 2026 12:00:00 +0000
Message-ID: <standalone-smoke@example.com>
Subject: Standalone release smoke

One synthetic message for the standalone runtime contract.
EOF

run init-db
run import-eml "$scratch/mail" --identifier standalone@example.com --no-attachments
run build-cache --full-rebuild
# messages is a Parquet-backed view. The list expression also requires DuckDB,
# so an accidental SQLite fallback cannot satisfy this assertion.
actual="$(run query --format csv "SELECT COUNT(*) AS messages FROM messages WHERE subject = 'Standalone release smoke' AND list_contains(['standalone'], 'standalone')")"
expected="$(printf 'messages\n1')"
if [ "$actual" != "$expected" ]; then
  printf 'standalone smoke: unexpected DuckDB result:\n%s\n' "$actual" >&2
  exit 1
fi
echo "standalone smoke passed: SQLite import and DuckDB Parquet query"
