# Discord Guild Import — Design

Date: 2026-07-19
Status: Approved

## Goal

Add native, read-only Discord guild archiving to msgvault. A Discord bot
imports the message history it can access into the existing source,
conversation, message, participant, attachment, FTS, analytics, and TUI
surfaces. The resulting archive is self-contained and does not require a
second crawler or database.

The first release covers:

- one msgvault source per Discord guild;
- text and announcement channels;
- forum posts and active or archived threads;
- full historical backfill plus scheduled incremental sync;
- messages, edits, recent and full-scan deletion detection, mentions, reply
  references, reaction summaries, and raw API payloads;
- best-effort enrichment from member data attached to observed messages; and
- attachment download into msgvault's content-addressed store.

## Non-goals

- Personal or group DM history. Bot tokens can read only guilds where the bot
  is installed. Msgvault will not support user-token or selfbot automation.
- A long-running Discord Gateway listener. Version 1 uses manual and
  daemon-scheduled REST syncs, consistent with the Teams provider.
- Normalized per-user reaction rows. Discord message payloads include reaction
  counts but require a paginated request per emoji to retrieve reactors.
- Voice, moderation, message sending, or any other write operation.
- A dependency on another crawler, database, or private codebase.

## Architecture

Discord is a native provider parallel to Teams and uses no new core tables.

```text
add-discord / sync-discord / backfill-discord-media
                         |
                  internal/discord
    client - importer - mapping - participants
       media - syncstate - token - API types
                         |
      existing msgvault store, FTS, attachments,
          embeddings, analytics, and TUI
```

Planned files:

```text
cmd/msgvault/cmd/add_discord.go
cmd/msgvault/cmd/sync_discord.go
cmd/msgvault/cmd/backfill_discord_media.go

internal/discord/client.go
internal/discord/importer.go
internal/discord/mapping.go
internal/discord/participants.go
internal/discord/media.go
internal/discord/syncstate.go
internal/discord/token.go
internal/discord/types.go
```

`internal/discord/client.go` is a minimal REST client, not a general Discord
SDK. The importer and setup flow use only this read surface:

- current bot identity and accessible guilds;
- guild metadata and channel catalog;
- active and archived thread catalogs;
- channel or thread messages and individual message refresh;
- attachment bytes from approved Discord CDN origins.

The client implements Discord's route-bucket and global rate limits using the
response rate-limit headers, honors `Retry-After`, and performs bounded retries
for 429 and server failures. It never exposes a write-method surface.

## Setup and credential binding

`msgvault add-discord` prompts for a bot token through protected input rather
than accepting it on the command line. It validates the bot identity, lists the
guilds the bot can access, and registers selected guilds. If exactly one guild
is available, setup selects it automatically and echoes its name and ID for
confirmation. An explicit `--guild <id>` always overrides automatic selection.
If several are available, the user must pass one or more `--guild <id>` flags;
setup never silently archives every accessible guild.

Each registered guild becomes a source with:

| Field | Value |
|---|---|
| `source_type` | `discord` |
| `identifier` | Discord guild ID |
| `display_name` | Current guild name |
| `oauth_app` | Optional bot-credential binding label |

The existing nullable `sources.oauth_app` column is reused only as a binding
label. The generic OAuth configuration loader does not read or interpret
Discord tokens, and no `[oauth.apps]` entry is required.

The Discord token manager stores each credential as
`tokens/discord_<bot-user-id>.json` with mode `0600`. The record includes its
optional binding label. `add-discord --oauth-app <name>` creates or resolves
that label through the Discord token records and assigns it to every guild
source registered through that bot.

Without `--oauth-app`, a NULL source binding resolves to the sole/default
Discord token. Adding a second unnamed bot is rejected, and a NULL binding is
an error when multiple records make it ambiguous. Adding any second bot is
rejected while NULL-bound Discord sources still exist; the user must first
promote the sole token and its sources to a named binding by re-running setup
with `--oauth-app`. Removing a Discord source deletes a token only when no
remaining Discord source references the resolved credential.

## Permissions and setup diagnostics

