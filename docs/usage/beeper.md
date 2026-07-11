---
title: Beeper
description: Archive every chat network bridged through Beeper Desktop via its local API.
---

[Beeper Desktop](https://www.beeper.com) bridges many chat networks — WhatsApp,
Signal, Telegram, Instagram, LinkedIn, X, Facebook Messenger, and more — into
one app. msgvault can archive all of them at once through Beeper Desktop's
local API. Each connected network account becomes its own msgvault source, so
a Signal thread and a Telegram thread stay separately filterable, while all
Beeper-archived messages share `message_type = beeper` for search.

Beeper sync is strictly read-only: msgvault only calls read endpoints of the
local API and never sends, edits, archives, or marks anything in Beeper.

## Prerequisites

- Beeper Desktop installed and running on the same machine as the msgvault
  daemon (the API listens on `localhost:23373` only).
- A Beeper Desktop access token: in Beeper Desktop open **Settings →
  Developer** and create an access token.

## Add Beeper

```bash
msgvault add-beeper
```

The command validates the token against the running Beeper Desktop, stores it
at `tokens/beeper.json` (0600), and registers one `beeper` source per
connected network account (e.g. `signal`, `telegram`, `whatsapp`).

Provide the token via the interactive prompt, `--token-file <path>`, or the
`MSGVAULT_BEEPER_TOKEN` environment variable:

```bash
MSGVAULT_BEEPER_TOKEN="..." msgvault add-beeper
msgvault add-beeper --token-file ~/beeper-token.txt
```

| Flag | Description |
|---|---|
| `--token-file` | Read the access token from a file |
| `--no-default-identity` | Do not auto-confirm each account's own identity (phone/email) as that source's "me" identity |

## Sync

```bash
# First run backfills all history; later runs are incremental.
msgvault sync-beeper

# Only specific networks.
msgvault sync-beeper --account signal --account telegram

# Repair path: re-fetch everything, upserting in place.
msgvault sync-beeper --full
```

The first sync walks every chat's full locally-available history. This is a
large one-time job for big archives (the API serves ~20 messages per request),
but it is fully resumable: interrupt it any time and the next run continues
from the saved checkpoint. Later runs only fetch chats with new activity.

Recent messages (last 24 hours) are re-checked on every incremental run so
edits, deletions, and reaction changes are captured; older in-place changes
are only picked up by `--full` runs.

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Beeper accountID to sync (repeatable) |
| `--limit` | `0` | Max messages per chat this run (limited backfills resume next run) |
| `--full` | `false` | Ignore stored cursors and re-fetch every message (repairs rows in place) |
| `--no-media` | `false` | Skip attachment downloads for this run |

## What is archived

- Message text, sender, timestamps, and per-network conversation threads
  (groups keep their member lists and admin roles).
- Reactions, reply relationships, mentions, and edit/deletion markers —
  content that was archived before a deletion stays archived.
- Voice-note transcriptions (when Beeper has them) are appended to the message
  body so they are searchable.
- Attachments (photos, videos, voice notes, files) are downloaded during sync
  into msgvault's content-addressed attachment store. Downloads that fail (or
  exceed `max_media_mb`) leave a pending marker and the message is archived
  anyway; retry them later with `msgvault backfill-beeper-media`. Use
  `--no-media` or `media = false` to skip downloads — note that skipped-by-flag
  downloads leave no pending markers, so `backfill-beeper-media` will not fetch
  them later; re-enable media and run `sync-beeper --full` instead.
- The verbatim Beeper API JSON for every message (`raw_format = beeper_json`),
  so nothing is lost even where msgvault's relational model is narrower.

Because Beeper's API serves what Beeper Desktop has synced locally, archive
depth equals your local Beeper history: a freshly added Beeper account may
only have recent messages until Beeper finishes its own backfill.

## Scheduled sync

Let the daemon run incremental syncs on a schedule:

```toml
[beeper]
enabled = true
schedule = "*/30 * * * *"
```

## Configuration

```toml
[beeper]
# url = "http://localhost:23373"   # Beeper Desktop API (default)
enabled = true                     # gate for the daemon schedule below
schedule = "*/30 * * * *"          # 5-field cron; empty = manual sync only
accounts = []                      # accountID include filter (empty = all)
exclude_accounts = []              # e.g. ["whatsapp"] — see below
rate_limit_qps = 20                # request rate against the local API
media = true                       # download attachment bytes
max_media_mb = 100                 # per-attachment size cap
```

### Overlap with native importers

If you already archive a network natively (e.g. `import-whatsapp` or
`import-imessage`), pick one path per network: msgvault does not deduplicate
messages across sources. Add the Beeper accountID to `exclude_accounts` to
keep Beeper sync away from that network. If both paths do run, the rows remain
separable (different sources and different `message_type` values), and
participants still unify across archives via phone-number and email matching
(the Beeper user ID is also persisted as an identifier, so later runs keep
resolving to the same person).

## Caveats

- **Reinstalling Beeper Desktop**: Beeper's message IDs are only stable per
  installation. msgvault verifies several anchor messages (across distinct
  chats) on every run; ordinary churn like deleting an anchored chat is
  tolerated, and only when no anchor survives are recently archived messages
  checked against the source. If the installation was rebuilt, the sync stops
  with an error; remove and re-add the Beeper sources in that case.
- **Remote daemons**: the Beeper API is loopback-only, so the msgvault daemon
  must run on the same machine as Beeper Desktop.
