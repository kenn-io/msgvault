// Package slack archives public channels or a user's channel memberships
// and DMs via the Slack Web API. It uses a user token from a user-created
// internal (non-distributed) app and selects conversation types by its scopes.
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
//     (unless broadcast). Tokens with search:read discover them through
//     search.messages plus periodic history audits. Tokens without search
//     access revisit conversation history on every sync.
package slack