The bot needs permission to view each desired channel and read message
history. Message Content Intent is required for message bodies. Participant
names are enriched from member data attached to observed message payloads;
version 1 does not import the guild roster and does not require Server Members
Intent for archival. Archived private-thread coverage depends on the bot's
channel and thread permissions.

Setup and sync output must distinguish:

- missing Message Content Intent;
- missing guild or channel access;
- archived private threads unavailable; and
- authentication or token-binding failure.

Discord sync is read-only and never sends, edits, deletes, reacts to, or
moderates remote content.

## Data-model mapping

The provider reuses the generic chat model.

| Discord concept | Msgvault representation |
|---|---|
| Guild | One `discord` source keyed by guild ID |
| Text or announcement channel | `conversations`, `conversation_type = "channel"` |
| Forum post or thread | Its own `channel` conversation/message container |
| Message | `messages`, `message_type = "discord"` |
| Discord snowflake | `source_message_id` and sync ordering key |
| Readable content | `message_bodies.body_text` and FTS |
| Reply | `reply_to_message_id` when resolvable; raw reference in metadata always |
| Author | `sender_id` and a `from` recipient row |
| User mention | A `mention` recipient row |
| Role or `@everyone` mention | Structured message metadata |
| Edit | Idempotent upsert plus `is_edited` |
| Upstream deletion | `deleted_from_source_at`; archived content remains |
| Attachment | Content-addressed file or metadata-only attachment row |
| API payload | `message_raw`, `raw_format = "discord_json"` |
| Reaction count | Stable `reaction_summaries` message metadata |

Each channel and thread is an independent conversation and message container.
Conversation metadata preserves the guild ID, parent channel ID, Discord
channel type, topic, NSFW state, archive state, lock state, and thread
attributes. Categories provide context but are not message containers.

### Participants and authors

Regular users and bots use a global `discord_user_id` participant identifier,
so the same Discord account can resolve to one participant across guilds.
Discord exposes no member email, so the importer performs no automatic
email-identity unification.

Member data attached to observed messages supplies usernames, global display
names, bot status, and guild nicknames without a roster request. Guild-specific
display names stay with the observed message or conversation metadata instead
of becoming a mutable global identity. Only observed authors and mentions
become conversation participants; the importer never adds an entire guild
roster to its conversations.

Regular bots carry `author_kind = "bot"` in message metadata. Webhook messages
use a separate `discord_webhook_id` participant identifier. The participant
keeps a stable best-known label, while each message retains the webhook's
observed username/avatar overrides and `author_kind = "webhook"`. One webhook
may therefore present different names over time without creating false user
identities. Application-authored messages use a stable application or bot
identity when available, otherwise a provider-scoped automated identity.

### Content and system messages

Readable body text resolves user, channel, and role mentions when names are
known and includes meaningful poll, sticker, and authored embed content. Link
previews are not indexed as though the author wrote them. Raw Discord markdown
and the complete API object remain available in `message_raw`.

All Discord message types are archived. Known system types, including joins,
boosts, pin notifications, and thread starters, render as readable system
text. Unknown future types are retained with raw JSON and conservative system
text instead of being silently dropped.

Every reply stores at least this provider metadata even if its target cannot
be resolved:

```json
{
  "referenced_message_id": "1234567890",
  "referenced_channel_id": "9876543210"
}
```

A linking pass fills `reply_to_message_id` after messages persist. A deleted,
out-of-range, or cross-container target remains explicit and can render as a
reply to an unavailable message. Later syncs may resolve it if the parent
becomes available.

### Reaction summaries

Version 1 stores a stable summary in message metadata:

```json
{
  "reaction_summaries": [
    {"emoji": "thumbsup", "emoji_id": "123456789012345678", "animated": false, "count": 12}
  ]
}
```

The raw payload preserves Discord's complete summary object. The importer does
not populate normalized `reactions` rows, because doing so requires at least
one additional paginated API request per emoji per reacted message. Discord is
therefore an explicit user-visible outlier: existing normalized reaction views
and analytics show no Discord reactions. The stable summaries remain archived
in message metadata for future provider-aware consumers, but current generic
message-detail models do not expose them. A future opt-in reactor-detail pass
can populate normalized rows without changing the message import.

