// Package slack archives a Slack workspace user's own conversations —
// public/private channels they are a member of, group DMs, and 1:1 DMs —
// via the Slack Web API using a user token from a user-created internal
// (non-distributed) Slack app.
//
// Design: docs/internal/slack-ingestion-design.md. The package follows the
// beeper/teams importer anatomy: a read-only rate-limited client, a
// per-conversation cursor model persisted in sync_runs.cursor_after, and
// persistence through the shared store schema (no new core tables).
//
// Two properties are load-bearing (probed live 2026-07-18):
//   - Internal apps are exempt from the 2025 non-Marketplace rate-limit
//     clampdown: conversations.history serves 999-message pages at Tier 3
//     rates.
//   - Thread replies never appear in oldest-filtered conversations.history
//     (unless broadcast), so incremental sync tracks per-root reply cursors
//     and re-polls conversations.replies for roots within a lookback window.
package slack
