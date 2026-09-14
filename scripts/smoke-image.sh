#!/bin/sh
# Runs inside the built image, both during Docker builds and after loading it.
set -eu

scratch="$(mktemp -d)"
export MSGVAULT_HOME="$scratch"
cleanup() {
  status=$?
  trap - EXIT
  msgvault daemon stop || status=1
  rm -rf "$scratch"
  exit "$status"
}
trap cleanup EXIT

cat > "$scratch/config.toml" <<'EOF'
[server]
api_port = 8080
bind_addr = "127.0.0.1"

[analytics]
auto_build_cache = false

[vector]
enabled = false
EOF

msgvault version
msgvault --help > /dev/null
msgvault init-db
test -s "$scratch/msgvault.db"
msgvault build-cache --full-rebuild
test "$(msgvault query --format csv 'SELECT COUNT(*) AS messages FROM messages')" = "$(printf 'messages\n0')"

wget -q -O /dev/null http://127.0.0.1:8080/health
wget -q -O "$scratch/index.html" http://127.0.0.1:8080/
asset="$(sed -n 's/.*src="\(\/assets\/[^" ]*\.js\)".*/\1/p' "$scratch/index.html")"
test -n "$asset"
wget -q -O "$scratch/asset.js" "http://127.0.0.1:8080$asset"
test -s "$scratch/asset.js"

# Runtime is binary-only: no frontend toolchain or external asset tree.
test ! -e /usr/local/bin/bun
test ! -e /usr/local/bin/node
test ! -d /usr/share/msgvault/web