For Unicode emoji, `emoji` contains the character and `emoji_id` is omitted.
For custom guild emoji, `emoji` contains the name, `emoji_id` contains its
stable Discord ID, and `animated` records whether it is animated. This shape is
the stable archive contract for future provider-aware display and
reactor-detail backfill.

## Attachments and media

Attachment bytes at or below a configurable size cap are downloaded into
msgvault's content-addressed store through `export.StoreAttachmentFile`.
Version 1 defaults to a 50 MiB cap. Larger attachments retain filename, MIME
type, size, dimensions, source attachment ID, and the observed CDN URL as
metadata.

The dedicated `media.go` boundary validates HTTPS scheme, exact approved
Discord CDN hosts, and attachment paths. CDN requests never receive the bot
token. Redirects are disabled or revalidated so attacker-controlled message
content cannot redirect a credentialed request. API next-page links likewise
remain restricted to the configured Discord API origin.

Discord attachment URLs contain expiring signatures. An archived URL is
provenance, not a durable lazy-download path. `backfill-discord-media`
re-fetches the originating message through the API to obtain a fresh signed
URL before downloading. Download failures leave retryable attachment metadata
and do not prevent the durable message cursor from advancing.

An over-cap or failed attachment becomes permanently unfetchable if its
message is later deleted upstream, because no API path remains for obtaining a
fresh URL. Users who need those files must raise the cap before the original
history sync rather than relying on a later backfill.

## Sync state

Sync state is JSON stored in a completed run's `sync_runs.cursor_after` and in
the active or failed run's `cursor_before` checkpoints:

```json
{
  "containers": {
    "<channel-or-thread-id>": {
      "high_water": "<newest-persisted-snowflake>",
      "backfill_before": "<oldest-page-cursor>",
      "backfill_upper": "<pinned-starting-head>",
      "backfill_complete": true
    }
  },
  "thread_catalog": {
    "<parent-channel-id>": {
      "public_archive_watermark": "<timestamp>",
      "private_archive_watermark": "<timestamp>"
    }
  }
}
```

Every channel and thread has independent progress. There is no shared channel
tree cursor.

### Resumption precedence

The importer loads state using the existing Teams precedence:

1. Load the most recent completed run's `cursor_after` as the baseline.
2. If the newest run is failed or running and has `cursor_before`, merge that
   newer checkpoint over the baseline.
3. Use monotonic maximums for high-water and catalog fields. Use the newer
   checkpoint's opaque backfill position and pinned bounds.
4. Fail on malformed state rather than silently resetting progress. `--full`
   is the explicit repair path.

This uses `GetLastSuccessfulSync` and `GetLatestCheckpointedSync`, whose store
contract makes an interrupted checkpoint stale after a later completed run.

## Catalog discovery

Each run refreshes guild and top-level channel metadata, discovers active
threads, and enumerates archived public and private threads under eligible
parents.

An initial or `--full` run exhaustively enumerates archived threads.
Incremental runs retrieve active threads and walk recent archived-thread pages
back to the saved per-parent watermark. This catches a thread created and
archived between scheduled runs. Previously stored threads remain eligible
even when absent from the active catalog.

A public or private archive watermark advances only after all relevant pages
for that parent complete successfully. A partial catalog response never makes
stored conversations disappear.

## Initial backfill

For each selected container, the importer pins the current message head in
`backfill_upper`, then pages backward with Discord's `before` cursor in batches
of up to 100.

Each page is mapped and durably upserted before `backfill_before` advances.
Core persistence includes the message row and body, raw JSON, FTS data,
participants, mentions, reply metadata, reaction summary, and attachment
metadata. Binary media failure records retryable state but does not invalidate
otherwise durable message persistence.

State is checkpointed after every completed page, so interruption repeats at
most one page. Reaching the beginning, or an explicit `--after` lower bound,
sets `backfill_complete`. Messages arriving above the pinned starting head are
collected by the forward phase or a later incremental run.

## Incremental sync and bounded repair

Each channel and thread independently performs:

1. A forward scan with `after = high_water`.
2. A complete newest-to-oldest scan of the trailing edit window, default seven
   days.
