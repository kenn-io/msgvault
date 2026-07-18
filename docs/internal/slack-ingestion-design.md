# Slack Ingestion — Design

Date: 2026-07-18
Status: Draft, load-bearing pass complete — LB-1..5 probed live against a
Slack Developer Program sandbox (team `TesterMsgVault`, 2026-07-18) with
a seeded 1,100-message channel. LB-3 was falsified and the incremental
design revised accordingly; verdicts inline below.

## Goal

Sync and store the user's Slack messages — public/private channels the
user is a member of, group DMs (mpim), and 1:1 DMs — into msgvault so
they are searchable alongside mail and other chat sources through the
existing TUI, FTS, and Parquet analytics. Text (with mrkdwn), threads,
reactions, @mentions, edits, and shared files are preserved.

## Decisions

- **Acquisition: live Web API sync via a user-created internal Slack
  app** (a "custom app" installed only to the user's own workspace),
  using a **user token (`xoxp-…`)**, not a bot token. A user token sees
  exactly what the user sees — private channels, DMs, group DMs —
  without inviting a bot anywhere. This mirrors the Teams decision
  (own-data, delegated access, user registers their own app).
- **Rate-limit posture (load-bearing, verified against Slack docs
  2026-07-18):** Slack's 2025 rate-limit clampdown
  (`conversations.history`/`conversations.replies` at 1 req/min,
  15 messages/req) applies to **commercially distributed non-Marketplace
  apps**. **Internal custom apps are explicitly exempt** and retain the
  historical limits (Tier 3, ~50 req/min, up to 999 messages/req).
  msgvault's "register your own app" model lands in the exempt category.
  See LB-1 for the live probe that must confirm this before backfill
  performance claims are made.
- **Data scope:** conversations the user is a *member* of, enumerated
  via `users.conversations`. Public channels the user hasn't joined are
  out of scope for v1 (see LB-4).
- **Attachments:** download file bytes into content-addressed storage,
  subject to a configurable size cap (Beeper precedent); files above the
  cap store filename + permalink + metadata only. Only URLs on
  `files.slack.com` are ever fetched with the bearer token (see
  Security).
- **Slack export ZIP import is out of scope** for this build — separate
  follow-up spec if wanted (useful for workspaces where app creation is
  admin-blocked, and for Free-plan workspaces where the API only serves
  90 days of history).
- **Edits/deletes are best-effort** (same stance as Teams deletes):
  Slack history queries are keyed by original message `ts`, so edits and
  deletions of already-archived messages are only caught by a periodic
  re-scan window, not by incremental sync. See "Incremental sync".

## Architecture

Parallel to `internal/teams/` and `internal/beeper/` — no new core
tables, no TUI/query changes beyond registering the new message type.

```
cmd/msgvault/cmd/add_slack.go        ← token setup + auth.test validation
cmd/msgvault/cmd/sync_slack.go       ← full + incremental sync
cmd/msgvault/cmd/backfill_slack_media.go
        │
internal/slack/                      ← new package
   ├── client.go       Web API client (cursor paging, 429 + Retry-After, tier-aware limiter)
   ├── importer.go     orchestration: enumerate conversations → history/replies → persist
   ├── mapping.go      Slack message JSON → store.Message (+ body, recipients, reactions)
   ├── participants.go user-ID → participant resolution (users.list cache, email unification)
   ├── media.go        files.slack.com downloads → content-addressed store
   ├── syncstate.go    per-conversation ts cursors in sync_runs.cursor_after
   ├── token.go        token storage/loading (~/.msgvault/tokens/slack_<team>.json)
   └── types.go        Slack API DTOs
        │
internal/store/…                     ← reused as-is (UpsertMessage,
                                        EnsureConversationWithType,
                                        EnsureConversationParticipant,
                                        RecomputeConversationStats, …)
```

Per sync run: validate token (`auth.test`) → refresh the users cache
(`users.list`) → enumerate memberships (`users.conversations`, all four
types) → per conversation: page `conversations.history` from the saved
cursor, fetch `conversations.replies` for each thread root, persist
messages/reactions/mentions/files → advance and flush that
conversation's cursor → `RecomputeConversationStats` +
`CompleteSync` with final counts.

## Data model mapping

Reuses the existing generic chat schema — **no new core tables**.

| Slack concept | msgvault storage |
|---|---|
| Workspace + user | `sources` row, `source_type = "slack"`, `identifier = "<team_domain>:<user_id>"` (from `auth.test`) |
| Public/private channel | `conversations`, `conversation_type = "channel"`, `title = "#name"` |
| Group DM (mpim) | `conversations`, `conversation_type = "group_chat"` |
| 1:1 DM (im) | `conversations`, `conversation_type = "direct_chat"` |
| Message | `messages`, `message_type = "slack"`, `source_message_id = "<channel_id>:<ts>"` |
| Thread reply | `reply_to_message_id` → root (`thread_ts` identifies the root) |
| Sender | `message_recipients` `"from"`; conversation members via `conversation_participants` |
| `<@U…>` mention | `message_recipients` `"mention"` (resolved via users cache) |
| mrkdwn text | `message_bodies.body_text` (mrkdwn → plain text for FTS; raw mrkdwn preserved in `message_raw`) |
| Reactions | `reactions` table (emoji name; custom emoji stored by name) |
| File (≤ size cap) | `attachments` (downloaded bytes, content-addressed) |
| File (> cap) / external | `attachments` row: filename + permalink + metadata, no bytes |
| Edited message | `UpsertMessage` ON CONFLICT update + `SetMessageEdited` (when the `edited` field is present) |
| Raw message JSON | `message_raw`, `raw_format = "slack_json"` (verbatim API object, Beeper precedent) |

Registration touch-points: add `"slack"` to `KnownMessageTypes` and
`TextMessageTypes` (`internal/query/text_models.go`) — TUI Texts mode,
type filters, and snippet fallback then derive automatically.

Notes:

- **`ts` is the message identity** within a channel (string, e.g.
  `"1734567890.123456"`); it is unique per channel only, hence the
  composite `source_message_id`. It is also the timestamp — normalize to
  UTC (`time.Unix` on the integer part; keep full string for identity).
- **Identity unification.** `users.list` with `users:read.email` yields
  `profile.email` for workspace members; store the Slack user ID as a
  `slack_user_id` identifier on `participants` and dedup against
  existing mail identities by email — same pattern as Teams' AAD-id →
  email resolution, giving cross-platform search of one human.
  Bots (`is_bot`), deactivated users, and Slack Connect guests may lack
  emails — store id-only, best-effort (Teams precedent).
- **System subtypes** (`channel_join`, `channel_leave`, etc.) are
  persisted as messages (they carry history value) but excluded from
  FTS-noise-sensitive surfaces the same way other importers handle
  service messages; `bot_message` subtype keeps its `bot_id`/`username`
  as the sender.

## Auth & permissions

`msgvault add-slack` flow (token-paste model, Beeper precedent — no
OAuth redirect server needed for an internal app):

1. User creates an app at api.slack.com/apps ("From scratch", their
   workspace), adds **user token scopes**:
   `channels:history`, `groups:history`, `im:history`, `mpim:history`
   (message reads); `channels:read`, `groups:read`, `im:read`,
   `mpim:read` (conversation enumeration); `users:read`,
   `users:read.email` (identity resolution); `files:read` (file
   downloads); `reactions:read`; `team:read`.
2. "Install to Workspace" → copy the **User OAuth Token** (`xoxp-…`).
3. `msgvault add-slack` prompts for the token (stdin, not argv),
   validates with `auth.test`, derives `team_domain`/`team_id`/`user_id`,
   writes `~/.msgvault/tokens/slack_<team_id>.json` (0600), creates the
   `sources` row.

Multiple workspaces = multiple `add-slack` runs = multiple sources; the
TUI account filter (`a`) separates them for free.

Config:

```toml
[slack]
# include = ["#eng", "#general"]   # default: all memberships
# exclude = ["#noise"]
# max_media_bytes = 52428800       # 50 MiB default, 0 = metadata-only
# sync_interval = "1h"             # daemon scheduling
# edit_rescan_window = "168h"      # see Incremental sync
# thread_lookback = "720h"         # how long a thread root stays tracked for new replies
```

## Incremental sync & checkpointing

**Cursor model** (`internal/slack/syncstate.go`, persisted as JSON in
`sync_runs.cursor_after`, Teams precedent):

```go
type SyncState struct {
    // channelID -> max message ts persisted for that conversation
    Conversations map[string]string `json:"conversations"`
    // channelID -> rootTs -> max reply ts persisted for that thread.
    // Pruned to roots younger than thread_lookback (see below).
    Threads map[string]map[string]string `json:"threads"`
}
```

- **Backfill:** `conversations.history` with cursor pagination
  (`limit=999` for internal apps), oldest→newest per page batch; for
  every root with `thread_ts == ts` and `reply_count > 0`, fetch
  `conversations.replies` (also paginated). The conversation's cursor
  advances **only after** its messages, replies, raw JSON, and reaction
  rows have persisted without error; the state is flushed via
  `UpdateSyncCheckpoint` **after each conversation** so an interrupted
  backfill resumes mid-stream (Teams precedent, and the exact class of
  data-loss bug roborev hammered on in #398 — a failed replies fetch
  must fail that conversation's cursor advance, not just increment an
  error counter).
- **Incremental (channel level):** `conversations.history` with
  `oldest=<cursor>`, `inclusive=false` — catches new roots and
  broadcast replies only.
- **Incremental (threads) — revised after LB-3 was falsified.** Thread
  replies do **not** appear in `conversations.history` unless the author
  chose "also send to channel", and the updated parent is not re-served
  either (history filters on the parent's original `ts`). Verified
  mitigation: `conversations.replies` honors `oldest`, and a re-fetched
  parent carries live `reply_count`/`latest_reply`. Design: track
  per-root reply cursors in `SyncState.Threads`; each incremental sync
  calls `conversations.replies` with `oldest=<thread cursor>` for every
  tracked root, tracking roots for a configurable `thread_lookback`
  (default 30 days) before pruning. Replies to threads older than the
  lookback are caught only by `--full` (or the LB-6 search-based
  discovery fast-follow). Empty replies calls are cheap at internal-app
  limits (~50 req/min, LB-1), but this bounds per-sync call count to
  O(active threads), which is why the lookback exists. Implementation
  gotcha (probe-verified): `conversations.replies` includes the parent
  itself as the first message — the importer must skip it or rely on
  upsert idempotency.
- **Edits & deletes (structural limitation):** history is keyed by
  original `ts`, so an edit or delete of a message older than the cursor
  is invisible to incremental sync. Mitigation: each scheduled sync
  re-pages a trailing `edit_rescan_window` (default 7 days) and upserts
  in place — `UpsertMessage` ON CONFLICT catches edits, and messages
  present in the archive but absent from the re-scanned window are
  *not* deleted locally (archive semantics: local copy survives remote
  deletion, Beeper precedent). Documented as best-effort.
- **Rate limiting:** honor `Retry-After` on 429 globally; a token-bucket
  limiter keyed by method tier (Tier 2: `conversations.list`,
  `users.list`; Tier 3: `history`/`replies`; Tier 4: `auth.test`),
  matching the Gmail limiter pattern.
- **Failure accounting:** final `UpdateSyncCheckpoint` with counts +
  errors before `CompleteSync`, so a partially-failed run never reports
  clean (roborev finding on #398).

## Security

- **Never fetch a user-controlled URL with the bearer token.** Message
  and file JSON contain attacker-influenceable URLs. Media downloads are
  restricted to `url_private` values whose host is exactly
  `files.slack.com` (scheme https, no redirects followed off-host);
  anything else is recorded as metadata-only. This is the Slack analogue
  of the Graph-token-exfiltration finding roborev raised on the Teams PR
  (crafted hosted-content URL → attacker host).
- Token file 0600 under `~/.msgvault/tokens/`; token accepted via
  prompt, never argv (shell history).
- Client is read-only by construction: the package exposes only GET/
  read-method calls (`conversations.*` reads, `users.*`, `auth.test`) —
  no `chat.*` write surface exists in the client (Beeper precedent).

## CLI & daemon integration

- `msgvault add-slack` — token setup (above); `remove-account` gains
  slack support (token file + source rows), Teams/Beeper precedent.
- `msgvault sync-slack [<team>]` — full/incremental auto-detected from
  stored cursors; `--limit N` (conversations), `--full` (ignore cursors,
  re-fetch + upsert in place), `--no-threads` for scoped first runs.
- `msgvault backfill-slack-media` — retry failed/over-cap downloads
  (`--only-incomplete`), Teams/Beeper precedent.
- `msgvault serve` — scheduled syncs alongside other sources; the
  daemon's all-source-types-per-identifier behavior (from #398) needs no
  change since Slack identifiers are team-scoped and won't collide with
  mail addresses.
- TUI / FTS / Parquet: no changes beyond the message-type registration —
  Slack rows flow through the standard `messages` path.

## Testing

- Table-driven testify tests (`assert`/`require`), no `t.Errorf`
  patterns; synthetic fixtures only (`alice`, `U0ALICE`,
  `user@example.com`) — no real workspace data.
- Mapping tests over recorded (synthetic) Slack JSON: message subtypes,
  threads, edits, reactions, mentions, mrkdwn → text.
- A fake Slack HTTP server (Beeper's `fake_server_test.go` precedent)
  driving the full sync → store → search path, including: cursor
  pagination, 429/Retry-After, mid-backfill interrupt + resume, replies
  fetch failure ⇒ cursor not advanced, and the files.slack.com host
  restriction (attempted off-host fetch must not carry the token).
- Live validation against a real workspace before the PR (Teams
  precedent: it shipped with a ~313k-message live validation note).

## Load-bearing findings (probed live 2026-07-18, Developer Program
sandbox, fresh internal app, 1,100-message seeded channel)

- **LB-1 — internal-app rate limits: VERIFIED.** `conversations.history`
  with `limit=999` returned full 999-message pages at 42 req/min
  sustained (12 consecutive calls, no 429). The 2025 clampdown
  (1 req/min, 15 msgs) does not apply to internal custom apps; the
  design's backfill performance assumptions hold.
- **LB-2 — full-history depth: INCONCLUSIVE in sandbox** (all seeded
  data is same-day, so there is no >90-day boundary to cross). Slack's
  plan docs state paid plans serve full history via API; treat as
  docs-verified and confirm during the first real-workspace sync.
- **LB-3 — late thread replies: FALSIFIED.** A reply to an
  already-archived root does **not** appear in `conversations.history
  oldest=<cursor>` (empty page), and the parent is not re-served with
  updated metadata either. Verified mitigation (see Incremental sync):
  `conversations.replies` honors `oldest` and returns the late reply;
  the re-fetched parent carries `reply_count`/`latest_reply`. Design
  revised to per-root reply cursors with a `thread_lookback` window.
- **LB-4 — non-member public channels: WORKS.** The user token read
  `conversations.history` of a public channel after leaving it. A
  `[slack] include_unjoined_public = true` option is a viable fast
  follow; v1 still defaults to memberships only.
- **LB-5 — conversation-type enumeration: PARTIAL.** `users.conversations`
  returned `public_channel` and `im` correctly; `mpim` untested because
  the sandbox has no group DM (needs 3+ users). Re-verify in passing on
  the first workspace that has one; no design impact expected.

Follow-up probe candidates:

- **LB-6 — search-based reply discovery.** `search.messages` (user
  scope `search:read`) with an `after:<date>` modifier may return new
  thread replies globally, removing the `thread_lookback` blind spot
  for replies to old threads. Not yet probed (scope not granted to the
  probe app); candidate for a fast follow to the incremental design.

## Out of scope

- Slack export ZIP import (`import-slack-export`) — separate follow-up
  spec; needed for admin-blocked workspaces and pre-90-day Free-plan
  history.
- Session-token acquisition (`xoxc`/`xoxd` browser tokens, slackdump
  style) — ToS-gray, not something msgvault should ship.
- Canvases, huddles, clips, workflows, and Slack Connect external-shared
  channel edge cases beyond what `users.conversations` returns.
- Custom emoji image download (`emoji.list` mapping) — reactions store
  the emoji name; image backfill can be a fast follow.
