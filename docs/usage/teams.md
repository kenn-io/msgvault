---
last_edited: "2026-09-08"
title: Microsoft Teams
description: Archive Microsoft Teams chats and channels through delegated Microsoft Graph sync.
---

Search Teams chats, channel discussions, and replies alongside your email and
other messages. msgvault archives their text, participants, source JSON, and
eligible inline images through Microsoft Graph.

Teams messages use `message_type = teams`. Sync reads the provider without
sending messages, editing Teams content, or changing channel membership.

## Prerequisites

Teams ingestion uses Microsoft Graph delegated OAuth, not IMAP. It requires the
same `[microsoft]` `client_id` config block used by `add-o365`, but it stores a
separate Graph token under `tokens/teams_<email>.json`. An Outlook IMAP token
created by `add-o365` does not authorize Teams sync.

Register a Microsoft Entra app as described in
[OAuth Setup](/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail), with:

- Redirect URI: `http://localhost:8089/callback/microsoft`
- Public client flows enabled
- Delegated Microsoft Graph permissions:
  `Chat.Read`, `ChannelMessage.Read.All`, `Team.ReadBasic.All`,
  `Channel.ReadBasic.All`, `User.Read`, `User.ReadBasic.All`,
  `TeamMember.Read.All`, and `ChannelMember.Read.All`

Some tenants require an administrator to grant consent for channel-message
permissions before users can authorize the app.

Configure msgvault:

```toml
[microsoft]
client_id = "your-azure-app-client-id"
# tenant_id = "your-org-tenant-id"  # optional; default is "common"
```

## Authorize Teams

```bash
msgvault add-teams user@example.com
```

The command opens a browser, requests the Graph scopes above, verifies the
returned identity, stores the token as `teams_user@example.com.json`, creates a
`teams` source, and confirms the account email as the default "me" identity.

| Flag | Description |
|---|---|
| `--tenant` | Azure AD tenant ID for this authorization; defaults to `common` |
| `--no-default-identity` | Do not auto-confirm the email address as this source's "me" identity |

## Sync Teams

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

The first run walks chats, joined teams, channels, root channel messages, and
replies. Later runs resume from stored cursors and checkpoints. If a run is
interrupted, re-run `sync-teams`; completed conversations are skipped and
incomplete ones continue.

| Flag | Description |
|---|---|
| `--no-channels` | Sync chats only and skip team channels |
| `--limit` | Maximum messages per conversation (`0` means no limit) |
| `--full` | Ignore stored cursors and re-fetch every message, repairing/backfilling rows in place |

## What Gets Archived

- One-on-one chats, self-chat, group chats, meeting chats, team channels, and
  channel replies.
- Plain-text body text derived from Graph HTML bodies, plus the original HTML
  body when present.
- Sender and conversation members as participants, so Teams contacts can appear
  in the same contact graph as email and text-message contacts.
- The original Graph message JSON in raw-message storage with format
  `teams_json`.
- Link attachments as attachment references.
- Inline hosted-content images downloaded into msgvault's attachment store.
- Call-recording event links in the searchable body text.

Self-chat is included automatically when the account has messages to itself;
it needs no extra configuration. A failed self-chat check is reported in the
sync error count while ordinary chats continue.

Deleted Teams messages are marked deleted in the archive when Graph reports a
`deletedDateTime`; previously archived content is retained.

## Inline Media Backfill

If you imported Teams messages before inline hosted-content downloads were
available, or a transient Graph error left some inline images missing, run:

```bash
msgvault backfill-teams-media user@example.com
msgvault backfill-teams-media user@example.com --only-incomplete
```

The backfill scans stored Teams HTML bodies for `hostedContents` URLs and
downloads eligible images. Files with the same content share storage. The
[media policy](#media-policy) still determines which conversations and file
sizes are eligible.

## Media Policy

Attachment downloads follow the shared chat media policy. By default media
from chats and channels with more than 20 members is skipped with a typed
`participant_threshold` marker, while one-to-one and small group chats keep
theirs. Adjust it under `[teams]`:

```toml
[teams]
media = true
media_scope = "all"            # all, direct (chats only), or none
media_max_participants = 20    # 0 = no cap
max_media_mb = 250

[teams.accounts_config."user@example.com"]
max_media_mb = 500
```

See [Media policy](/docs/configuration/#media-policy) for the full vocabulary and
`msgvault purge-excluded-media` for removing media a changed policy would no
longer collect.

## Scheduled Sync

`msgvault serve` can schedule Teams syncs through the normal `[[accounts]]`
block after `add-teams` creates the source and token:

```toml
[[accounts]]
email = "user@example.com"
schedule = "*/30 * * * *"
enabled = true
```

The scheduler resolves the entry to the `teams` source when that account has a
Teams source. Scheduled Teams syncs include channels.

## Search and Query

Teams messages use `message_type = teams`:

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

Manual `sync-teams` does not run the embedding worker immediately. If vector
search is enabled and you want newly synced Teams messages in semantic/hybrid
results, run `msgvault embeddings build` after the sync, or configure
`[vector.embed.schedule].run_after_sync = true` for scheduled daemon syncs.

In the [Web UI](/docs/web-ui/), Teams direct chats, group chats, and channel
conversations appear as conversation rows in Everything and can be combined
with the same search, filters, and grouping as other archive modalities. In
the [TUI](/docs/usage/tui/), press `m` to switch from Email mode to Texts mode.
