# Generic Single-Meeting Ingestion

Design for accepting one normalized meeting through the msgvault HTTP API.
Written 2026-07-23; status: approved.

## Summary

Msgvault will expose an authenticated `POST /api/v1/import/meeting` endpoint.
The caller supplies one provider-neutral meeting containing its stable external
ID, title, time range, summary, transcript, and optional people and metadata.
Msgvault renders and stores the record through the same canonical meeting path
used by the Granola and Circleback importers.

The endpoint is intentionally an ingestion primitive, not another provider
integration. A local script or external automation remains responsible for
detecting completed meetings, fetching provider data, translating it into the
normalized request, removing provider-specific private fields, and retrying
failed deliveries. Msgvault owns authentication, validation, canonical
formatting, idempotent persistence, sync accounting, search indexing, cache
refresh, and meeting presentation.

The intended topology is:

```text
meeting tool
    -> caller-managed completion automation
    -> provider-neutral meeting JSON
    -> authenticated POST /api/v1/import/meeting
    -> msgvault canonical meeting storage
```

## Goals

- Import one completed meeting into a local or remote msgvault daemon.
- Reuse the archive shape and body rendering already established by Granola.
- Keep the endpoint independent of any meeting provider's API or local schema.
- Preserve structured summary, transcript, timing, optional organizer and
  attendees, and caller-selected metadata.
- Make repeated delivery update one stable archived meeting rather than create
  duplicates.
- Make imported records appear in existing meeting search, API, and TUI views.
- Keep the client contract small enough for a shell script to call.
- Support both SQLite and PostgreSQL through existing store abstractions.

## Non-goals

- Polling, authenticating to, or storing credentials for a meeting provider.
- Accepting an unmodified provider response or recognizing provider schemas.
- Shipping a webhook executable, retry queue, launch agent, or background
  client.
- Uploading audio, attachments, or arbitrary files.
- Parsing plain transcript text into speaker or timestamp structures.
- Inventing an organizer, attendee, speaker, or account identity the caller did
  not provide.
- Providing caller-selected `source_type`, `message_type`, raw format, or
  conversation type.
- Providing scoped API tokens as part of this change.
- Adding batch ingestion. One request represents exactly one meeting.

## Existing msgvault contract

Granola and Circleback store each meeting as:

- one provider source with a stable identifier;
- one `meeting` conversation;
- one `meeting_transcript` message;
- a stable source message ID;
- a body containing title, time, attendee names, summary, and transcript;
- optional organizer and attendee participants;
- provider metadata and JSON raw data;
- full-text search content; and
- an import sync run followed by conversation-stat and analytics-cache
  maintenance.

Generic ingestion reuses that result without reusing provider clients, cursors,
pagination, polling, or provider-specific models.

## API contract

### Request

`POST /api/v1/import/meeting`

Authentication uses the server's existing API-key support. The endpoint accepts
`application/json` only and applies a 16 MiB request-body limit before decoding.

```json
{
  "source": {
    "identifier": "local-meetings",
    "display_name": "Local Meetings",
    "account_email": "user@example.com"
  },
  "meeting": {
    "external_id": "42",
    "title": "Weekly planning",
    "started_at": "2026-07-23T18:00:00Z",
    "ended_at": "2026-07-23T18:30:00Z",
    "summary_markdown": "## Summary\n\nReviewed the launch plan.",
    "summary_text": "",
    "transcript": "",
    "transcript_segments": [
      {
        "speaker": "Steve",
        "text": "Let's review the launch plan.",
        "offset_seconds": 4
      }
    ],
    "organizer": {
      "name": "Test Organizer",
      "email": "organizer@example.com"
    },
    "attendees": [
      {
        "name": "Test Attendee",
        "email": "attendee@example.com"
      }
    ],
    "metadata": {
      "calendar_event_id": "synthetic-event-42"
    }
  }
}
```

The decoder accepts exactly one JSON object, rejects trailing JSON values and
unknown fields, and preserves unknown keys only within the explicitly
extensible `meeting.metadata` object.

### Source fields

`source.identifier` is the caller-selected stable name for one logical import
stream. It is required, trimmed, non-empty, and limited to 128 UTF-8 bytes.
Changing it creates a different source and therefore a different idempotency
scope.

