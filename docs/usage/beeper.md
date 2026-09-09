---
last_edited: "2026-09-08"
title: Beeper
description: Archive every chat network connected to Beeper Desktop via its local API.
---

Archive the chat networks you have connected to
[Beeper Desktop](https://www.beeper.com) through its local API. Each network
account becomes a separate msgvault source, so you can search them together
or filter to one account.

All messages imported this way use `message_type = beeper`. Sync only reads
Beeper; it does not send or edit messages or mark conversations read.

## Prerequisites

- Beeper Desktop installed and running on the same machine as the msgvault
  daemon (the API listens on `localhost:23373` only).
- A Beeper Desktop access token: in Beeper Desktop open **Settings →
  Developer** and create an access token.

## Review identities across sources

Beeper can expose the same person through several networks or through both a
Beeper account and a native msgvault source. The importer compares stable
provider and Beeper identifiers. Strong matching evidence can link identities
automatically; matching display names alone do not.

A same-service, same-scope username match can become a review candidate instead
of an automatic link. Conflicting existing bindings remain conflicts. This is
why two entries for the same person may stay separate after a sync.

Open the Web [Directory review queues](/docs/web-ui/#directory-and-reviews) to inspect
identity candidates and accept or reject the proposed match. Check the source
and identifier evidence before linking. See [people and source identities](/docs/usage/people/)
for the difference between an observed participant and a curated profile.

## Add Beeper

```bash
msgvault add-beeper
```

The command validates the token against the running Beeper Desktop, stores it
at `tokens/beeper.json` (0600), and registers one `beeper` source per
connected network account (e.g. `signal`, `telegram`, `whatsapp`,
`imessage_…`).

Beeper's accounts API omits some networks it serves natively rather than
bridging — currently iMessage — so those are found from chat data instead.
They are printed as *found via chats* and behave like any other `beeper`
source afterwards.

`add-beeper` is safe to re-run and does not disturb existing sources, so run it
again after connecting a new network in Beeper Desktop to register it.

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
- Attachment metadata and eligible downloaded photos, videos, voice notes,
  and files.
- The original Beeper message JSON (`raw_format = beeper_json`) for later
  inspection and repair.

### Media downloads and retries

By default, Beeper media downloads include direct chats and groups with at most
20 participants, with a 250 MiB limit per file. Larger rooms keep their message
text and attachment metadata. Adjust the shared
[media policy](/docs/configuration/#media-policy) to change those limits.

| Download result | How to collect the file later |
|---|---|
| A download failed and remains pending | Run `msgvault backfill-beeper-media` |
| Skipped by the size or participant cap | Change the applicable policy, then retry media backfill |
| Deferred with `--no-media` | Run `msgvault backfill-beeper-media` |
| Excluded by `media = false` | Re-enable media, then run `msgvault backfill-beeper-media` |

Policy skips record a reason such as `size_cap` or `participant_threshold`.
A failed download does not stop the message from being archived. A one-run
`--no-media` deferral leaves pending markers. A disabled media policy leaves
excluded markers, which become eligible when the policy allows them.

Because Beeper's API serves what Beeper Desktop has synced locally, archive
depth equals your local Beeper history: a freshly added Beeper account may only
have recent messages until Beeper finishes its own backfill. That backfill can
land hours or weeks later, behind history msgvault has already walked, so once a
day each sync re-checks the oldest end of every completed chat and resumes the
backfill wherever Beeper has since filled more in. Nothing is needed to trigger
this, and the run reports how many chats it reopened.

## Repairing derived data

Message bodies, snippets, the search index, and attachment classification are
derived from the API payload at import time, so improvements to how they are
derived do not reach messages already archived.

Each account automatically refreshes these fields once on its next sync when
the derivation version changes. It uses the stored JSON and reports how many
rows it repaired. To run that repair now or finish an interrupted pass:

```bash
msgvault repair-derived --source-type beeper
msgvault repair-derived --source-type beeper --identifier instagramgo
```

It needs no Beeper Desktop connection, repairs messages Beeper no longer holds,
and rewrites only derived columns — raw payloads, downloaded media, and sync
cursors are untouched, so it is idempotent.

## Link previews

Media that arrives as a forwarded link preview — an Instagram reel, an x.com
post — is recorded with the URL it previews in `attachments.attachment_metadata`
(`{"shared_url": "..."}`). Voice note transcripts can also appear there under
`source_transcript`. Use `shared_url` to distinguish a shared link preview
from an original photo or file. Download eligibility still follows the
configured media policy.

The metadata copy of `source_transcript.text` is capped at 32
KiB on a UTF-8 boundary. A clipped value includes `"truncated": true`; the
field is omitted when the complete transcript fits. The full transcript stays
in the searchable message body. To see the split:

```sql
SELECT CASE WHEN COALESCE(json_extract_string(a.attachment_metadata, '$.shared_url'), '') <> ''
            THEN 1 ELSE 0 END AS is_share,
       COUNT(*), SUM(a.size)
FROM attachments a
JOIN messages m ON m.id = a.message_id
WHERE m.message_type = 'beeper'
GROUP BY is_share;
```

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
media_scope = "all"                # all, direct, or none
media_max_participants = 20        # skip media from larger rooms; 0 = no cap
max_media_mb = 250                 # per-attachment size cap

# [beeper.accounts_config.signal]   # per-account override, keyed by accountID
# media = true
# max_media_mb = 500
```

See [Media policy](/docs/configuration/#media-policy) for how the scope, participant
cap, size cap, and per-account overrides combine, and
`msgvault purge-excluded-media` for removing media a changed policy would no
longer collect.

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
- **iMessage**: Beeper only carries iMessage on macOS, so archiving it this way
  needs a Mac running Beeper Desktop beside the msgvault daemon. Networks found
  from chat data are looked for in a bounded scan of your most recently active
  conversations, so one with no chats — or none recent enough to fall inside
  that window — stays invisible until it sees activity; send or receive a
  message, then re-run `add-beeper`. The scan logs a warning when it stops at
  its bound.
