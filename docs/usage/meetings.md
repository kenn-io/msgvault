---
last_edited: "2026-10-04"
title: Meeting Transcripts
description: Archive call recordings, AI meeting notes, and transcripts from Twilio, Bland, Granola, Plaud, Circleback, Notion, and Muesli into your searchable local archive.
---

Find meeting decisions and transcripts in the same archive as your email and
chats. Each meeting becomes one searchable message with a title, notes or
summary, and a transcript when the source provides one. Verified participant
emails connect meetings to the people you already know in msgvault.

## Choose a meeting source

| Source | Connection | Main coverage limit |
|---|---|---|
| [Twilio](#twilio) (unreleased) | Account auth token or API key | Recordings and transcripts Twilio still retains |
| [Bland](#bland) (unreleased) | Org API key | Calls and the postcall data Bland still retains |
| [Granola](#granola) | API key | Requires access to Granola's public API |
| [Notion AI Meeting Notes](#notion-ai-meeting-notes) | Notion integration token | At most 50 attendee-visible meetings per discovery query |
| [Plaud](#plaud) | Browser authorization to its hosted MCP server | Requires Cloud Sync and existing Plaud transcription |
| [Circleback](#circleback) | Browser authorization to its MCP server | Older note edits require a full refresh |
| [Muesli](#muesli) | Local database on the same Mac | msgvault must run on the Mac where Muesli records |
| [Another meeting source](#import-from-any-meeting-source) | Authenticated JSON import | Your integration supplies each meeting and its updates |

Provider sync reads meeting data without changing the source service. Recording
media is not downloaded by the Notion, Plaud, Circleback, or Muesli integrations.

## Browse and search

Start `msgvault serve` and open the [Web UI](/docs/web-ui/) to find meetings in
Everything. Filter to meeting notes or search across notes, email, and chats.
To search only meetings from the CLI:

```bash
msgvault search "quarterly budget" --message-type meeting_transcript
```

In the [TUI](/docs/usage/tui/), press `m` until the title shows **Meetings**.
Use `A` to select a source, `/` to search, and `Enter` to open the note and
transcript. In the detail view, `/` finds text and `n`/`N` moves between
matches. Meetings mode does not offer selection or deletion.

Unscoped search includes meetings and chats. Email-specific CLI and TUI
aggregates remain email-only unless you choose another message type.

## Export context and read follow-ups

In Everything, select meeting rows and choose **Export meeting context**. You
can select explicit rows or **Select all matching items** for the current
search and filters. The export includes exactly that selection. Mixed selections
fail with **Select meetings only**; narrow selections larger than 100 meetings.
Choose JSON or Markdown. **Include transcript** is off by default.

The downloaded file contains the server's context packet, including meeting
references, participants, summary, notes, recorded actions, and coverage states.
The default content budget is 131072 UTF-8 bytes. CLI and API clients can set
4096 through 1048576 bytes. Packets report truncation and omitted meeting IDs;
check those fields before treating an export as complete. A missing summary
stays missing. msgvault does not generate a replacement from the transcript.

**Meeting activity and follow-ups** appears in a meeting-filtered Everything
view, participant and domain reading panes, Directory profiles, and
Relationships. It follows the current scope. Filter action items by source
status or exact assignee email, then use **Open archived meeting** to read the
source evidence. Back returns to the same workspace and scope.

Action status is the last archived source status, not a local task list.
msgvault does not infer assignees, create follow-ups, or mark source tasks done.
Supported empty action lists, unsupported sources, unavailable evidence, and
partial evidence remain distinct. Granola has no structured action support;
Circleback and generic imports preserve explicit actions; Notion exposes
checkboxes from archived summary and notes blocks. A Notion checkbox does not
supply an assignee merely because a name appears in its text.

Use the daemon-backed CLI for the same evidence:

```bash
msgvault meetings context --id 42 --id 43 --format json --output meeting-context.json
msgvault meetings actions --domain example.com --status pending --assignee alex@example.com
msgvault meetings metrics --person-id 7 --after 2026-01-01 --before 2026-03-01 --json
```

The three MCP read tools are `get_meeting_context`, `list_meeting_action_items`,
and `get_meeting_metrics`. They require no AI provider call or profile-write
permission. CLI dates use `YYYY-MM-DD`; HTTP and MCP scope dates use full
RFC3339 timestamps. See the [CLI flags](../cli-reference.md#meetings),
[HTTP contract](../api-server.md#meeting-intelligence), and
[MCP examples](chat.md#meeting-evidence).

## Understand meeting time and coverage

Meeting metrics show the number of meetings, known and unknown durations,
total known time, average known duration, and monthly activity. Unknown
durations are excluded from averages. When no duration is known, the average
is unavailable (`null` in JSON), not zero.

| Duration basis | Evidence |
|---|---|
| Provider | Explicit provider duration, Notion recording start/end, or generic meeting start/end |
| Scheduled | Calendar start and end |
| Transcript span | Earliest through latest usable transcript timing |
| Unknown duration | No usable duration evidence; no duration basis is assigned |

JSON names these bases `provider`, `scheduled`, and `transcript_span`.
Scheduled time and transcript span are estimates of different things. The
basis breakdown keeps those differences visible. Months without meetings are
omitted; undated meetings appear in a separate count.

Meeting reads include source-deleted records by default while their archive
content remains present. Use `--deletion active` or `--deletion deleted` to
narrow actions and metrics. Locally deleted records are excluded. This does not
change the [garbage collection workflow](../cli-reference.md#gc).

Explore scopes preserve the full search predicate and its cache/search
identity. Their transfer ceiling is 10000 matching message IDs; narrow a scope
that exceeds it. A changed or expired result requires a reload. Errors remain
visible instead of silently dropping filters or sampling visible rows.

Existing archives gain meeting projections from their stored raw evidence on
upgrade, without a provider resync. Evidence absent from an older raw snapshot
still appears as unavailable or partial. Upgrade the daemon as well as clients;
meeting operations need daemon API schema 2.27.0 or newer.

## Source labels and account identity

Each meeting source has two distinct values:

- `identifier` is a stable label used in commands, source metadata, schedules,
  and (for Circleback and Plaud) the token filename. It can be an arbitrary name such as
  `work`.
- `account_email` is the normalized primary email used to determine whether
  the meeting organizer is you (`is_from_me`).

`account_email` is required independently of `identifier`. Config loading
rejects a missing or invalid value with guidance to preserve the source label
and add the account email separately. Plaud validates this against the live
account; it does not assume the account owner organized every recording.

`add-granola`, `add-plaud`, `add-circleback`, `add-notion-meetings`, and
`add-muesli` always confirm the primary email for their source. Add other confirmed aliases with the identity command:

```bash
msgvault identity add work you+meetings@example.com
```

Adding a new confirmed identity immediately repairs `is_from_me` on matching
messages already stored for that source. No provider resync is required.

## How meetings connect to people

A meeting shows up on a person when one of its attendees is an email or phone
number that already belongs to that person, for example from mail, chat, or a
promoted profile. The attendee becomes a participant, and the next activity
update (hourly, or `msgvault activity build`) adds the meeting to the person's
timeline, relationships, and last-contact information.

Some sources also know that several emails and phone numbers are the same
human. msgvault links those identities through the source's stable identifier,
so a meeting reaches the person even when it only names an address the person
has never used with you:

| Source | Identities per attendee | Linked through |
|---|---|---|
| Granola, Circleback | One email | Not needed |
| Plaud | Speaker display labels only | Names do not create identities |
| Notion AI Meeting Notes | The user's verified email | The Notion user ID, so a user whose email changes keeps one person |
| Muesli | Email, or the emails and phones on the attendee's Apple Contacts card | The Contacts card, excluding addresses shared by unlinked cards |
| Import API | `email` and `phone` | The person's `id` within the import source |

These links follow the same rules as other automatic identity links:

- Only a shared stable identifier links identities. Matching names never do.
- Muesli excludes addresses shared by unlinked Contacts cards from automatic
  linking, even when only one card appears in a meeting. The addresses remain
  in the meeting evidence.
- If the identities already belong to two different people, msgvault leaves
  them apart and records a conflict. So does an address that another card or
  `id` from the same source already claims in archived observations.
  Review conflicts in the Web [Directory review queues](/docs/web-ui/#directory-and-reviews).
- Rejecting a proposed link keeps that pair apart. The same identities can
  still connect through another identity of the same person.

Attendees known only by name appear in the meeting body but do not link to a
person. A profile imported from CardDAV links meetings once its email or phone
participant is promoted or linked to it; see [people](/docs/usage/people/).

## Import from any meeting source

The provider-neutral import API archives one meeting at a time and requires no
`[[granola]]`, `[[plaud]]`, `[[circleback]]`, or other provider configuration.
Configure an API key, start `msgvault serve`, then send authenticated JSON to
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
      "action_items": [
        {"title": "Send the recap", "assignee_email": "alex@example.com", "status": "pending"}
      ],
      "transcript_segments": [
        {"speaker": "Alex", "text": "Let's ship it.", "offset_seconds": 4}
      ]
    }
  }'
```

Each organizer or attendee needs an `email`, a `phone`, or both. A phone must be
international (a leading `+` or `00`) and is stored in E.164 form; national
numbers are rejected rather than guessed. Add a stable `id` for the person in
your source to link their email and phone, including across meetings:

```json
{"name": "Alex Example", "email": "alex@example.com", "phone": "+1 604 555 0100", "id": "crm-42"}
```

Choose a stable `source.identifier` for the upstream dataset and preserve the
upstream meeting ID as `meeting.external_id`. That pair is the idempotency key:
the first import returns `201` with status `created`; unchanged retries and
replacements return `200` with status `updated` without creating duplicates.
`source.account_email` identifies you for sender attribution and becomes a
confirmed identity for the whole source.

Use `"action_items": []` when the source supports actions and recorded none.
Omit `action_items` when it does not provide structured actions. These states
are different: omission is unsupported, while an explicit empty list is
available evidence with zero actions. `null` is invalid. Each action needs a
nonblank `title`; optional fields are `source_id`, `description`,
`assignee_name`, `assignee_email`, `status`, and `due_date`.

Each meeting needs at least one summary, a plain transcript, or segmented
transcript. Plain and segmented transcripts are mutually exclusive. Timestamps
use RFC 3339 with an explicit offset, request bodies are limited to 16 MiB, and
provider-specific fields belong under `meeting.metadata`. These sources are
on-demand: import through the API again to add or update meetings rather than
using **Sync now** or a scheduler.

## Twilio

This integration is unreleased. Each recorded Twilio call becomes one meeting
with its recordings saved as audio attachments, so you keep the audio after
Twilio deletes it. Calls without a recording aren't archived. Sync only reads: it never places calls, turns on recording, or
starts paid transcription.

### Connect and sync

Add a [`[[twilio]]` entry](../configuration.md#twilio-sources) for each account
or subaccount, then register and sync it:

```bash
msgvault add-twilio work
msgvault sync-twilio work --probe        # check access without showing calls
msgvault sync-twilio work
msgvault sync-twilio work --limit 20     # process at most 20 calls
msgvault sync-twilio work --full         # revisit every call Twilio still lists
```

`sync-twilio` with no identifier syncs every configured account. Set `schedule`
on the entry to let the daemon sync it. See the
[CLI reference](../cli-reference.md#sync-twilio) for every flag.

A `--limit` run that stops before the end of the call list says so and prints
the command to continue, for example `Run: msgvault sync-twilio work --limit 20`.
Run it again until the summary says the sync is complete.

### What gets stored

- **Audio.** Each completed recording is downloaded as WAV, in two channels
  when Twilio has them. WAV takes about 1 MB per minute of a mono recording
  and 2 MB for dual-channel. The default limit is 250 MiB per recording.
  An opened meeting in the Web UI links each stored recording for download.
- **Transcripts.** Legacy recording transcriptions and completed Conversation
  Intelligence transcripts. When a recording has both, the Intelligence
  transcript is the one you read and search. Transcripts are stored as
  Twilio serves them, so an Intelligence service with PII redaction on
  stores redacted text.
- **Call details.** Phone numbers, direction, status and duration stay as
  provider evidence. A phone number is not treated as proof of who spoke.

Summaries and action items are not available from Twilio. A call with audio
but no transcript shows the transcript as unavailable. An encrypted recording
is archived as an unavailable attachment with no audio. Recordings kept on
external storage, Relay and Batch transcripts, and recordings without a call
SID aren't archived.

### Late recordings, failures, and coverage

Twilio adds recordings and transcripts after a call ends. Each sync lists every
recording created since seven days before the previous sync started, and
fetches each listed recording's call again, so audio and transcripts that
arrive within seven days of a recording's creation reach its meeting on a later
sync. Ones that arrive after that need `msgvault sync-twilio work --full`,
which reaches calls whose recordings Twilio still lists, including ones
deleted within the last 40 days. Each listed call costs about 4 to 6 Twilio requests per sync, which
suits a schedule of every few hours for up to a few hundred calls a day.

If Twilio refuses a recording download (HTTP 400, 401, 403, 404 or 410) or
returns something other than audio, the sync still completes and the recording
shows as failed. A refused transcript read (an HTTP 4xx other than 429), or a
completed transcription with no text, leaves that transcript unavailable with a note. A
network, rate-limit or server error on a call marks the sync failed and names
the call; other calls still sync, and later syncs retry it while the call is
within the seven-day window. A call already older than seven days that fails
this way during a first sync or `--full` needs another `--full`. Once a call leaves the window, recordings still waiting for audio
are marked failed, or unavailable if Twilio never finished them, and the sync
summary says how many; only `--full` tries them again. A local storage failure
stops the sync. A recording over the size cap is skipped with a note naming
it; raise `max_media_mb` and run `--full` to fetch it. Recordings skipped while
`media = false` are not retried either; after turning `media` back on, run
`--full` to download them.

Twilio can delete call details while keeping the recording. msgvault then
reports `call_metadata_unavailable` and archives the audio and transcript
without the phone numbers. Archived audio and text survive later deletions.

US1, IE1 (Dublin) and AU1 (Sydney) each use their own regional credentials.
Conversation Intelligence transcripts are read in US1 only, so an IE1 or AU1
account archives legacy transcriptions alone.

## Bland

This integration is unreleased. Each Bland call becomes one meeting with
its summary, transcript and recording, so you keep them after Bland deletes
them. Sync only reads: it never places calls or sets up webhooks.

### Connect and sync

Add a [`[[bland]]` entry](../configuration.md#bland-sources) for each org API
key, then register and sync it:

```bash
msgvault add-bland work
msgvault sync-bland work --probe        # check access without showing calls
msgvault sync-bland work
msgvault sync-bland work --limit 20     # fetch at most 20 calls
msgvault sync-bland work --full         # revisit every call
```

A `--limit` run that stops before the end of the call list prints the command
to continue, for example `Run: msgvault sync-bland work --limit 20`. Run it
again until the summary says the sync is complete.

### What gets stored

- **Summary and transcript.** The call's summary and its corrected transcript,
  which comes from Bland's delayed postcall webhook payload. Bland produces it
  only for calls with a configured postcall webhook (with citation
  extraction); without one, Bland keeps only the original transcript from the
  call record, and that's what's archived. Transferred conversations are
  appended with their offsets. When Bland later omits text it sent before, the
  archived text stays.
- **Audio.** The call's MP3 or WAV recording. The default limit is 250 MiB.
- **Call details.** The call record and the last postcall webhook payload Bland
  retains. Phone numbers stay as provider evidence, not proof of who spoke.

### Late recordings, failures, and coverage

Bland adds summaries, corrected transcripts and recordings after a call ends.
Each sync lists every call created since seven days before the previous sync
started, and fetches each one again, so a summary, transcript or recording
that arrives while a call is within seven days of its creation reaches its
meeting on a later sync. Ones that arrive after that need
`msgvault sync-bland work --full`. A call is archived even before its
artifacts exist, and a call still in progress is archived with what it has and
updated on later syncs as it ends. A call scheduled to run more than a week after it was created needs `msgvault sync-bland work --full` after it finishes. A
call deleted on Bland during a first sync or `--full` can make that sync skip
one other call; the next sync gets it if it's within the seven-day window,
and another `--full` gets it otherwise. Each listed call costs about 2 Bland requests per sync, plus the
recording download until it's stored, which suits a schedule of every few
hours for up to a few hundred calls a day.

If Bland refuses a recording download (HTTP 400, 401, 403, 404 or 410) or
returns something other than audio, the sync still completes and the recording
shows as failed. A refused call detail or postcall read (an HTTP 4xx answer
such as 401 or 403) leaves that part out with a note. A call whose details are
refused is archived from the call list, and its postcall data is still
fetched; a call whose postcall read is refused is still archived with
what its details have. Either way, the refusal is noted in the sync summary,
and any transcript the call already has is kept; when it has none, the
meeting's stored transcript is unavailable with the status
`provider_refused`. `add-bland` notes a refused postcall read the same
way instead of failing. A malformed call detail answer, including one
without a creation time or with a malformed transcript, leaves the call as it
was archived; a malformed postcall answer archives the call from its details.
Either of those, or a network, rate-limit or server error on a call, marks the
sync failed and names the call and the malformed field; other calls still sync, and later syncs retry it while the call is within the seven-day window. A call already
older than seven days that fails this way during a first sync or `--full`
needs another `--full`. A recording is fetched once the call details or
postcall data report its URL; a call without one gets no recording. Once a call leaves the window, recordings still waiting for audio are
marked failed, or unavailable if Bland never produced them, and the sync
summary says how many; only `--full` tries them again. A local storage failure
stops the sync. A recording over the size cap is skipped with a note naming
it; raise `max_media_mb` and run `--full` to fetch it.

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

### Scheduling and partial results

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

Every run fetches the details of visible meetings that pass your filters.
Unchanged content is skipped. Use `--full` to send those meetings through the
archive update path again when stored sync state and archived content disagree.
It still cannot discover history beyond the visible window.

- `--after` filters meetings returned by Notion locally and implies `--full`.
- `--limit` caps the visible meetings fetched and checked.
- Due transcript retries run in addition to that limit.

Notion may publish notes before a transcript. Missing transcripts retry every
six hours until seven days after the best known meeting end, or for 48 hours
from discovery when timing is unknown. A temporary omission never erases a
transcript already archived. Hard per-meeting failures retain the previous
successful state; meetings written earlier in that failed run remain safe and
idempotent.

### What gets stored (Notion)

Each meeting-note block becomes one `meeting_transcript` message in a
`meeting` conversation. Its block ID identifies the meeting on later syncs.
The body includes title, time, attendees, summary, notes, and transcript
sections. The raw archive (`notion_meeting_json`) keeps the source meeting
object, note blocks, Markdown, attendee labels, resolved user details, and
warnings for later inspection.

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

## Plaud

Archive cloud recordings from an ordinary active Plaud account as searchable
meetings. Enable Cloud Sync and transcribe recordings in Plaud first. msgvault
reads the full transcripts, speaker labels, recording dates, duration, and every
note tab through Plaud's hosted MCP service. It does not download audio or
change recordings in Plaud.

### Authorize your account

Add an entry to `config.toml` on the daemon host:

```toml
[[plaud]]
identifier = "work"
account_email = "you@example.com"
enabled = true
schedule = "30 */6 * * *"
```

Run `msgvault add-plaud work` on that host and approve the browser request.
Plaud redirects to `localhost:8091/callback/plaud`. For a headless host,
forward that port with SSH and open the authorization URL in your local
browser. A configured remote refuses `add-plaud` before proxying; run
`msgvault --local add-plaud work` in the remote shell instead.

msgvault checks the live account email before registration and on every sync.
It must match `account_email`. The identifier is a stable label, not an email
or organizer identity. A registered source retains its confirmed owner. Use a
new identifier for another account. Credentials are stored in
`tokens/plaud_<identifier>.json`; changing the MCP endpoint requires fresh
authorization before those credentials can be used there.

### Sync recordings

```bash
msgvault sync-plaud work
msgvault sync-plaud work --limit 20
msgvault sync-plaud work --full --after 2025-01-01
msgvault sync-plaud work --probe
```

Every normal run enumerates the recording inventory and checks complete
transcripts and notes for edits, including speaker corrections and later
transcript pages. Unchanged content skips archive writes. `--full` repairs
existing records while preserving stable file IDs. `--after` filters by
recording date locally and implies `--full`.

`--limit` bounds the recordings hydrated per run. New recordings come first;
then runs rotate through the least recently attempted recordings. Failed and
date-scoped runs save this rotation state too. A failed recording remains
eligible on its next turn, so it cannot block later recordings in limited runs.
Rotation state is stored once per source; run history retains outcomes and counts.
Plaud pagination is eventually consistent, so changes during a run may be
reconciled on a later run.

A recording awaiting transcription can still archive its metadata and notes.
Later runs retry pending content. Temporarily missing transcripts or note tabs
preserve the previously archived content. Recordings deleted from Plaud remain
in the archive. A failed or canceled run remains marked failed and refreshes
search and cache for additions or updates already committed.

`--probe` prints tool names, input schemas, and a first-page recording count.
It prints no meeting titles, file IDs, transcripts, or note bodies. It helps
diagnose provider contract changes without modifying the archive.

`msgvault serve` runs enabled entries with a schedule. Removing the registered
source stops sync from recreating it; run `add-plaud` to register it again.
See the [configuration reference](../configuration.md#plaud-sources) and
[CLI reference](../cli-reference.md#sync-plaud) for exact fields and flags.

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

## Muesli

[Muesli](https://github.com/Muesli-HQ/muesli) records and transcribes meetings
on a Mac and keeps them in a local SQLite database. msgvault reads that
database directly and read-only. It never changes Muesli's meetings or schema,
and it does not call `muesli-cli`, which updates the database whenever it runs.

### Prerequisites

The msgvault daemon reads the database on its own host, so run msgvault on the
Mac where Muesli records. If your archive lives on another machine, send each
meeting to that daemon with the [import API](#import-from-any-meeting-source)
from a Muesli post-meeting hook instead.

If macOS blocks the read, grant the process that runs `msgvault serve` Full
Disk Access in System Settings.

### Configure and register

```toml
[[muesli]]
identifier = "mac"
account_email = "you@example.com"   # you, the person who records
# db_path = "~/Library/Application Support/Muesli/muesli.db"  # default
schedule = "*/30 * * * *"           # optional daemon schedule
enabled = true
```

`db_path` defaults to the stable app's database. Development builds of Muesli
use a different support folder, such as `MuesliDev`; set `db_path` for those.

### Attendees from Apple Contacts

People you tag in Muesli usually come from Apple Contacts. msgvault reads the
Mac's Contacts stores read-only, finds each attendee's card by its Contacts ID
or exact email, and links its unshared emails and phones. An address on multiple
unlinked Contacts cards stays in the meeting evidence but does not create an
identity link. A contact tagged
with only a phone number therefore reaches the person you already chat with at
that number.

- Reading Contacts needs Full Disk Access for the process that runs
  `msgvault serve`. Without it, meetings still sync, and `sync-muesli` reports
  `Contacts: unavailable`.
- Phone numbers typed with `+` or `00` always work. Set `phone_country_code`
  (for example `"1"` or `"44"`) to also use numbers typed without a country
  code.
- When only some Contacts accounts can be read, msgvault still uses Contacts
  IDs to retain meeting evidence, but stops matching by email and creating
  automatic identity links. An unreadable account could hold another card
  with the same address. Linking resumes when every account is readable.
- When Contacts is unreadable or a card disappears, an attendee still present
  in Muesli keeps the identities archived for that meeting. Those retained
  addresses do not assert current Contacts ownership. Removing the attendee
  in Muesli removes its meeting association.
- Set `contacts = false` to turn the lookup off.

```bash
msgvault add-muesli mac
```

`add-muesli` checks that the file opens read-only as a Muesli database, then
registers the source.

### Sync

```bash
msgvault sync-muesli                     # all configured databases
msgvault sync-muesli mac --limit 5
msgvault sync-muesli --after 2026-01-01
msgvault sync-muesli --full
```

Every run reads the whole database and updates meetings that changed in place,
including title, notes, transcript, participant, and folder edits. Unchanged
meetings are skipped.

- Meetings still recording or processing wait for a later run.
- Meetings deleted in Muesli stay in the archive unchanged.
- `--after` keeps meetings that start on or after the date, read as UTC.
- `--limit` caps the meetings processed in one run.
- `--full` rewrites every archived meeting, which refreshes attribution after
  you add an identity.

### What gets stored (Muesli)

Each meeting becomes one `meeting_transcript` message in a `meeting`
conversation. The body holds the title, time, participant names, Muesli's AI
notes, the notes you typed, and the transcript. When Muesli skipped or failed
the summary, the body omits that notice instead of showing it as a summary.

Muesli does not record an organizer. msgvault attributes each meeting to
`account_email` as its organizer. Participants with an email address become
recipients and connect to your existing people. Participants without one,
such as a contact picked by name, appear only by name.

The raw archive (`muesli_json`) keeps the meeting's text, times, status,
template name, calendar event ID, folder path, and participant names, emails,
phones, and sources. It never stores audio or audio file paths, screen text,
template prompts, or Apple Contacts identifiers.

Remove the archive source with:

```bash
msgvault remove-account mac --type muesli --yes
```
