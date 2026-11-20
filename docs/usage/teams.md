---
last_edited: "2026-11-20"
title: Microsoft Teams
description: Archive Microsoft Teams chats and channels through delegated Microsoft Graph sync.
---

Archive your Microsoft Teams chats, channel discussions, and replies to search
them alongside your email and other messages. msgvault pulls message text,
participants, source JSON, and eligible inline images through Microsoft Graph.

Teams messages use `message_type = teams`. Sync is read-only: it never sends
messages, edits Teams content, or changes channel membership.

## Prerequisites

Teams sync uses Microsoft Graph delegated OAuth rather than IMAP. It requires
the same `[microsoft]` `client_id` block used by `add-o365`, and stores a
separate Graph token under `tokens/teams_<email>.json`. An Outlook IMAP token
from `add-o365` doesn't authorize Teams sync.

1. Register a Microsoft Entra app as described in
   [OAuth Setup](/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail), with:

   - Redirect URI: `http://localhost:8089/callback/microsoft`
   - Public client flows enabled
   - Delegated Microsoft Graph permissions:
     `Chat.Read`, `ChannelMessage.Read.All`, `Team.ReadBasic.All`,
     `Channel.ReadBasic.All`, `User.Read`, `User.ReadBasic.All`,
     `TeamMember.Read.All`, and `ChannelMember.Read.All`

   Some tenants require an administrator to grant consent for channel-message
   permissions before you can authorize the app.

2. Add the app to your `config.toml`:

   ```toml
   [microsoft]
   client_id = "your-azure-app-client-id"
   # tenant_id = "your-org-tenant-id"  # optional; default is "common"
   ```

## Authorize Teams

Authorize your Teams account with `add-teams`:

```bash
msgvault add-teams user@example.com
```

This opens a browser, requests the Graph scopes above, verifies the returned
identity, saves the token as `teams_user@example.com.json`, creates a `teams`
source, and confirms the account email as your default "me" identity.

| Flag | Description |
|---|---|
| `--tenant` | Azure AD tenant ID for this authorization; defaults to `common` |
| `--no-default-identity` | Do not auto-confirm the email address as this source's "me" identity |

## Sync Teams

Run `sync-teams` to pull your Teams history:

```bash
# Full first run or incremental later runs are auto-detected.
msgvault sync-teams user@example.com

# Chats only
msgvault sync-teams user@example.com --no-channels

# Test with a per-conversation limit
msgvault sync-teams user@example.com --limit 100

# Re-fetch all messages and upsert them in place
msgvault sync-teams user@example.com --full
```

The first run walks your chats, joined teams, channels, root channel messages,
and replies. Later runs resume from stored cursors and checkpoints. If a sync
is interrupted, run `sync-teams` again; msgvault skips completed conversations
and picks up incomplete ones where they left off.

| Flag | Description |
|---|---|
| `--no-channels` | Sync chats only and skip team channels |
| `--limit` | Maximum messages per conversation (`0` means no limit) |
| `--full` | Ignore stored cursors and re-fetch every message, repairing/backfilling rows in place |

## What gets archived

- One-on-one chats, self-chat, group chats, meeting chats, team channels, and
  channel replies.
- Plain-text bodies derived from Graph HTML bodies, along with the original
  HTML body when present.
- Senders and conversation members as participants, so your Teams contacts can
  appear in the same contact graph as your email and text-message contacts.
- The original Graph message JSON in raw-message storage with format
  `teams_json`.
- Link attachments as attachment references.
- Inline hosted-content images downloaded into msgvault's attachment store.
- Call-recording event links in the searchable body text.

Self-chat is included automatically whenever the account has messages to itself,
with no extra configuration. If the self-chat check fails, msgvault counts it
in the sync error total and continues syncing ordinary chats.

When Graph reports a `deletedDateTime`, msgvault marks the Teams message as
deleted in the archive while keeping its previously archived content.

## Inline media backfill

If you imported Teams messages before inline hosted-content downloads were
available, or if a transient Graph error left some inline images missing, run
`backfill-teams-media`:

```bash
msgvault backfill-teams-media user@example.com
msgvault backfill-teams-media user@example.com --only-incomplete
```

The backfill scans stored Teams HTML bodies for `hostedContents` URLs and
downloads eligible images. Files with the same content share storage, and the
[media policy](#media-policy) still controls which conversations and file sizes
are eligible.

## Media policy

Attachment downloads follow the shared chat media policy. By default, msgvault
skips media from chats and channels with more than 20 members and records a
typed `participant_threshold` marker, while keeping media from one-to-one and
small group chats. You can change those defaults under `[teams]`:

```toml
[teams]
media = true
media_scope = "all"            # all, direct (chats only), or none
media_max_participants = 20    # 0 = no cap
max_media_mb = 250

[teams.accounts_config."user@example.com"]
max_media_mb = 500
```

See [Media policy](/docs/configuration/#media-policy) for the full vocabulary,
and use `msgvault purge-excluded-media` to remove media that a changed policy
would no longer collect.

## Scheduled sync

After `add-teams` creates the source and token, you can schedule Teams syncs in
`msgvault serve` with the standard `[[accounts]]` block:

```toml
[[accounts]]
email = "user@example.com"
schedule = "*/30 * * * *"
enabled = true
```

The scheduler syncs each source registered for that address, including its
`teams` source. Scheduled Teams syncs include channels.

## Search and query

Teams messages use `message_type = teams`. Filter them in search and SQL
queries:

```bash
msgvault search "incident review" --message-type teams
msgvault search "message_type:teams incident review"
msgvault query --format table "
  SELECT sent_at, from_email, subject, snippet
  FROM v_messages
  WHERE message_type = 'teams'
  ORDER BY sent_at DESC
  LIMIT 20
"
```

Manual `sync-teams` runs don't trigger the embedding worker right away. If
vector search is enabled and you want newly synced Teams messages in semantic
or hybrid results, run `msgvault embeddings build` after syncing, or set
`[vector.embed.schedule].run_after_sync = true` for scheduled daemon syncs.

In the [Web UI](/docs/web-ui/), Teams direct chats, group chats, and channel
conversations appear as conversation rows in **Messages**, where you can use
the same search, filters, and grouping as other archive sources. In the
[TUI](/docs/usage/tui/), press `m` to switch from Email mode to Texts mode.