`source.display_name` is optional, trimmed, and limited to 256 UTF-8 bytes. On
first delivery it defaults to `source.identifier`. A non-empty value on a later
delivery updates the source display name.

`source.account_email` is the primary identity for the imported source. It is
required, normalized with existing email conventions, and stored as a confirmed
`account-email` source identity. It is not automatically an organizer,
attendee, sender, or conversation member. It is used only to determine whether
an explicitly supplied organizer belongs to the importing account.

Every source created through this endpoint has the fixed source type
`meeting_import`. Callers cannot select `granola`, `circleback`, or another
existing source type.

### Meeting fields

`meeting.external_id` is required, trimmed, non-empty, and limited to 256 UTF-8
bytes. It is an opaque, case-sensitive identifier unique within
`source.identifier`.

`meeting.title` is optional, trimmed, and limited to 4,096 UTF-8 bytes. An empty
title falls back to `Meeting on YYYY-MM-DD`, derived from `started_at`.

`meeting.started_at` is required and must be an RFC 3339 timestamp with an
explicit offset. It is normalized to UTC for persistence.

`meeting.ended_at` is optional. When present, it must use the same timestamp
contract and be later than or equal to `started_at`. Duration is derived from
the normalized range.

`meeting.summary_markdown` and `meeting.summary_text` are optional strings.
Markdown takes precedence when both are present, matching Granola.

`meeting.transcript` is optional plain text. Msgvault trims outer whitespace but
otherwise preserves its line breaks and speaker labels. It does not parse,
rewrite, or timestamp transcript lines.

`meeting.transcript_segments` is an optional structured alternative for callers
that know speaker boundaries. Each segment requires non-empty `speaker` and
`text` strings. `offset_seconds` is optional; when present it must be finite and
non-negative. Segment offsets must be non-decreasing.

The caller supplies speaker identity. Passing `Speaker 1` preserves that
anonymous label; passing `Steve` renders `Steve`. Msgvault does not perform
speaker diarization, voice recognition, attendee-to-voice matching, or
cross-meeting speaker identification.

`transcript` and `transcript_segments` are mutually exclusive after trimming
empty values. This avoids storing two competing versions of the same
transcript.

At least one of the selected summary, plain transcript, or structured transcript
segments must be non-empty after trimming.

`meeting.organizer` is optional. When present, it contains an optional display
name and a required valid email address.

`meeting.attendees` is optional. Each attendee requires a valid email address
and may include a display name. Addresses are normalized and deduplicated
case-insensitively while preserving first-seen order. The organizer may also be
an attendee; participant and recipient rows still remain deduplicated.

`meeting.metadata` is an optional JSON object controlled by the caller. Msgvault
stores it under a namespaced metadata key but does not interpret its contents.
The caller is responsible for excluding secrets, local paths, or other private
provider fields it does not want archived.

## Canonical storage mapping

The importer calls:

```text
GetOrCreateSource("meeting_import", source.identifier)
```

It sets or updates the source display name, confirms `source.account_email`,
and runs the normal post-source-create migration hook.

The meeting mapping is:

| Msgvault field | Request value |
| --- | --- |
| Source type | `meeting_import` |
| Source identifier | `source.identifier` |
| Source display name | `source.display_name` or identifier fallback |
| Source message ID | `meeting:<meeting.external_id>` |
| Source conversation ID | `meeting:<meeting.external_id>` |
| Conversation type | `meeting` |
| Message type | `meeting_transcript` |
| Subject and conversation title | normalized title or date fallback |
| Sent time | normalized `meeting.started_at` |
| Sender | normalized organizer, when supplied |
| Is from me | organizer matches any confirmed source identity |
| From recipients | organizer, when supplied |
| To recipients | normalized attendees |
| Conversation members | normalized attendees |
| Raw format | `meeting_json` |

An absent organizer produces no sender and no `from` recipient, with
`is_from_me=false`. An empty attendee list produces no `to` recipients and no
conversation members. Redelivery replaces organizer, recipients, and
conversation membership, including replacing a previously populated set with
an empty set.

## Canonical body

