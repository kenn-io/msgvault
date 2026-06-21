#!/usr/bin/env bash
# Runs inside the Docker container. Drives tmux + freeze to produce SVG screenshots.
set -euo pipefail

OUTPUT_DIR="${1:-/output}"
FREEZE_CFG="/tapes/freeze.json"
SESSION="mv"

mkdir -p "$OUTPUT_DIR"

# Helper: capture current tmux pane to SVG via freeze
capture() {
    local name="$1"
    tmux capture-pane -pet "$SESSION" | freeze -o "$OUTPUT_DIR/${name}.svg" --language ansi -c "$FREEZE_CFG"
    echo "  captured $name.svg"
}

# Helper: send keys and wait
send() {
    tmux send-keys -t "$SESSION" "$@"
}

# Helper: poll tmux pane (plain text, no ANSI) until a pattern appears (or timeout)
wait_until() {
    local pattern="$1"
    local timeout="${2:-30}"
    local elapsed=0
    while ! tmux capture-pane -pt "$SESSION" | grep -q "$pattern"; do
        sleep 0.5
        elapsed=$((elapsed + 1))
        if [[ $elapsed -ge $((timeout * 2)) ]]; then
            echo "  WARNING: timed out waiting for pattern: $pattern"
            return 1
        fi
    done
}

# --- Start tmux session ---
# Set terminal options before creating the session so the pane inherits them
tmux -f /dev/null start-server
tmux set-option -g default-terminal "tmux-256color"
tmux set-option -ga terminal-overrides ",*:Tc"
tmux new-session -d -s "$SESSION" -x 120 -y 40
tmux send-keys -t "$SESSION" "export COLORTERM=truecolor" Enter
sleep 0.3

# =====================
# TUI Screenshots
# =====================
echo "==> TUI screenshots"

send "MSGVAULT_HOME=/data msgvault tui" Enter
wait_until "Sender Name"

# 1. Senders view (default)
sleep 0.5
capture "tui-senders"

# All messages view: press a to show individual messages
send "a"
wait_until "Date"
sleep 0.5
capture "tui-all-messages"

# Message detail: press Enter to view a message
send Enter
wait_until "Subject:"
sleep 0.5
capture "tui-message-detail"

# Back to senders
send Escape
sleep 0.5
send Escape
sleep 0.5
wait_until "Sender Name"
sleep 0.5

# 2. Drill into a sender
send Down
sleep 0.3
send Down
sleep 0.3
send Down
sleep 0.3
send Enter
wait_until "Date"
sleep 0.5
capture "tui-drilldown"

# Go back to senders for thread screenshot
send Escape
sleep 0.5

# Thread view: search for sarah.benson to find the curated thread
send "/"
sleep 0.5
send -l "sarah.benson"
sleep 1.5
send Enter
sleep 0.5
wait_until "sarah.benson@company.io"

# Drill into Sarah Benson
send Enter
wait_until "Date"
sleep 0.5

# Find the Infrastructure Migration thread and open it
send "T"
sleep 1
capture "tui-thread"

# Go back to top-level senders
send Escape
sleep 0.5
send Escape
sleep 0.5
send Escape
sleep 0.5
wait_until "Sender Name"
sleep 0.5

# Sub-grouping: drill into a sender first, then press g to re-aggregate
send Down
sleep 0.3
send Down
sleep 0.3
send Down
sleep 0.3
send Enter
wait_until "Date"
sleep 0.5
send "g"
wait_until "Recipient Name"
sleep 0.5
capture "tui-subgroup-recipients"

# Switch to time grouping (t jumps directly to Time from any view)
send "t"
wait_until "Time"
sleep 0.5
capture "tui-subgroup-time"

# Navigate back to top-level
send Escape
sleep 0.5
send Escape
sleep 1

# --- Search screenshots (at top-level Senders) ---
wait_until "Sender Name"

# Search for a sender
send "/"
sleep 0.5
send -l "benson"
sleep 1.5
# Enter to confirm/fix the search
send Enter
sleep 0.5
wait_until "benson@"
capture "tui-search-sender"

# Drill into the benson result
send Enter
wait_until "Date"
sleep 0.5
capture "tui-search-drilldown"

# Search within the drilled-down messages
send "/"
sleep 0.5
send -l "spring"
sleep 1.5
wait_until "spring"
capture "tui-search-subject"

# Clear search and return to top-level Senders
send Escape
sleep 0.5
send Escape
sleep 0.5
send Escape
sleep 0.5
wait_until "Sender Name"
sleep 0.5

# Filter modal screenshot
send "f"
wait_until "Filter Messages"
sleep 0.5
capture "tui-filter-modal"
send Enter
sleep 0.5

# 3. Domains view (cycle: Sender -> Sender Name -> Recipient -> Recipient Name -> Domain)
send "g"
wait_until "Sender Name"
sleep 0.3
send "g"
wait_until "Recipient"
sleep 0.3
send "g"
wait_until "Recipient Name"
sleep 0.3
send "g"
wait_until "Domain"
sleep 0.5
capture "tui-domains"

# 4. Labels view
send "g"
wait_until "Label"
sleep 0.5
capture "tui-labels"

# 5. Time view - capture all three granularities
send "g"
wait_until "Time"
sleep 0.5
# Default is monthly
capture "tui-time-monthly"

# Cycle to daily
send "t"
sleep 0.5
capture "tui-time-daily"

# Cycle to yearly
send "t"
sleep 0.5
capture "tui-time-yearly"

# 6. Multi-row selection (back to Sender first)
# From Time, cycle: Sender
send "g"
wait_until "Sender Name"
sleep 0.5
send Down
sleep 0.3
send Space
sleep 0.3
send Down
sleep 0.3
send Space
sleep 0.3
send Down
sleep 0.3
send Space
sleep 0.5
capture "tui-selection"

# 7. Group deletion confirmation dialog
send Down
sleep 0.3
send D
wait_until "Confirm Deletion"
sleep 0.5
capture "tui-deletion"

# Cancel and quit
send "n"
sleep 0.5
send "q"
sleep 1

# =====================
# CLI Screenshots
# =====================
echo "==> CLI screenshots"

# Set up a clean prompt and export MSGVAULT_HOME so it doesn't appear in commands
send "export PS1='$ '" Enter
sleep 0.3
send "export MSGVAULT_HOME=/data" Enter
sleep 0.3
send "clear" Enter
sleep 0.5

# 8. stats
send "msgvault stats" Enter
wait_until "Database:"
sleep 0.5
capture "stats"

# 9. list-senders
send "clear" Enter
sleep 0.5
send "msgvault list-senders --limit 15" Enter
wait_until "SENDER"
sleep 0.5
capture "list-senders"

# Cleanup
tmux kill-session -t "$SESSION" 2>/dev/null || true

echo ""
echo "Done! Generated $(ls "$OUTPUT_DIR"/*.svg 2>/dev/null | wc -l) SVG files in $OUTPUT_DIR"
