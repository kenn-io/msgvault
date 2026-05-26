#!/usr/bin/env bash
# Capture or compare coverage for a Go package before/after a migration.
# Usage:
#   scripts/testify-coverage-diff.sh before <pkg-path>   # snapshot baseline
#   scripts/testify-coverage-diff.sh after  <pkg-path>   # snapshot post-migration and diff
set -euo pipefail

MODE="${1:?usage: $0 before|after <pkg-path>}"
PKG="${2:?usage: $0 before|after <pkg-path>}"
SAFE_PKG="$(echo "$PKG" | tr '/' '_' | tr -d './')"
DIR=".testify-coverage"
mkdir -p "$DIR"

OUT="${DIR}/${MODE}-${SAFE_PKG}.out"
FUNCS="${DIR}/${MODE}-${SAFE_PKG}.funcs.txt"
LOG="${DIR}/${MODE}-${SAFE_PKG}.log"

go test -tags "fts5 sqlite_vec" -count=1 \
    -coverprofile="$OUT" \
    "$PKG" 2>&1 | tee "$LOG"

go tool cover -func="$OUT" > "$FUNCS"

if [[ "$MODE" == "after" ]]; then
    BEFORE_FUNCS="${DIR}/before-${SAFE_PKG}.funcs.txt"
    if [[ ! -f "$BEFORE_FUNCS" ]]; then
        echo "ERROR: no baseline at $BEFORE_FUNCS — run 'before' first" >&2
        exit 1
    fi
    DIFF="${DIR}/diff-${SAFE_PKG}.txt"
    diff -u "$BEFORE_FUNCS" "$FUNCS" > "$DIFF" || true
    if [[ -s "$DIFF" ]]; then
        echo "=== Coverage diff for $PKG ==="
        cat "$DIFF"
        echo "=== End diff ==="
        echo "WARNING: coverage changed — verify each delta is acceptable." >&2
    else
        echo "OK: coverage unchanged for $PKG"
    fi
fi
