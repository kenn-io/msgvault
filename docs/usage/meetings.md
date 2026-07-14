---
title: Meeting Transcripts
description: Archive AI meeting notes and transcripts from Granola and Circleback into your searchable local archive.
---

msgvault can archive meeting notes and transcripts from AI meeting-notes
services into the same local database as your email. Each meeting becomes one
searchable message: the subject is the meeting title, the body carries the AI
summary followed by the full speaker-labeled transcript, and the organizer and
attendees join the same contact graph as the people you email.

Meeting sync is **read-only**: msgvault never modifies anything in the source
service. Meetings are cached and fully searchable with `msgvault search` and
the TUI. An unscoped search intentionally returns every cached message type,
including meetings and chats. Ordinary analytics remain email-only unless you
explicitly filter by message type, so meeting attendees do not inflate email
sender/recipient statistics.

## Source labels and account identity

Each meeting source has two distinct values:

- `identifier` is a stable label used in commands, source metadata, schedules,
  and (for Circleback) the token filename. It can be an arbitrary name such as
  `work`.
- `account_email` is the normalized primary email used to determine whether
  the meeting organizer is you (`is_from_me`).

`account_email` is required independently of `identifier`. Config loading
rejects a missing or invalid value with guidance to preserve the source label
and add the account email separately.

`add-granola` and `add-circleback` always confirm the primary email for their
source. Add other confirmed aliases with the identity command:

```bash
msgvault identity add work you+meetings@example.com
```

When the primary email or aliases change, run a full provider sync to repair
`is_from_me` on meetings already in the archive:

```bash
msgvault sync-granola work --full
msgvault sync-circleback work --full
```

## Granola

### Prerequisites