Msgvault builds one deterministic plain-text body using the Granola convention:

```text
Weekly planning
When: 2026-07-23 18:00 - 18:30
Attendees: Test Attendee

## Summary

Reviewed the launch plan.

Transcript:
[00:04] Steve: Let's review the launch plan.
```

The rules are:

1. Write the normalized title.
2. Write `When:` using UTC time. Include the end time when supplied.
3. Write attendee display names, omitting attendees with no name.
4. Write trimmed `summary_markdown`, or `summary_text` when Markdown is empty.
5. Write `Transcript:` followed by the transcript when present.
6. For structured segments, render `[mm:ss] Speaker: text` when an offset is
   present and `Speaker: text` otherwise. Preserve the supplied speaker name.
7. Trim only the complete body's outer whitespace.

The snippet is the first 200 runes of the complete body, matching existing
meeting importers. FTS indexes the subject and body. Organizer and attendee
email addresses enter their dedicated FTS address columns and are not repeated
inside the body.

## Metadata and raw record

Msgvault-generated message metadata records:

- platform: `meeting_import`;
- external meeting ID;
- source identifier;
- normalized start and optional end;
- derived duration in seconds;
- organizer email, when present;
- attendee count;
- whether summary and transcript content are present;
- structured transcript segment count; and
- caller metadata under `provider_metadata`, when supplied.

The raw record is a canonical serialization of the normalized `meeting` object,
not the complete HTTP request. It contains only the fields in this contract and
uses the raw format `meeting_json`. It does not include the API key or other
request headers.

The endpoint does not attempt provider-specific sanitization. Anything placed
in summary, transcript, people fields, or `metadata` is intentionally archived.

## Idempotency and updates

The database uniqueness constraint on `(source_id, source_message_id)` is the
idempotency boundary:

```text
source type meeting_import
    + source.identifier
    + "meeting:" + meeting.external_id
```

The first delivery creates the message. Every later delivery for the same key
atomically replaces the complete canonical snapshot: title, time, body, raw
record, metadata, organizer, attendees, conversation membership, and FTS.

The endpoint does not compute a semantic hash and does not expose an
`unchanged` status. An identical retry performs a safe upsert and returns
`updated`. This keeps the generic contract small while guaranteeing that
retries never create duplicate meetings.

## Sync and cache lifecycle

Each request is a one-item import run:

1. Resolve or create the source and confirm its identity.
2. Start a sync run for the source.
3. Determine whether the stable message key already exists.
4. Persist the complete meeting with `store.PersistMessage`.
5. Record processed `1` and either added `1` or updated `1`.
6. Recompute conversation statistics for the source.
7. Complete the sync run.
8. Run the existing staleness-aware analytics-cache refresh.

An error after the sync starts records a failed sync with the latest counters.
Persistence remains atomic at the single-message boundary. Sync accounting,
conversation-stat recomputation, and cache publication are subsequent
idempotent operations rather than part of the same database transaction.

If persistence succeeds but a later step fails, the endpoint returns
`500 internal_error`. Retrying the same stable key safely rewrites the meeting,
repairs statistics, and retries the cache refresh. Failed and updated sync-run
watermarks ensure cache staleness remains detectable.

The endpoint stays behind the daemon's normal mutation operation gate. Cache
refresh is synchronous, uses the request context, and remains skipped for
PostgreSQL through the existing helper.

## Responses

A newly created meeting returns HTTP 201:

```json
{
  "status": "created",
  "source_id": 3,
  "message_id": 901,
  "source_message_id": "meeting:42"
}
```

An existing meeting updated in place returns HTTP 200 with `status` set to
`updated` and the same archive identifiers.

Errors use msgvault's existing `{error,message}` envelope:

| Status | Error | Meaning |
| --- | --- | --- |
| 400 | `bad_request` | Malformed, trailing, or structurally invalid JSON |
| 401 | `unauthorized` | Missing or invalid API key |
| 413 | `request_too_large` | Body exceeds 16 MiB |
| 415 | `unsupported_media_type` | Content type is not JSON |
| 422 | `validation_failed` | A typed field violates the semantic contract |
| 500 | `internal_error` | Persistence, lifecycle, or cache refresh failed |
| 503 | `service_unavailable` | Import capability is unavailable or the daemon is busy |

