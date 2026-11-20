---
last_edited: "2026-11-19"
title: Meeting Transcripts
description: Archive call recordings, AI meeting notes, and transcripts from Chatwoot, Omi, Twilio, Bland, Granola, Plaud, Pocket, Circleback, Notion, Muesli, and Twenty into your searchable local archive.
---

You can search meeting decisions and transcripts right alongside your email and
chats. Each meeting becomes one searchable message with a title, notes or a
summary, and a transcript when the source provides one. Verified participant
emails connect those meetings to the people you already have in msgvault.

## Choose a meeting source

| Source | Connection | Main coverage limit |
|---|---|---|
| [Omi](#omi) | Developer API key | Completed, non-discarded, unlocked conversations exposed by the backend |
| [Twilio](#twilio) | Account auth token or API key | Recordings and transcripts Twilio still retains |
| [Bland](#bland) | Org API key | Calls and the postcall data Bland still retains |
| [Chatwoot](../guides/chatwoot.md) calls | Account API token | Calls in selected inboxes; recordings and transcripts refresh for seven days |
| [Granola](#granola) | API key | Requires access to Granola's public API |
| [Notion AI Meeting Notes](#notion-ai-meeting-notes) | Meeting PAT or integration, optional users integration | At most 50 attendee-visible meetings per discovery query |
| [Plaud](#plaud) | Browser authorization to its hosted MCP server | Requires Cloud Sync and existing Plaud transcription |
| [Circleback](#circleback) | Browser authorization to its MCP server | Older note edits require a full refresh |
| [Pocket](#pocket) | Personal API key | Requires Pocket API access and supported transcript/summary response shapes |
| [Muesli](#muesli) | Local database on the same Mac | msgvault must run on the Mac where Muesli records |
| [Twenty](#twenty-call-recordings) | Read-only workspace API key | Reads every call recording in the workspace, whichever Twenty app wrote it |
| [Another meeting source](#import-from-any-meeting-source) | Authenticated JSON import | Your integration supplies each meeting and its updates |

Provider sync is read-only and never changes the source service. The Omi,
Notion, Plaud, Pocket, Circleback, and Twenty integrations do not download
recording media, while Muesli can optionally archive locally retained
recordings.

## Browse and search

Start `msgvault serve` and open the [Web UI](/docs/web-ui/) to browse meetings
in **Messages**. From there, you can filter to meeting notes or search across
notes, email, and chats together. To search only meetings from the CLI, run:

```bash
msgvault search "quarterly budget" --message-type meeting_transcript
```

In the [TUI](/docs/usage/tui/), press `m` until the title shows **Meetings**.
Press `A` to select a source, `/` to search, and `Enter` to open the note and
transcript. Inside the detail view, `/` finds text and `n`/`N` moves between
matches. Meetings mode does not support selection or deletion.

Unscoped search includes meetings and chats. Email-specific CLI and TUI
aggregates stay email-only unless you pick another message type.

## Link calendar events and notes

Run `msgvault meetings links <note-id>` to see the calendar copies and notes
that share an exact occurrence. You can also pass `msg:<id>` or an event `ref`
from the `REF` column of `msgvault calendar events`, for example
`msgvault meetings links evt:23@20270321T100000Z`. The API and MCP return the
same record.

Automatic matching depends on the source format:

- Twenty notes match on their iCalendar UID and scheduled time.
- Granola notes match on their Google event ID only when their owner matches
  the archived calendar's mailbox.
- Twenty notes with the same UID and time, and Granola notes with the same
  instance-suffixed Google ID, can link even before you import a calendar.
- A bare Google series ID cannot identify a single week, so those notes stay
  separate. A calendar single event or exception can prove that a bare ID names
  one event, including after a reschedule.
- Other note formats stay separate unless the owner links them.

To find calendar events that overlap a meeting note, or notes that overlap a
calendar event, run `msgvault meetings suggest <ref>`. Suggestions rank shared
attendees, overlap, and title words. You can accept a suggestion with
`msgvault meetings link <ref> <ref>`. The API and the MCP tool
`suggest_meeting_links` return the same suggestions.

When a note has no known duration, the search uses a 30-minute window and
reports the resulting estimated overlap. Known overlaps rank ahead of estimated
overlaps when attendee counts tie, and title words break ties between estimates.

In each suggestion, `ref` identifies the calendar event and `member_message_id`
identifies the meeting note. For a note seed, `ref` is the suggested event; for
an event seed, `member_message_id` is the suggested note. The `title` and
`start` fields describe the suggestion.

You can manage links manually with three commands:

- `msgvault meetings link <ref> <ref>` joins copies, including notes from an
  unscheduled call.
- `msgvault meetings unlink <ref> <ref>` separates an automatic match.
- `msgvault meetings reset-link <ref> <ref>` clears your choice and restores
  automatic matching.

Running `link` or `unlink` replaces any previous choice, and choices survive
projection rebuilds. Deduplication and source merges carry choices to the kept
copy, while undoing deduplication leaves that copy unchanged. If carried
choices conflict, a choice already on the kept copy wins; otherwise, the newest
carried choice wins. When a third copy matches both sides of a split, it joins
the first compatible group in a stable order. If a link or split would break
another saved link, msgvault returns a conflict; reset the conflicting pair
first to make that change.

Link records show whether a calendar copy was cancelled or deleted at its
source. When a recurrence cannot be expanded, the record keeps the copies it
found and lists the affected message IDs and reasons in `unexpandable`. The CLI
table prints those warnings to stderr, while JSON and MCP return them with the
record.

An archived cancellation stays in the record even when its series master has
not been archived. Its `start` and `end` are `null` in JSON and `unknown` in
the CLI table, and `unexpandable` reports the missing series and bounds. A
known bound stays intact even if the other is missing, and importing the master
later fills in the times. Note matches, calendar candidate reads, and
recurrence steps share one request work limit; exceeding it returns
`query_too_broad`.

## Export context and read follow-ups

You can export a structured context packet for one or more meetings from
**Messages**:

1. Select the meeting rows you want, or choose **Select all N matching** for
   the current search and filters. Mixed selections fail with **Select meetings
   only**, and selections larger than 100 meetings need to be narrowed first.
2. In the selection bar, choose **Meeting context…**.
3. In the popover, pick JSON or Markdown. Turn on **Include transcript** if you
   want the full transcript (**Include transcript** is off by default).
4. Choose **Export meeting context**.

To export a single meeting instead, expand its message, choose
**⋯ → Export meeting…**, and pick JSON or Markdown. The export always includes
only your exact selection.

The downloaded file holds the server's context packet: meeting references,
participants, summary, notes, recorded actions, and coverage states. The
default content budget is 131072 UTF-8 bytes, while CLI and API clients can set
anywhere from 4096 to 1048576 bytes. Packets report truncation and omitted
meeting IDs; check those fields before treating an export as complete. A
missing summary stays missing; msgvault does not generate a replacement from
the transcript.

When you filter **Messages** to meetings, the view displays a meeting count and
total known duration. Choose **Action items** to view recorded follow-ups,
filter by status or exact assignee email, or load more results. Expand
**Meeting metrics** in that dialog to see duration evidence, average duration,
and monthly activity. Each expanded meeting also lists its own recorded actions
below the body. Choose **Open archived meeting** to view the source evidence;
**Back** returns you to the same workspace and scope.

**People** offers the same meeting summary and **Action items** for a selected
person or domain, including a selected calendar day. Participant and domain
reading panes keep the full meeting activity panel. A saved person's **Details**
view shows that panel across all of their resolved identities; people without
identities have no meeting panel.

Action status reflects the last archived source status rather than a local task
list. msgvault does not infer assignees, create follow-ups, or mark source
tasks done. Supported empty action lists, unsupported sources, unavailable
evidence, and partial evidence stay distinct. Granola and Twenty have no
structured action support. Omi, Circleback, and generic imports preserve
explicit actions, and Notion exposes checkboxes from archived summary and notes
blocks. A Notion checkbox does not supply an assignee merely because a name
appears in its text.

You can pull the same evidence from the daemon-backed CLI:

```bash
msgvault meetings context --id 42 --id 43 --format json --output meeting-context.json
msgvault meetings actions --domain example.com --status pending --assignee alex@example.com
msgvault meetings metrics --person-id 7 --after 2026-01-01 --before 2026-03-01 --json
```

The three MCP read tools are `get_meeting_context`, `list_meeting_action_items`,
and `get_meeting_metrics`. None of them require an AI provider call or
profile-write permission. CLI dates use `YYYY-MM-DD`, while HTTP and MCP scope
dates use full RFC3339 timestamps. See the [CLI flags](../cli-reference.md#meetings),
[HTTP contract](../api-server.md#meeting-intelligence), and
[MCP examples](chat.md#meeting-evidence) for the full contracts.

## Understand meeting time and coverage

Meeting metrics report your meeting count, known and unknown durations, total
known time, average known duration, and monthly activity. Unknown durations are
left out of averages. When no duration is known, the average is unavailable
(`null` in JSON), not zero.

| Duration basis | Evidence |
|---|---|
| Provider | Explicit provider duration, Notion or Twenty recording start/end, or generic meeting start/end |
| Scheduled | Calendar start and end |
| Transcript span | Earliest through latest usable transcript timing |
| Unknown duration | No usable duration evidence; no duration basis is assigned |

JSON names these bases `provider`, `scheduled`, and `transcript_span`.
Scheduled time and transcript span estimate different things, and the basis
breakdown keeps those differences visible. Months with no meetings are omitted,
and undated meetings appear in their own count.

Meeting reads include source-deleted records by default as long as their
archive content is still present. Pass `--deletion active` or
`--deletion deleted` to narrow actions and metrics. Locally deleted records are
excluded. This does not change the
[garbage collection workflow](../cli-reference.md#gc).

Explore scopes preserve the full search predicate and its cache/search
identity. Their transfer ceiling is 10000 matching message IDs; narrow any
scope that exceeds it. If a result changes or expires, you need to reload it.
Errors stay visible instead of silently dropping filters or sampling visible
rows.

Existing archives gain meeting projections from their stored raw evidence on
upgrade, with no provider resync required. Evidence missing from an older raw
snapshot still shows as unavailable or partial. Upgrade both the daemon and
clients; meeting operations require daemon API schema 2.27.0 or newer.

## Twenty call recordings

You can archive summaries and diarized transcripts from the call recordings in
a Twenty workspace. msgvault reads existing recordings through Twenty's GraphQL
API; it does not install apps, run recording bots, or download recording media.
See [Twenty releases](https://twenty.com/releases) and the
[official API guide](https://docs.twenty.com/developers/extend/capabilities/apis).

Call recordings are a shared Twenty object written by Call Recorder as well as
Twenty's Granola, Fathom, Fireflies, and Teams integrations. msgvault syncs
every call recording the API key can read and stores the writing app's
`applicationId` in the archived evidence and as `application_id` in meeting
metadata. If you also sync Granola directly with msgvault, a meeting that
Twenty's Granola integration copied appears once from each source.

To connect a Twenty workspace:

1. Create an API key in Twenty under **Settings → APIs & Webhooks**.
2. Assign a role with read access to Call Recordings, Calendar Events, and
   Calendar Event Participants.
3. Add the configuration on the host running `msgvault serve`:

   ```toml
   [[twenty]]
   identifier = "work"
   account_email = "you@example.com"
   base_url = "https://api.twenty.com"
   api_key = "YOUR_READ_ONLY_API_KEY"
   enabled = true
   schedule = "15 */6 * * *"
   ```

For self-hosted Twenty, set `base_url` to your instance's API origin, such as
`https://crm.example.com`, using a root URL with no path, query, or embedded
credentials. HTTPS is required except for loopback HTTP. Requests reject
redirects so your key stays at the configured origin.

Register, probe, and sync the source from the CLI:

```bash
msgvault add-twenty work
msgvault sync-twenty work
msgvault sync-twenty work --probe
msgvault sync-twenty work --after 2026-01-01 --limit 10
msgvault sync-twenty work --full
```

Registration verifies all three object types and their required fields before
creating the archive source. Running with `--probe` performs the same read-only
check without writing to the archive or printing meeting content. If you have
multiple configured sources, specify an identifier when registering or probing;
running `sync-twenty` with no identifier syncs all of them. When a check or
sync fails, the error includes Twenty's own error code and message, such as a
missing permission or a field an older self-hosted version lacks.

Each incremental run fetches recordings updated since the previous successful
run, along with their calendar events and participants, one page at a time. It
also re-reads the five minutes before that cutoff, so an edit saved while the
previous run was scanning still arrives; recordings already archived at the
same update time are skipped. Because late summaries and transcripts update the
recording, the next run picks them up and updates the same meeting. Attendee
edits made after the recording show up on the next `--full` run.

msgvault paces requests to match Twenty's documented API rate limit and retries
rate limits, server errors, and network failures. Pending, failed, or empty
transcript markers are never archived as text, and Call Recorder's "Summary
unavailable" notice is not archived as a summary. A usable summary can still be
archived while the transcript is pending.

If a recording cannot be archived, for example because it has no usable time or
exceeds the 64 MiB response or content cap, msgvault skips it, reports it in
the sync summary, and records it on the sync run. Sync continues with the next
recording and retries the skipped one when it changes in Twenty. If a page of
recordings exceeds the response cap, msgvault retries it with fewer recordings
and keeps that smaller page size for the rest of the run.

- `--after` is an inclusive UTC meeting-date filter applied after reading
  recordings. Occurrence time uses the recording start, then calendar start,
  then recording creation time. A run with `--after` leaves the sync position
  unchanged, so it suits one-off backfills.
- `--limit` caps eligible meetings and reports partial coverage when discovery
  stops early. The next run without `--after` continues where it stopped.
- `--full` rescans every recording and refreshes projections and attribution
  even when evidence matches. Run it after changing account identities. If
  `--limit` stops a full rescan, the next run without `--after` (with or
  without `--full`) continues the rescan until it reaches the end.

Raw evidence keeps the returned recording, calendar, and participant fields.
Calendar email handles supply identities, while speaker names and display-only
handles remain display evidence. Malformed transcript word or speaker entries
are omitted without discarding the rest of the transcript. Duration uses valid
recording start/end, then scheduled calendar time, then transcript timing.
Structured actions are not supported, and summary prose does not create action
items.

Recordings deleted in Twenty stay in your archive. A recording whose calendar
event is deleted or unlinked keeps its already-archived attendees; a recording
linked to a different event that the key cannot read drops the old event's
attendees. API, permission, and cancellation failures fail the sync and
preserve previously committed evidence. Removing the local source stops
scheduled sync from recreating it; run `add-twenty` to register it again.

## Source labels and account identity

Each meeting source uses two distinct values:

- `identifier` is a stable label used in commands, source metadata, schedules,
  and, for Circleback and Plaud, the token filename. It can be any name you
  like, such as `work`.
- `account_email` is the normalized primary email used to determine whether
  you organized the meeting (`is_from_me`).

`account_email` is required independently of `identifier`. If it is missing or
invalid, config loading rejects it and prompts you to keep the source label and
add the account email separately. Plaud validates this against the live account
and does not assume the account owner organized every recording.

`add-omi`, `add-granola`, `add-plaud`, `add-pocket`, `add-circleback`,
`add-notion-meetings`, and `add-muesli` always confirm the primary email for
their source. To add other confirmed aliases, run:

```bash
msgvault identity add work you+meetings@example.com
```

Adding a confirmed identity immediately repairs `is_from_me` on matching
messages already stored for that source, with no provider resync needed.

## How meetings connect to people

A meeting appears on a person's record when one of its attendees has an email
or phone number that already belongs to that person, for example from mail,
chat, or a promoted profile. That attendee becomes a participant, and the next
activity update (hourly, or when you run `msgvault activity build`) adds the
meeting to the person's timeline, relationships, and last-contact information.

Some sources also know when several emails and phone numbers belong to the same
person. msgvault links those identities through the source's stable identifier,
so a meeting reaches the person even when it only names an address they have
never used with you:

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
  linking, even when only one card appears in a meeting. Those addresses still
  stay in the meeting evidence.
- If the identities already belong to two different people, msgvault leaves
  them separate and records a conflict. The same happens when another card or
  `id` from the same source already claims an address in archived observations.
  Review conflicts in the Web UI under
  [People: Possible duplicates](/docs/web-ui/#saved-profiles-and-possible-duplicates).
- Rejecting a proposed link keeps that pair apart, though the same identities
  can still connect through another identity belonging to the same person.

Attendees known only by name appear in the meeting body but do not link to a
person. A profile imported from CardDAV links to meetings once its email or
phone participant is promoted or linked to it; see [people](/docs/usage/people/).

## Import from any meeting source

The provider-neutral import API lets you archive one meeting at a time without
configuring `[[granola]]`, `[[plaud]]`, `[[circleback]]`, or any other
provider. Configure an API key, start `msgvault serve`, and send authenticated
JSON to `POST /api/v1/import/meeting`:

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

Each organizer or attendee needs an `email`, a `phone`, or both. Phone numbers
must be international (starting with `+` or `00`) and are stored in E.164 form;
national numbers are rejected rather than guessed. Include a stable `id` for
the person in your source to link their email and phone, including across
meetings:

```json
{"name": "Alex Example", "email": "alex@example.com", "phone": "+1 604 555 0100", "id": "crm-42"}
```

Pick a stable `source.identifier` for your upstream dataset and preserve the
upstream meeting ID as `meeting.external_id`. Together, that pair is the
idempotency key: the first import returns `201` with status `created`, and
unchanged retries or replacements return `200` with status `updated` without
creating duplicates. `source.account_email` identifies you for sender
attribution and becomes a confirmed identity for the whole source.

Pass `"action_items": []` when your source supports actions and recorded none,
and omit `action_items` when it does not provide structured actions. Omitting
the field marks actions as unsupported, whereas an explicit empty list records
available evidence with zero actions. Passing `null` is invalid. Every action
requires a nonblank `title`, with optional `source_id`, `description`,
`assignee_name`, `assignee_email`, `status`, and `due_date` fields.

Each meeting needs at least one of a summary, a plain transcript, or a
segmented transcript. Plain and segmented transcripts are mutually exclusive.
Timestamps use RFC 3339 with an explicit offset, request bodies are capped at
16 MiB, and provider-specific fields belong under `meeting.metadata`. Imported
sources are on-demand. To add or update meetings, import through the API again
rather than using **Sync now** or a scheduler.

## Omi

You can archive conversations from Omi's hosted service, including paid
accounts, or from your own Omi backend. This integration reads from the
read-only [Developer API](https://docs.omi.me/doc/developer/api/conversations).

To connect Omi:

1. Create a Developer API key with `conversations:read` in
   **Settings → Developer** on the account and backend that hold your
   conversations. MCP keys (`omi_mcp_…`) do not authenticate this REST API.
2. Add an entry to `config.toml`:

   ```toml
   [[omi]]
   identifier = "omi-personal"
   account_email = "you@example.com"
   api_key = "omi_dev_..."
   enabled = true
   schedule = "0 */6 * * *"
   # For a self-hosted backend, uncomment and set its root URL:
   # base_url = "http://localhost:8000"
   ```

   Keep the key private. For a self-hosted backend, set `base_url` to the URL
   reachable from the msgvault daemon; a reverse-proxy path prefix is allowed,
   and you should omit `/v1/dev`. Use HTTPS for non-loopback backends. Plain
   HTTP is accepted only for loopback hosts such as `localhost`, `127.0.0.1`,
   or `[::1]`. If you omit `base_url`, msgvault uses `https://api.omi.me`.
   Hosted and self-hosted accounts can coexist as separate entries with
   different identifiers and keys.
3. Register and validate each source, then sync:

   ```bash
   msgvault add-omi omi-personal
   msgvault sync-omi omi-personal --limit 5
   msgvault sync-omi omi-personal
   ```

Registration uses one transcript-list request and can wait for shared pacing or
a provider cooldown.

Each conversation becomes a meeting with its summary, action descriptions,
completion status, due dates, and timestamped speaker transcript when available.
Omi's Developer API returns a smaller projection than its backend records: it
does not include note sections, attendee rosters, action owner names, or action
IDs. msgvault keeps the original returned JSON in the archive, including fields
it does not project.

Speaker names and IDs appear as display labels. Because Omi supplies no
verified attendee emails, these labels do not create email identities. Your
configured account email records archive ownership; it does not establish who
organized the meeting or mean that Omi verified the address.

### How Omi sync runs

Omi allows 25 transcript list requests per hour per API key, and each request
returns up to 200 conversations. msgvault spaces requests about 144 seconds
apart, and scheduled syncs, manual commands, and every source sharing the same
key and backend share that spacing.

- **First sync.** The first scan reads all accessible history. Each page of 200
  takes two requests: one reads ahead and one confirms the page did not shift.
  Every scan finishes with one more request to confirm no conversation appeared
  and pushed an older one past the last page. A 10,000-conversation archive
  takes about 100 requests, or roughly four hours.
- **Later syncs.** Later scans read conversations created since the last
  complete scan, plus a 48-hour overlap to catch late processing and recent
  edits. Unchanged conversations are not rewritten.
- **Passes.** Each source syncs in passes of up to five minutes. A pass saves
  its place, and the next pass continues the same scan with the same options
  and date range. Conversations created during a scan arrive in the next one.
  With multiple sources, `sync-omi` moves on to the next source when a pass
  pauses.
- **Paused passes.** A pass that runs out of time, yields to other archive
  work, or waits on a provider cooldown appears as **Paused** in **Sources**
  and **Past runs**. It keeps its checkpoint and records no error. A scheduled
  pass that saved progress continues right away, while a cooldown waits for the
  next scheduled trigger.
- **Cooldowns.** When Omi returns HTTP 429, msgvault waits for its
  `Retry-After` delay, up to 24 hours, or one hour if the header is missing or
  malformed. Cooldowns survive restarts and apply to every process using the
  key. A manual sync tells you when requests can resume.
- **Server errors.** If the backend keeps returning HTTP 5xx errors until the
  pass ends, msgvault records the sync as failed with the HTTP status. The next
  run resumes from the saved checkpoint.

An enabled entry with a schedule supports daemon sync and **Sync Now**.

### Rescans and scan flags

Omi has no updated-since filter. To pick up edits to conversations older than
the 48-hour overlap, or conversations that become visible later with an older
creation date, run `sync-omi --full`.

- `--after YYYY-MM-DD` limits the scan to conversations created on or after
  that date and implies `--full`.
- `--limit N` caps processed conversations, including unchanged ones.
- Runs with either flag keep the creation watermark that later syncs start from.
- Passing any scan flag explicitly, even `--limit=0` or `--full=false`, replaces
  an unfinished scan whose options differ, and the command reports the
  replacement.

### Omi coverage limits

- The Developer API returns completed conversations that are not discarded or
  locked.
- Omi removes locked and malformed records after paging. If it removes an
  entire page, older accessible conversations behind that page can be missed.
  Run `sync-omi --full` after older records become accessible.
- If a conversation is deleted or discarded between requests, msgvault detects
  the shifted page and restarts paging from the last confirmed conversation.
- Conversations without a transcript, including title-only records, are
  archived with transcript coverage marked unavailable. If a later response
  omits a transcript msgvault already archived, msgvault keeps the archived
  record and notes that in the sync output.
- An explicit empty transcript is valid. Blank speech is skipped, and speech
  with missing, reversed, or negative timestamps is kept without a timestamp.
- A conversation with malformed field types stops the scan. Meetings already
  saved are kept, and the next run retries the saved page.
- Recording media and photos stay at Omi. Archived meetings stay in your
  archive even after they are deleted upstream.

Use a new source identifier for a different account or backend dataset. When
you rotate the key for the same account, keep the same identifier so sync keeps
its saved progress.

## Twilio

Each recorded Twilio call becomes one meeting with its recordings saved as
audio attachments, so you keep the audio even after Twilio deletes it. Calls
without a recording are not archived. Sync is read-only: it never places calls,
turns on recording, or starts paid transcription.

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

Running `sync-twilio` with no identifier syncs every configured account. Set
`schedule` on the entry to let the daemon sync it automatically. See the
[CLI reference](../cli-reference.md#sync-twilio) for every flag.

When a `--limit` run stops before reaching the end of the call list, it tells
you and prints the command to continue, for example
`Run: msgvault sync-twilio work --limit 20`. Repeat that command until the
summary reports that the sync is complete.

### What gets stored

- **Audio.** Each completed recording is downloaded as WAV, in two channels
  when Twilio has them. WAV takes about 1 MB per minute for a mono recording
  and 2 MB per minute for dual-channel. The default cap is 1 GiB per recording,
  which matches the largest file msgvault's Docbank audio route delivers; you
  can change it with [`max_media_mb`](../configuration.md#twilio-sources). When
  you open a meeting in the Web UI, each stored recording has a download link.
- **Transcripts.** msgvault archives legacy recording transcriptions and
  completed Conversation Intelligence transcripts. When a recording has both,
  you read and search the Intelligence transcript. Transcripts are stored as
  Twilio serves them, so an Intelligence service with PII redaction turned on
  stores redacted text.
- **Call details.** Phone numbers, direction, status, and duration stay as
  provider evidence. A phone number is not treated as proof of who spoke.

Twilio does not provide summaries or action items. A call with audio and no
transcript shows the transcript as unavailable. An encrypted recording is
archived as an unavailable attachment with no audio. Recordings stored on
external storage, Relay and Batch transcripts, and recordings without a call
SID are not archived.

### Late recordings, failures, and coverage

Twilio adds recordings and transcripts after a call ends. Each sync lists every
recording created since seven days before the previous sync started and fetches
each listed recording's call again, so audio and transcripts that arrive within
seven days of a recording's creation reach its meeting on a later sync.
Audio and transcripts arriving after that window need
`msgvault sync-twilio work --full`, which reaches calls whose recordings Twilio
still lists, including recordings deleted within the last 40 days.
Each listed call takes about 4 to 6 Twilio
requests per sync, which fits a schedule of every few hours for up to a few
hundred calls a day.

If Twilio refuses a recording download (HTTP 400, 401, 403, 404, or 410) or
returns something other than audio, the sync still finishes and marks the
recording as failed. A refused transcript read (an HTTP 4xx other than 429) or
a completed transcription with no text leaves that transcript unavailable with
a note.

A network, rate-limit, or server error on a call marks the sync failed and names
the call, while other calls still sync. Failed calls stay queued for later
syncs even when they are older than seven days. Runs without `--after` retry
them, pending recordings do not age out while a retry is queued, and retries
count toward `--limit`. Limited runs resume unfinished discovery before
retrying failed calls.

Once a call leaves the seven-day window with no queued failure, recordings
still waiting for audio are marked failed, or unavailable if Twilio never
finished them. The sync summary reports how many, and only `--full` tries them
again. A full sync bounded by `--after` advances the incremental starting point
only if it covers the previous incremental window. A local storage failure
stops the sync.

If a recording exceeds the size cap, msgvault skips it with a note naming the
recording; raise `max_media_mb` and run `--full` to fetch it. Recordings
skipped while `media = false` are not retried automatically either; after
turning `media` back on, run `--full` to download them.

Twilio can delete call details while keeping the recording. In that case,
msgvault reports `call_metadata_unavailable` and archives the audio and
transcript without the phone numbers. Archived audio and text survive later
deletions in Twilio.

US1, IE1 (Dublin), and AU1 (Sydney) each use their own regional credentials.
Conversation Intelligence transcripts are read in US1 only, so an IE1 or AU1
account archives legacy transcriptions alone.

## Bland

Each Bland call becomes one meeting with its summary, transcript, and recording,
so you keep them even after Bland deletes them. Sync is read-only: it never
places calls or sets up webhooks.

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

Running `sync-bland` with no identifier syncs every configured account. Set
`schedule` on the entry to let the daemon sync it automatically. See the
[CLI reference](../cli-reference.md#sync-bland) for every flag.

When a `--limit` run stops before the end of the call list, it prints the
command to continue, for example `Run: msgvault sync-bland work --limit 20`.
Run that command again until the summary reports that the sync is complete.

### What gets stored

- **Summary and transcript.** msgvault stores the call's summary and its
  corrected transcript from Bland's delayed postcall webhook payload. Bland
  produces this only for calls with a configured postcall webhook (with
  citation extraction); without one, Bland keeps only the original transcript
  from the call record, and msgvault archives that instead. Transferred
  conversations are appended with their offsets. If Bland later omits text it
  sent earlier, the archived text stays intact.
- **Audio.** msgvault downloads the call's MP3 or WAV recording. The default
  cap is 1 GiB per recording, the same as Twilio's; change it with
  [`max_media_mb`](../configuration.md#bland-sources).
- **Call details.** msgvault keeps the call record and the last postcall
  webhook payload Bland retains. It stores the payload itself, not the webhook
  URL or delivery history around it, because a webhook URL is often a secret.
  Phone numbers stay as provider evidence, not proof of who spoke.

### Late artifacts and coverage

Bland adds summaries, corrected transcripts, and recordings after a call ends.

- **Relist window.** Each sync lists every call created since seven days before
  the previous sync started and fetches each one again. A summary, transcript,
  or recording that arrives within seven days of the call's creation reaches
  its meeting on a later sync. Anything arriving later needs
  `msgvault sync-bland work --full`.
- **Calls in progress.** A call is archived before its artifacts exist with
  whatever data it has, and later syncs update it once it ends. A call
  scheduled to run more than a week after it was created needs `--full` after
  it finishes.
- **Recordings.** msgvault fetches a recording once the call details or
  postcall data report its URL; a call without a URL gets no recording. Bland
  deletes recordings after a retention period, which the postcall data reports
  as `recording_expiration`. Schedule syncs well inside that period; a
  recording Bland deleted before msgvault stored it cannot be recovered.
- **Deleted calls.** If a call is deleted on Bland during a first sync or
  `--full`, that sync can skip one other call. The next sync picks it up if it
  falls within the seven-day window; otherwise, run `--full` again.
- **Request cost.** Each listed call takes about 2 Bland requests per sync,
  plus the recording download until it is stored. That fits a schedule of every
  few hours for up to a few hundred calls a day.

### Failures

- **Refused recording.** If Bland refuses a recording download (HTTP 400, 401,
  403, 404, or 410) or returns something other than audio, the sync still
  finishes and marks the recording as failed.
- **Refused details or postcall read.** An HTTP 4xx answer other than 404 or
  429 leaves that part out, and the sync summary counts the affected calls in
  one note per read and status, such as
  `postcall data refused (HTTP 403) for 12 call(s)`. A call whose details are
  refused is archived from the call list, and its postcall data is still
  fetched. A call whose postcall read is refused is archived from its details.
  Any transcript the call already has is kept; if it has none, the stored
  transcript is marked unavailable with the status `provider_refused`. Both
  `add-bland` and `--probe` report a refused read instead of failing.
- **Malformed answer.** A malformed call detail answer, including one without
  a creation time or with a malformed transcript, leaves the call as it was
  archived. A malformed postcall answer archives the call from its details.
- **Call errors.** A malformed answer, or a network, rate-limit, or server
  error on a call, marks the sync failed and names the call along with any
  malformed field. Other calls still sync. Failed calls stay queued for later
  syncs even when they are older than seven days. Runs without `--after` retry
  them, retries count toward `--limit`, and pending recordings do not age out
  while a retry is queued.
- **Aged-out recordings.** Once a call leaves the window with no queued
  failure, recordings still waiting for audio are marked failed, or unavailable
  if Bland never produced them. The sync summary reports how many, and only
  `--full` tries them again.
- **Size cap.** A recording over the size cap is skipped with a note naming it;
  raise `max_media_mb` and run `--full` to fetch it. Recordings skipped while
  `media = false` are not retried automatically either; after turning `media`
  back on, run `--full` to download them.
- **Local storage failure.** A failure writing to the local attachment store
  stops the sync.

## Granola

### Prerequisites

- A [Granola](https://granola.ai) account on a **Business** plan. The public
  API is not available on individual plans.
- An API key, created in the Granola desktop app under **Settings**. Keys start
  with `grn_…`.

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

If you have only one configured entry, you can omit the identifier argument.

### Sync

```bash
msgvault sync-granola                      # all configured accounts
msgvault sync-granola work                 # one account
msgvault sync-granola --limit 5            # limited production validation
msgvault sync-granola --full               # re-fetch everything, repair in place
msgvault sync-granola --after 2024-01-01   # bound a full sync by creation date
```

Sync is incremental: msgvault fetches only notes updated since the last
successful run (using Granola's `updated_after` filter), so note edits and late
transcriptions both flow into your archive. Re-fetched notes update in place
without creating duplicates.

To test a newly configured account with a small production sync, run:

```bash
msgvault sync-granola work --limit 5
msgvault search "meeting topic" --message-type meeting_transcript
```

Check a few results to confirm the title, summary, speaker-labeled transcript,
organizer, attendees, and `is_from_me` attribution look right. Running the same
limited sync again updates the existing meeting rows rather than creating
duplicates. Once everything looks good, run `msgvault sync-granola work`
without a limit to switch to normal incremental sync.

### Scheduling and partial results

When `schedule` is set, `msgvault serve` runs the sync on that cron cadence,
like `[[gcal]]` calendar sources. Registration stays durable: if you
remove the Granola source from the archive, a configured schedule refuses to
recreate it and prompts you to run `msgvault add-granola <identifier>`.

If one note fails after other notes were written, msgvault records and reports
the run as failed and does not advance the successful cursor. Before returning
that partial-sync error, the CLI or scheduler refreshes the searchable cache
for any successful additions or updates, so already-written notes stay
searchable. Fix the reported problem and rerun the same sync.

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

Notion sync uses the official read-only Meeting Notes and block APIs. It never
modifies pages, uploads content, or downloads recording media. Visibility is
attendee-scoped to the Notion user tied to the integration. It is not a
workspace-wide export.

### Configure and register

Use a personal access token (PAT) or an integration with AI Meeting Notes and
Read Content access for meeting content. PATs cannot list workspace users or
retrieve other users, and they have no User Information capability toggle. To
include attendee emails, create an internal integration in the same workspace
with **Read user information including email addresses**, then pass its token
as `users_token`. Keep the meeting PAT so discovery continues to use the
meeting owner's attendee visibility.

```toml
[[notion_meetings]]
identifier = "notion-personal"
account_email = "you@example.com"
token = "ntn_..."
users_token = "ntn_..."          # optional workspace users integration
schedule = "15 */6 * * *"         # optional daemon schedule
enabled = true
```

Keep `config.toml` owner-readable. msgvault reads the token from config and
never writes it to logs, sync cursors, archived raw evidence, or command
output.

Register and probe the source:

```bash
msgvault add-notion-meetings notion-personal
msgvault sync-notion-meetings notion-personal --probe
```

Both commands print capability and result-count diagnostics without printing
meeting titles, notes, transcripts, attendee details, block IDs, page URLs, or
the tokens. Probe output tests the optional users token on one sampled attendee
and reports whether that attendee has a verified email.

### Notion attendee emails

The users token retrieves known attendee IDs directly, including workspace
members and guests. Only users with `person.email_verified = true` and a usable
email become anchored participants; healthy unverified users stay display-only.

Each optional user lookup has a 60-second timeout, including retries, and
healthy lookups can continue throughout the sync. msgvault caches successful
responses and missing IDs for the duration of the run. Invalid credentials, a
missing User Information capability, provider retry exhaustion, and transport
failures stop further uncached lookups for that sync. Previously verified
attendees survive failed or skipped lookups, and the next sync retries with
fresh state. People without a Notion account cannot be resolved.

After you add a users token, run `msgvault sync-notion-meetings <identifier>`
to update participants on existing visible meetings. The 50-meeting discovery
window still applies.

### Sync and discovery limit

```bash
msgvault sync-notion-meetings                         # all configured identities
msgvault sync-notion-meetings notion-personal        # one identity
msgvault sync-notion-meetings notion-personal --limit 3
msgvault sync-notion-meetings --full
msgvault sync-notion-meetings --after 2026-01-01
```

Notion's Meeting Notes query currently returns at most 50 records and can
indicate that more exist without returning a cursor. msgvault makes one
maximum-size query per run, never loops the same page, and reports partial
coverage when Notion sets `has_more`. Native sync is reliable for the visible
recent window, but it is not a complete historical workspace export.

Every run fetches the details of visible meetings that pass your filters and
skips unchanged content. Run with `--full` to send those meetings through the
archive update path again when stored sync state and archived content disagree.
Even with `--full`, sync cannot discover history beyond the visible window.

- `--after` filters meetings returned by Notion locally and implies `--full`.
- `--limit` caps the visible meetings fetched and checked.
- Due meeting retries run in addition to that limit.

Meetings under data sources import their summary, notes, and transcript from
their referenced section blocks. When a page meeting has no transcript section
or its section reads completely empty, msgvault uses a complete transcript from
the parent page's Markdown. An incomplete section keeps its saved text for
retry.

Provider or network errors, incomplete pagination, and request, response-size,
block-count, and nesting limits can leave sections incomplete. msgvault keeps
their archived text and evidence and retries section blocks every six hours,
even after a meeting leaves the query window. Incomplete sections stay queued
until a complete read is saved.

If page Markdown is incomplete, msgvault keeps the saved transcript and
retries. When a transcript is missing without an incomplete section, background
retries stop at the deadline. The sync summary reports partial coverage when a
section is incomplete or the Notion query reports more meetings.

When a transcript reference disappears without a replacement, msgvault keeps
the saved transcript and retries. A complete empty referenced transcript
section clears saved text when page Markdown has no replacement, and other
omitted sections clear saved text.

If combined evidence exceeds 64 MiB, msgvault keeps the last readable archive
and reports partial coverage.

A meeting whose saved evidence has a newer version keeps that evidence, and the
sync reports an error for it. Existing `[[notion_meetings]]` settings need no
changes. Other hard per-meeting failures retain the previous successful state,
and meetings written earlier in that failed run stay safe and idempotent.

### What gets stored (Notion)

Each meeting-note block becomes one `meeting_transcript` message in a
`meeting` conversation, identified on later syncs by its block ID. The body
includes title, time, attendees, summary, notes, and transcript sections. The
raw archive (`notion_meeting_json`) keeps the source meeting object, note
blocks, Markdown, attendee labels, resolved user details, and warnings for
later inspection.

Notion's `created_by` user is stored as creator metadata and is never assumed
to be the organizer. Only attendees with provider-verified email addresses
become participant rows; unknown IDs and names remain display-only evidence.

If registration reports an invalid token, Meeting Notes access, or Read Content
errors, fix that integration capability and rerun `add-notion-meetings`. For
attendee emails, fix the separate users integration and its **Read user
information including email addresses** capability (a PAT cannot resolve other
users). Optional lookup failures preserve meeting content. To remove the
source, run:

```bash
msgvault remove-account notion-personal --type notion_meetings --yes
```

After removal, a configured schedule refuses to recreate the source until you
run `add-notion-meetings` again.

## Plaud

You can archive cloud recordings from an ordinary active Plaud account as
searchable meetings. Enable Cloud Sync and transcribe recordings in Plaud
first. msgvault reads the full transcripts, speaker labels, recording dates,
duration, and every note tab through Plaud's hosted MCP service. It does not
download audio or change recordings in Plaud.

### Authorize your account

1. Add an entry to `config.toml` on the daemon host:

   ```toml
   [[plaud]]
   identifier = "work"
   account_email = "you@example.com"
   enabled = true
   schedule = "30 */6 * * *"
   ```

2. Run `msgvault add-plaud work` on that host and approve the browser request.
   Plaud redirects to `localhost:8091/callback/plaud`. On a headless host,
   forward that port over SSH and open the authorization URL in your local
   browser. A configured remote refuses `add-plaud` before proxying; run
   `msgvault --local add-plaud work` in the remote shell instead.

msgvault checks the live account email before registration and on every sync,
and it must match `account_email`. The identifier is a stable label, not an
email or organizer identity. A registered source retains its confirmed owner.
Use a new identifier for another account. Credentials live in
`tokens/plaud_<identifier>.json`; if you change the MCP endpoint, you need to
authorize again before using those credentials there.

### Sync recordings

```bash
msgvault sync-plaud work
msgvault sync-plaud work --limit 20
msgvault sync-plaud work --full --after 2025-01-01
msgvault sync-plaud work --probe
```

Every normal run lists the recording inventory and checks complete transcripts
and notes for edits, including speaker corrections and later transcript pages.
Unchanged content skips archive writes. `--full` repairs existing records while
keeping stable file IDs, and `--after` filters by recording date locally and
implies `--full`.

`--limit` caps how many recordings are hydrated per run. New recordings come
first, then runs rotate through the least recently attempted recordings. Failed
and date-scoped runs save this rotation state as well. A failed recording stays
eligible on its next turn, so it cannot block later recordings in limited runs.
Rotation state is stored once per source, while run history keeps outcomes and
counts. Because Plaud pagination is eventually consistent, changes made during
a run may be reconciled on a later run.

Plaud limits how fast an account can call its service. When Plaud rejects a
call, msgvault waits up to a minute before each retry and slows its remaining
calls for the rest of the run. Large libraries can still hit the limit on every
scheduled run; set `limit` on the `[[plaud]]` entry to spread checks across
several scheduled runs, for example `limit = 50` with a six-hour schedule.

A recording waiting for transcription can still archive its metadata and notes,
and later runs retry pending content. Temporarily missing transcripts or note
tabs keep the previously archived content. Recordings deleted from Plaud stay
in your archive. A failed or canceled run stays marked failed and refreshes
search and cache for additions or updates already committed.

Run with `--probe` to print tool names, input schemas, and a first-page
recording count without printing meeting titles, file IDs, transcripts, or note
bodies. Use it to diagnose provider contract changes without touching the
archive.

`msgvault serve` runs enabled entries that have a schedule. Removing the
registered source stops sync from recreating it; run `add-plaud` to register
it again. See the [configuration reference](../configuration.md#plaud-sources)
and [CLI reference](../cli-reference.md#sync-plaud) for exact fields and flags.

## Pocket

You can archive personal Pocket recordings with their transcripts, completed
summaries, structured actions, speaker labels, and provider durations.

1. Create a personal API key in Pocket under **Settings → Developer → API Keys**,
   as described in [Pocket's MCP setup](https://docs.heypocketai.com/docs/mcp).
2. Add a [`[[pocket]]` entry](../configuration.md#pocket-sources) with your
   account email and a stable label.
3. Set `POCKET_API_KEY`, or your configured `api_key_env`, in the daemon's
   environment and restart the daemon.
4. Run `msgvault add-pocket personal`, then run `msgvault sync-pocket personal`.

msgvault uses REST to discover recordings and fetch their contents, and Pocket's
MCP `get_account_info` tool to verify the email and stable user ID before
registration and on every sync. Organization membership on a personal account
is allowed, while organization API keys and recordings belonging to other
owners are outside this integration's scope.

Each sync checks all recording metadata, so edits to older meetings can be
refreshed. Use [`--limit`, `--after`, and `--full`](../cli-reference.md#sync-pocket)
to bound or repair a run. Missing, pending, or failed transcript and summary
processing keeps prior usable sections, while a successfully completed empty
section clears that section. If something disappears from Pocket, msgvault never
deletes the archived evidence.

Transcripts follow the `transcript.segments` schema in the
[REST detail reference](https://docs.heypocketai.com/docs/api/recordings/get-recording-details).
msgvault archives each segment's original-language text rather than a
translation. Segments show Pocket's resolved speaker name when one exists, or
the raw label such as `SPEAKER_00` otherwise. Only resolved names become
meeting participants.

The REST reference leaves summarizations unspecified. msgvault parses
completed `v2.summary` and `v2.actionItems` packages in the shapes shown in
[Pocket's webhook examples](https://docs.heypocketai.com/docs/api/webhooks).
Tests use synthetic responses rather than captured live accounts, so live
summary compatibility and plan entitlement remain unverified. An incompatible
non-null shape fails its recording without replacing existing evidence. API
responses and combined archived evidence are capped at 64 MiB.

Recording audio, source task updates, and fabricated speaker identities are not
included. Names without email addresses stay speaker labels. msgvault does not
generate a replacement summary or infer an action assignee.

## Circleback

Circleback has no REST API, so msgvault pulls data through its MCP server using
OAuth with dynamic client registration. No secret lives in your config.

### Configure and authorize

1. Add a `[[circleback]]` entry to `config.toml`:

   ```toml
   [[circleback]]
   identifier = "work"              # stable source label and token key
   account_email = "you@example.com" # primary identity for organizer matching
   schedule = "30 */6 * * *"        # optional: daemon cron schedule
   enabled = true
   ```

2. Run the authorization command:

   ```bash
   msgvault add-circleback work
   ```

   This opens a browser for Circleback authorization and saves the token under
   `tokens/circleback_<identifier>.json`.

Circleback OAuth uses a fixed `localhost:8090` callback. If you have a
configured remote msgvault server, `add-circleback` fails before proxying
because the callback and token must live on the daemon host. Run the command in
a shell on that host instead. When connecting over SSH, forward the callback
port:

```bash
ssh -L 8090:localhost:8090 user@daemon-host
# In that SSH session:
msgvault --local add-circleback work
```

If the remote host cannot open your workstation's browser, copy the printed
authorization URL into a local browser. The callback reaches the daemon-host
process through the SSH tunnel. On the daemon host, `--local` targets that
host's own archive; on a workstation, it would authorize a separate local
archive instead of your configured remote.

### Sync

```bash
msgvault sync-circleback                     # all configured accounts
msgvault sync-circleback --limit 5           # limited validation sync
msgvault sync-circleback --full              # re-fetch everything
msgvault sync-circleback --probe             # print tool inventory + sample result
```

Each incremental run lists meeting IDs with no scheduled-date bound, so newly
created backfills show up even when the meeting happened long ago. msgvault
fetches full details for unknown meetings and for known meetings created within
the 48-hour refresh overlap. Identical snapshots are skipped without
invalidating the search cache. To pick up edits to older known meetings, run
`--full`.

Circleback can publish notes before its transcript is ready. A recognized
missing or empty transcript is archived with state `pending`, then retried
every six hours. The retry deadline is seven days after the scheduled meeting
time, or a bounded 48-hour window for records without usable times. Future
meetings first retry at their known end time, or start plus one hour. When the
deadline expires, the state becomes `unavailable`, and `--full` can still check
and promote it later if a transcript appears.

Due transcript maintenance runs before newly searched meetings and does not
count toward `--limit`. For example, `--limit 5` processes at most five new
search results plus every bounded maintenance item that is due. Provider,
contract, missing-result, ingest, archive-recovery, and cancellation failures
fail the sync run and leave the prior successful cursor in place. Item-atomic
writes completed before the error stay safe to revisit on the next run.

Circleback's tool outputs have no published schema. If a sync imports meetings
with missing fields, run `--probe` to inspect the live field names; the
importer archives verbatim payloads for successfully decoded results.
Schema-drift payloads that cannot be decoded are rejected instead. Diagnose
them with `--probe`, then run `--full` after decoder support is updated.

### What gets stored (Circleback)

In addition to the shared fields above, action items (title, assignee, status),
insights, and tags go into the message metadata and body, while the meeting
recording URL and `recording_url_fetched_at` stay in the archived provider
metadata. msgvault does not expose recording URLs as durable attachments, and
downloading or archiving recording media is not supported.

## Muesli

[Muesli](https://github.com/Muesli-HQ/muesli) records and transcribes meetings
on a Mac and stores them in a local SQLite database. msgvault reads that
database directly in read-only mode. It never modifies Muesli's meetings or
schema, and it does not call `muesli-cli`, which updates the database whenever
it runs.

### Prerequisites

Run the normal msgvault client on the Mac where Muesli records. Your archive
can still live on another host by configuring the existing
[remote connection](../configuration.md#remote) on the Mac. The client reads
Muesli and Contacts locally, while the daemon owns the archive and accepts
authenticated, bounded meeting transfers. The daemon never receives a Muesli or
Contacts database.

Without remote mode, same-host sync runs through the local daemon. If macOS
blocks the read, grant Full Disk Access to the process reading the files: the
client in remote mode, or `msgvault serve` in same-host mode.

### Configure and register

Add a `[[muesli]]` entry to `config.toml`:

```toml
[[muesli]]
identifier = "mac"
account_email = "you@example.com"   # you, the person who records
# db_path = "~/Library/Application Support/Muesli/muesli.db"  # default
schedule = "*/30 * * * *"           # daemon schedule, or remote recorder --watch
enabled = true
```

`db_path` defaults to the stable app's database. If you run development builds
of Muesli, they use a different support folder such as `MuesliDev`, so set
`db_path` to point there.

### Attendees from Apple Contacts

People you tag in Muesli usually come from Apple Contacts. msgvault reads the
Mac's Contacts stores read-only, finds each attendee's card by its Contacts ID
or exact email, and links its unshared emails and phones. An address on
multiple unlinked Contacts cards stays in the meeting evidence without creating
an identity link. A contact tagged with only a phone number therefore reaches
the person you already chat with at that number.

- Reading Contacts requires Full Disk Access for the process running the client
  in remote mode or `msgvault serve` in same-host mode. Without it, meetings
  still sync, and `sync-muesli` reports `Contacts: unavailable`.
- Phone numbers typed with `+` or `00` always work. To also use numbers typed
  without a country code, set `phone_country_code`, for example `"1"` or `"44"`.
- When only some Contacts accounts can be read, msgvault still uses Contacts
  IDs to retain meeting evidence, but stops matching by email and creating
  automatic identity links. An unreadable account could hold another card with
  the same address. Linking resumes once every account is readable.
- When Contacts is unreadable or a card disappears, an attendee still present
  in Muesli keeps the identities archived for that meeting. Those retained
  addresses do not assert current Contacts ownership. Removing the attendee in
  Muesli removes its meeting association.
- Set `contacts = false` to turn off the Contacts lookup.

Register the source:

```bash
msgvault add-muesli mac
```

`add-muesli` verifies that the file opens read-only as a Muesli database, then
registers the source.

### Sync

```bash
msgvault sync-muesli                     # all configured databases
msgvault sync-muesli mac --limit 5
msgvault sync-muesli --after 2026-01-01
msgvault sync-muesli --full
```

Every run reads the whole database and updates changed meetings in place,
including title, notes, transcript, participant, and folder edits. Unchanged
meetings are skipped.

- Meetings still recording or processing wait for a later run.
- Meetings deleted in Muesli stay in the archive unchanged.
- `--after` keeps meetings that start on or after the date, read as UTC.
- `--limit` caps the meetings processed in one run.
- `--full` rewrites every archived meeting, refreshing attribution after you
  add an identity.

### Automatic sync on a remote recorder

After registration, keep the native watcher running on the Mac:

```bash
msgvault sync-muesli --watch
```

The watcher immediately scans enabled sources with a `schedule`, then rescans
on their 5-field cron schedules. The process must stay running. msgvault does
not install a service. Each rescan reads the whole database so late transcript,
notes, and participant edits import. It uploads only meetings that are new or
changed since the daemon acknowledged them, plus meetings with pending
duplicate-card suggestions. The record of acknowledged meetings lives in
`<data_dir>/muesli-sync` on the Mac; `add-muesli` clears it and `--full`
ignores it. Source locks in the same directory serialize overlapping native
watcher, hook, and manual processes before they read a snapshot. A lost
acknowledgement is safe to retry: the source label, meeting row ID, and
creation time preserve archive identity.

For prompt import after a recording ends, install the native executable
launcher:

```bash
msgvault muesli-hook --install /path/to/launchers
```

Select the resulting `msgvault-muesli-hook` executable in Muesli. Its event
carries a meeting ID, not an archive export or webhook URL. The launcher reads
that row using msgvault's default configuration and requires exactly one Muesli
source. Because the link points at the `msgvault` you ran (such as Homebrew's
`bin` entry), upgrades keep it working; rerun the install to repoint it. Keep
scheduled rescanning enabled to recover missed hooks and later edits. See the
[CLI contract](../cli-reference.md#muesli-hook) for event fields, limits, and
failure behavior.

### Review duplicate Contacts cards

When an attendee's email appears on multiple unlinked Contacts cards, msgvault
preserves the ambiguity and offers matches to phone identities already in your
archive. This built-in path works with both local and remote sync when every
Contacts store is readable. Unknown phones do not create archive identities. An
email shared by so many cards that its phones exceed the 50-identity transfer
limit gets no suggestions, though its meetings still import.

Inspect the suggested email and phone in **People** under **Possible
duplicates** or from the CLI:

```bash
msgvault identity matches list
msgvault identity matches show <candidate-id>
msgvault identity matches accept <candidate-id> --review-token <token>
# Keep an incorrect suggestion apart instead:
msgvault identity matches reject <candidate-id> --review-token <token>
```

Use the token from `show` after reviewing its evidence. Accepting a match
connects the meeting attendee to the existing phone-backed person, and the next
activity update includes the meeting without reimporting its transcript.
Repeated scans preserve accepted and rejected choices. An unchanged meeting can
gain a suggestion if its phone identity arrives in the archive later.

Evidence describes a historical Contacts observation and does not establish
current ownership. Sync never chooses between divergent phones or merges two
curated people. If both endpoints already have curated profiles, review them
and use the
[explicit profile merge workflow](/docs/usage/people/#merge-duplicate-profiles-and-reverse-a-merge).

Routine imports follow the existing automatic analytics-refresh policy. An
unchanged remote upload does not request a build. A same-host scheduled scan
checks the cache even when nothing changed, so a build delayed by
`min_rebuild_interval` after a hook or manual import still runs. Pass
`--build-cache` for an explicit refresh or `--no-build-cache` to skip it.

### Muesli recordings

To capture recordings, enable them per source on the Mac that reads Muesli:

```toml
[[muesli]]
identifier = "mac"
account_email = "you@example.com"
recordings = true
recording_max_bytes = 4294967296  # optional; 4 GiB by default
```

Muesli must retain the audio through `meeting_recording_save_policy`. Its
`meeting_recording_file_format` can be M4A or WAV, and msgvault detects the
actual file format. It reads `meetings.saved_recording_path` within the
`meeting-recordings` folder beside the configured database. Older databases
without that column still sync transcripts. Temporary mic and system captures,
paths outside that folder, and symlinks to files are skipped.

Run `msgvault sync-muesli mac` normally. Audio becomes an ordinary attachment
on the meeting message, available in the Web UI and through the message/file
API. The archive stores bytes by content hash and updates one recording
attachment per meeting. Rescanning also checks unchanged meetings for late
audio. Missing, unreadable, incomplete, or recently modified files wait for a
later sync, and a file must be untouched for at least five seconds. Keep
scheduled rescanning or remote `--watch` enabled to pick up deferred files.
Source deletion never removes an archived recording.

In remote mode, the client snapshots the local audio and streams it separately
with owner credentials. Neither the database nor the recording path reaches the
daemon. The recorder's `recording_max_bytes` and the daemon's
`[server].muesli_recording_max_bytes` each default to 4 GiB per file. An
oversized recording reports a retryable error while its meeting still imports;
raise both applicable caps and sync again. Transfers use bounded memory and the
existing 30-minute upload read deadline. Interrupted transfers restart on the
next sync and cannot publish truncated audio.

Stored recordings use the shared Docbank attachment and audio classification,
consent, and content-hash deduplication. The existing audio worker uploads WAV
files (up to its 1 GiB inspection limit) when `[integrations.docbank].enabled`
and `all_sources_upload_consent` are true. When Docbank is disabled, nothing is
sent; enabling it without upload consent only records local discovery. M4A and
larger files go through the attachment mirror, which has its own separate
`attachment_upload_consent`; see
[Mirror stored attachments to Docbank](../configuration.md#mirror-stored-attachments-to-docbank).
See [Docbank configuration](../configuration.md#send-stored-audio-to-docbank)
for the shared integration's settings and limits.

### What gets stored (Muesli)

Each meeting becomes one `meeting_transcript` message in a `meeting`
conversation. The body holds the title, time, participant names, Muesli's AI
notes, the notes you typed, and the transcript. When Muesli skipped or failed
the summary, the body omits that notice instead of showing it as a summary.

Muesli does not record an organizer. msgvault attributes each meeting to
`account_email` as its organizer. Participants with an email address become
recipients and connect to your existing people. Participants without one, such
as a contact picked by name, appear only by name.

The raw archive (`muesli_json`) keeps the meeting's text, times, status,
template name, calendar event ID, folder path, and participant names, emails,
phones, review-only phone suggestions, and sources. Suggestions never become
attendee recipients or automatic links. The raw archive never stores audio file
paths, screen text, template prompts, or Apple Contacts identifiers.

To remove the archive source, run:

```bash
msgvault remove-account mac --type muesli --yes
```