- A [Granola](https://granola.ai) account on a **Business** (or Enterprise)
  plan — the public API is not available on individual plans.
- An API key, created in the Granola desktop app under **Settings**. Keys look
  like `grn_…`.

!!! note "Why not the local cache?"
    Current Granola versions encrypt their on-disk cache, so msgvault reads
    the official API instead of scraping local files.

### Configure and register

Add one `[[granola]]` entry per Granola account to `config.toml`:

```toml
[[granola]]
identifier = "work"              # stable source label
account_email = "you@example.com" # primary identity for organizer matching
api_key = "grn_..."              # from the desktop app's settings
schedule = "0 */6 * * *"         # optional: daemon cron schedule
enabled = true
```

Then validate the key and register the source:

```bash
msgvault add-granola work
```

With a single configured entry the identifier argument may be omitted.

### Sync

```bash
msgvault sync-granola                      # all configured accounts
msgvault sync-granola work                 # one account
msgvault sync-granola --limit 5            # limited production validation
msgvault sync-granola --full               # re-fetch everything, repair in place
msgvault sync-granola --after 2024-01-01   # bound a full sync by creation date
```

Sync is incremental: only notes updated since the last successful run are
fetched (Granola's `updated_after` filter), so edits to notes and late
transcription both flow into the archive. Re-fetched notes are upserted in
place — no duplicates.

To try a newly configured account with a small production sync, start with:

```bash
msgvault sync-granola work --limit 5
msgvault search "meeting topic" --message-type meeting_transcript
```

Inspect a few results for the expected title, summary, speaker-labeled
transcript, organizer, attendees, and `is_from_me` attribution. Running the
same limited sync again updates the existing meeting rows rather than creating
duplicates. Once the results look correct, run `msgvault sync-granola work`
without a limit to continue normal incremental operation.

### Browse in the TUI

Launch `msgvault tui` and press `m` until the title bar shows **Meetings**.
The list combines Granola and Circleback meetings and shows their date, title,
organizer, and source. Press `A` to select one meeting source, `/` to search
meeting titles, people, transcripts, and notes, and `Enter` to open the full
transcript. The detail view renders summary Markdown with terminal-friendly
headings, lists, emphasis, code, and preserved transcript line breaks. Inside
the detail view, `/` finds text and `n`/`N` moves between matches. Meetings
mode is read-only; selection and deletion actions are not available.

Meetings remains available in the mode cycle when the archive is empty and
shows setup guidance. If Texts mode is unavailable, `m` skips it and still
reaches Meetings.

With a `schedule` set, `msgvault serve` runs the sync on that cron cadence,
like `[[gcal]]` calendar sources. Registration is intentionally durable: if
the Granola source is removed from the archive, a configured schedule refuses
to recreate it and tells you to run `msgvault add-granola <identifier>`.

If one note fails after other notes were written, the run is recorded and
reported as failed and the successful cursor does not advance. The CLI or
scheduler refreshes the searchable cache for any successful additions or
updates before returning that partial-sync error, so already-written notes
remain searchable. Fix the reported problem and rerun the same sync.

### What gets stored

| Archive field | Granola source |
|---|---|
| Subject / conversation title | Note title (falling back to the calendar event title) |
| Sent time | Scheduled meeting start, else first transcript timestamp |
| From | Meeting organizer (else the note owner) |
| To | Attendees |
| Body | AI summary (markdown) + `[mm:ss] Speaker: text` transcript |
| Metadata | Duration, web link, calendar event ID, folders, segment count |
| Raw archive | The verbatim API response (`granola_json`) |

## Circleback

Circleback exposes no REST API — msgvault pulls data through its MCP server
(OAuth with dynamic client registration; no secret lives in your config).

### Configure and authorize

```toml
[[circleback]]
identifier = "work"              # stable source label and token key
account_email = "you@example.com" # primary identity for organizer matching
schedule = "30 */6 * * *"        # optional: daemon cron schedule
enabled = true
```

```bash
msgvault add-circleback work
```

This opens a browser for Circleback authorization and stores the token under
`tokens/circleback_<identifier>.json`.

Circleback OAuth uses a fixed `localhost:8090` callback. With a configured
remote msgvault server, `add-circleback` fails before proxying because the
callback and token must live on the daemon host. Run the command in a shell on
that host instead. When connecting over SSH, forward the callback port:

```bash
ssh -L 8090:localhost:8090 user@daemon-host
# In that SSH session:
msgvault --local add-circleback work
```

If the remote host cannot open your workstation's browser, copy the printed
authorization URL into a local browser. The callback reaches the daemon-host
process through the SSH tunnel. On the daemon host, `--local` means that host's
own archive; on a workstation it would authorize a separate local archive
instead of the configured remote.

### Sync

```bash
msgvault sync-circleback                     # all configured accounts
msgvault sync-circleback --limit 5           # limited validation sync
msgvault sync-circleback --full              # re-fetch everything
msgvault sync-circleback --probe             # print tool inventory + sample result
```

Each incremental run enumerates meeting IDs without a scheduled-date bound,
so a newly created backfill is discovered even when the meeting happened long
ago. Unknown meetings and known meetings created within the 48-hour refresh
overlap are fetched in detail. Identical snapshots are skipped without
invalidating the search cache; edits to older known meetings are picked up by
`--full`.

Circleback can publish notes before its transcript is ready. A recognized
missing or empty transcript is archived with state `pending`, then retried on
a six-hour cadence. The retry deadline is seven days after the scheduled
meeting time; records without usable times use a bounded 48-hour window.
Future meetings first retry at their known end time (or start plus one hour).
When the deadline expires the state becomes `unavailable`; `--full` can check
and promote it later if a transcript appears.

Due transcript maintenance is processed before newly searched meetings and is
not counted against `--limit`. For example, `--limit 5` means at most five new
search results plus every bounded maintenance item that is due. Provider,
contract, missing-result, ingest, archive-recovery, and cancellation failures
fail the sync run and leave the prior successful cursor in place; item-atomic
writes completed before the error remain safe to revisit on the next run.

Circleback's tool outputs have no published schema. If a sync imports
meetings with missing fields, run `--probe` to see the live field names —
the importer archives verbatim payloads for successfully decoded results.
Schema-drift payloads that cannot be decoded are rejected instead; diagnose
them with `--probe`, then run `--full` after decoder support is updated.

### What gets stored (Circleback)

In addition to the shared fields above: action items (title, assignee,
status), insights, and tags land in the message metadata and body; the
meeting recording URL and `recording_url_fetched_at` remain in the archived
provider metadata. Circleback recording URLs expire after about 24 hours, so
msgvault does not expose them as durable attachments. Downloading and archiving
recording media is not yet supported.

## Searching

Unscoped search returns all cached matching message types. Filter to meetings
when the question is meeting-specific:

```bash
msgvault search "quarterly budget" --message-type meeting_transcript
```