Validation and server errors never echo summary or transcript content.

## TUI and search behavior

`meeting_import` becomes a recognized meeting source type in the TUI. Imported
meetings participate in the same combined meeting list and detail view as
Granola and Circleback.

The source column uses the source display name, falling back to `Imported` when
no usable label is available. The meeting source selector includes imported
meeting sources, and empty-state copy describes provider and imported meeting
sources without naming only Granola and Circleback.

All existing message, meeting-type search, query, and detail APIs work through
the canonical `meeting_transcript` message type. No separate read API is added.

## Security and privacy

- Existing API-key authentication is mandatory under the server's normal
  binding rules.
- The existing server API key remains broadly privileged; this design does not
  imply an ingest-only scope.
- Private-network transport does not replace API authentication.
- Rate limiting, request timeouts, and mutation serialization remain active.
- The request size is bounded before decoding.
- The caller cannot select internal source or message types.
- No file path is read and no remote URL is fetched.
- Logs may include source identifier, external meeting ID, result, duration,
  and error class, but never summary, transcript, people, metadata, or API-key
  content.
- Provider-specific sanitization belongs in the caller because only the caller
  understands the source payload.

## API compatibility

The endpoint is an additive API feature and requires a minor OpenAPI schema
version bump. Its request and response schemas are published in the checked-in
OpenAPI documents and generated Go client.

The strict top-level schema catches misspelled contract fields. Future optional
fields require an additive schema release; provider-specific extension data
belongs in `meeting.metadata`.

## Documentation

Public documentation will:

- describe the generic endpoint independently of any provider;
- explain the stable source and external-ID idempotency boundary;
- show one synthetic request and response;
- document organizer, attendee, summary, transcript, and metadata behavior;
- warn that caller metadata is stored as supplied;
- explain that retries update one meeting;
- state that cache-refresh failure may follow a successful database write and
  that retry is safe; and
- provide a short example of adapting a local meeting tool without shipping or
  supervising that adapter.

## Testing strategy

- Request tests cover authentication, media type, body limit, malformed and
  trailing JSON, unknown fields, timestamp and email validation, source limits,
  missing content, and provider metadata.
- Formatter tests prove Granola-compatible title, time, attendee, summary, and
  transcript rendering, including exact plain-transcript line preservation,
  caller-supplied speaker names, anonymous speaker labels, optional segment
  offsets, and summary Markdown precedence.
- Importer tests cover create, identical retry, changed redelivery, two sources
  sharing one external ID, replacement of organizer and attendees with empty
  values, `is_from_me`, metadata/raw storage, FTS, and atomic related-row
  persistence.
- Lifecycle tests prove sync-run counters, failed-run recording, conversation
  statistics, cache update detection, and retry after cache-refresh failure.
- SQLite tests run through the standard test store. PostgreSQL compatibility
  runs through the existing optional test DSN.
- TUI tests cover meeting source filtering, labels, empty states, and rendering
  with and without an organizer.
- An API-to-store integration test posts a synthetic meeting, reads it through
  the public API, redelivers changed content, and confirms the same message ID
  now exposes the replacement snapshot.

## Alternatives considered

### Accept one opaque body string

This is smaller but moves canonical meeting rendering into every client, loses
structured organizer and attendee mapping, and makes imported meetings
inconsistent with Granola and Circleback.

### Accept an unmodified provider response

This makes the server provider-aware, forces it to track external schemas and
private fields, and recreates a full integration for each caller. It is the
complexity this generic endpoint is intended to avoid.

### Import a generated email

Existing MBOX ingestion can archive searchable text but always creates an
`email` message. It does not produce the meeting conversation type, meeting TUI
behavior, or structured participant mapping.

### Write directly to the database

Direct writes bypass store invariants, FTS, sync accounting, conversation
statistics, cache staleness, operation serialization, and PostgreSQL
compatibility. They are not a supported integration boundary.

### Add batch ingestion immediately

Batching adds partial-success response design, per-item errors, memory limits,
and more complex sync accounting. A completion hook naturally emits one
meeting, so one request per meeting is the smallest useful contract.
