---
last_edited: "2026-11-11"
title: Beeper
description: Archive every chat network connected to Beeper Desktop through its local API.
---

Archive the chat networks connected to
[Beeper Desktop](https://www.beeper.com) through its local API. Each network
account becomes its own msgvault source, so you can search them together or
filter to a single account.

Every message imported this way uses `message_type = beeper`. Sync only reads
from Beeper and never sends or edits messages or marks conversations as read.

Separately, you can enable [Beeper chat drafts](../cli-reference.md#beeper-chat-drafts)
per source to place text in Beeper's message box for review. msgvault never
sends it. Writes stop if the box already contains unrelated text, though text
typed between the check and the write can still be lost.

## Prerequisites

- Beeper Desktop installed and running on the same machine as the msgvault
  daemon. The local API listens on `localhost:23373` only.
- A Beeper Desktop access token. To create one, open **Settings → Developer**
  in Beeper Desktop.

## Review identities across sources

Beeper can expose the same person across several networks, or through both a
Beeper account and a native msgvault source. The importer compares stable
provider and Beeper identifiers, and strong matching evidence can link
identities automatically. Matching display names alone do not.

A same-service, same-scope username match can become a review candidate rather
than an automatic link, while conflicting existing bindings remain conflicts.
As a result, two entries for the same person may stay separate after a sync.

To review identity candidates, open
[People: Possible duplicates](/docs/web-ui/#saved-profiles-and-possible-duplicates)
in the Web UI, inspect the source and identifier evidence, and accept or reject
the proposed match. For the difference between an observed participant and a
curated profile, see [people and source identities](/docs/usage/people/).

## Add Beeper

```bash
msgvault add-beeper
```

This command validates your token against the running Beeper Desktop instance,
stores it at `tokens/beeper.json` with mode 0600, and registers one `beeper`
source for each connected network account, for example `signal`, `telegram`,
`whatsapp`, or `imessage_…`.

Beeper's accounts API omits some networks it serves natively rather than
bridging, currently iMessage, so msgvault finds those from chat data instead.
They appear as *found via chats* and behave like any other `beeper` source
afterward.

You can safely run `add-beeper` again without disturbing existing sources, so
re-run it after connecting a new network in Beeper Desktop to register it.

Pass the token through the interactive prompt, with `--token-file <path>`, or
in the `MSGVAULT_BEEPER_TOKEN` environment variable:

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

The first sync walks every chat's full locally available history. Because the
API serves about 20 messages per request, this is a large one-time job for big
archives, though it is fully resumable. You can interrupt it at any time, and
the next run continues from the saved checkpoint. Later runs fetch only chats
with new activity.

Every incremental run rechecks messages from the last 24 hours so edits,
deletions, and reaction changes are captured. msgvault rewrites only messages
that changed since they were archived and counts them as `updated` in the
summary, while `added` counts only messages archived for the first time. Older
in-place changes are picked up only by `--full` runs.

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Beeper accountID to sync (repeatable) |
| `--limit` | `0` | Max messages per chat this run (limited backfills resume next run) |
| `--full` | `false` | Ignore stored cursors and re-fetch every message (repairs rows in place) |
| `--no-media` | `false` | Skip attachment downloads for this run |

### Failed chats

If a chat page or attachment fails to download, sync continues rather than
stopping. The account summary reports an error count followed by the chats
behind those errors:

```text
  signal done in 2m14s: 812 chats, 5310 messages (402 added), 54 errors
  Failed chats (49 errors in 2 chats):
    Book Club (!abc:beeper.local): 36 attachment, 2 fetch errors; last: asset download timed out
    !def:beeper.local: 11 fetch errors; last: connect to Beeper Desktop refused
  5 other errors aren't listed by chat; re-run to retry.
  Retry with the same global options (such as --local):
    fetch errors: msgvault sync-beeper --account signal
    attachment errors: msgvault backfill-beeper-media --account signal
```

Each chat line shows its title or Beeper chat ID, error count, failed steps,
and newest error. Counts cover the whole run, and up to 20 chats are listed
along with a separate line for errors with no chat. When retrying an account's
failures, pass the same global options, such as `--local`.

## What is archived

- Message text, sender, timestamps, and per-network conversation threads.
  Groups keep their member lists and admin roles.
- Reactions, reply relationships, mentions, and edit or deletion markers. Any
  content archived before a deletion stays archived.
- Voice-note transcriptions, when Beeper has them, appended to the message body
  so they are searchable.
- Attachment metadata and eligible downloaded photos, videos, voice notes, and
  files.
- The original Beeper message JSON, stored with `raw_format = beeper_json`, for
  later inspection and repair.

### Media downloads and retries

By default, msgvault downloads Beeper media from direct chats and groups with
at most 20 participants, with a 250 MiB limit per file. Larger rooms keep their
message text and attachment metadata. To change those limits, adjust the shared
[media policy](/docs/configuration/#media-policy).

| Download result | How to collect the file later |
|---|---|
| A download failed and remains pending | Run `msgvault backfill-beeper-media` |
| Skipped by the size or participant cap | Change the applicable policy, then retry media backfill |
| Deferred with `--no-media` | Run `msgvault backfill-beeper-media` |
| Excluded by `media = false` | Re-enable media, then run `msgvault backfill-beeper-media` |
| No longer available at the source | Not collectable; msgvault does not retry it |

When policy skips a file, msgvault records a reason such as `size_cap` or
`participant_threshold`. A failed download does not stop the message from being
archived. The source's freshness still advances if every failed download has a
stored pending marker, nothing else in the run failed, and the run was not
stopped early; the error stays in the run summary either way. Passing
`--no-media` for one run leaves pending markers, while a disabled media policy
leaves excluded markers that become eligible when the policy allows them.

Some networks delete old media from their servers. WhatsApp, for example,
reports `Media is no longer available`. msgvault requests such a file once,
records it as `unavailable` with reason `source_unavailable`, and never retries
it.

Beeper's `Media no longer exists` response is also terminal after one request.

Beeper can also report `Media not found`, `M_NOT_FOUND`, or
`failed to download file: HTTP 404`, sometimes wrapped in an HTTP 502 response.
A plain HTTP 404 that names none of these counts as an ordinary failed
download. When a not-found occurs, msgvault stops retrying that download for
the current run while keeping the marker pending. A not-found alone never ends
retries for good. It becomes terminal only once the message is at least 30 days
old and a separate, later sync or backfill run gets not-found again for the
same attachment. Anything else in between resets that count. A successful
download clears the history, and a different kind of error resets it so the
next not-found starts over rather than continuing. Recent media stays pending
no matter how many times it repeats, and missing or future message timestamps
also leave the download retryable.

The age and repeat-on-a-separate-run requirement limits how many requests an
old missing file receives, though neither proves the file is permanently gone.
An old file that later returns stays skipped, including during
`sync-beeper --full`, and two runs close enough together to both land inside
one brief bridge outage can retire a file the bridge would otherwise have
recovered. Sync and backfill summaries report newly terminal files as
"media no longer available at source".

Each not-found costs one request per attachment per run. If a server error or
throttling response comes before the not-found in the same run, msgvault
records an ordinary failed download instead, which starts the repeat count
over. Other failed downloads get at most three attempts per attachment per run
and stay pending for the next backfill.

Because Beeper's API serves what Beeper Desktop has synced locally, your
archive depth equals your local Beeper history. A freshly added Beeper account
may only have recent messages until Beeper finishes its own backfill. That
backfill can land hours or weeks later, behind history msgvault has already
walked, so once a day each sync rechecks the oldest end of every completed chat
and resumes backfilling wherever Beeper has since filled more in. You do not
need to trigger this, and the run reports how many chats it reopened.

## Repair derived data

msgvault derives message bodies, snippets, the search index, and attachment
classification from the API payload at import time, so improvements to how they
are derived do not reach messages already archived.

When the derivation version changes, each account automatically refreshes these
fields once on its next sync using the stored JSON and reports how many rows it
repaired. To run that repair now or finish an interrupted pass, run:

```bash
msgvault repair-derived --source-type beeper
msgvault repair-derived --source-type beeper --identifier instagramgo
```

This command needs no Beeper Desktop connection, repairs messages Beeper no
longer holds, and rewrites only derived columns. Raw payloads, downloaded
media, and sync cursors stay untouched, so it is idempotent.

## Link previews

When media arrives as a forwarded link preview, such as an Instagram reel or an
x.com post, msgvault records the URL it previews in
`attachments.attachment_metadata` as `{"shared_url": "..."}`. Voice note
transcripts can also appear there under `source_transcript`. Use `shared_url`
to distinguish a shared link preview from an original photo or file, while
download eligibility still follows your configured media policy.

The metadata copy of `source_transcript.text` is capped at 32 KiB on a UTF-8
boundary. A clipped value includes `"truncated": true`, and the field is
omitted when the complete transcript fits. The full transcript stays in the
searchable message body. To see the split, run:

```sql
SELECT CASE WHEN COALESCE(json_extract_string(a.attachment_metadata, '$.shared_url'), '') <> ''
            THEN 1 ELSE 0 END AS is_share,
       COUNT(*), SUM(a.size)
FROM attachments a
JOIN messages m ON m.id = a.message_id
WHERE m.message_type = 'beeper'
GROUP BY is_share;
```

## Mirror all attachments to Docbank

To mirror stored attachments from all sources to Docbank, enable
[attachment upload consent](../configuration.md#mirror-stored-attachments-to-docbank).

## Send audio to Docbank

The daemon can send stored audio from any captured source, including messaging
and email importers, to a separately running Docbank media service. Docbank
keeps the recording while msgvault records which Docbank source and occurrence
belong to each live message. For Beeper attachments, msgvault imports the
complete transcript as supplied evidence, while other sources use a configured
ASR profile or remain unprocessed. Configure the destination under
[`[integrations.docbank]`](/docs/configuration/#send-stored-audio-to-docbank).

What you need:

- A Docbank server with the media HTTP routes from
  [docbank#346](https://github.com/kenn-io/docbank/pull/346). The Docbank
  library built into msgvault does not provide them.
- `all_sources_upload_consent = true`, which allows audio from every captured
  source to be sent to that URL. If you are upgrading from the Beeper-only
  route, check the [consent settings](/docs/configuration/#send-stored-audio-to-docbank).
  Docbank's own processing consent decides whether a supplied transcript or
  configured ASR profile is processed.
- An optional `asr_profile = "asr"` setting if you want Docbank to process
  stored audio without usable source text. Leave it empty to retain the audio
  without a processing request.
- WAV or MP3 audio. msgvault checks the bytes and sends them as `audio/wav` or
  `audio/mpeg`, whatever type the provider reported. Docbank accepts no other
  codec, so OGG/Opus, M4A, and other formats stay local. Attachments identified
  as audio receive the `unsupported_media` code when full verification finds an
  unsupported format. msgvault never converts audio or runs speech recognition.
- Stored WAV or MP3 bytes from any captured source, including Beeper, messaging
  providers, email importers, and future providers. An importer must have
  captured the bytes and a stable part identity first; this route never
  downloads missing media or invents a placeholder hash.

What happens:

- Standalone Beeper voice notes and ordinary audio qualify once their bytes are
  stored. Other captured sources qualify when their stored row has a stable
  source part, a standalone or unknown role, and WAV or MP3 bytes. Previews,
  stickers, other known inline roles, and readable non-audio files stay local.
- msgvault reads Beeper's complete transcript from the archived raw message
  rather than the 32 KiB metadata copy. Other sources have no generic
  transcript metadata contract, so they use only the configured ASR profile or
  remain unprocessed. Authored message text and caption URLs are never
  transcript evidence.
- Audio without source text is retained and uses the configured ASR profile
  when one is set. With an empty profile, it remains `unprocessed`.
- For sources other than Beeper, the occurrence timestamp comes from the
  archived message's `sent_at`, and the route does not read the raw message.
- The job backfills existing audio in pages of up to 100 attachments, then
  checks up to 100 attachment changes each minute. A day after the previous
  scan finishes, it starts another full scan to catch transcripts that arrive
  later. Unchanged mappings need no write transaction.
- Each pass performs at most one upload, transcript import, processing request,
  or status check. A status check can make up to three HTTP requests to read
  the job, source, and processing receipt.
- Discovery reads, parsing, and uploads run outside the daemon's operation
  lock, and the job takes the lock for individual mapping writes and progress
  updates. If the lock stays busy, for example during a backup, the
  pass ends and a later pass reuses the saved operation ID. Background media
  work does not reset the daemon's idle timer, and shutdown cancels and drains
  an active pass.
- Uploads use a temporary copy under `data_dir/tmp/beeper-media` that msgvault
  removes when the attempt ends. The current Docbank inspector also reads the
  complete recording into memory. The default Beeper download cap is 250 MiB,
  and this route accepts sources up to 1 GiB. Memory use includes the recording
  plus inspection and allocation overhead; these file limits are not RAM
  limits.
- When the same recording appears in several messages, msgvault creates one
  occurrence per message while Docbank stores the bytes once. Supplied
  transcripts share a processing job when the provider, exact text, language,
  and audio match. Audio without a transcript shares an ASR job when the bytes
  and configured profile match, including across providers. These are separate
  jobs. A Beeper transcript does not suppress ASR for an email attachment
  containing the same recording.
- A hidden, source-deleted, removed, or replaced message loses its mapping and
  becomes `revoked`, including audio still waiting to be sent, while other
  messages sharing the audio keep theirs. Reaction changes leave the mapping
  live. If no live or pending occurrence can supply the recording, unprepared
  processing stops waiting, and restoring an occurrence reopens it. Prepared
  requests keep their saved identity and retry even after revocation because
  Docbank may have accepted them before msgvault saved the receipt. msgvault
  decides which occurrences are live, while Docbank keeps the shared evidence.
- Network errors, HTTP 429 and 5xx responses, and timed-out requests retry
  after five minutes with the same operation ID. Each request gets 30 seconds,
  and an audio upload gets one more second per 256 KiB. Rejected credentials or
  requests stay `blocked` until the daemon restarts, which also resumes
  checking a queued job. Unsupported codecs and other local source problems
  stay `blocked` across restarts until the message changes, and their
  transcript delivery stays `blocked` with the same code. Missing or corrupt
  local bytes, or a temporary upload copy that cannot be written, wait as
  `source_unavailable` and retry after five minutes. For other sources, an
  unreadable attachment needs an audio type or WAV or MP3 filename to enter
  that retry queue; without those hints or a recognized header, discovery
  leaves the attachment undecided, creates no media mapping or processing
  request, and rechecks it in the next daily full scan. Discovery reads only
  the header, while upload preparation verifies the complete audio before
  sending it.
- A processed delivery reaches `done` only after Docbank reports coverage for
  its own processing request, not for another transcript of the same audio. A
  failed Docbank job or a failed processing request ends as `done` with
  `operation_state` set to `failed`. A job marked `operator_required` stays
  `blocked` without further polling. Resolve it in Docbank, then restart the
  msgvault daemon to resume checking its state.

Provider transcripts stay searchable through normal message text. To search
generated transcripts as well, set
`integrations.docbank.voice_transcript_search` and restart the daemon. Turning
it off removes generated transcripts from the search index. To check progress,
run:

```sql
SELECT retention_state, error_code, COUNT(*)
FROM beeper_media_occurrences
GROUP BY retention_state, error_code;

SELECT phase, coverage_state, COUNT(*)
FROM beeper_media_deliveries
GROUP BY phase, coverage_state;
```

### Read transcripts in the Web reader

To read a recording's transcript, open a conversation in the Web reader and
expand a message with audio. Each recording shows its filename, size, and
transcript state. A ready transcript says whether it came from the provider or
was generated, marks partial text, and shows each line's start time and speaker
when Docbank recorded them. Supplied text requires Docbank to identify the
exact input sent for this recording using its
[supplied-input attribution](https://github.com/kenn-io/docbank/pull/825).
Older servers that omit that identity show the transcript as unavailable, and
the same happens when Docbank serves another input for the same audio,
including after an edit is reverted.

The Web reader is the only place that shows a recording's full transcript. Web
search, `msgvault media search`, and the MCP `search_media` tool find matching
transcript text. Person-scoped Media and Files, and the CLI, TUI, and MCP
message readers, are later work. msgvault reads the text from Docbank while the
message stays open without storing it, refreshing every 2 to 30 seconds while
the view is visible and immediately when you return to the tab. If you turn
Docbank off, archived audio stays visible with its transcript unavailable.
Unmapped captured audio stays unavailable until discovery records processing
work. Audio whose bytes are missing still says so, and capture policy
exclusions read unavailable.

## Scheduled sync

Let the daemon run incremental syncs on a schedule:

```toml
[beeper]
enabled = true
schedule = "*/30 * * * *"
```

A scheduled run keeps other sources on their cadence:

- **Bounded runs.** One scheduled Beeper job works for at most 3 minutes across
  all accounts and stops early when another scheduled source has waited for a
  minute. It stops between chats, history pages, or member updates, and the
  next run continues message and roster work, including optional contact
  matching, from saved progress.
- **Account rotation.** After stopping within an account, the next run starts
  with the following account. Each account keeps its own progress.
- **Yielding to waiting work.** When a run stops because other work is waiting,
  a follow-up runs after that work and visits only the accounts that have not
  had a turn yet, including one cut short at a boundary. Once every account has
  had a turn, the next run waits for the next scheduled time. Running
  **Sync now** or reaching a scheduled time while a follow-up waits starts a
  new pass over every account instead. The daemon keeps this progress in
  memory, so a restart starts over.
- **New messages first.** Chats with only new messages sync before chats still
  backfilling history. Completed chats are skipped until the current discovery
  cycle finishes, so later chats also get a turn.
- **Fetch errors.** A transient page-fetch failure retries twice. If it still
  fails, the run finishes with an error count and the scheduler reports
  `partial Beeper sync: N fetch error(s)`. Healthy chats keep their progress,
  and the failed chats retry on the next run.

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

See [Media policy](/docs/configuration/#media-policy) for how scope,
participant cap, size cap, and per-account overrides combine, and use
`msgvault purge-excluded-media` to remove media that a changed policy would no
longer collect.

### Overlap with native importers

If you already archive a network natively, for example with `import-whatsapp`
or `import-imessage`, pick one path per network. msgvault does not deduplicate
messages across sources. Add the Beeper accountID to `exclude_accounts` to keep
Beeper sync away from that network. If both paths do run, the rows remain
separable under different sources and different `message_type` values, while
participants still unify across archives through phone-number and email
matching. msgvault also persists the Beeper user ID as an identifier, so later
runs keep resolving to the same person.

Telegram users also link to
[Telegram Desktop imports](text-messages.md#import-telegram) by numeric user ID
when a sync revisits their chat; `sync-beeper --full` covers quiet chats.

Linking users does not merge their messages, so a chat archived from both
sources appears twice in search. To search one copy, see
[Search one copy of overlapping chats](searching.md#search-one-copy-of-overlapping-chats).

## Caveats

- **Reinstalling Beeper Desktop.** Beeper's message IDs are stable only per
  installation. On every run, msgvault verifies several anchor messages across
  distinct chats. Ordinary churn, such as deleting an anchored chat, is
  tolerated, and msgvault checks recently archived messages against the source
  only when no anchor survives. If the installation was rebuilt, sync stops
  with an error and marks the account, and scheduled runs skip that account
  with one warning instead of failing every run. After repairing Beeper
  Desktop, run `msgvault sync-beeper --account <id>` to verify again and clear
  the mark, or remove and re-add the Beeper source.
- **Remote daemons.** The Beeper API is loopback-only, so the msgvault daemon
  must run on the same machine as Beeper Desktop.
- **iMessage.** Beeper carries iMessage only on macOS, so archiving it this way
  needs a Mac running Beeper Desktop beside the msgvault daemon. msgvault looks
  for networks found from chat data in a bounded scan of your most recently
  active conversations, so a network with no chats, or none recent enough to
  fall inside that window, stays invisible until it sees activity. Send or
  receive a message, then re-run `add-beeper`. The scan logs a warning when it
  stops at its bound.
