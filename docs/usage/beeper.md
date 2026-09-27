---
last_edited: "2026-09-26"
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

## Send audio to Docbank

The daemon can copy stored Beeper audio to a separately running Docbank media
service. Docbank keeps the recording, imports Beeper's own transcript, and
processes it with its `supplied-transcript` profile. msgvault records which
Docbank source and occurrence belong to each live message. Configure the
destination in
[`[integrations.docbank]`](/docs/configuration/#send-beeper-audio-to-docbank).

What you need:

- A Docbank server with the media HTTP routes
  ([docbank#346](https://github.com/kenn-io/docbank/pull/346)). The Docbank
  library built into msgvault does not provide them.
- `upload_consent = true`. It allows transport to that URL only. Docbank's own
  processing consent decides whether the transcript is processed.
- WAV or MP3 audio. msgvault checks the bytes and sends them as `audio/wav` or
  `audio/mpeg`, whatever type the provider reported. Docbank accepts no other
  codec, so OGG/Opus, M4A and other formats stay local with the
  `unsupported_media` code. msgvault never converts audio or runs speech
  recognition.

What happens:

- Voice notes and ordinary audio both qualify when they are stored, standalone
  Beeper attachments. Previews, stickers and other sources are skipped.
- msgvault reads the complete transcript for that attachment from the archived
  raw message, not the 32 KiB metadata copy. Audio without a transcript is
  still kept by Docbank and reported as `unprocessed`.
- The job backfills existing audio in pages of up to 100 attachments. After
  that first scan, it checks up to 100 attachment changes each minute. It
  starts another full scan a day after the previous scan finishes, to catch
  transcripts that arrive later. Unchanged mappings need no write transaction.
- Each pass performs at most one upload, transcript import, processing request,
  or status check. A status check can make up to three HTTP requests to read
  the job, source, and processing receipt.
- Discovery reads, parsing, and uploads run outside the daemon's operation
  lock. The job takes the lock for individual mapping writes and progress
  updates. If the lock stays busy, for example during a backup, the pass ends;
  a later pass reuses the saved operation ID. Background media work does not
  reset the daemon's idle timer. Shutdown cancels and drains an active pass.
- Uploads use a temporary copy under `data_dir/tmp/beeper-media`, removed when
  the attempt ends. The current Docbank inspector also reads the complete
  recording into memory. The default Beeper download cap is 250 MiB, and this
  route accepts sources up to 1 GiB; memory use includes the recording plus
  inspection and allocation overhead. These file limits are not RAM limits.
- The same recording in several messages gets one occurrence per message.
  Docbank stores the bytes once, and each exact transcript is processed once.
- A hidden, source-deleted, removed or replaced message loses its mapping
  (`revoked`), including audio still waiting to be sent. Other messages
  sharing the audio keep theirs. Reaction changes leave the mapping live.
  If no live or pending occurrence can supply the recording, an unstarted
  transcript delivery stops waiting; restoring an occurrence reopens it.
  msgvault decides which occurrences are live; Docbank keeps the shared evidence.
- Network errors, HTTP 429 and 5xx responses retry after five minutes with the
  same operation ID. So does a request that runs out of time: each request
  gets 30 seconds, and an audio upload gets one more second per 256 KiB.
  Rejected credentials or requests stay `blocked` until the daemon restarts,
  which also resumes checking a queued job. Unsupported codecs and other
  local source problems stay `blocked` across restarts until the message
  changes, and so does their transcript delivery, with the same code.
  Missing or corrupt local bytes, or a temporary upload copy that can't be
  written, wait as `source_unavailable` and retry after five minutes.
- A processed delivery reaches `done` only after Docbank reports coverage
  for its own processing request, not for another transcript of the same
  audio. A failed Docbank job or a failed processing request ends as `done`
  with `operation_state` set to `failed`. A job marked `operator_required`
  stays `blocked` without further polling. Resolve it in Docbank, then restart
  the msgvault daemon to resume checking its state.

Provider transcripts stay searchable through the normal message text. This
route does not add search over Docbank's processed output yet. Check progress
with a query:

```sql
SELECT retention_state, error_code, COUNT(*)
FROM beeper_media_occurrences
GROUP BY retention_state, error_code;

SELECT phase, coverage_state, COUNT(*)
FROM beeper_media_deliveries
GROUP BY phase, coverage_state;
```

## Scheduled sync

Let the daemon run incremental syncs on a schedule:

```toml
[beeper]
enabled = true
schedule = "*/30 * * * *"
```

A scheduled run keeps other sources on their cadence:

- **Bounded runs.** One scheduled Beeper job works for at most 3 minutes
  across all accounts. It also stops early when another scheduled source has
  waited for it for a minute. It stops at a chat or history-page boundary,
  completes the run, and the next run continues from the saved cursors.
- **Account rotation.** After stopping within an account, the next run starts
  with the following account. Each account retains its own progress.
- **New messages first.** Chats with only new messages sync before chats
  still backfilling history. Completed chats are skipped until the current
  discovery cycle finishes, so later chats also get a turn.
- **Fetch errors.** A transient page-fetch failure is retried twice. If it
  still fails, the run completes with an error count and the scheduler reports
  `partial Beeper sync: N fetch error(s)`. Healthy chats keep their progress,
  and the failed chats are retried on the next run.

Manual `msgvault sync-beeper` runs have no time budget.

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
  with an error and marks the account. Scheduled runs then skip that account
  with one warning instead of failing every run. After repairing Beeper
  Desktop, run `msgvault sync-beeper --account <id>` to verify again and clear
  the mark, or remove and re-add the Beeper source.
- **Remote daemons**: the Beeper API is loopback-only, so the msgvault daemon
  must run on the same machine as Beeper Desktop.
- **iMessage**: Beeper only carries iMessage on macOS, so archiving it this way
  needs a Mac running Beeper Desktop beside the msgvault daemon. Networks found
  from chat data are looked for in a bounded scan of your most recently active
  conversations, so one with no chats — or none recent enough to fall inside
  that window — stays invisible until it sees activity; send or receive a
  message, then re-run `add-beeper`. The scan logs a warning when it stops at
  its bound.