3. Idempotent upserts for every message in that window, refreshing edits, raw
   payloads, reaction summaries, attachment metadata, and readable content.
4. A remote-versus-archived ID comparison over a fixed snowflake interval.
5. `deleted_from_source_at` marking for archived IDs missing remotely.

The comparison interval is pinned as `(lower, upper]` when the window scan
starts:

- `lower` is the edit-window start converted to a Discord snowflake;
- the importer snapshots archived IDs for that container once at scan start;
- `upper` is the greater of the remote container head and the highest archived
  ID in that pinned local snapshot;
- remote paging begins with `before = successor(upper)` and stops at `lower`;
- remote enumeration and the archived-side query use the identical container
  ID and snowflake predicates; and
- the bounds remain fixed for the scan's lifetime.

Including the pinned local maximum lets repair detect when the newest archived
message was deleted and the current remote head is older. Messages persisted
above `upper` after the snapshot, whether from the forward scan or a concurrent
arrival, cannot enter either side of the comparison and cannot be falsely
tombstoned. Cancellation, incomplete pagination, rate-limit exhaustion,
missing access, malformed payloads, or persistence errors suppress deletion
marking for that container.

Reaction summaries inherit the same consistency boundary: recent changes are
captured inside the trailing window, and historical changes during `--full`.
Edits and deletions outside the incremental window are not discovered until a
full repair.

## Full repair and deletion semantics

`sync-discord --full` ignores normal completion cursors and exhaustively
re-enumerates each selected container. Its deletion comparison uses the same
fixed interval rule, with zero as the lower snowflake for an unbounded run.
With `--after`, both enumeration and comparison are limited to that lower
bound; earlier archive rows remain untouched.

Only a complete and successfully persisted interval can mark missing IDs with
`deleted_from_source_at`. The message row, body, raw JSON, reactions summary,
and attachments remain in the archive. Messages deleted before msgvault first
observes them are unrecoverable.

Deletion comparison is per container, not per guild. A complete scan of one
thread can safely mark its missing messages even when an unrelated channel
fails, while the failed container performs no deletion marking.

## Missing and inaccessible containers

A 403 response records `container_inaccessible_since` in conversation
metadata. A 404 Unknown Channel records `container_missing_since` and
`container_missing_reason = "unknown_channel"`. Neither response tombstones
the container's messages or removes its cursor.

A catalog refresh preserves an existing access marker. Only a later successful
message scan and reconciliation for that container clears it. Version 1 thus
records likely upstream channel or thread deletion without turning one API
response into mass message tombstones. A missing conversation remains archived
but stops updating.

## Run completion and failure accounting

After container processing, the importer resolves deferred reply links,
enqueues changed messages for embeddings, and calls
`RecomputeConversationStats`. Member fields already attached to observed
message payloads are mapped as part of normal message ingestion.

Authentication, guild discovery, required catalog, decoding, transient API,
and database failures fail the sync run while preserving safe checkpoints. A
container that explicitly returns missing access or Unknown Channel is
recorded as skipped and does not erase state. Media download failures are
warnings with an independent retry path.

Only a clean core run calls `CompleteSync`; other runs call `FailSync` with the
latest checkpoint intact. Manual and scheduled syncs use the same importer and
state machine.

## CLI and configuration

Planned commands:

```text
msgvault add-discord [--oauth-app <name>] [--guild <id> ...]
msgvault sync-discord [<guild-id-or-name>] [--full] [--after <time>]
msgvault backfill-discord-media [<guild-id-or-name>] [--only-incomplete]
msgvault remove-account <guild-id> --type discord
```

With an explicit argument, `sync-discord` resolves exactly one registered
guild by ID or unambiguous display name. With no argument, it syncs every
registered Discord source sequentially in stable source-ID order and reports
each guild's result; one guild failure does not prevent later guilds from
running, but the overall command returns an aggregate error.

Discord becomes a known text message type so existing search, TUI, API,
analytics, and message-type filters include it. The daemon resolves and
schedules each guild source independently through the existing source
scheduler and `runScheduledSync` dispatch.

Provider configuration supplies conservative defaults and optional per-guild
channel filters:

