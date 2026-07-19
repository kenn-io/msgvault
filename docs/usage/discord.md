---
title: Discord
description: Archive Discord guild channels, threads, and attachments through a read-only bot.
---

msgvault can archive Discord guild history into the same local database as
email and other chat sources. Each guild is a separate `discord` source, and
each text channel, announcement channel, thread, or forum post is a separate
conversation. Messages use `message_type = discord`.

Discord sync is read-only. It never sends or edits messages, reacts, changes
membership, or moderates a guild.

## Scope and prerequisites

Discord's bot API exposes guilds the bot has joined. It does **not** expose a
person's direct-message history. msgvault does not accept user tokens and does
not implement a selfbot; using a user token for automated access violates
Discord's rules.

Create a dedicated application and bot in the
[Discord Developer Portal](https://discord.com/developers/applications):

1. Create an application, add its bot user, and copy the bot token.
2. Enable **Message Content Intent** so message bodies are available.
3. Optionally enable **Server Members Intent** for better participant-name
   enrichment. A member-list failure does not block message archival.
4. Install the bot in each guild you want to archive with the `bot` scope and
   only **View Channels** and **Read Message History** permissions.
5. Explicitly grant the bot access to any private channels or threads that
   should be archived.

Do not grant Administrator, Send Messages, Manage Messages, or moderation
permissions. Archived private-thread coverage depends on what the bot can see.
`add-discord` reports missing channel history, member enrichment, private
thread access, and likely Message Content Intent problems without printing the
token.

## Register guilds

Run setup and paste the token at the masked prompt:

```bash
msgvault add-discord
```

The token is never accepted as a command-line flag. It is stored in an
owner-only file under the msgvault token directory, separate from
`config.toml`.

Setup lists every guild visible to that bot. If there is exactly one, msgvault
selects it automatically and echoes its name and ID. If there are several,
select one or more by repeating `--guild`:

```bash
msgvault add-discord \
  --guild 123456789012345678 \
  --guild 234567890123456789
```

`--guild` always overrides automatic selection. One registered guild creates
one source whose stable identifier is the guild ID; a mutable guild name is
only its display name.

### Multiple bot credentials

By default, a guild with no binding label resolves to the sole stored Discord
bot token. This is convenient for one bot, but msgvault rejects a second
unnamed bot rather than silently changing the default.

Use `--oauth-app` as a Discord credential binding label when more than one bot
is needed:

```bash
msgvault add-discord --oauth-app archive-bot-a \
  --guild 123456789012345678
msgvault add-discord --oauth-app archive-bot-b \
  --guild 345678901234567890
```

The label is resolved from protected Discord token records; it does not need
an `[oauth.apps]` entry. To promote an existing sole unnamed credential before
adding a second bot, rerun setup for that bot with `--oauth-app`. Its existing
NULL-bound guild sources are promoted to the new label. A binding that remains
ambiguous fails instead of guessing.

## Configure media, repairs, and channel filters

Discord settings are optional. The defaults download attachments up to 50 MiB
and re-scan the trailing seven days for edits, deletions, and changed reaction
counts:

```toml
[discord]
max_media_bytes = 52428800
edit_rescan_window = "168h"

[discord.guilds."123456789012345678"]
include = ["456789012345678901"]
exclude = ["567890123456789012"]
```

`include` and `exclude` contain Discord channel, thread, or forum-post IDs. An
empty `include` means every accessible message container. Top-level channels
match their own IDs. Threads and forum posts inherit their parent's state, but
an explicitly listed child ID overrides inheritance. An explicit child
`exclude` overrides an included parent, an explicit child `include` overrides
an excluded parent, and `exclude` wins if the same ID appears in both lists.

The guild key must be the exact guild ID used by its source. Tokens and
credential binding labels never belong in this config block.

## Sync manually

Sync one registered guild by ID or by an unambiguous display name:

```bash
msgvault sync-discord 123456789012345678
```

With no argument, every registered Discord source runs sequentially in stable
source order:

```bash
msgvault sync-discord
```

A failure in one guild does not stop later guilds, but the command exits
non-zero with an aggregate error. Interrupted runs resume from durable page
checkpoints.

Use `--after` for an exclusive lower bound in `YYYY-MM-DD` or RFC3339 form:

```bash
msgvault sync-discord 123456789012345678 --after 2025-01-01
```

Use `--full` to ignore normal completion cursors, re-fetch all available
history, repair existing rows, and detect historical upstream deletions:

```bash
msgvault sync-discord 123456789012345678 --full
msgvault sync-discord 123456789012345678 --full \
  --after 2025-01-01T00:00:00Z
```

When combined, `--full --after` repairs only the bounded range and leaves
earlier archived rows untouched.

## Scheduled sync

After registering a guild, schedule its exact guild ID with the standard
`[[accounts]]` block:

```toml
[[accounts]]
email = "123456789012345678"
schedule = "*/30 * * * *"
enabled = true
```

Run `msgvault serve` to activate schedules. Discord display names are neither
stable nor unique, so scheduled entries must use the guild ID. Each guild is
resolved and synced independently through the same importer as
`sync-discord`; schedule multiple guilds with separate `[[accounts]]` blocks.

## What gets archived

- Text and announcement channels, active and archived threads, and forum posts.
- Readable message text plus the complete Discord JSON payload.
- Observed authors and mentions as participants; msgvault does not add the
  entire guild roster to every conversation.
- User-authored and bot-authored messages. Webhooks keep a stable webhook
  identity while preserving per-message username and avatar overrides.
- Known system events, such as joins, boosts, pins, and thread starters, as
  readable system text. Unknown future message types retain raw JSON and a
  conservative rendering instead of being dropped.
- Reply links when their target exists. The referenced message and channel IDs
  remain in metadata when a target was deleted, excluded by `--after`, or
  lives in another container, so the reply can appear as unavailable.
- Attachment metadata and, within the configured cap, content-addressed bytes.

Reaction metadata is stored as stable summaries such as `👍 12`, including
custom emoji name, ID, animation state, and count. Version 1 does not fetch
reactor identities or populate normalized reaction rows, so reaction-detail
views and reaction analytics show no Discord reactions. The summaries remain
archived in stable message metadata for future provider-aware consumers; the
current generic CLI and API message-detail models do not expose them.

## Edit and deletion consistency

New messages are collected incrementally. On each normal sync, every channel
and thread independently re-fetches its trailing `edit_rescan_window` (seven
days by default). Edits, reaction-count changes, and upstream deletions inside
that window are refreshed. Changes older than the window require
`sync-discord --full`.

Deletion detection only runs after a complete, successful scan of the exact
container and time range. A partial page, cancellation, API failure, or lost
access never causes msgvault to infer deletions. Upstream-deleted messages are
marked deleted but their archived content and attachments remain.

A `403` records `container_inaccessible_since`; a Discord `404 Unknown
Channel` records `container_missing_since` and its reason. Neither response
mass-deletes messages or discards the cursor. A successful later scan clears
the marker. Messages deleted before msgvault first observed them cannot be
recovered.

## Attachment backfill and limits

Retry attachment downloads after a transient failure or after raising
`max_media_bytes`:

```bash
# Scan all archived Discord messages that have attachments.
msgvault backfill-discord-media 123456789012345678

# Select only messages with incomplete attachment rows.
msgvault backfill-discord-media 123456789012345678 --only-incomplete

# Process every registered guild.
msgvault backfill-discord-media --only-incomplete
```

The default scan counts already-complete messages but does no download work
for them. Backfill re-fetches each source message to obtain a fresh attachment
URL before retrying incomplete files.

Discord CDN URLs are signed and expire. A stored URL is provenance, not a
durable lazy-download link. If an over-cap or failed attachment matters, raise
the cap before the initial history sync when possible. Once its source message
is deleted upstream, no API path remains to obtain a fresh signed URL and the
metadata-only attachment is permanently unfetchable.

## Search, TUI, and API

Discord messages use the existing search and query surfaces:

```bash
msgvault search "release planning" --message-type discord
msgvault search "message_type:discord incident review"
msgvault query --format table "
  SELECT sent_at, from_name, subject, snippet
  FROM v_messages
  WHERE message_type = 'discord'
  ORDER BY sent_at DESC
  LIMIT 20
"
```

In the [TUI](/usage/tui/), press `m` to switch to Texts mode. Discord channels
and threads appear alongside other chat conversations. The HTTP search and
message endpoints use the same `message_type = discord` filter; see the
[Web Server](/api-server/). If vector search is enabled, run
`msgvault embeddings build` after a manual sync or enable
`[vector.embed.schedule].run_after_sync` for scheduled syncs.

## Remove a guild

Remove one guild source and its archived rows with its stable guild ID:

```bash
msgvault remove-account 123456789012345678 --type discord
```

Removal does not modify Discord. A bot token shared by another registered
guild is preserved; it is deleted only after its last referencing guild source
is removed. A credential is also preserved when removal is forced during an
active sync. Content-addressed attachment files referenced by another source
are preserved as well.
