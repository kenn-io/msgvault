---
last_edited: "2026-08-31"
title: Meeting Transcripts
description: Archive AI meeting notes and transcripts from Granola, Circleback, and Notion into your searchable local archive.
---

msgvault can archive meeting notes and transcripts from AI meeting-notes
services into the same local database as your email. Each meeting becomes one
searchable message: the subject is the meeting title, the body carries the AI
summary followed by the full speaker-labeled transcript, and the organizer and
attendees join the same contact graph as the people you email.

Meeting sync is **read-only**: msgvault never modifies anything in the source
service. Meetings are cached and fully searchable with `msgvault search`, the
Web UI, and the TUI. An unscoped search intentionally returns every cached
message type, including meetings and chats. The Web UI's Everything workspace
uses modality-aware rows and filters; email-specific CLI/TUI aggregates remain
email-only unless you explicitly choose another message type.

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

`add-granola`, `add-circleback`, and `add-notion-meetings` always confirm the primary email for their
source. Add other confirmed aliases with the identity command:

```bash
msgvault identity add work you+meetings@example.com
```

Adding a new confirmed identity immediately repairs `is_from_me` on matching
messages already stored for that source. No provider resync is required.

## Import from any meeting source

The provider-neutral import API archives one meeting at a time and requires no
`[[granola]]`, `[[circleback]]`, or other provider configuration. Configure an
API key, start `msgvault serve`, then send authenticated JSON to
`POST /api/v1/import/meeting`:

```bash
curl http://localhost:8080/api/v1/import/meeting \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  --data '{
    "source": {
      "identifier": "local-meetings",
      "display_name": "Local Meetings",
      "account_email": "you@example.com"
    },
    "meeting": {
      "external_id": "weekly-planning-42",
      "title": "Weekly planning",
      "started_at": "2026-07-29T09:00:00-04:00",
      "summary_markdown": "## Decisions\n\nShip the new importer.",
      "transcript_segments": [
        {"speaker": "Alex", "text": "Let's ship it.", "offset_seconds": 4}
      ]
    }
  }'
```

Choose a stable `source.identifier` for the upstream dataset and preserve the
upstream meeting ID as `meeting.external_id`. That pair is the idempotency key:
the first import returns `201` with status `created`; unchanged retries and
replacements return `200` with status `updated` without creating duplicates.
`source.account_email` identifies you for sender attribution and becomes a
confirmed identity for the whole source.

Each meeting needs at least one summary, a plain transcript, or segmented
transcript. Plain and segmented transcripts are mutually exclusive. Timestamps
use RFC 3339 with an explicit offset, request bodies are limited to 16 MiB, and
provider-specific fields belong under `meeting.metadata`. These sources are
on-demand: import through the API again to add or update meetings rather than
using **Sync now** or a scheduler.

## Granola

### Prerequisites

- A [Granola](https://granola.ai) account on a **Business** plan — the public
  API is not available on individual plans.
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

### Browse in the Web UI or TUI

Start `msgvault serve` and open the [Web UI](/docs/web-ui/) to include meetings in
Everything, search their titles and transcripts, group them with other archive
modalities, or filter to meeting notes only. Open a result to read the note in
its containing context.

Launch `msgvault tui` and press `m` until the title bar shows **Meetings**.
The list combines Granola, Circleback, and Notion meetings and shows their date, title,
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

## Notion AI Meeting Notes

Notion sync uses the official read-only Meeting Notes and block APIs. It does
not modify pages, upload content, or download recording media. Visibility is
attendee-scoped to the Notion user associated with the integration; it is not
a workspace-wide export.

### Configure and register

Create a Notion integration with AI Meeting Notes and Read Content access.
Grant User Information access if you want attendee IDs resolved to verified
emails and relationship participants. Without it, meetings still sync, but
attendees remain display-only names or IDs.

```toml
[[notion_meetings]]
identifier = "notion-personal"
account_email = "you@example.com"
token = "ntn_..."
schedule = "15 */6 * * *"         # optional daemon schedule
enabled = true
```

Keep `config.toml` owner-readable: the token is read from config and is never
written to logs, sync cursors, archived raw evidence, or command output.

```bash
msgvault add-notion-meetings notion-personal
msgvault sync-notion-meetings notion-personal --probe
```

Both commands print capability and result-count diagnostics without printing
meeting titles, notes, transcripts, attendee details, block IDs, page URLs, or
the token.

### Sync and discovery limit

```bash
msgvault sync-notion-meetings                         # all configured identities
msgvault sync-notion-meetings notion-personal        # one identity
msgvault sync-notion-meetings notion-personal --limit 3
msgvault sync-notion-meetings --full
msgvault sync-notion-meetings --after 2026-01-01
```

Notion's Meeting Notes query currently returns at most 50 records and can say
that more exist without providing a cursor. msgvault makes one maximum-size
query per run, never loops the same page, and reports partial coverage when
Notion sets `has_more`. This means native sync is reliable for the visible
recent window but is not a complete historical workspace export.

Every run hydrates the visible meetings that pass local filters, then compares
their complete snapshots; matching checksums skip the archive write. `--full`
instead sends every selected snapshot through archive upsert, which can repair
cursor/archive divergence while identical archived content remains a no-op. It
does not expose history beyond the visible window. `--after` is a local filter
over returned meetings. `--limit` caps visible meetings hydrated and verified
but does not suppress due transcript maintenance.

Notion may publish notes before a transcript. Missing transcripts retry every
six hours until seven days after the best known meeting end, or for 48 hours
from discovery when timing is unknown. A temporary omission never erases a
transcript already archived. Hard per-meeting failures retain the previous
successful state; meetings written earlier in that failed run remain safe and
idempotent.

### What gets stored (Notion)

Each meeting-note block becomes one `meeting_transcript` message in one
`meeting` conversation, keyed by the block ID. The body contains deterministic
title, time, attendee, summary, notes, and transcript sections. Raw format
`notion_meeting_json` preserves the query object, block trees, Markdown page,
privacy-safe attendee display labels, minimal resolved users, and warnings.

Notion's `created_by` user is stored as creator metadata and is never assumed
to be the organizer. Only attendees with provider-verified email addresses
become participant rows. Unknown IDs and names remain display-only evidence.

If registration reports invalid token, Meeting Notes access, or Read Content
errors, correct that integration capability and rerun `add-notion-meetings`.
User Information errors are non-fatal. Remove the archive source with:

```bash
msgvault remove-account notion-personal --type notion_meetings --yes
```

A configured schedule will then refuse to recreate it until
`add-notion-meetings` is run again.

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
provider metadata. msgvault does not expose recording URLs as durable
attachments, and downloading or archiving recording media is not supported.

## Searching

Unscoped search returns all cached matching message types. Filter to meetings
when the question is meeting-specific:

```bash
msgvault search "quarterly budget" --message-type meeting_transcript
```
