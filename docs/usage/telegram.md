---
last_edited: "2026-10-30"
title: Telegram
description: Archive Telegram history and keep it current with read-only sync.
---

# Archive Telegram

Connect a Telegram account to archive its accessible chats and keep them current
with scheduled sync. msgvault uses Telegram's API as an unofficial client. It
reads history, edits, deletions and eligible media; it does not send messages,
mark them read, join chats or change them. Telegram monitors unofficial clients
and may limit or restrict an account. Use your own API credentials and review
[Telegram's API terms](https://core.telegram.org/api/terms).

## Connect and sync

1. Create your own `api_id` and `api_hash` at
   [my.telegram.org](https://my.telegram.org/apps).
2. Run `msgvault add-telegram` and follow the prompts for your credentials,
   phone number, verification code and, if enabled, two-step password. To scan
   a QR code instead, run `msgvault add-telegram --qr`.
3. Run `msgvault sync-telegram`. The first run backfills accessible history;
   later runs fetch new messages and explicit edits and deletions.

Login happens on the invoking workstation. With a remote daemon, the verified
session is sent through the authenticated daemon CLI connection and saved on
the daemon. Keep the token directory private. If Telegram revokes the session,
run `msgvault add-telegram` again for that account.

With no argument, sync processes every active registered Telegram account.
Pass its exact `user<ID>` identifier to select one. Private chats, bots, Saved
Messages, basic groups and supergroups are included by default. Broadcast
channels are excluded unless selected with `--include-channels`, `--chat`, or
configured for the account as shown below. See the
[CLI reference](../cli-reference.md#sync-telegram) for bounded history and
first-backfill takeout options.

## Schedule sync

Add the exact account identifier printed during registration to `config.toml`:

```toml
[[accounts]]
email = "user123456789"
schedule = "*/15 * * * *"
enabled = true

[telegram.accounts_config.user123456789]
channels = [-1000000000123]
```

Omit `channels` to leave broadcast channels out of scheduled runs. Entries are
marked Telegram channel IDs, including the `-100` prefix. Start `msgvault serve`
to run the schedule. The Web UI's Sync now action runs a configured account
immediately. Manual and scheduled runs serialize on the source lock.

By default msgvault limits requests to about two per second and stops a run if
one Telegram flood wait exceeds five minutes. Set `requests_per_second` and
`max_flood_wait` in `[telegram]` to change these limits; the login workstation
uses its own configuration and the daemon uses its configuration for sync.
See [configuration](../configuration.md#telegram) for the settings and media
policy.

## Media, edits and gaps

The default media policy downloads eligible photos, documents, voice and video
notes, and stickers up to 250 MiB from conversations with at most 20 members.
If Telegram does not provide a group membership count, the download remains
pending when that limit applies. Metadata is retained for skipped or failed
files. Change the [media policy](../configuration.md#media-policy) and run
`msgvault backfill-telegram-media user123456789` to retry existing occurrences.
Its `--max-media-mb` and `--max-participants` flags can override those two
limits for that run; `0` disables the participant limit.

Edits update archived text and record edit evidence. Explicit deletions mark
messages deleted while retaining their archived content and attachments. Lost
access or an incomplete response does not imply deletion. When Telegram
reports an update gap, msgvault warns and rescans the recent seven-day window;
older edits or deletions may remain missing. Inaccessible channels retain their
last applied update position.

## Exports, overlap and AI use

A [Telegram Desktop JSON export](text-messages.md#import-telegram) and live
sync for the same account share source and message keys. Reimported exports are
append-only; live sync applies later edits and explicit deletions. A Beeper
Telegram bridge uses a separate source and can archive duplicate messages.
When archived Beeper identity evidence confirms the same account, sync warns
about the overlap. Choose one sync path per account when possible.

Telegram's [API terms §1.5](https://core.telegram.org/api/terms) prohibit using
Telegram platform data for AI or machine-learning development or deployment.
msgvault does not automatically exclude Telegram sources from embeddings or
owner search. The [embedding scope](../configuration.md) can restrict newly
generated embeddings to specified types or accounts, and delegated agent
tokens can be restricted to selected sources; review those settings before
enabling AI features.