```toml
[discord]
max_media_bytes = 52428800
edit_rescan_window = "168h"

[discord.guilds."123456789012345678"]
include = ["111111111111111111"]
exclude = ["222222222222222222"]
```

The default is every bot-accessible text/announcement channel, forum post,
and active or archived thread. Include/exclude filters are applied to message
containers. Top-level channels match their own IDs. Threads and forum posts
inherit their parent channel's included or excluded state, while a thread ID
listed explicitly in `include` or `exclude` overrides that inheritance. When
the same container is listed in both sets, `exclude` wins. Tokens and binding
labels are never stored in this configuration.

## Security

- Tokens are read from protected input and stored in `0600` files under the
  configured token directory.
- Bot tokens are attached only to exact configured Discord API origins.
- API absolute pagination URLs are rejected if their scheme or host differs
  from the configured origin.
- CDN downloads never carry the bot token and accept only approved HTTPS
  origins and attachment paths.
- Redirects are disabled or fully revalidated before following.
- The client contains an explicit read-only route allowlist.
- Logs, progress output, sync errors, fixtures, and docs never include tokens
  or real guild/member data.
- Sync does not use user tokens or Gateway/selfbot behavior.

## Testing

All Go tests use testify `assert` and `require`, with expected values first.
Tests use synthetic names and IDs and run with the repository's required
`fts5 sqlite_vec` build tags.

### Mapping tests

Table-driven mapping tests cover:

- regular users, bots, webhooks, and application messages;
- known and unknown system message types;
- markdown, mentions, polls, stickers, embeds, and link previews;
- resolved and unavailable reply targets;
- edits and stable reaction-summary metadata; and
- attachment metadata and raw JSON retention.

### Client tests

An `httptest` Discord API exercises the production client for:

- route-bucket and global rate limits;
- 429 `Retry-After` and bounded server-error retries;
- pagination and strict API-origin validation;
- active, public archived, and private archived thread enumeration;
- missing permissions and Unknown Channel classification; and
- token and redirect isolation on CDN requests.

### Import integration tests

The fake API drives the real importer into a test store and verifies:

- guild-per-source and container-per-conversation mapping;
- first-run backfill, page checkpoints, interruption, and resume;
- `cursor_after` baseline plus newer `cursor_before` precedence;
- independent channel/thread high-water marks;
- threads created and archived between incremental runs;
- a forward arrival above a pinned edit-window head is not tombstoned;
- identical `(lower, upper]` predicates on remote and archived comparisons;
- edit, reaction-summary, recent deletion, and full historical deletion repair;
- incomplete or denied scans never mark deletions;
- one failed container cannot affect sibling deletion comparisons;
- 403/404 metadata markers and clearing after recovery;
- media cap, failed download, fresh-URL backfill, and content-addressed dedup;
- FTS visibility, conversation statistics, and embedding enqueue behavior; and
- rerun idempotency without duplicate messages or attachments.

Credential tests cover unnamed-token ambiguity, named binding resolution, and
reference-counted token deletion. Store helpers added for interval comparison,
conversation metadata, or pending media receive SQLite and PostgreSQL coverage
where dialect behavior differs.

An optional environment-gated live smoke test may validate Discord's real
rate-limit headers and archived-thread behavior against a dedicated synthetic
test guild. No live token, guild ID, member identity, or payload is committed.

## Documentation deliverables

Implementation includes:

- `docs/usage/discord.md`, following the Teams usage guide's structure;
- CLI reference entries for all three Discord commands;
- configuration reference for media, edit-window, and guild filters;
- setup documentation for bot creation, intents, and permissions;
- daemon scheduling and search examples; and
- an explicit limitations section covering guild-only scope, bounded edit and
  reaction consistency, normalized reaction absence, signed attachment URLs,
  and missing-container behavior.

## Future work

- Optional Gateway ingestion for near-real-time create/edit/delete events.
- Opt-in reactor-detail backfill that populates normalized `reactions` rows.
- More explicit conversation-level upstream-deletion state if the core model
  later gains a generic field for it.
- Additional attachment recovery workflows where the API still exposes a
  fresh message payload.
