---
last_edited: "2026-11-23"
title: Web UI & API Server
description: Daemon-served analytical Web UI and REST API for your msgvault archive, with optional background sync scheduling.
---


## Overview

Running `msgvault serve` starts an HTTP server that exposes your archive through
the first-party Web UI at `/` and a REST API under `/api`. You can also enable
its background sync scheduler to keep accounts up to date on a cron schedule.
The release binary embeds the complete UI; see [Web UI](/docs/web-ui/) for
browser login, secure remote deployment, search states, and keyboard controls.

### Choose an integration path

| Need | Start here |
|---|---|
| Browse the archive | [Web UI](web-ui.md) |
| Use a generated Go client | `pkg/client` in the repository |
| Build a client in another language | `/openapi.json` from the daemon or `msgvault openapi` |
| Submit a background backfill | [Historical import jobs](#historical-import-jobs) |
| Archive messages from a source without a connector | [Import a message](#post-apiv1importmessage) |
| Follow background work | `/api/v1/operations/runs` and `/api/v1/operations/status` |
| Integrate an AI assistant | [MCP server](usage/chat.md) |

Connect AI assistants through the [native MCP endpoints](usage/chat.md#native-daemon-endpoints).

### Person fact decisions

Call `GET /api/v1/people/{id}/fact-decisions` to list decisions in newest-first order. Pass `claim_key` to narrow the list to one claim before applying `limit` and `offset`. Each decision includes its target, resolver version, score, and the score maxima for that version, though an unknown resolver version omits score maxima. The outcome is `saved`, `value-retired`, `current-value-kept`, `pinned-value-kept`, `too-close-to-choose`, or `not-used`, and `reason` explains the decision. Older decisions return a `null` outcome when their history cannot establish the result.

Call `GET /api/v1/people/{id}/fact-summary` to retrieve `claimed_fields`, `latest_claim_at`, `has_values`, and `has_fact_history`. The claim count groups claims by target kind and key, and `latest_claim_at` is `null` when no claims exist. Values cover attribute values and employments, while fact history covers claims, evidence, and pin events. To retrieve a single evidence row, filter `GET /api/v1/people/{id}/fact-evidence` by `evidence_id`.

### Filename suggestions

API schema 3.15 adds `POST /api/v1/files/suggest` and
`POST /api/v1/people/{id}/files/suggest` for suggesting attachment filenames
without changing existing file or message search. Both endpoints are read-only
under existing owner authentication; delegated agent tokens and remote-client
credentials gain no new access. They query archive metadata directly and need no
analytical cache or embedding provider.

Send one JSON object of at most 64 KiB. The endpoint rejects unknown fields,
invalid field casing, duplicate field names, and trailing objects. It does not
accept a query-language field, cursor, caller-selected sort, or semantic-search
input.

| Field | Contract |
|---|---|
| `filename_query` | Required; nonempty normalized text, at most 128 runes, eight tokens and 64 runes per token |
| `source_ids` | Optional positive IDs; at most 64 supplied values; empty means every source |
| `account_scopes` | Optional structured account selections; at most 16 scopes and 64 addresses per scope |
| `after`, `before` | Optional RFC3339 instants; inclusive `after`, exclusive `before`; when both are supplied, `after` must precede `before` |
| `mime_families` | At most eight supplied values: `image`, `pdf`, `audio`, `video`, `text`, `document`, `archive`, `other` |
| `limit` | Optional integer 1–100, default 20; explicit `null` is invalid |
| `directions` | Person endpoint only: `from_person`, `to_person`, `group`; omitted means all three |

All filters intersect, and input array limits count supplied values before
deduplication. Each account scope accepts optional `source_id`, `addresses`,
`unattributed`, and `inbound` fields. Within one scope, `addresses` and
`unattributed` act as alternatives, while separate scopes intersect. Supply
addresses as canonical lowercase bare mailboxes. Set `inbound` to select
received mail; without it, sent copies and calendar events can also match.
Source-specific and unattributed selections keep ordinary account-filter
semantics.

The response returns `suggestions`, `candidate_limit_reached`, and
`index_revision`. Each row includes `id`, `message_id`, `conversation_id`,
`source_id`, `occurred_at` (always present, possibly `null`), the current
`filename`, `mime_type` (possibly empty), `mime_family`, `filename_match`
(`exact`, `prefix`, `typo`), `size_bytes`, `content_state`, and
`content_available`. File search and suggestion rows derive `content_state`
from metadata as `metadata_only`, `url_only`, or `unverified`. An `unverified`
row records a hash without checking storage and sets `content_available` to
`null`; the other two states set it to `false`. File details check whether
storage opens and can return `local_content`, `missing_blob`, or an error,
though opening storage does not verify the bytes and a read or integrity check
can still fail. Suggestion rows never include storage paths, download URLs, or
content hashes. On the person endpoint, rows also include `person_provenance`
with the matching participant IDs, roles, and directions.

Responses include no total count or pagination cursor. A `false`
`candidate_limit_reached` proves that the candidate stream was exhausted for
that index snapshot, subject to your requested limit and index freshness. A
`true` value means suggestions may be incomplete, even when the list is empty.
A person can have at most 64 resolved participants, and provenance is bounded
to 4,096 unique message/participant/role edges per request. Overflow fails the
whole request with HTTP 422 `filename_scope_too_large` and never changes
identities.

Invalid input returns HTTP 400, and an absent person keeps ordinary HTTP 404
behavior. Missing index support, unfinished bootstrap, or a mismatched index
consumer returns retryable HTTP 503 `filename_index_unavailable`; requests
never scan or rebuild the archive. See
[ranking and maintenance](architecture/search-ranking.md#filename-suggestions)
and [CLI usage](cli-reference.md#typo-tolerant-filenames).

### Calendar control

Call `POST /api/v1/calendar/control` with one strict JSON object containing
`action` and `account`. Event mutations also require `calendar_id`, while
availability queries require either `calendar_id` or `calendar_ids`. Use owner
authentication or a delegated agent token. The endpoint rejects
unknown fields, duplicate keys, noncanonical field casing, null fields, and
multiple JSON objects, and caps the request body at 1 MiB.

| Field | Contract |
|---|---|
| `action` | `create`, `update`, `delete`, `move`, `respond`, `freebusy`, or `conflicts` |
| `account` | Configured `[[gcal]]` source name or OAuth account |
| `calendar_id` | Exact event target calendar ID, `primary`, or configured alias; availability uses it only when `calendar_ids` is empty |
| `event_id` | Required for update/delete/move/respond |
| `event` | Partial writable event fields: `summary`, `description`, `location`, `start`, `end`, `recurrence`, `attendees`, `reminders`; omitted update fields are preserved, explicit empty values clear |
| `add_attendees` | Guest email array for update, preserving existing attendees; excludes `event.attendees` replacement |
| `send_updates` | `none` by default, `all`, or `externalOnly` |
| `scope`, `original_start` | Recurring scope `single` (default), `all`, or `future` for update/delete; original occurrence start is RFC3339 or an all-day date |
| `destination` | Required calendar ID or alias for move |
| `response` | Required self RSVP: `accepted`, `declined`, or `tentative` |
| `time_min`, `time_max`, `calendar_ids`, `time_zone` | Availability range, selected calendars (at most 50), and IANA time zone; explicit `calendar_ids` are the only calendars validated and authorized |
| `dry_run`, `read_only` | Verify and plan without writes; reject all event mutations |
| `expected_plan_fingerprint` | Optional precondition from a prior dry run; return 409 `calendar_plan_changed` before writing if the current write plan, normalized OAuth account, or normalized notification mode differs |

Format `start` and `end` with Google's `dateTime` (RFC3339) or `date`
(all-day `YYYY-MM-DD`) and an optional `timeZone`; all-day `end` is exclusive.
Each guest uses `email` plus optional `displayName`, `optional`, and `resource`,
and the endpoint rejects caller-supplied RSVP state, organizer, and self flags.
Reminders take `useDefault` and `overrides`, where each override specifies a
`method` (`popup` or `email`) and `minutes` from `0` to `40320`.

The daemon requires an enabled source, explicit `write_calendars`,
event-write OAuth consent, and a live `owner` or `writer`
`accessRole`. Guest changes also require `invite_calendars`. Delegated grants
must match the exact `gcal` source identifier `account-email/calendar-id`:
`calendar.read` for availability, `calendar.event.read` for provider-derived
event details in delegated plans and write receipts, `calendar.write` for
changes, and `calendar.invite` in addition for guest changes. The daemon never
substitutes the authenticated account for a requested non-primary calendar.
When an availability request supplies `calendar_ids`, the daemon authorizes each
listed calendar and ignores `calendar_id`.

The response includes `plan`, `writes`, resolved calendar and account IDs, and
the notification mode. For owners and grants with `calendar.event.read`,
existing-event plan entries include `target.summary` and `target.start`. The
plan fingerprint covers this preview metadata, and the daemon never sends it in
a provider mutation. For availability requests with `calendar_ids`,
`calendar_id` names the first resolved selection and `freebusy.calendars` lists
every selection.

Each completed write includes its returned event, `message_id`, `archived`, and
an optional `archive_error`. The daemon sends provider mutations once. If a
later provider write fails, the response returns the completed writes alongside
`error` and sets `outcome_code` to `calendar_partial` or
`calendar_outcome_unknown`; uncertain outcomes also set `outcome_unknown: true`
on the HTTP 200 result while keeping receipts for completed writes. An archive
failure also keeps the receipt of the successful Google change. Reconcile these
results before deciding whether another mutation is safe, and do not replay an
uncertain mutation based only on its response.

Invalid requests return HTTP 400, source, consent, grant, or role denials
return 403, and missing events return 404. Local daemon setup failures return
500 to owners and a generic 403 to delegated callers. Provider failures with no
partial result return 502; an unknown outcome with no completed writes returns
`calendar_outcome_unknown`. Reconcile the current calendar state before taking
further action, and do not replay the uncertain operation based only on this
response. The serialized operation gate protects provider mutations and their
archive writes, including delegated calls, while body decoding, authorization,
availability queries, and dry runs neither hold the gate nor wait for a sync to
release it.

See [Calendar usage](usage/calendar.md#control-events) for setup, recurrence
limits, notification behavior, and reconciliation instructions.
`POST /api/v1/cli/add-calendar/plan` also accepts `write=true` to plan opt-in
`calendar.events` consent while keeping existing Google scopes.

## API compatibility

The API publishes its generated OpenAPI contract at `/openapi.json`.
`msgvault openapi` prints the checked-in contract without starting a daemon or
opening an archive. OpenAPI `info.version` is the **API schema version**;
it is separate from the binary release version. The current schema is **5.28.0**.

Schema 5.27.0 accepts `mode=vector` and `mode=hybrid` searches from callers limited to some sources;
they return hits from those sources only.

Schema 5.26.0 adds `if_exists: "return"` to `POST /api/v1/people/create`.
It returns a single matching saved person unchanged with HTTP 200 and
`ensure.status: matched_existing`, or creates a new person with HTTP 201 and
`ensure.status: created`. Return-mode HTTP 409 conflicts include `details` with all
ambiguous candidates or explicit participant-promotion guidance. The MCP
`create_person` argument requires this schema version or newer.

Schema 5.25.0 allows `diagnose-gmail-inventory` through the daemon command interface.

Schema 5.20.0 adds `message_type` to file details.

Schema 5.19.0 adds `GET /api/v1/cli/messages/export`, which streams
`export-messages` output as `application/x-ndjson`. It takes `start`, `end`,
repeatable `message_type` and `source`, and `person_id`, with the same
validation as the CLI flags. Bad filters return `400` and an unknown source
returns `404` before any output; a failure while streaming ends the stream
without its completion record. Read-only remote client keys may call it.

Schema 5.18.0 adds Directory cadence to `GET /api/v1/people/directory`: the `cadence` filter (`overdue`, `due`, `no_list`, `unknown`), `due_within_days`, the `most_overdue` and `next_due` sorts, a `cadence` object and `last_contact_channel` on each person, and page-level `cadence_presets` and `cadence_coverage`. `expected_value_id` 0 on attribute writes requires that no current value exists. CLI and MCP cadence lookups require a daemon supporting this schema.

Schema 5.17.0 adds person, source, conversation and incoming filters to the
[message change feed](#get-apiv1messageschanges).

Schema 5.16.0 adds `POST /api/v1/people/create` for standalone curated
profiles. The MCP `create_person` tool requires this schema version or newer.

Schema 5.15.0 applies the `mode` of [media search](#media-transcript-search)
to recordings as well as attachments, and adds `actual_mode` and
`degradations` to recording results.

Schema 5.14.0 adds each fact decision's submitted claim value.

Schema 5.13.0 adds fact decision outcomes, evidence filtering, and a person fact summary.

Schema 5.12.0 adds owner-only [related copies](#get-apiv1messagesidfamily).

Schema 5.11.0 adds meeting link suggestions.

Schema 5.10.0 adds owner meeting link decisions.

Schema 5.9.0 adds meeting records.

Schema 5.8.0 advertises the existing structured person profile routes to MCP:
`GET` and `PATCH /api/v1/people/{id}/profile`,
`GET /api/v1/people/{id}/profile/history`, and
`GET /api/v1/people/{id}/profile/media/{media_id}/content`.

Schema 5.7.0 adds first-appearance evidence to person contact state.

Schema 5.6.0 adds `POST /api/v1/identity/cache-refresh` to rebuild
identity-derived datasets without changing links or profiles.

Schema 5.5.0 adds `POST /api/v1/sources/{source_id}/account-attribution/repair`
to retry account matching for one source.

Schema 5.4.0 allows native tag reads for messages from deleted sources.

Schema 5.3.0 adds `auth_method` to IMAP accounts in `GET /api/v1/cli/accounts`:
`password` or `xoauth2`, present when the source's settings are readable.

Schema 5.2.0 adds open Kata issue search and optional task search queries.

Schema 5.1.0 adds time-zone-aware Month and Year grouping, group overviews, and item totals for time groups.

Schema 5.0.0 changes `content_available` in file lists and suggestions from a
boolean to a nullable boolean. A null value means storage has not been checked;
older clients can interpret it as false. Upgrade clients and the daemon together.

Schema 4.1.0 adds `labels` on merge-list and merge-detail responses: recorded names and saved contact points from the merge snapshot, reconciled current participant addresses, known participant IDs, and the count of distinct addresses added to the surviving profile. Addresses and current or recorded contact points carry an opaque `identity_key` for comparison across representations, services, and scopes. Current contact points include `service_label`, the registered name of their `service_slug`. History and Split use those labels. Summary labels are decoded from the stored snapshot during list and detail reads; opening the disclosure makes a separate request for its raw snapshot.

Schema 4.2.0 adds semantic/hybrid attachment retrieval and per-list mode, coverage and degradation fields to [media search](#media-transcript-search).

Schema 3.51.0 adds attachment processing outcomes and worker errors.

Schema 3.50.0 adds [`POST /api/v1/import/message`](#post-apiv1importmessage), which archives one message from a source msgvault has no connector for.

Schema 3.49.0 adds the `verification.read` modifier for delegated archive reads.

Schema 3.48.0 adds owner-only local embedding setup and progress through `GET` and `POST /api/v1/embeddings/local`.

Schema 3.47.0 adds optional `hints` to CLI and fast-search responses. Each hint contains `code`, `message`, and `retry_with.query`.

Schema 3.46.0 adds `has_more` to CLI full-text search and optional `ranking_limit_reached` and `candidate_limit_reached` to semantic search. `has_more` is true when another ranked hit exists and the next page fits within the configured ranking cap. `candidate_limit_reached` carries `k_per_signal` when hybrid retrieval measures an eligible message outside the combined retained candidates. `ranking_limit_reached` carries the effective page cap when an extra ranked hit exists, or omitted candidates would need a page beyond the cap. Raise each reported server setting above its value, restart the server, then request a larger page. Pool saturation alone sets neither cap field.

Schema 3.45.0 adds optional `sender_label` to message and file results, including person files. It identifies the sender separately from the conversation participants.

Schema 3.40.0 adds timezone-aware daily message grouping and `sent_only` group filtering. It also filters participant timelines to events and meetings attended by the owner.

Schema 3.39.0 adds the optional `account` to staged deletion list summaries.

Schema 3.38.0 adds exact sender and To participant identities to conversation windows and `kind` filters for canonical Email, Conversation, Event, Meeting, and Item entries. Conversation classification includes older chat messages whose stored message type is empty.

Schema 3.35.0 adds `GET /api/v1/calendar/events`, which lists archived calendar occurrences in a window with repeating events expanded. `msgvault calendar events` requires a daemon supporting this schema.

Schema 3.32.0 adds scheduled-work queue times and CardDAV scheduler activity: `queued_at` in scheduler account and CardDAV status, `scheduler_queued_at` in Sources status, and CardDAV `scheduler_running`.

Schema 3.29.0 adds the optional `address` filter to `GET /api/v1/people/directory`. It matches literal case-insensitive fragments on a saved person's currently bound identities or current curated contact points without semantic search. CLI and MCP address lookups require a daemon supporting this schema.

Schema 3.33.0 adds the read-only [person tier preview](#get-apiv1peopletiers) and [explanation](#get-apiv1peopleidtier) routes. The `preview_person_tiers` and `explain_person_tier` MCP tools require a daemon supporting this schema.

Schema 3.34.0 adds `calls_short` and `calls_long`, the answered-call counts, to person tier signals.

Schema 3.28.0 adds `calendar` to `GET /messages/{id}` and the CLI message route for calendar events: `status`, `sequence`, `start`, `end`, `all_day`, `time_zone`, and `ical_uid`. A field is `null` when the stored event does not carry it, such as a cancellation recorded before any details were seen. Calendar sync does not store a `sequence` of 0, so a `null` sequence usually means 0.

Schema 3.30.0 adds `hidden_text_present` to message details and search rows. It is a best-effort flag: true means the stored HTML likely hides readable text, and false does not guarantee that text is visible.

Schema 3.42.0 adds `imessage.media_max_participants` and `imessage.max_media_mb` to the settings catalog and updates.

Schema 3.19.0 adds pipe-separated OR in full-text message search queries.
Upgrade clients and daemon together across incompatible schema versions,
including remote deployments.

Schema 3.11.0 adds `GET /media/search/status` to report whether Docbank transcript search is configured. It also adds source filename, title and date to transcript search results and optional `conversation_title` to `GET /messages/{id}`.

Schema 3.9.0 adds source-scoped agent read permissions and optional `expires_at`. Admitted FTS search responses, including aggregates, filtered messages and total statistics with search text, carry optional `index_state`. Agent checks are bounded; owner CLI searches retain automatic verification and repair.
See [scoped archive reads](cli-reference.md#scoped-archive-reads).

Schema 3.22.0 adds account aliases and lifecycle fields, account merging, and
owner source-maintenance requests. Existing source identifiers and account
requests remain valid.

Schema 3.23.0 adds the [Docbank attachment mirror](#docbank-attachment-mirror)
routes, delivery receipts and status states.

Schema 3.26.0 adds `max_results` to `GET /messages/gmail-ids` for bounded query staging and `matched_count` to its bounded responses. See [staged deletion through MCP](https://msgvault.io/docs/usage/chat/#staged-deletion-via-mcp) for supported sources, skipped matches, and the staging limit.

Schema 3.44.0 adds the [open-loop](#open-loops) routes, including
`POST /api/v1/loops/{message_id}/reopen` to clear a done or snooze decision on
one exact question.

Schema 4.0.0 moves document processing and attachment text search to Docbank. Local document commands, search, status, and operation lanes are removed. Use [media search](https://msgvault.io/docs/api-server/#media-transcript-search) for mirrored attachment text. It also adds attachment processing outcomes, worker errors, and authenticated streamed Muesli recording imports.

Schema 3.0.0 removes the unguarded
`POST /api/v1/identity/match-candidates/{id}/accept` and `/reject` routes.
Use the corresponding `/review/accept` and `/review/reject` routes with a fresh
review token. Upgrade the CLI and daemon together; clients with an incompatible
schema fail before issuing archive requests. The HTTP prefix remains `/api/v1`.
This schema also adds consented identity scoring. See
[identity match review and scoring](#identity-match-review-and-scoring).

Schema 3.14.0 adds `paused` source syncs to operation history and its state filter.

Schema 3.15.0 adds [native email tags](#native-email-tags).

Schema 3.18.0 adds optional `related_message_id` to message detail,
conversation windows, and `show-message --json`. It links a
[Chatwoot](guides/chatwoot.md) call and its meeting. Other messages and
unavailable counterparts omit it.

Schema 3.13.0 adds the `docbank_rendition` evidence kind, which
[cites a Docbank transcript](usage/kata-issues.md#cite-a-docbank-transcript).

Schema 3.12.0 adds `GET /api/v1/integrations/kata/issues/{ref}/context`, which
[reads an issue's evidence back](usage/kata-issues.md#read-an-issues-evidence-back),
and the `ContextPassage` schema with optional `before` and `after` text.

Schema 3.12.0 adds `GET /api/v1/integrations/kata/issues`, which
[finds the Kata issues that cite a source](usage/kata-issues.md#find-issues-that-already-cite-a-source).
It checks the newest 500 evidence-bearing issues, or 20 if the listing exceeds 16 MiB. `truncated: true` means the list may be incomplete; check Kata before filing again.

Schema 3.4.0 adds [Kata issues from archive evidence](usage/kata-issues.md).
It also adds the `microsoft` CardDAV account provider, the
`microsoft_authorization_required` CardDAV error and repair codes, and the
`microsoft_contact_too_large` error code, which publish, approve, conflict
resolution and a sync of one selected connection return with 413 when a card
exceeds Outlook's 4 MB write limit. A sync of all connections returns 200 and
reports the code as that connection's failure, and run history records it as
the failure code.

Schema 3.5.0 adds the `account:` and `received:` search operators.

Schema 3.8.0 adds [read-only remote client credentials](guides/remote-deployment.md#read-only-remote-clients), HTTP 409 for ambiguous raw-message references, and HTTP 413 for remote read limits.

Schema 3.10.0 adds `GET /api/v1/people/by-uid`, which resolves current person
UIDs, retired person UIDs, and UIDs on mapped CardDAV resources. It returns a
conflict when one UID resolves to multiple people. Person responses expose
`carddav_bindings`; directory responses now include both `vcard_uid` and
`carddav_bindings`.

Schema 3.11.0 adds structured `account_scopes` filters, the Explore account
filter, and the virtual account catalog on `/cli/accounts`.

Schema 3.1.0 adds [calendar event control](#calendar-control),
availability queries, and opt-in `write` on Calendar consent plans.

Schema 3.2.0 adds `counts_pending` to `GET /api/v1/cli/accounts`. See
[archive statistics](#get-apiv1stats) for when it appears.

Schema 3.6.0 adds `GET /api/v1/messages/{id}/recordings`, which lists a
message's recordings with their Docbank transcript state. Existing routes are
unchanged.

Schema 3.7.0 adds `GET /api/v1/media/search` for scoped lexical transcript search.

Schema 3.14.0 adds `filename:` search filters, optional match snippets,
attachment names and counts, and `hybrid_available` to CLI search responses.

Schema 3.20.0 adds [contact route discovery](#contact-route-discovery).

Schema 3.21.0 adds `GET /api/v1/embeddings/status` and MCP
`get_embeddings_status`. See [message embedding status](#message-embedding-status).

Schema 2.35.0 adds `scope_escalation_source_type` (`gmail` or `msmail`) to
`POST /api/v1/cli/delete-staged/plan` responses that require a permission
upgrade. With `scope_escalation_account`, it lets the local CLI select the
authorization provider before starting the daemon worker.

Schema 2.34.0 adds optional `display_name` to `POST /api/v1/people`.
Omitted or null seeds a new profile with the first nonblank observed name in
its linked participant cluster, ordered by participant ID. An explicit string
takes precedence; empty or whitespace-only strings leave the name unset.
Re-promotion preserves the saved name, including an edited or cleared value.

Schema 2.33.0 adds `GET /api/v1/cli/message/original`, which returns one
message's original MIME with its account and conversation, and
`GET /api/v1/cli/message/thread`, which lists visible archived conversation
members in chronological order and reports which have original MIME stored.
The thread route accepts `all=true` to capture membership in one response;
otherwise it uses `limit` and `offset` pagination. Hidden duplicates are excluded.
The original route accepts a positive `max_bytes` below 9223372036854775807
to bound decoded MIME and its stored payload, allowing for compression overhead.
Exceeding the bound returns `413 original_message_too_large`. Omit `max_bytes`
for an unrestricted export.

Schema 2.31.0 adds analytics query freshness metadata, accepted background
cache-build jobs, job status lookup, and restricted archive SQL for MCP.

Schema 2.30.0 adds Kata availability and person agenda reads, creation, linking,
list placement, and unlinking. See [Kata configuration](configuration.md#integrationskata)
for setup and limits.

Schema 2.29.0 adds `took_ms` and a required `timings` breakdown
(`query_embedding_ms`, `retrieval_ms`, and `hydration_ms`) to vector and hybrid
`/api/v1/search` responses. SQLite responses also include an `accelerator`
field identifying the retrieval path.

Schema 2.27.0 adds deterministic meeting context export, archived action-item
listing, and duration metrics with exact direct or Explore scope.

Schema 2.28.0 adds optional `text_enabled` and `visual_enabled` fields to
authenticated health responses. They report configured search lanes; request
handlers still check readiness when each search runs.

Schema 2.26.0 adds optional `web_url` metadata to message result schemas. The
URL opens that message in the selected daemon's browser interface.

Schema 2.25.0 adds the CardDAV publication review flow:
`GET /api/v1/carddav/publications/{person_id}/preview` returns the exact
vCard the next write would send plus an approval token, and
`POST /api/v1/carddav/publications/{person_id}/approve` approves it. For a
conflict preview, follow approval with explicit `keep_local` conflict resolution;
other previews publish on approval.
Publication responses gain `inference_review_required`; see the
[CardDAV guide](usage/people-carddav.md#review-inferred-changes-before-they-are-exported).

Schema 2.24.0 adds Google Contacts authorization and CardDAV provider selection.

Schema 2.23.0 describes Settings structure. A sectioned group lists its
`sections`, and each setting in such a group names its `section`; groups
without sections omit both. A secret's state gains an optional `hint`: the
first three and last three characters of the value joined by an ellipsis
(`sk-…x9Q`), so a client can show which key is set. It is omitted for a
value under twelve characters, for the CardDAV password, and when nothing
is set; the value itself is never returned. `validation` gains two optional
fields:

- `format: "cron"` marks a five-field cron schedule, optionally preceded by a
  `CRON_TZ=<zone>` or `TZ=<zone>` prefix. The daemon accepts exactly the
  [cron schedules](configuration.md#cron-schedules) its configuration file
  accepts. The daemon trims surrounding whitespace and treats
  a zone prefix with no fields after it as the empty value, then validates
  the expression on PATCH; an empty value is rejected when the setting is
  `required`.
- `off` names the value that switches a setting off (`value`), what happens
  while it is off (`label`), a starting value for switching it on (`suggest`),
  and `on_minimum`, the smallest value accepted while on. A value between the
  off value and `on_minimum` is raised to `on_minimum` on PATCH rather than
  rejected. `minimum` and `maximum` keep covering every accepted value
  including the off value, so a client that ignores `off` still accepts what
  the daemon stores.

The daemon no longer emits the `sync`, `logging`, `activity`, and `backup`
groups, which folded into `sources`, `server`, and `archive`; the `group` enum
keeps them so clients still accept older daemons.

Schema 2.21.0 adds Saved View execution at
`POST /api/v1/saved-views/{id}/run`, publishes the accepted Saved View
definition values, and includes incompatibility reasons when reading stored
views. Explore file responses also declare when semantic search uses only
active messages.

Participant analytics live under `/api/v1/participants/*`; durable curated
profiles live under `/api/v1/people/*`. CardDAV publication and conflict
responses are bounded projections that omit raw vCards and resource hrefs;
only the explicit publication preview route returns a raw vCard.
See [release changes](changelog.md#upgrade-and-compatibility) for removed paths
and the 1.x/2.x transition.

Schema 3.37.0 returns HTTP 200 from `GET /api/v1/participants/{id}` for a participant with no sent or received messages, with `activity_count` set to 0 and `first_at` and `last_at` omitted. `GET /api/v1/participants/{id}/inboxes` also returns HTTP 200 with an empty list for that participant. `GET /api/v1/relationships/{id}/calendar` still returns 404 for that participant because it has no relationship score; check `activity_count` before requesting the calendar. The generated Go client in `pkg/client` declares `PersonSummary.FirstAt` and `LastAt` as `*time.Time`.

### Person UIDs and CardDAV bindings

Person responses (`GET /api/v1/people/{id}`, `GET /api/v1/people`, person
updates, and semantic person search) return `vcard_uid` for each person and
include `carddav_bindings` when the person has mapped CardDAV cards. Directory
rows always include both fields, with `carddav_bindings` as an empty array when
no card is mapped. Each binding names the CardDAV `connection` and `book` along
with the resource `href`, `remote_uid`, and `mapping_status`.

Call `GET /api/v1/people/by-uid?uid={uid}` to resolve an exact current person
UID, a retired UID that still aliases a person, or the UID of a CardDAV card
mapped to that person. If the same CardDAV UID maps to different people, the
API returns `409 person_uid_ambiguous`.

### Native email tags

You can read or update an archived message's live Gmail labels, IMAP keywords,
or Microsoft Graph categories through one pair of routes. These routes require
API schema 3.15.0 and owner authorization; delegated agent tokens cannot call
them, and the daemon owns provider credentials. Reads and previews work while
the source syncs. A write waits up to 10 seconds for the daemon work gate and
returns 503 `operation_in_progress` if the gate stays busy. After acquiring the
gate, the write returns 409 `sync_active` if the source is still syncing; if
the sync finishes while the write waits, the write proceeds.

| Method and path | Contract |
|---|---|
| `GET /api/v1/messages/{id}/tags` | Read current native tags; optional `mailbox` query selects an exact recorded IMAP copy |
| `POST /api/v1/messages/{id}/tags` | Apply `add` and `remove` arrays, optional `mailbox`, and optional `dry_run` |

On API schema 5.4.0 or newer, `GET` can read tags for a message marked deleted
from its source, such as a message still in Trash, as long as the provider can
still resolve the stored identity. The read returns what the provider observes
and leaves the archive unchanged. `POST`, including `dry_run`, still rejects
source-deleted messages, and both methods reject locally hidden messages.

Pass the positive archived message ID as `id`. If you omit `mailbox` on an IMAP
message, the daemon selects the current original copy first and then the sole
current membership. When the original copy is absent and multiple copies
survive, you must pass an explicit `mailbox`.

Each of `add` and `remove` accepts at most 100 nonblank UTF-8 strings. Before
waiting for other work or checking provider credentials, the daemon validates
tag names, removes duplicates, and rejects overlapping `add` and `remove` sets.
Provider rules vary:

- **IMAP.** Accepts ASCII keyword atoms up to 255 bytes and compares them
  without case.
- **Gmail.** Requires existing user label IDs.
- **Microsoft Graph.** Accepts category names up to 255 Unicode characters,
  excludes commas, and compares names without case while preserving provider
  spelling. Writes require `Mail.ReadWrite`. A Graph 429 returns
  `provider_write_failed` (retry after the limit clears), while transport
  failures and 5xx responses stay `remote_unknown`.

See [`message-tags`](cli-reference.md#message-tags) for provider requirements.

```json
{"add":["Label_123"],"remove":["Label_456"],"dry_run":true}
```

The response includes `message_id`, `source_id`, `provider`, `tags`, `before`,
`available_tags` (`id` and `name`), `dry_run`, and `verified`. For IMAP, it
also returns `mailbox`, `uidvalidity`, `uid`, the observed `flags`, and
`can_create_keywords` when the server supports new persistent keywords. Gmail
returns all current label IDs, including system labels, while `available_tags`
lists only editable user labels. IMAP returns custom keywords in `tags`.
Microsoft Graph returns `provider: "msmail"` and category names in `tags`, with
`available_tags` listing the observed categories (no master-category catalog or
`MailboxSettings.Read` permission is required).

Reads return verified observed state, whereas previews return projected tags
with `verified: false` and never update the archive. A successful write reads
the tags back from the provider and then persists the observed Gmail labels,
IMAP membership flags, or Microsoft categories. Microsoft categories are stored
as separate `Category: <name>` archive labels and survive later folder syncs.
IMAP folder labels and local read state do not change. Gmail and Microsoft
category changes mark derived data stale and request a cache refresh.

Error responses include `error`, `message`, and, when available, `result` with
the last observed state. If a verification `GET` fails after a matching
mutation succeeds, the mutation response can supply newer tags. `before` keeps
the original snapshot, `verified` stays `false`, and the archive stays
unchanged until readback succeeds. Error snapshots omit `available_tags`. The
daemon client accepts tag error responses up to 1 MiB so observed tags, flags,
and recovery messages are not truncated.

Status codes map as follows:

- `400`: invalid tags.
- `403`: insufficient provider scope.
- `404`: missing archived message.
- `409`: stale provider identity, or `sync_active` if a source sync is still
  active after gate acquisition.
- `500` (`remote_accepted_local_failed`): preserves verified provider evidence
  and asks for a sync.
- `501`: unsupported provider or missing persistent keyword support.
- `502` (`remote_unknown` or `verification_failed`): read tags before retrying
  because the write may have applied.

See the generated OpenAPI contract for full response schemas.

### Identity match review and scoring

Call `POST /api/v1/identity/cache-refresh` to rebuild identity-derived datasets
without changing links or profiles. It requires API authentication and the
mutation gate, and returns `identity_revision` and `cache_state: "ready"` on
success. If a SQLite cache is absent, it returns HTTP 503 with
`identity_cache_build_required`; run `msgvault build-cache` before retrying.
PostgreSQL returns HTTP 503 with `identity_cache_unsupported` because it has
no Parquet cache. Other build failures return HTTP 500, and a missing refresh
capability returns HTTP 503.

To review identity suggestions, call `GET /api/v1/identity/match-candidates`
and `GET /api/v1/identity/match-candidates/{id}`. Each response includes the
evidence, blockers, and a `review_token` tied to that exact snapshot. Accept or
reject a candidate by posting that token to
`POST /api/v1/identity/match-candidates/{id}/review/accept` or
`.../review/reject`. If the evidence or endpoints changed since you fetched the
candidate, the API returns `409 identity_match_review_stale`; fetch the
candidate again and make a fresh decision. Other conflicts keep their own error
codes, including `person_merge_required` when separate profiles need a merge
review. The older `.../{id}/accept` and `.../{id}/reject` routes are gone, so
clients must use the reviewed routes with a token.

Identity scoring runs only when you request it. Call
`GET /api/v1/identity/scoring/status` to check `ready`, `blocker`, credential
and consent status, the exact provider `disclosure`, its
`disclosure_fingerprint`, and a `data_fields` description of the raw identity
fields sent. Post `{"disclosure_fingerprint":"<fingerprint>"}` to
`POST /api/v1/identity/scoring/consent` to grant consent for the current
disclosure, or send the same body to `POST /api/v1/identity/scoring/revoke` to
withdraw consent for that fingerprint without changing configuration.

Start a scoring batch with `POST /api/v1/identity/scoring/run`, passing `{}` or
`{"limit":20}`. The limit defaults to your configured batch size and cannot
exceed it. The daemon generates review suggestions, checks local blockers,
sends eligible pairs to the provider, and journals the results without ever
accepting matches automatically.

A completed or partially completed run returns HTTP 200 with `results` and
`processed`. Each result includes a candidate ID, review token, proposed
action, status, and blockers. If the batch stops early, the response also
includes an `error` object with `code` and `message`, so always inspect `error`
even on HTTP 200. Completed results stay available. Error codes include
`consent_required`, `scoring_scan_incomplete`, and `scoring_run_failed`.
Preflight failures return standard non-200 error responses.

Call `GET /api/v1/identity/scoring/history` to read redacted judgments. You can
filter by `candidate_id` (zero or omitted returns all), set `limit` (default
100, range 1–100), and pass `before_id` for older entries using the
`next_before_id` returned by the previous page. Find the complete contracts in
`/openapi.json` from the daemon or `msgvault openapi`, and see
[configuration](configuration.md#people-identity-scoring) and the
[people guide](usage/people.md#optional-identity-scoring) for setup and consent.

### Archive and processing boundaries

The API reads and writes the same archive database and attachment store as
other clients. SQLite is the default, while PostgreSQL has
[documented feature limits](architecture/postgresql.md). Keyword search and
ordinary reads run against stored data, whereas semantic and hybrid search also
call the configured embedding endpoint. Profile and enrichment operations have
their own provider and consent contracts, and Docbank owns attachment text
processing and its consent.

Operation history records durable worker runs, date filters, filter-bound
pagination, error codes, and supported actions. Check the OpenAPI document for
exact request and response fields.

If you are integrating from Go, use the generated client in `pkg/client`. Its
wrapper handles msgvault-specific response details, such as deletion staging
dry-runs returning `200` while created manifests return `201`.

### Startup and readiness

The HTTP listener, health endpoint, and API routing come up before analytics
cache maintenance finishes. With `engine = "auto"`, aggregate requests use live
SQL while the cache builds or opens, then switch to DuckDB once it succeeds. If
no usable cache opens, the daemon stays on live SQL, and a failed automatic
refresh keeps the last usable publication. With `engine = "duckdb"`, analytics
stay unavailable until the required cache is ready, so analytics routes return
`503` during startup and never fall back to SQL. Cache-dependent routes also
return a structured `503` during automatic cache maintenance; inspect its
`readiness` field to tell a transient `building` state from an unavailable
cache and retry without user action. To skip automatic startup maintenance, set
`auto_build_cache = false`.

## Quick start

Add a `[server]` section to your `config.toml`:

```toml
[server]
api_port = 8080
api_key = "your-secret-key"
```

Start the server:

```bash
msgvault serve
```

Test connectivity:

```bash
# Health check (no auth required)
curl http://localhost:8080/health

# Archive stats (auth required)
curl -H "Authorization: Bearer your-secret-key" http://localhost:8080/api/v1/stats

# Generated OpenAPI document
curl http://localhost:8080/openapi.json
```

## Authentication

When `api_key` is set in your config, archive API endpoints require
authentication. The public application shell, `/health`, and the
browser-session bootstrap and login routes stay open so the UI can check
whether login is required. You can pass the API key in three ways:

| Method | Header | Example |
|---|---|---|
| Bearer token | `Authorization: Bearer <key>` | `Authorization: Bearer my-secret` |
| API key header | `X-API-Key: <key>` | `X-API-Key: my-secret` |
| Plain auth header | `Authorization: <key>` | `Authorization: my-secret` |

If no effective API key is configured, requests need no authentication. Secure
startup requires a key when binding to non-loopback addresses, and
`msgvault serve` creates and persists one if no credential source is
configured. See [server credentials](configuration.md#server) for file and
environment sources, persistence, and explicit insecure mode.

Keys listed under `[[server.remote_clients]]` authenticate the same way, but
they only reach a fixed set of read operations; every other route returns 403.
See [read-only remote clients](guides/remote-deployment.md#read-only-remote-clients).

## Historical import jobs {#historical-import-jobs}

You can start a Gmail or IMAP history backfill and poll its progress without
holding an HTTP request open. These endpoints reuse the daemon's existing
account credentials and full-sync path, and they do not accept uploaded archive
files. See [the importing guide](/docs/usage/importing/#historical-import-jobs)
for a step-by-step example, or
[MCP source status and sync](/docs/usage/chat/#source-status-and-sync) for the
assistant workflow.

### Start an import {#post-apiv1imports}

**Endpoint:** `POST /api/v1/imports`

Send one JSON object with `Content-Type: application/json`. The request body
limit is 16 KiB, and the endpoint rejects unknown fields and extra JSON values.

| Field | Required | Meaning |
|---|---|---|
| `account` | Yes | Exact account identifier or display name, matched case-insensitively, for one Gmail or IMAP source |
| `after` | No | Lower date bound in `YYYY-MM-DD` form |
| `before` | No | Upper date bound in `YYYY-MM-DD` form; must be later than `after` when both are present |
| `limit` | No | Non-negative message limit; `0` or omission means unlimited |
| `query` | No | Gmail search expression; a nonempty value is rejected for IMAP |
| `noresume` | No | Set `true` to start fresh instead of using a resumable checkpoint; defaults to `false` |

A successful request returns **`202 Accepted`** with a durable `job_id` and the
status record described below. The daemon starts the background work
immediately and does not create a persistent queue that resumes automatically
after a restart.

| Error | Meaning |
|---|---|
| `404 not_found` | No matching account exists |
| `409 ambiguous_account` | More than one syncable source matches the account name |
| `409 sync_already_active` | The source already has an active sync or import |
| `422 account_not_syncable` | The matching source is not Gmail or IMAP |
| `422 validation_failed` | A required value, date range, limit, or provider-specific field is invalid |
| `413 request_too_large` | The body exceeds 16 KiB |
| `415 unsupported_media_type` | The request is not JSON |
| `503 service_unavailable` | The daemon cannot start historical imports, including during shutdown |

Malformed JSON and unknown fields return `400 bad_request`. Normal API
authentication and archive-operation locking apply here too.

### Read progress {#get-apiv1importsjobid}

**Endpoint:** `GET /api/v1/imports/{job_id}`

Returns **`200 OK`** for an existing job or **`404 not_found`** for an unknown
ID. The start and progress responses share the same shape:

| Field | Meaning |
|---|---|
| `job_id` | Durable ID to save and poll |
| `account` | Resolved source identifier |
| `status` | `pending`, `running`, `done`, or `failed` |
| `processed`, `added`, `skipped` | Counts across sync runs belonging to this job |
| `created_at` | UTC creation time |
| `started_at` | UTC start time, or `null` before work starts |
| `finished_at` | UTC completion time, or `null` while unfinished |
| `summary` | Present for `done`; contains `processed`, `added`, `updated`, `skipped`, and `errors` |
| `error` | Present for `failed`; currently the generic message `import failed` |

`skipped` is the non-negative remainder of processed messages after additions
and updates. Check `summary.errors` alongside the terminal status when you
assess coverage. Detailed failure information lives in daemon logs and sync-run
diagnostics rather than the job's generic `error` field.

### Disconnects, failures, and restarts

Closing your HTTP connection does not cancel a submitted job, though daemon
shutdown cancels running import work. On the next start, the daemon marks jobs
left in `pending` or `running` state as `failed` while keeping their IDs and
progress for inspection.

Messages already committed stay in the archive after a failure. To continue
through the normal resumable importer, submit a new job with the same account
and bounds, and leave `noresume` set to `false` if you want to reuse saved
progress. There is no dedicated cancellation endpoint for these jobs.

## API endpoints

### Query the analytics cache {#post-apiv1query}

**Endpoint:** `POST /api/v1/query`

Send one read-only SQL statement as `{"sql":"SELECT 1"}`. To request a
background cache refresh, set `fresh` to `true` in the JSON body or pass
`?fresh=true`; conflicting body and query values are rejected. For SQLite
archives, this endpoint queries published Parquet through DuckDB. The
`[analytics].engine` setting picks the engine for aggregate views and does not
change this raw SQL endpoint.

A `200` response returns `columns`, `rows`, and `row_count`. When the query
runs against a committed Parquet publication, `cache` adds `generation` and
`published_at`, plus `stale_reason`, `pending_additions`, or `building` when
applicable. A usable stale publication stays queryable during
`min_rebuild_interval` and while a refresh runs, including messages deleted
since publication; see the [cache freshness policy](configuration.md#analytics).

When you pass `fresh=true`, the endpoint accepts the refresh with `202`,
`job_id`, and `status` instead of holding the request open. Automatic recovery
of a missing or incompatible cache also returns `202` when enabled. A fresh
request checks archive writes committed before the request, and if a build is
already running, the response identifies a queued follow-up check. Poll
`GET /api/v1/cache-builds/{job_id}` for `queued`, `running`, `published`, or
`failed`. Once the job reaches `published`, repeat your SQL query with
`fresh=false` to fetch rows. A `published` status means the check completed
successfully and the cache is usable; if no changes were found, the existing
generation stays in place. A `failed` job includes an `error` message.

The daemon keeps the last 100 completed jobs plus running and queued jobs in
memory, so an evicted job or one from before a daemon restart returns `404`.

`POST /api/v1/query/archive` uses the same request and response shapes. MCP
calls this endpoint to run SQL in a separate DuckDB instance restricted to the
analytics directory, with outside-file access, network access, and extension
loading disabled. Both endpoints require daemon owner authentication; the
original `/query` endpoint keeps privileged file access. If the restricted
engine is unavailable, `/query/archive` returns `503` without falling back to
the privileged engine.

---

### Person creation conflicts {#person-creation-conflicts}

`ensure` belongs to the shared `Person` response schema, but is emitted only
by person creation with `if_exists: "return"`. Other `Person` responses omit it.

With `if_exists: "return"`, `POST /api/v1/people/create` returns HTTP 409
when contacts identify multiple targets or a single unbound participant cluster.
The response has `error`, `message`, and `details` fields:

| `error` | `details` fields |
| --- | --- |
| `person_ambiguous` | `candidates`: array of `{person_id, participant_id, name}`; `match_basis`: array of matching evidence. Each candidate identifies a saved person or an unbound cluster; its other ID is null. |
| `person_matches_participant` | `participant_id`: the unbound cluster's representative ID; `match_basis`: array of matching evidence; `next`: explicit promotion guidance. |

Each `match_basis` entry contains `kind` (`email` or `phone`), normalized
`value`, `matched` (`contact_point`, `participant`, `identifier`, or
`observation`), and nullable `person_id` and `participant_id` fields.

`details.next.cli` is an argv array, and `details.next.mcp` identifies a tool
and its arguments. For example, promotion guidance for participant 9 is:

```json
{
  "cli": ["msgvault", "person", "promote", "9", "--json"],
  "mcp": {
    "tool": "promote_person",
    "arguments": {"participant_id": 9}
  }
}
```

Resolve ambiguity or promote the participant explicitly before retrying.
Omitting `if_exists` or using `"error"` retains the original
`person_contact_exists` conflict without `details`.

### Curated person network {#get-apiv1peopleidnetwork}

**Endpoint:** `GET /api/v1/people/{id}/network`

Returns a read-only, person-centered projection built only from durable typed
relationships and employments. It never derives nodes or edges from messages,
conversations, participant co-occurrence, or the analytical cache.

`depth` accepts `1`, `2`, or `3` and defaults to `1`. `include_ended` defaults
to `false`; set it to `true` to include ended relationships and employment
records. The deterministic breadth-first response holds at most 250 nodes and
500 edges. When `truncated` is `true`, the response is a bounded prefix and can
contain fewer than either maximum. The root durable person is always returned,
even with no qualifying connections.

```json
{
  "root_person_id": 42,
  "depth": 2,
  "truncated": false,
  "nodes": [
    {"id": "person:42", "kind": "person", "entity_id": 42, "label": "Example Person", "hop": 0},
    {"id": "organization:21", "kind": "organization", "entity_id": 21, "label": "Example Organization", "hop": 1}
  ],
  "edges": [
    {"id": "employment:7", "kind": "employment", "source_node_id": "person:42", "target_node_id": "organization:21", "label": "Engineer"}
  ]
}
```

Invalid depths return `400`; an unknown durable person returns `404`. The
projection has no ETag because it is not a mutation resource.

---

### Person tier preview {#get-apiv1peopletiers}

**Endpoint:** `GET /api/v1/people/tiers`

Lists saved people in tier 1 or tier 2, ordered with tier 1 first and the most
recent contact first within each tier. The daemon reads the rules, windows, and
thresholds from [`[people.tiers]`](/docs/configuration/#people-tiers) in the
config file at request time, so this matches `msgvault person tiers preview --json`.
This route is read-only; to track a person, call `PUT /api/v1/people/{id}/tracking`.

```json
{
  "people": [
    {"person_id": 7, "display_name": "Ana Example", "tier": 1, "rule": "tier1_intensity", "tracked": false, "last_contact_date": "2026-10-28"}
  ],
  "as_of": "2026-10-29",
  "readiness": {"ready": true, "reasons": []},
  "projection": {"timezone": "UTC", "max_direct": 25}
}
```

`readiness.ready` is `false` while the activity projection lags behind or was
built under different `[activity]` settings, in which case counts can be too
low. While the projection rebuilds after a change to `[activity] timezone` or
`max_direct_counterparts`, the route returns `409` /
`activity_projection_rebuilding`. Invalid `[people.tiers]` settings return
`503` / `person_tiers_config_invalid`.

---

### Person tier explanation {#get-apiv1peopleidtier}

**Endpoint:** `GET /api/v1/people/{id}/tier`

Shows every count behind one saved person's tier alongside its threshold,
including people in tier 3. The response includes the preview row's fields plus
`exclusion` (`owner`, `automated_address`, `calendar_room`, or `null`),
`signals` (the windowed counts), and `thresholds` (each check with `observed`,
`required`, `enabled`, and `met`). An unknown person returns `404` /
`person_profile_not_found`, and rebuild and config errors match the preview
route.

---

### Person brief enrollment {#get-apiv1peopleidbrief-enrollment}

**Endpoint:** `GET /api/v1/people/{id}/brief-enrollment`

Checks whether a durable person is enrolled in the "last time we talked" brief.
Enrolling a person enables brief generation when eligible messages and provider
budget are available.

```json
{"person_id": 7, "enrolled": true, "enabled_at": "2026-08-14T09:12:03Z", "actor": "api"}
```

When a person has no enrollment row, `enrolled` is `false`, `enabled_at` is
`null`, and `actor` is empty. An unknown durable person returns `404` /
`person_profile_not_found`.

---

### Replace person brief enrollment {#put-apiv1peopleidbrief-enrollment}

**Endpoint:** `PUT /api/v1/people/{id}/brief-enrollment`

**Request:**

```json
{"enrolled": true, "track": true}
```

`enrolled` is required. Enrollment requires a tracking row: pass `track: true`
to create one in the same transaction, and otherwise an untracked person is
rejected with `409` / `person_brief_not_tracked`. When `enrolled` is `false`,
the endpoint ignores `track`. The response uses the same shape as the read
endpoint above.

---

### Current person brief {#get-apiv1peopleidbrief}

**Endpoint:** `GET /api/v1/people/{id}/brief`

Returns the person's current brief version, or `404` / `person_brief_not_found`
when no version is stored.

```json
{
  "version": 2,
  "status": "current",
  "generated_at": "2026-08-29T18:44:02Z",
  "rendered_text": "Last time you talked (Aug 29, chat): they were preparing for a role change. They said they were learning to cook on weekends. You may want to ask how the transition went.",
  "renderer_policy": "person-brief-render-v1",
  "sentences": [
    {"kind": "last_interaction", "index": 0, "text": "Last time you talked (Aug 29, chat): they were preparing for a role change.", "evidence_ordinals": [0]},
    {"kind": "highlight", "index": 0, "text": "They said they were learning to cook on weekends.", "evidence_ordinals": [0]},
    {"kind": "follow_up", "index": 0, "text": "You may want to ask how the transition went.", "evidence_ordinals": [0]}
  ],
  "structured": {
    "last_meaningful_interaction": {"evidence_id": "evidence:...", "summary": "they were preparing for a role change"},
    "highlights": [
      {"text": "they were learning to cook on weekends", "speaker": "person", "evidence_ids": ["evidence:..."], "observed_at": "2026-08-29", "confidence_basis_points": 700}
    ],
    "follow_ups": [
      {"question": "how the transition went", "why": "the change was still pending", "highlight_index": 0, "evidence_ids": ["evidence:..."]}
    ],
    "appreciations": [],
    "uncertainties": [],
    "possible_attributes": []
  },
  "evidence": [
    {
      "ordinal": 0,
      "evidence_id": 4411,
      "evidence_key": "9c2f...",
      "source_ref": "person-sweep/v1:...",
      "source_url": "",
      "directness": "direct-self",
      "event_time": "2026-08-29T18:41:55Z",
      "evidence_supported": true
    }
  ],
  "boundary": {
    "lanes": ["conversation_text"],
    "from_event_time": "2026-07-01T00:00:00Z",
    "through_event_time": "2026-08-29T18:41:55Z",
    "through_sequence": 184233,
    "item_count": 31,
    "input_bytes": 48211,
    "packet_sha256": "..."
  },
  "dropped_item_count": 1,
  "program_id": "msgvault-person-brief",
  "program_version": "v1",
  "provider": "glm",
  "model": "glm-5.3",
  "rejected_at": null,
  "rejected_reason": "",
  "superseded_at": null
}
```

`structured` holds the validated record, and `rendered_text` is derived from it.
`sentences` maps each rendered sentence back to its structured item; the server
rebuilds it on read and returns an empty array when a version was stored under
a renderer policy this build does not recognize (named by `renderer_policy`).
Each sentence's `evidence_ordinals` lists the `ordinal` values in this
response's `evidence` array that the sentence cites, so you can expand a single
sentence to its own citations. When the version's citation order cannot be
accounted for exactly, `evidence_ordinals` is empty and a client falls back to
the whole `evidence` array rather than guessing a citation. `evidence` lists the
archive items the brief used as a whole, with `evidence_supported: false` once a
later status event invalidates an item's source. `dropped_item_count` counts the
structured items msgvault refused. Responses set `Cache-Control: no-store`, and
a brief never contains an excerpt of the archive text it came from.

---

### Person brief version history {#get-apiv1peopleidbriefversions}

**Endpoint:** `GET /api/v1/people/{id}/brief/versions`

| Parameter | Type | Default | Description |
|---|---|---|---|
| `limit` | integer | `20` | Versions to return, 1 to 200. Out-of-range values return `400` / `invalid_limit` |

Returns newest-first history in a `versions` array, with each entry shaped like
the current-brief response. Versions are immutable: regenerating a brief inserts
the new version as `current` and marks the previous one `superseded`.

---

### Reject a person brief {#post-apiv1peopleidbriefreject}

**Endpoint:** `POST /api/v1/people/{id}/brief/reject`

**Request:**

```json
{"reason": "merges two different threads"}
```

Marks the current version `rejected` and returns it in the same shape as the
current-brief response, with `rejected_at` and `rejected_reason` set. `reason`
is optional. The rejected version stays readable in history, and the next
eligible run creates a replacement without waiting out `min_interval`. If the
person has no current version, this endpoint returns `404` /
`person_brief_not_found`, and `GET /api/v1/people/{id}/brief` does the same
after a rejection until a replacement version is generated.

---

### Generate a person brief {#post-apiv1peopleidbriefgenerate}

**Endpoint:** `POST /api/v1/people/{id}/brief/generate`

Runs one forced sweep attempt for that person and waits for it to finish. Send
no request body.

```json
{"run_id": "9d1c...", "attempt_id": "5f20...", "brief_version": 3, "brief_failure_class": ""}
```

`brief_version` is the version the attempt stored, or `0` when it stored none.
`brief_failure_class` names a brief-generation failure when the overall attempt
succeeded, and it can also be empty with version `0`: having no eligible
evidence is a successful attempt that produces no brief, as with an email-only
person. The forced run bypasses the minimum interval and the new-activity check,
but still requires enrollment, a consented provider profile with
`allow_sensitive = true`, and available budget. It spends provider budget on
every call it makes, and when a person's cursors are already caught up, it may
also run one bounded extraction page.

Before running, generation checks for these conditions:

| Status | Code | Meaning |
|---|---|---|
| `409` | `person_brief_not_enrolled` | Enroll the person first |
| `409` | `person_brief_lane_disabled` | `[people.sweep.brief] enabled` is `false` |
| `409` | `person_brief_policy_refused` | The provider profile does not allow sensitive content |
| `409` | `person_brief_no_supported_lane` | The provider profile does not allow `conversation_text` |
| `409` | `person_brief_busy` | Another worker holds the person's sweep lease; retry after it finishes |
| `503` | `brief_generation_unavailable` | The daemon started without the people sweep worker, for example with `[people.sweep] enabled = false` |

Briefs use only supported chat and text-message sources within
`conversation_text`; email, meeting transcripts, documents, and owner-authored
messages are excluded. A scheduled sweep skips brief generation if the profile
does not permit its sources or sensitive content.

After enabling sweeps or changing the selected provider, restart the daemon.
Manual brief generation uses the configuration loaded at startup.

---

### Health check {#get-health}

**Endpoint:** `GET /health`

Checks whether the server is reachable. Requires no authentication.

**Response:**

```json
{"status": "ok", "analytics_engine": "sql-fallback"}
```

`analytics_engine` reports the active mode: `sql-fallback` while
`engine = "auto"` waits for or cannot use a cache, `duckdb` after a cache opens
or builds, `sql` for deliberate live SQL, `postgres` for PostgreSQL, and
`initializing` while required DuckDB analytics prepare. `/health` stays
available during startup, while analytics routes return `503` until the required
engine is ready.

Authenticated `GET /api/v1/health` adds `api_schema_version`. Starting in schema
2.28.0, its `vector` object can also include `text_enabled` and `visual_enabled`.
Each flag reflects whether its own lane is configured, reporting `false` for an
unconfigured lane and `true` for a configured lane, even while that lane
initializes or fails. Public and delegated health responses omit them.

---

### Archive statistics {#get-apiv1stats}

**Endpoint:** `GET /api/v1/stats`

Returns archive statistics. When vector search is configured on the server, the
response adds a `vector_search` object describing the index state.

While a sync or cache build loads the archive, `/api/v1/stats` responds within
about 2 seconds:

- If fresh counts are not ready in time, the response reuses the previous counts
  and sets `"stale": true`, with `as_of` showing when they were computed. Fresh
  counts replace them once they finish.
- Vector statistics have a 3-second budget. If they miss it, the response sets
  `"vector_stats_unavailable": true` instead of failing.

`GET /api/v1/cli/accounts` bounds its message counts the same way, with `stale`
and `as_of` at the top level. Before the first counts finish, no earlier
snapshot exists, so the request fails with `503 query_timeout`. To get the
accounts anyway, send the `X-Msgvault-Allow-Pending-Counts: true` header: the
response sets `"counts_pending": true` and uses `0` as a placeholder for each
account's `message_count` and `source_deleted_count`.

Once a count snapshot exists, fresh counts have 500 ms. If they do not finish in
time, the response returns the previous counts with `"stale": true` and their
original `as_of` while the refresh continues. The CLI displays these counts with
their age, such as `1,234 (counted 3 minutes ago)`.

Virtual-account counts keep their two-second cold wait. Once a matching catalog
exists, callers that send the pending-counts header reuse it immediately after
expiry while it refreshes, whereas strict callers keep the two-second refresh
wait.

**Response (vector search disabled):**

```json
{
  "total_messages": 142857,
  "total_threads": 48293,
  "total_accounts": 2,
  "total_labels": 47,
  "total_attachments": 31204,
  "database_size_bytes": 8589934592
}
```

**Response (vector search enabled):**

```json
{
  "total_messages": 142857,
  "total_threads": 48293,
  "total_accounts": 2,
  "total_labels": 47,
  "total_attachments": 31204,
  "database_size_bytes": 8589934592,
  "vector_search": {
    "enabled": true,
    "active_generation": {
      "id": 3,
      "model": "nomic-embed-text-v1.5",
      "dimension": 768,
      "fingerprint": "nomic-embed-text-v1.5:768:p1-111111:c32768:e1",
      "state": "active",
      "activated_at": "2026-04-18T15:12:33Z",
      "message_count": 142820
    },
    "building_generation": {
      "id": 4,
      "model": "nomic-embed-text-v2",
      "dimension": 768,
      "started_at": "2026-04-19T09:02:10Z",
      "progress": { "done": 8200, "total": 142857 }
    },
    "missing_embeddings_total": 134657
  }
}
```

`active_generation` is always present in the object and is `null` until the
first build completes. `building_generation` appears only while a rebuild is in
flight. `missing_embeddings_total` counts live messages that still need
embedding for the generation the worker targets next: the building generation
during a rebuild, or the active generation otherwise. While a rebuild runs, the
old active generation keeps serving vector and hybrid search, and
active-generation top-ups pause until the building generation activates. See
[Vector Search](/docs/usage/vector-search/) for the end-to-end workflow.

---

### List messages {#get-apiv1messages}

**Endpoint:** `GET /api/v1/messages`

Paginated message list.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `page` | int | `1` | Page number |
| `page_size` | int | `20` | Results per page |

**Response:**

```json
{
  "total": 142857,
  "page": 1,
  "page_size": 20,
  "messages": [
    {
      "id": 12345,
      "subject": "Q4 Planning",
      "message_type": "email",
      "from": "alice@example.com",
      "to": ["bob@example.com"],
      "cc": ["carol@example.com"],
      "sent_at": "2024-10-15T09:30:00Z",
      "snippet": "Here's the draft for Q4...",
      "labels": ["INBOX", "IMPORTANT"],
      "has_attachments": true,
      "size_bytes": 52480
    }
  ]
}
```

---

### Filter messages {#get-apiv1messagesfilter}

**Endpoint:** `GET /api/v1/messages/filter`

List messages with structured filters backed by the query engine. This is the
API equivalent of drilling into aggregate/search results and is the endpoint to
use for message-type filtering when you do not need full-text ranking.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `sender` / `sender_name` | string | — | Sender address/phone or display-name filter |
| `recipient` / `recipient_name` | string | — | Recipient address/phone or display-name filter |
| `domain` | string | — | Participant domain filter |
| `label` | string | — | Label filter |
| `message_type` | string | — | Stored message type, for example `email`, `teams`, `discord`, `calendar_event`, or `sms` |
| `source_id` | int | — | Restrict to one source |
| `conversation_id` | int | — | Restrict to one conversation/thread |
| `after` / `before` | date | — | RFC3339 or `YYYY-MM-DD` bounds |
| `attachments_only` | bool | `false` | Only include messages with attachments |
| `hide_deleted` | bool | `false` | Exclude messages marked deleted at the source |
| `offset` | int | `0` | Zero-based row offset |
| `limit` | int | `500` | Maximum rows to return; capped at 500 outside conversation fetches |
| `sort` | enum | `date` | `date`, `size`, or `subject` |
| `direction` | enum | `desc` | `asc` or `desc` |

**Response:**

```json
{
  "count": 1,
  "has_more": false,
  "offset": 0,
  "limit": 500,
  "messages": [
    {
      "id": 12345,
      "subject": "Incident review",
      "message_type": "teams",
      "from": "alice@example.com",
      "to": [],
      "sent_at": "2026-07-01T15:30:00Z",
      "snippet": "Follow-up from the channel discussion...",
      "labels": [],
      "has_attachments": false,
      "size_bytes": 0
    }
  ]
}
```

The companion `GET /api/v1/messages/gmail-ids` endpoint returns source message IDs in `gmail_ids` and source metadata in `targets` for deletion staging. Without `max_results`, it selects Gmail and Microsoft Graph mail (`msmail`). The route and field names are shared by all supported providers.

| Parameter | Behavior |
| --- | --- |
| `q` | Search expression. Combine it with structured filters to narrow the selection. |
| `search_mode` | Required with `q`: `fast` searches metadata, `deep` includes bodies, and `aggregate` selects an aggregate row using `view_type` and `aggregate_key`. |
| `max_results` | Integer from 1 through 100001. Requires `q` and `search_mode=fast` or `deep`. Caps search matches before resolving deletion targets. Empty or invalid values return `invalid_max_results`. |

Bounded responses include `matched_count` before target lookup, so request one extra match to detect overflow; see [staged deletion through MCP](https://msgvault.io/docs/usage/chat/#staged-deletion-via-mcp) for supported sources and skipped matches.

The endpoint also accepts `sender` / `sender_name`, `recipient` / `recipient_name`, `domain`, `label`, `message_type`, `conversation_id`, `attachments_only`, `source_id`, `source_ids`, and `after` / `before`, applying them the same way `/messages/filter` does. Structured selections honor `limit`, while search selections use `max_results` as their bound. Results always exclude deleted messages and sort newest-first, so `hide_deleted`, `offset`, `sort`, and `direction` have no effect.

---

### Changed messages {#get-apiv1messageschanges}

**Endpoint:** `GET /api/v1/messages/changes`

Lists messages whose content changed at or after a cursor, oldest change first.
Poll it, re-read the rows it returns, and store the cursor it hands back. Unlike
`/messages/filter`, this feed orders rows by when a message *changed*, not by
when it was sent, so ten-year-old mail imported today shows up in the very next
page.

Pass `person_id` to select messages sent by a person's currently linked
identities, repeat `source_id` or `conversation_id` to select those sources or
conversations, and set `incoming_only=true` to exclude owner-sent messages on a
best-effort basis. Direction corrections do not move the change watermark, so
they can leave a missed or stale result. Different filters combine with AND,
while values within one list combine with OR, up to 50 positive IDs per list.
Owners can also call the MCP `list_changes` tool. Linking a new identity keeps
your cursor valid and applies to later changes; run a fresh walk to reconcile
its older history.

A filtered request scans at most 2,000 changes and can return an empty page with
`has_more=true`; keep following `next_cursor`. A first walk over 400,000
non-matching changes therefore takes about 200 requests. Once the available range
is exhausted, the cursor can advance through the empty tail to the commit bound.
A filtered cursor is bound to its selectors, so changing or removing them
returns `invalid_cursor`. Start over with an unfiltered cursor or the original
filter. Older servers reject filtered cursors because they do not understand the
format.

A filtered feed reports arrivals and in-scope edits. If a row leaves a filter's
scope, for example after a conversation move, sender re-attribution, or source
consolidation, that filter does not report it, so consumers caching rows must
reconcile them with a periodic unfiltered or wider-scope re-read. When a message
has no sender ID, the person filter uses its first `From` recipient; changes to
recipients alone do not move the watermark.

**What it is.** An invalidation feed over one projection of a message: the
columns this endpoint returns, plus body edits and two columns it does not
return. It tells you *that* a message changed and gives you the current value of
those columns. It does not tell you how many times it changed, what it held in
between, or whether anything outside that projection changed.

**What it is not.** It is not a general archive mirror. Labels, recipients,
attachment metadata and content, raw MIME, body deletions, participant edits,
and conversation metadata are all outside the feed, and several of those,
especially label re-syncs, are the most frequent changes in a mail archive. Hard
deletions are outside it too, because a row removed from the database leaves
nothing behind to report.

Use this feed to keep a projection of the tracked columns fresh or to react when
messages change. Do not build a full archive mirror on it unless you refresh the
untracked data on your own schedule and reconcile deletions independently. See
[What this feed does not report](#what-this-feed-does-not-report) and the
[delivery contract](#delivery-contract) for the exact boundaries.

The feed includes messages hidden by deduplication and messages deleted at the
source, with their `deleted_at` and `deleted_from_source_at` timestamps set. A
consumer caching the tracked columns needs both to learn when a message it holds
has been hidden or deleted.

This is the only message listing that returns dedup-hidden rows; every other
listing filters them out with `deleted_at IS NULL` and has no parameter to turn
that off. Source-deleted messages also appear in `/messages/filter` by default
(where `hide_deleted=true` suppresses them), but this feed returns them
unconditionally with no parameter to hide them.

The watermark moves for every mutable field the feed returns, for message-body
edits, and for two columns the feed omits: the sender pointer
(`messages.sender_id`) and the platform metadata payload. It never moves for
anything else; see
[What this feed does not report](#what-this-feed-does-not-report).

Part of what moves the watermark is readable only from `/api/v1/messages/{id}`,
so the feed *does* invalidate some fields of that response. When a message
arrives in the feed, re-fetch its detail response if you cache either of these:

* `body` and `body_html`. Adding or editing a body moves the watermark, whereas
  deleting a body does not, so a cached body can still go stale (listed in the
  exceptions below). Beeper repair can also update `body_html` and advance
  `content_changed_at`, causing the message to reappear in this feed.
* `from`. It is derived from the sender pointer, and changing that pointer moves
  the watermark. Renaming or correcting the participant it points to does not,
  so the address or display name can still change underneath you.

The feed never invalidates the rest of the detail response: `to`, `cc`, `bcc`,
`labels`, and the `attachments` array (filenames, hashes, sizes, and all) can
change without the message reappearing. Refresh those on your own schedule.

> **Existing archives are modified on first open, and the index build stops
> writes.** Adding feed support adds a `content_changed_at` column to
> `messages`, runs a one-time full-table backfill, builds an index on it, and
> installs watermark triggers. Trigger installation is a versioned migration, so
> it runs once per trigger definition rather than taking trigger locks on every
> open. On PostgreSQL, the backfill bumps `last_modified` on every row once at
> upgrade.
>
> The backfill commits in batches and installs the watermark triggers first, so
> concurrent writes stay safe throughout and, on PostgreSQL, unrelated rows can
> still be written between batches. The index build is the part that stops
> writes: it runs a plain `CREATE INDEX`, which blocks every write to `messages`
> on PostgreSQL for the length of the build and holds SQLite's single writer
> slot for the same stretch. Both directions block: an import already writing
> holds up startup, and an import that starts during the build waits for it. On
> a large archive, treat the first open as a planned write outage for the length
> of the index build (not the whole upgrade), and remember that the daemon does
> not serve requests until all steps finish.
>
> The upgrade is deliberately **not atomic**. The column addition, each
> committed backfill batch, the ledger entry recording the backfill as done, and
> the index are four separate durable steps. If one fails or you interrupt it,
> startup aborts, everything already committed stays, and the next open resumes
> where it left off instead of starting over.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `cursor` | string | — | The `next_cursor` of the previous response, sent back verbatim. Omit — or send it empty — to start from the beginning of the archive |
| `limit` | int | `100` | Maximum rows to return; capped at 500. Values below 1 fall back to the default |

> **Polling cost: the feed takes the SQLite write lock.** On SQLite, each
> request briefly acquires the single write lock to establish how far writes
> have committed, so unlike an ordinary read it competes with an active import.
> Measured on one machine against three concurrent writers, eight clients paging
> the feed in a tight loop cut writer throughput to **15%** of the unloaded
> rate, whereas eight clients running an equivalent plain `SELECT` left **49%**.
> One consumer polling once a second is free. The endpoint limits each client IP
> to two requests per second with a burst of four; honor a `429` response's
> `Retry-After` header. Poll on an interval, and drain a backlog with `has_more`
> or a larger `limit` instead of polling faster. PostgreSQL establishes the same
> bound without taking a lock.

**The cursor is opaque.** Store the `next_cursor` returned by a response and
send it back unchanged; do not parse, construct, compare, or order cursors. What
it encodes belongs to the server and may change without notice. Omit it, or send
it empty, to start from the beginning of the archive.

The token is not authenticated. The server holds no secret and does not sign it,
so it cannot distinguish a cursor it issued from a well-formed one you built
yourself, and it does not try. A fabricated cursor naming this archive is
accepted; it only moves your own position in your own feed and reaches no
message you could not already ask for. What the server *does* reject with
`400 invalid_cursor`, rather than silently restarting from the beginning, is a
token it cannot parse, a cursor format this build does not understand, or a
cursor issued against a **different archive**.

Plan for that last case. A cursor marks a position in one specific archive and
no other, so a stored cursor keeps working across a restart, an upgrade, or a
file-level restore of the same archive, but fails if you point the daemon at a
different `--db` or an archive rebuilt from scratch. Surviving a restore is not
the same as staying correct across one: a restore that rolls the archive *back*
past your cursor loses what it undid, as the
[delivery contract](#delivery-contract) explains. Rejection is the point: honoring
a cursor from another archive would resume at an unrelated position and silently
skip earlier records. Restart the sync from the beginning of the archive instead.

**Response:**

```json
{
  "messages": [
    {
      "id": 918,
      "source_id": 1,
      "source_message_id": "18f2c9d0a1b3",
      "conversation_id": 44,
      "message_type": "email",
      "subject": "Q4 planning",
      "snippet": "Here's the draft for Q4...",
      "sent_at": "2026-03-01T10:00:00Z",
      "size_estimate": 8412,
      "has_attachments": false,
      "attachment_count": 0,
      "content_changed_at": "2026-07-26T10:00:00.731123Z"
    }
  ],
  "count": 1,
  "has_more": true,
  "next_cursor": "1.eyJ0IjoiMjAyNi0wNy0yNlQxMDowMDowMC43MzExMjNaIiwiaSI6OTE4LCJhIjoiM2YwYjljMmU3ZDUxNDg2YWIwYzRlMjlmN2E2MWQ4NTM0YzllMGIxN2QyYTZmMzg1MTllNGM3YjBhMjVkNjMxOCJ9",
  "server_time": "2026-07-26T10:00:03.114500Z",
  "complete_through": "2026-07-26T10:00:03.114488Z"
}
```

Each row is a complete snapshot, never a patch, so an unset or empty field is
left out of the row entirely rather than sent as `null` or `""`. Read an absent
key as "empty" and overwrite whatever you had cached for it. Always present:
`id`, `source_id`, `conversation_id`, `size_estimate`, `has_attachments`,
`attachment_count`, `content_changed_at`. `messages` is always an array, never
`null`.

`server_time` is the database server's clock at the moment the page was read,
not the client's and not the daemon process's. `has_more` reports whether more
rows are already waiting; when it is `true`, request the next page immediately
instead of waiting for the next poll.

`next_cursor` marks where the next request resumes. It is always present and
never empty, so you always have a value to send back on an empty page or at the
very beginning of the archive.

`complete_through` reports how far the feed is caught up, which differs from the
current clock time. Every change committed strictly before it, at or after the
cursor you sent, is now *reachable* in this page or in a page you can fetch
right away, barring the exceptions in the
[delivery contract](#delivery-contract). It is **not a cursor**: `next_cursor`
is the only cursor, and resuming from `complete_through` while `has_more` is
`true` skips every row in between.

`complete_through` is never after `server_time`, and on a healthy server it
trails `server_time` by microseconds; see [Delivery contract](#delivery-contract)
for when it stops tracking and what that means. A page never reaches
`complete_through` because rows stamped at that exact instant are held back, so
the newest changes arrive on the next poll.

On a freshly started server, `complete_through` can be `null` because no bound
has been established yet. That happens when a writer beats every attempt to read
the bound, and it resolves on the first quiet moment. The page carries no rows
and echoes your cursor unchanged, so it is safe to keep polling; once the bound
is established, the feed delivers the archive from your cursor onward.

On an empty page, the response normally echoes your cursor. The exception is a
cursor standing *above* the database clock: it matches nothing, and echoing it
would leave you polling a feed that reports "caught up" forever while the
archive changes, so the response moves the cursor down to the start of
`complete_through` instead. Using the start guarantees that a row stamped at that
exact instant stays ahead of the cursor regardless of its message ID. It lands
on the commit bound rather than on the clock because a change that is stamped
but not yet committed sits between the two, and a cursor at the clock would step
over it. If the server has not yet established a bound, it has no proven-safe
point to move to and echoes your cursor unchanged.

You can hit this state without doing anything wrong, such as after an NTP
correction, a resumed VM, or a restore onto slower hardware. Moving the cursor
gets you going again, though it does not undo whatever put you ahead of the
clock. If the clock stepped backward, changes already stamped below the new
position are gone from your walk; see the
[delivery contract](#delivery-contract).

#### Walking the feed

First request (no cursor, so the feed starts at the beginning of the archive):

```bash
curl -H "X-API-Key: $MSGVAULT_API_KEY" \
  "http://localhost:8080/api/v1/messages/changes?limit=100"
```

```json
{
  "messages": ["... 100 messages ..."],
  "count": 100,
  "has_more": true,
  "next_cursor": "1.eyJ0IjoiMjAyNi0wNy0yMFQxODowNDoxMS45MDIzMTdaIiwiaSI6NDQ3MSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ",
  "server_time": "2026-07-26T10:00:03.114500Z",
  "complete_through": "2026-07-26T10:00:03.114488Z"
}
```

`has_more` is `true`, so send the next request immediately. It comes from a
one-row lookahead: the server asked for one row more than your `limit` and got
it, so a further row was already there to be read. Expect the next page to carry
rows.

Second request (using the cursor from the first response, verbatim):

```bash
curl -H "X-API-Key: $MSGVAULT_API_KEY" \
  "http://localhost:8080/api/v1/messages/changes?cursor=1.eyJ0IjoiMjAyNi0wNy0yMFQxODowNDoxMS45MDIzMTdaIiwiaSI6NDQ3MSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ&limit=100"
```

```json
{
  "messages": ["... 12 messages ..."],
  "count": 12,
  "has_more": false,
  "next_cursor": "1.eyJ0IjoiMjAyNi0wNy0yNFQwOToxMjo1Ny40MTgwMDRaIiwiaSI6NTEwOSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ",
  "server_time": "2026-07-26T10:00:04.550118Z",
  "complete_through": "2026-07-26T10:00:04.550102Z"
}
```

Third request (`has_more` was `false`, so wait for your poll interval). Nothing
changed in between, so the page is empty and the cursor comes back unchanged:

```bash
curl -H "X-API-Key: $MSGVAULT_API_KEY" \
  "http://localhost:8080/api/v1/messages/changes?cursor=1.eyJ0IjoiMjAyNi0wNy0yNFQwOToxMjo1Ny40MTgwMDRaIiwiaSI6NTEwOSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ&limit=100"
```

```json
{
  "messages": [],
  "count": 0,
  "has_more": false,
  "next_cursor": "1.eyJ0IjoiMjAyNi0wNy0yNFQwOToxMjo1Ny40MTgwMDRaIiwiaSI6NTEwOSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ",
  "server_time": "2026-07-26T10:00:34.771205Z",
  "complete_through": "2026-07-26T10:00:34.771190Z"
}
```

The loop is straightforward: re-read the rows, save `next_cursor`, send it on
the next request, and repeat immediately while `has_more` is `true` or on your
poll interval when it is `false`. An empty page echoes the cursor you sent, so a
caught-up consumer can send the returned cursor back indefinitely without
re-reading the archive.

A page with `has_more: true` followed by an empty page is unusual: the lookahead
row existed when the first page ran, so something happened to it afterward. It
was either hard-deleted, or its watermark moved forward to or above the next
page's [commit bound](#delivery-contract) (holding it back until the bound
catches up), or moved backward below your cursor (the clock-step case in the
delivery contract). Only the backward step loses a change you would otherwise
have received, since a hard deletion was never reportable, and none of the three
is a reason to stop polling.

#### Delivery contract {#delivery-contract}

**What the feed guarantees.** After a change to a tracked column written through
the application with the watermark left to the database triggers, following
`next_cursor` delivers the message across as many pages as `has_more` calls for,
regardless of how long its writing transaction took. Each row carries the
message's current values at the moment the page was read.

**What kind of feed that makes it.** This is a latest-state invalidation feed,
not an event stream. The archive stores one mutable watermark per message and
keeps no change history, so a page tells you "this message is stale, re-read it"
rather than how many times it changed or what it held in between. Two
consequences follow, and neither is a defect to work around:

* Multiple changes to one message between two polls arrive as **one** row
  carrying the latest state. Intermediate states are gone from the archive, so
  no cursor or re-read can recover them.
* The same change can arrive **more than once**. Resuming from an earlier cursor
  re-delivers rows rather than erroring or skipping, and nothing distinguishes a
  re-delivery from a fresh change.

Build your consumer to re-read messages it already holds, and do not count feed
rows as distinct changes or drive logic from their count.

**Every exception to that guarantee is in "What is still best-effort" below.**
That list is complete and is the only place that enumerates them; every other
passage in this document and in the source points there instead of restating it.

`complete_through` makes the guarantee hold: each page stops below the oldest
write that could still commit rather than below the clock, so for every write
the bound can see, the cursor cannot come to rest above a stamped but
uncommitted change. The guarantee applies to `next_cursor`, not to any single
response. Keep following `next_cursor` and nothing outside the exception list is
lost; substituting `complete_through` skips the rows between it and
`next_cursor`.

**What it costs.** The feed cannot advance past the start of any open
transaction that holds or is queued for a write lock on the message table. A
batch import, a source-deletion run, or a client that writes a message and sits
on its `BEGIN` freezes `complete_through` until it ends. Only transactions
touching the message table count, and only while they are open: long reads, idle
connections, writes to other tables, and routine autovacuum work never hold the
feed back, regardless of database role.

On PostgreSQL, the check tests the lock rather than the write, by design. A
transaction still *waiting* for a write lock on `messages`, such as behind an
`ALTER TABLE`, counts from the moment it queues and holds the bound back to its
own transaction start rather than when it began waiting. Waiting to write
immediately precedes writing, and the bound must sit below a write before it
happens rather than after; counting a transaction that never writes creates a
visible gap, while missing one that does write silently skips a row.

On SQLite, where you cannot inspect which transaction is open, any write
transaction held longer than a moment has the same effect.

During a freeze, the feed keeps serving: a consumer with a backlog keeps
draining every row committed below the frozen bound, page after page, as
usual. The freeze affects the far end of the walk. Once your cursor reaches the
bound, the feed returns no rows and `has_more: false`, which looks like being
caught up. The difference is that `complete_through` stops tracking
`server_time` while `server_time` keeps moving. **That gap is the signal.** If
it grows past a few seconds, something is holding a write transaction open. Past
a minute, the server logs a warning (`message change feed is not advancing`,
with the lag and a cause) at most once a minute while the condition lasts. A
normal batch write opens a gap while the batch runs and closes it afterward;
that is normal operation, not a fault.

One case has no gap to measure and no minute to wait: a server that has never
established a bound because a write transaction was open on every attempt since
startup. `complete_through` is `null` then, so there is no lag to report, and
the server logs the same warning on the first request under the same
once-a-minute throttle, with `lag: "unknown"`, `complete_through: "none"`, and
its own cause. Read it as "this server has never seen the message table quiet",
which on a freshly started daemon usually means an import was already running at
startup.

Read the gap as "how stale the bound is", not "how long that transaction has
been open". Those two numbers match only on PostgreSQL, where the bound is the
open transaction's start time. SQLite cannot ask when another connection's
transaction began; its bound is the last moment the server saw the write lock
free, so the gap measures the age of that observation instead. A writer is in
flight whenever the gap is open, but on SQLite it may have started seconds ago
and still show a gap of hours because no poll took a reading in between. On
SQLite the gap is an upper bound on the writer's age; on PostgreSQL it measures
that age directly.

**What is still best-effort.** This is the canonical list of exceptions to the
guarantee above; no other passage in this document or in the source enumerates
them. The feed does not promise once-and-only-once delivery or prompt delivery,
and some changes are outside what it can observe:

* Rows may be delivered more than once. Resuming from an earlier cursor
  re-delivers rather than erroring or skipping.
* Changes to one message coalesce. Multiple changes between two polls arrive as
  a single row carrying the latest state, because the archive holds one mutable
  watermark per message and no change history. Nothing on the wire says how many
  changes a row represents, and intermediate states are not stored, so no cursor
  recovers them. If you need every individual change, this feed cannot provide
  them.
* Hard deletions are never reported. A row removed from the database outright
  leaves nothing behind to report.
* Changes outside the tracked columns are not reported; see
  [What this feed does not report](#what-this-feed-does-not-report).
* **PostgreSQL only:** PostgreSQL hides other roles' connections from a role that
  is neither a superuser nor a member of `pg_read_all_stats`, and a writer the
  server cannot see cannot hold the bound back. Rather than quietly losing rows,
  the feed stops advancing while a hidden connection is *writing to the message
  table*: `complete_through` freezes at the last reading taken while every such
  writer was visible, so this shows up as a stalled feed rather than missing
  changes. A hidden connection that is idle, reading, or writing another table
  changes nothing. msgvault uses one role, so this happens only if something else
  writes the same message table; if it does and the feed stalls, grant the
  msgvault role `pg_read_all_stats`. When there is nothing to fall back to, such
  as a server that started while a hidden writer was already inside its
  transaction, the endpoint returns `500` instead of a page and the server log
  names the grant. It clears on its own when that transaction ends.
* **PostgreSQL only:** A *prepared* transaction (two-phase commit) holds its
  locks without an owning session, so the feed cannot see when it began. One
  that wrote to the message table and then committed could publish its change
  behind a cursor that already moved past it. Prepared transactions require
  `max_prepared_transactions > 0`, which is off by default, and msgvault never
  uses two-phase commit. If another application runs prepared transactions
  against the same database, reconcile independently rather than relying on the
  feed.
* **A stamp at or above the current bound waits for the bound to reach it.** The
  feed orders by the watermark stored on the row rather than when the write
  committed, and each page stops strictly below `complete_through`. A row
  stamped at or above the bound is held back, and when it is the only row
  outstanding, the feed reports `has_more: false`, which looks like being caught
  up. **That is a delay, not a loss:** no cursor returned by this page moves
  past the waiting stamp, so the row arrives on the first page read after the
  bound clears it. A moved cursor returned by an empty page stands exactly *on*
  the bound, which is still safe because it names the *start* of that instant
  rather than a position after a row in it, so every row stamped there remains
  ahead of it regardless of message ID. A watermark written by hand for a future
  instant waits until that instant arrives; a server with no bound yet is this
  case at its limit, selecting nothing until the first bound reading. A clock
  stepped backward also leaves your cursor above the clock and looks identical,
  but what it strands is stamped *below* your cursor, where no bound ever brings
  it back; that is the next bullet, and it is a loss rather than a wait. On
  SQLite, the column can also hold a value no bound ever reaches, where the wait
  never ends and the change is lost (case 2 below).
* **A database clock that steps backward loses changes committed below your
  cursor while it climbs back.** The feed orders by the wall-clock watermark on
  the row and your cursor only moves forward, so both rely on a monotonic
  database clock. When the clock steps backward, such as from an NTP step, a
  resumed or migrated VM, or a restore onto a host whose clock is behind, writes
  after the step get stamped in clock time your walk already passed. Every row
  stamped below your cursor fails the keyset comparison on that poll and every
  poll after it. Both backends stamp from the database server's wall clock, so
  both are affected. **This is a loss, not a delay.** The row is not waiting for
  anything, and nothing on the wire distinguishes it from a healthy feed. The
  loss is bounded by the size of the step: you lose changes committed during the
  window of clock time the step replays, and normal delivery resumes once the
  clock passes your cursor again. Moving a cursor that stands above the clock
  narrows the window but cannot close it, and it does not occur if your consumer
  polls late enough for the clock to climb back above the cursor. Making the
  cursor opaque changes none of this. No server-issued cursor reaches back below
  itself; a full re-read from an empty cursor does because these rows stay
  selectable (see the reconciliation note at the end of this list).
* **Restoring the archive from an older snapshot loses everything the rollback
  undid.** A cursor names the archive that issued it, and a file-level restore
  of the same archive keeps that identity on purpose so stored cursors survive
  ordinary restores. However, a cursor issued *after* the snapshot was taken
  still validates against the restored database, and everything the rollback
  undid now sits below it: a row reverted to an older watermark fails the keyset
  comparison on every poll, and a row removed by the rollback produces no event
  at all. Your mirror keeps holding content the archive no longer has. **This is
  a loss, not a delay**, and nothing on the wire distinguishes it from a healthy
  feed. It has the same shape and cause as the backward clock step above: the
  walk orders by a wall-clock watermark with no monotonic sequence behind it.
  Copying the archive **file** (with `cp`, a filesystem snapshot, or a backup
  restored under another path) forks it the same way and keeps the original's
  identity, so a cursor from either copy is accepted by the other even as they
  drift apart. A subset built with `msgvault create-subset` does not share that
  identity: it is a new database with its own identity, so a cursor from the
  original returns `400 invalid_cursor` and a consumer of the subset starts from
  an empty cursor. To recover after a snapshot restore, run a full re-read from
  an empty cursor to restore the tracked fields of every surviving row, though a
  row removed by the rollback looks the same as a hard deletion (see the
  reconciliation note below). **After restoring an archive from a snapshot,
  restart your consumers from an empty cursor.**
* **A `NULL` watermark is invisible to the feed on either backend.** Each page
  compares `content_changed_at` against both bounds and `NULL` satisfies
  neither, so no cursor returns the row. Normal write paths never produce a
  `NULL` watermark because every writer stamps the column and the first-run
  backfill populates older databases, so this takes direct SQL, and any content
  change in the same statement goes unreported. The next ordinary change to a
  tracked column stamps a fresh watermark and brings the row back into the feed
  with its current content; without one, it stays invisible.
* **SQLite only:** Direct SQL can store a `NULL` or malformed
  `content_changed_at`; normal write paths do not. Rather than inventing a
  timestamp and moving the cursor, the feed returns an error for a `NULL` or
  malformed watermark selected by a page. Because SQLite orders non-date values
  by raw type and text, a corrupt value outside the requested range can stay
  unreachable. Repair the stored watermark directly before resuming.
* **Reconcile periodically if your consumer must not diverge.** Run a scheduled
  full re-read from an empty cursor to re-deliver every row the feed can select.
  That restores tracked fields for any selectable malformed row and any row
  stranded by a backward clock step, though it cannot reach a malformed
  watermark that sorts outside the feed range and recovers nothing outside the
  tracked columns. It also cannot distinguish a hard deletion from an
  out-of-range corrupt watermark: both look like a row your mirror holds that
  the walk never returns. To tell them apart, run a direct `SELECT` over
  `messages`: an absent row was hard-deleted, while a present row has a
  watermark the feed cannot reach.

Re-reading an overlapping window is safe, so if you want extra margin, keep an
older `next_cursor` of your own and resume from it, trading duplicates for
margin. Always build that margin from a cursor the feed returned, never from
`complete_through`.

#### What this feed does not report {#what-this-feed-does-not-report}

Triggers on the `messages` table and on `message_bodies` maintain the watermark.
The `messages` trigger covers every column the feed returns plus the sender
pointer and the platform metadata payload, which the feed omits. Everything else
on a message, including the remaining columns of `messages`, stays outside the
feed:

| Surface | Where it lives | Does changing it move the message into the feed? |
|---|---|---|
| Labels | `message_labels` | No — and label re-sync is the most frequent change in a mail archive |
| Recipients (to/cc/bcc) | `message_recipients` | No |
| Attachment metadata (filenames, hashes, sizes, storage paths) | `attachments` | Changes appear when they change the message's `has_attachments` or `attachment_count`, or a native `teams:recording:` pointer's ID or storage URL. Other attachment metadata changes don't move the watermark. |
| Raw MIME | `message_raw` | No — including raw MIME added after the message row |
| Read state and platform flags (`is_read`, `read_at`, `is_edited`, `archived_at`) | `messages` | No |
| Threading and identity pointers (`reply_to_message_id`, `rfc822_message_id`) | `messages` | No |
| Conversation metadata (thread title) | `conversations` | No |
| Message body | `message_bodies` | Yes for an added or edited body (see the deletion row below), but the feed reports only `snippet`; fetch the body from `/api/v1/messages/{id}` |
| Message body deletion | `message_bodies` | No — the body triggers fire on INSERT and UPDATE only. Adding or editing a body moves the watermark; deleting one does not |
| Sender *pointer* (`messages.sender_id`) | `messages` | Yes — but the feed does not return it, and only the pointer is tracked. Renaming or correcting the participant it points at changes `participants`, not `sender_id`, so it does **not** move the watermark |
| Platform metadata payload (`metadata`) | `messages` | Yes — but no endpoint returns it, so the wake-up is all you get. It is tracked so that a metadata change still invalidates your cached copy of the fields that *are* returned |

If you cache any untracked fields, refresh them on your own schedule; nothing in
this feed invalidates them.

#### Errors

| Status | `error` | Meaning |
|---|---|---|
| 400 | `invalid_cursor`, `invalid_limit` | A parameter is present but could not be used. A cursor this API cannot use — damaged in transit, left over from an incompatible server version, or issued against a different archive — is rejected rather than read as the beginning of the archive; the message says which of those it was, and in each case the only repair is to restart the sync from the beginning. A hand-built cursor is *not* rejected on those grounds alone: the token is unauthenticated, so a well-formed one naming this archive is honoured (see [the cursor is opaque](#get-apiv1messageschanges)). An *empty* value (`?cursor=`) is read as absent, exactly like omitting the parameter, so `?cursor=&limit=10` starts from the beginning of the archive |
| 401 | `unauthorized` | No API key, or one this server rejects. Every request to this endpoint is authenticated the same way the rest of the API is (see [Authentication](#authentication)) |
| 429 | `rate_limit_exceeded` | The caller has outrun the [rate limit](#rate-limiting); `Retry-After` says how long to wait. Worth handling here more than anywhere else, because polling is what this endpoint is for. Polling faster does not make the feed advance sooner — `complete_through` moves with the database, not with your poll rate — and on SQLite it costs the importer write throughput |
| 500 | `internal_error` | The watermark query failed. **PostgreSQL only:** this is also how the no-visibility-floor case above surfaces — a server that has never once read the bound while every writer of the message table was visible has no safe bound to report, so the endpoint refuses rather than returning a page that would step your cursor over an invisible writer's change. The server log names the `pg_read_all_stats` grant that fixes it; it also clears by itself when the foreign transaction ends |
| 503 | `feature_unavailable`, `query_timeout` | `feature_unavailable`: the configured store cannot answer the watermark query, or cannot say which archive it is, so it cannot issue a cursor you could safely resume from. `query_timeout`: the request outran the server's per-request time limit. Both are retryable from the same cursor. (A request the caller abandoned is answered `query_canceled` on the same status, which by definition nobody is left to read.) |
| 503 | `analytical_cache_unavailable` | The committed analytics cache is unavailable, for example while `remove-account` or a repair command rebuilds it under `engine = "duckdb"`. Retry the request. |

---

### Message details {#get-apiv1messagesid}

**Endpoint:** `GET /api/v1/messages/{id}`

Full message details including body and attachment metadata.

**Response:**

```json
{
  "id": 12345,
  "subject": "Q4 Planning",
  "message_type": "email",
  "from": "alice@example.com",
  "to": ["bob@example.com"],
  "cc": ["carol@example.com"],
  "bcc": ["dave@example.com"],
  "sent_at": "2024-10-15T09:30:00Z",
  "snippet": "Here's the draft for Q4...",
  "labels": ["INBOX", "IMPORTANT"],
  "has_attachments": true,
  "size_bytes": 52480,
  "body": "<plain text body, or HTML when no plain text body exists>",
  "body_text": "<stored plain-text body, or an empty string when no plain text is stored>",
  "body_html": "<html><body><p>Full HTML body</p></body></html>",
  "attachments": [
    {
      "id": 987,
      "filename": "q4-plan.pdf",
      "mime_type": "application/pdf",
      "size_bytes": 204800,
      "content_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    }
  ]
}
```

The response includes `cc`, `bcc`, and `body_html` only when present. `body`
returns the stored plain-text body if one exists, and falls back to the HTML
body otherwise so you still get message content.

API schema 3.41.0 adds `body_text` for a single message's stored plain-text
body, which can be generated from the HTML part and is empty when no plain text
is stored; conversation windows leave it unset. Older servers omit `body_text`,
while `body` keeps its HTML fallback.

Schema 3.42.0 adds an optional `sender_auth` object to message details and
`GET /api/v1/cli/message` for email sources with
[receiver pins](configuration.md#sender-authentication). It carries `verified`,
`domain`, `authserv_id`, `spf`, `dkim`, and `dmarc`, with only `verified`
always present and empty results setting `verified: false`. Verification covers
the `From` domain as reported by the pinned receiving server. List and search
responses omit this field.

---

### Related copies {#get-apiv1messagesidfamily}

**Endpoint:** `GET /api/v1/messages/{id}/family`

Returns the other archived copies of an email along with the replies and
forwards that reference it, across accounts and conversations. Only owner
credentials can call this route; delegated agent tokens are rejected, and
responses are never cached.

**Response:**

```json
{
  "anchor_message_id": 12345,
  "truncated": false,
  "pending_linkage": false,
  "members": [
    {
      "message_id": 12345,
      "source_id": 1,
      "source_type": "imap",
      "source_identifier": "work@example.com",
      "account_address": "work@example.com",
      "account_path": "inbound",
      "draft": false,
      "conversation_id": 77,
      "subject": "Q4 Planning",
      "sent_at": "2024-10-15T09:30:00Z",
      "linkage_outcome": "complete",
      "evidence": "anchor"
    }
  ]
}
```

`members` includes the opened email itself, ordered by ascending `message_id`.
`evidence` states how each member relates to it: `anchor`, `copy` (same
Message-ID), `reply`, `replied_to`, `forward`, `forwarded_original`,
`references`, `indirect` (found through another member), or `thread` (in the
provider's thread for the opened email or one of its exact copies, with no
header link). `account_path` is `inbound` or `sent`, and `draft` is `true`
while the copy is a draft. `linkage_outcome` is `complete`, `pending` before
the copy's identifiers are derived, `missing_message_id` when the copy was
indexed without a stored Message-ID for replies to match, or the reason its
identifiers could not be read; anything other than `complete` may hide
relations.

The lookup follows `Message-ID`, `In-Reply-To`, `References`, and
attached-original identifiers, plus for Gmail and Google Groups the provider
threads of the email and its exact copies. It stops at 500 copies or 5,000
identifiers and sets `truncated` alongside `truncated_reason` (`members` or
`identifiers`), though the identifier limit can trigger with far fewer than 500
copies. When more than 500 copies match, exact Message-ID copies stay first,
followed by direct replies and forwards, then shared references.
`pending_linkage` is `true` while any email in the archive still lacks derived
identifiers; because it could be a related copy the lookup cannot see, an empty
or short list is not complete. Deleted and non-email messages are omitted, and
a missing message returns 404.

---

### Attachment metadata {#get-apiv1attachmentsid}

**Endpoint:** `GET /api/v1/attachments/{id}`

Returns metadata for the numeric attachment ID exposed by message details,
including the SHA-256 `content_hash` you pass to the content endpoint:

```json
{
  "id": 987,
  "filename": "q4-plan.pdf",
  "mime_type": "application/pdf",
  "size_bytes": 204800,
  "content_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
```

---

### Docbank attachment mirror

Call `GET /api/v1/integrations/docbank/attachments` to inspect mirror status.
It returns `state`, `upload_consent`, `folder`, and `counts` (`delivered`,
`sending`, `remaining`, `failed`, and `reasons`). The `delivered`, `sending`,
and `failed` counts measure unique files per destination, while `remaining`
counts distinct eligible files that have no delivery record for that
destination. Eligible files have stored local bytes, including files kept after
source deletion and deduplication, and exclude files whose only messages were
deleted in msgvault. `state` is `running`, `docbank_disabled`, `consent_off`,
or `worker_unavailable`; off and unavailable workers omit `counts`, and an
unavailable mirror reader returns 503. File search and list rows omit receipts,
whereas `GET /api/v1/files/{id}` includes `docbank` receipts for the file's
content hash, including recovered local alias hashes.

When `document_profile` is configured and the worker is running, the status
response adds `processing` with `profile`, `waiting`, `requested`, `extracted`,
and `outcomes` counts keyed by Docbank failure or refusal code. Counts reflect
the profile fingerprint last read by the worker, and `waiting` counts files
covered by attachment search. For a running worker, `last_error` shows its most
recent blocking error, including missing consent or a missing profile. Because
status reads saved job outcomes from the archive, it makes no live Docbank
request.

Call `POST /api/v1/integrations/docbank/attachments/backfill` to reset
discovery and requeue failed deliveries as sending intents with attempts reset
in a single transaction. Backfill requests processing again for files that
failed due to missing consent, stale authority, a missing PDF password, an
unreadable password file, a quarantined job, or a malformed Docbank answer. It
clears only settled recoverable failures; a job that fails after backfill needs
another backfill, and other outcomes wait for a profile change. The endpoint
returns 202 with `{"status":"scheduled"}` even when the job is already running.
It requires attachment upload consent (returning 409 without it) and a
scheduled mirror job (returning 503 if the store or scheduler is unavailable).
Both routes require the API key.

Attachment metadata and message details include an optional `docbank` array of
receipts with `endpoint`, `folder`, `node_id`, `version_id`, and `blob_hash`,
shared across duplicate stored content hashes. For hashless Slack or Discord
rows, discovery and details recover the hash from a local storage path in
`<hash[:2]>/<hash>` format with lowercase SHA-256 hex and a matching prefix.
The mirror uploads these aliases from loose files or retained pack entries,
verifies their SHA-256, and names them `<first 12 hex of hash>-<sanitized basename>`.
See [configuration](configuration.md#mirror-stored-attachments-to-docbank) for
consent and failure recovery.

---

### Open loops

These owner-only routes back `msgvault loops`, the Open loops section in
People Details, and the MCP loop tools. See [Open loops](usage/open-loops.md)
for what counts as a loop.

`GET /api/v1/loops` lists loops, oldest first. Optional query parameters are
`person_id` and `limit` (1 to 200, default 50). The response has these fields:

| Field | Meaning |
|---|---|
| `items` | The loops. Each has `kind` (`reply_owed` or `awaiting_reply`), `anchor_message_id` (the message that started the wait), `question_message_ids` (the questions it covers, oldest first), `conversation_id`, `source_id`, `channel` (`email` or `chat`), `counterparty` (`participant_id`, optional `person_id`, `display_name`, `address`), `summary`, `owed_since_at`, and `last_activity_at`. |
| `truncated` | More loops are open than `limit` returned. |
| `coverage_incomplete` | The request stopped before checking all history, because it read 1,000 new messages or ran out of time. Items come only from conversations checked in full. Call again to continue; saved results carry over. |
| `skipped_unparseable` | Stored emails that couldn't be parsed. A reply in one of them may have answered a listed loop. |

A list doesn't wait for backups or syncs. While one is running, the list still
answers, but saves nothing new for the next request. A smaller `limit` doesn't
shorten the scan. A bad parameter returns 400.

`POST /api/v1/loops/{message_id}/done` marks a loop done.
`POST /api/v1/loops/{message_id}/snooze` with `{"until": "<RFC3339 time>"}`
hides it until that time. Use the item's `anchor_message_id` as `message_id`.
To mark only the questions you listed, pass `question_message_ids` as a query
parameter, repeated or comma-separated. It must include `message_id`. Without
it, the decision covers every question open now. Both routes re-read the
conversation first and return `anchor_message_id`, `state` (`done` or
`snoozed`), and `snoozed_until`.

`POST /api/v1/loops/{message_id}/reopen` clears the decision on that one
question and returns `state: "open"`.

| Status | Error | When |
|---|---|---|
| 400 | `invalid_request` | A bad ID or parameter, a snooze time in the past, or `question_message_ids` that leaves out `message_id`. |
| 404 | `loop_not_found` | The message doesn't start an open loop now. It was answered, it is still inside its 24- or 72-hour wait, or a listed question is no longer open. |
| 404 | `loop_decision_not_found` | Reopen found no decision on that question. |
| 503 | `loop_scan_incomplete` | Re-reading the conversation reached the 1,000-message limit. Nothing was saved. Retry after `Retry-After` (1 second). |
| 503 | `operation_in_progress` | Other archive work holds the daemon. Retry later. |

Responses are not cacheable. Delegated agent tokens receive 401, and remote
clients receive 403.

---

### Attachment content by hash {#get-apiv1attachmentshashcontent}

**Endpoint:** `GET /api/v1/attachments/{hash}/content`

Streams the raw bytes of an archived attachment from either loose or packed
storage. `{hash}` must be the 64-character SHA-256 `content_hash` returned by
message details or the attachment metadata endpoint.

```bash
curl -H "X-API-Key: $MSGVAULT_API_KEY" \
  --output attachment.bin \
  "http://localhost:8080/api/v1/attachments/$HASH/content"
```

Successful responses set the archived MIME type as `Content-Type` (falling
back to `application/octet-stream`), the archived filename in
`Content-Disposition`, `Content-Length`, and `X-Content-Type-Options: nosniff`.
An invalid hash returns `400`; a hash with no attachment row, or content that
is no longer available, returns `404`. The generated Go client's
`GetAttachmentContent` wrapper verifies the returned bytes against the
requested hash before returning them.

---

### Inline image content {#get-apiv1messagesidinlinecidcontent-id}

**Endpoint:** `GET /api/v1/messages/{id}/inline?cid=<content-id>`

Fetch an inline MIME image part by content ID. This is intended for rendering `cid:` images referenced by `body_html`.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `cid` | string | (required) | MIME `Content-ID` to fetch |

Only inline image parts are served. SVG images and non-image inline parts are rejected with `415 unsupported_type`. If the query engine cannot fetch raw MIME, the endpoint returns `501 not_supported`.

Successful responses set:

| Header | Description |
|---|---|
| `Content-Type` | Inline image content type |
| `Content-Disposition` | `inline` |
| `Cache-Control` | `private, max-age=31536000, immutable` |
| `X-Content-Type-Options` | `nosniff` |

---

### Media transcript search

Call `GET /api/v1/media/search/status` to check whether Docbank transcript
search is configured. It returns a boolean `configured` field without
contacting Docbank, reporting `false` when the recording reader or its Docbank
client is absent.

Call `GET /api/v1/media/search?q=quarterly%20numbers&mode=lexical&limit=20` to
search spoken words and get every matching live message occurrence. Each result
includes message, conversation, and attachment IDs, optional `filename`,
`containing_title`, and `occurred_at`, a supplied or generated origin, an
excerpt, and timing when Docbank recorded it. Pass `person_id` to scope to
durable person bindings or `participant_id` to match the identity cluster used
by Explore's **Person** filter; you cannot combine both. Repeated `direction`
values filter by `from_person`, `to_person`, or `group` and require one of
those IDs.

Recording search covers the full allowed population up to 4,096 distinct
versions and source selectors. If your scope exceeds that ceiling, the endpoint
returns `media_search_scope_limit` before contacting Docbank; narrow it with
`person_id` or `participant_id`. A recording with more than 64 distinct current
supplied transcript input IDs is omitted from search, with its message
occurrences counted in `unavailable_occurrences` and `partial=true` (repeated
occurrences of the same input count once). An unconfigured or unreachable
integration returns `media_search_unavailable`.

The response includes the recording `actual_mode` and `degradations`, Docbank
`coverage`, local `pending_occurrences` and `unavailable_occurrences`,
`attribution_unavailable`, `partial`, and `truncated`. Pending counts require
durable worker work, while unmapped audio counts as unavailable. A coverage
state of `unknown` stays `unknown`, and `partial` also reflects local gaps and
withheld excerpts. An empty result with complete coverage and `partial=false`
means the query found no transcript match. The output limit applies after
shared recordings expand into messages, and `truncated` indicates remaining
occurrences or Docbank's retrieval limit.

Selected transcript completeness must be `complete`, `partial`, or
`degraded_provenance`. The latter two set `partial=true`, while unsupported
values report `media_search_unavailable` for recordings.

Recording search requires Docbank's source-selected search contract:
`media_sources` selectors, a `media_source_selection` report marker, and
query-independent `media_selections` (including empty results). The daemon
sends each source's current supplied-input set, while generated transcripts
stay eligible when supplied work is queued. Docbank picks each source's current
transcript before ranking and returns build and exact source associations with
each plain excerpt; source selections carry origin and supplied-input identity.
Pending or missing transcripts leave coverage partial while ready matches stay
searchable. Older servers and remote lookup failures report
`media_search_unavailable` for recordings. Supplied text requires
`supplied_input_id` to match the occurrence's saved delivery; mismatches count
as unavailable attribution even when the query has no hits. The daemon
validates supplied transcript revisions before search and rechecks visibility,
recording identity, supplied text, and person scope afterward.

Schema 3.27.0 adds an optional `attachments` object with `results`,
`truncated`, and an optional `error` (`error` and `message`). Each result
contains `message_id`, `conversation_id`, `attachment_id`, `filename`,
`containing_title`, optional `occurred_at`, and a plain `excerpt`. It searches
delivered mirror versions using text extracted by Docbank, batching more than
4,096 files in groups of 4,096 and interleaving results by position within each
batch so ordering across batches is approximate. A Docbank result limit or
occurrence limit sets `truncated`, meaning more matches may exist, and Docbank
may include filename-only candidates in its limit even when all extracted-text
matches fit. Visibility and **Person** scope apply to both lists, while `limit`
applies independently to each list. An empty attachment list with no error
means no match in extracted text and says nothing about files whose text
Docbank has not extracted.

Schema 4.2.0 accepts `mode=semantic` and `mode=hybrid` for attachment text and
adds `requested_mode`, `actual_mode`, `coverage`, `partial`, and `degradations`
to `attachments`. `actual_mode` is `unavailable` when no search ran or remote
retrieval failed. `coverage` describes mirrored versions delivered to Docbank
and appears even without hits. The configured `attachment_search_profile` names
an existing Docbank processing profile for semantic and hybrid search, while
lexical search keeps the built-in `supplied-transcript` profile; retrieval
never schedules processing. Without `attachment_search_profile`, hybrid falls
back to lexical with `semantic_unconfigured` and semantic fails with
`media_search_mode_unavailable`. Fenced searches submit configured profiles and
report configuration, consent, and runtime failures, while empty scopes contact
no service. Docbank-reported hybrid downgrades are preserved, and downgrades,
degradations, or nonlexical coverage other than `complete` set attachment
`partial=true` (lexical `unknown` coverage alone does not). Each list reports
its own `partial` status, separate from `truncated`, and files are searched in
batches of 4,096.

Schema 5.15.0 applies `mode` to recordings as well. Semantic and hybrid
recording search use `integrations.docbank.recording_search_profile`, naming
the Docbank profile that embedded the transcripts. Docbank keys embeddings by
processing profile, and transcripts run under `supplied-transcript` or the ASR
profile rather than the attachment profile. Without a recording profile, hybrid
uses lexical with `semantic_unconfigured` and semantic sets
`recordings_error=media_search_mode_unavailable`. When the selected transcripts
have no embeddings, Docbank answers with lexical matches and
`semantic_unavailable`. Recording `actual_mode` is `unavailable` when no
recording search ran, such as with an empty scope. Downgrades and degradations
set `partial=true`. An empty recording scope still validates the query
lexically, so the query text reaches no encoder.

When attachment mirroring is off, `attachments` is omitted. Attachment failures
leave recording results intact and set `attachments.error`. Docbank-rejected
recording queries return HTTP 400 with `invalid_media_search`, while other
recording failures (including oversized scopes and remote timeouts) leave
attachment results intact and set `recordings_error` with `error` and
`message`. The request fails if both searches fail or its context ends.
Recording-only responses keep their existing error behavior.

### Message recordings {#get-apiv1messagesidrecordings}

**Endpoint:** `GET /api/v1/messages/{id}/recordings`

List a message's recordings and their transcript state. msgvault reads the
transcript from the configured [Docbank destination](usage/beeper.md#send-audio-to-docbank)
on each request and does not store it.

```json
{
  "message_id": 42,
  "recordings": [
    {
      "attachment_id": 5,
      "filename": "voice.wav",
      "size_bytes": 48213,
      "state": "ready",
      "transcript": {
        "origin": "supplied",
        "partial": false,
        "units": [
          { "text": "See you at noon.", "start_ms": 0, "end_ms": 1500, "speaker": "alice" }
        ]
      }
    }
  ]
}
```

`state` is one of:

| State | Meaning |
|---|---|
| `ready` | Docbank has transcript text; `transcript` is set |
| `processing` | The transcript is queued or running |
| `missing` | Docbank has the audio but no transcript |
| `failed` | Docbank's transcription failed or was cancelled |
| `unsupported` | Transcription eligibility or limits reject the recording |
| `media_missing` | The audio bytes are missing from the archive |
| `unavailable` | msgvault can't show a transcript; see below |

A recording is `unavailable` when:

- Docbank can't be reached in time, or the API key is missing.
- Docbank returns evidence that msgvault can't attribute to this recording,
  such as a provider transcript that another message or an earlier edit sent
  for the same audio.
- `all_sources_upload_consent` is off, so the media worker won't send audio
  that is waiting to be sent, including a provider transcript that changed.
- The media worker blocked the recording for a reason other than transcription eligibility or limits.
- Capture policy skipped the recording's bytes.

`origin` is `supplied` for a provider transcript and `generated` for speech
recognition. `partial` is true when Docbank reports truncated, incomplete or
omitted text. A unit's `start_ms`, `end_ms` and `speaker` are present only when
Docbank recorded them.

A `supplied` transcript shows only when Docbank identifies the exact input
msgvault sent for the message's current recording. This requires Docbank's
[supplied-input attribution](https://github.com/kenn-io/docbank/pull/825).
Older Docbank servers that omit this identity leave supplied text `unavailable`. Matching evidence can
show `ready` before the local worker polls completion. An edited transcript
reads as `processing` while delivery is pending and uploads are allowed.
A delivery that failed reads as `failed`.
If Docbank serves another input for the same audio, including after a transcript
is reverted, supplied text stays `unavailable` until Docbank serves the saved input.

Only live messages list recordings. A message that is hidden as a duplicate or
deleted from its source returns an empty list. Captured audio without a current
worker mapping reads as `unavailable` until discovery records processing work.
When the Docbank integration is off, or its `url`
is invalid, archived recordings stay visible with transcript state `unavailable`;
audio whose bytes are absent stays `media_missing`, or `unavailable` when capture policy skipped it. A
non-positive or non-numeric ID returns `400 invalid_id`.

Person-scoped Media and Files, search, CLI, TUI and MCP readers are later work.

The Web reader refreshes visible recordings while the message stays open.
Unchanged results increase the interval from 2 seconds up to 30 seconds.
Returning to the tab refreshes immediately. A changed message clears earlier
evidence before loading its recordings again.

Each request reads Docbank for retained recordings only, up to four at a time.
The reads share a budget of 20 seconds or half the remaining request time,
whichever is shorter. A read that runs out of budget leaves its recording
`unavailable`.

---

### Search message metadata {#get-apiv1searchfast}

**Endpoint:** `GET /api/v1/search/fast`

Search subject, snippet, sender and recipient metadata with the Gmail-style
query in `q`. The response includes the page in `messages` and the full match
count in `total_count`; no separate count request is needed. If the count is
unavailable, `total_count` is `-1`.

`from:` accepts a sender name, address, or domain. See [sender matching](https://msgvault.io/docs/usage/searching/#domain-search) for the matching rules.

Use `limit` and `offset` to page through results. `limit=0` returns the count
without fetching a page. Aggregate `stats` are omitted by default. Supply
`view_type=senders` (or another aggregate view) to request them. The TUI
requests these stats for its current view; MCP metadata search skips them.

Disconnecting an HTTP MCP client cancels its in-flight daemon search,
including older MCP protocol clients. Request completion and in-progress logs
include `remote_addr` and `query_shape`. The shape records recognized operator
and parameter names, with `text` marking free text. It excludes query values
and unknown operator names. `remote_addr` is the connected peer address;
forwarded-address headers do not replace it.

### Find archived chats {#find-archived-chats}

**Endpoint:** `GET /api/v1/conversations/search`

API schema 3.25.0 adds ranked chat discovery for owner clients.
Delegated agent tokens are denied by the existing archive-read policy.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `q` | string | required | 1–16 unique tokens, at most 256 UTF-8 bytes |
| `limit` | integer | 20 | Maximum results, from 1 to 100 |
| `source_id` | integer | all sources | Positive archive source ID |

Invalid arguments return `400`: malformed numbers use `invalid_limit` or
`invalid_source_id`; other invalid values use `invalid_query`. The response
contains `results` (an array, including when empty) and `has_more` (whether
additional matches exist beyond the limit). There is no cursor or exact count; narrow the
query or source, or increase the limit to inspect more candidates.

Each result contains `conversation_id`, `message_id`, `source_conversation_id`,
`source_id`, `source_type`, `source_identifier`, `source_display_name`,
`network`, `title`, `conversation_type`, `matched_tokens`, `matched_names`,
`evidence_truncated`, and `match_kind` (`exact` or `partial`). Use the archive
`conversation_id` to filter archived messages. `message_id` is the chat's
newest visible message, for opening the thread; the provider ID identifies the
original chat. Network labels come from source labels. Missing Beeper labels,
or suffixes equal to the account ID after trimming, are `unknown`; native
sources use their type.

The [CLI discovery reference](cli-reference.md#find-chat) owns token matching,
ranking, archive scope and scan-cost semantics. This endpoint uses that same
Store operation. It reads metadata, never message bodies, and makes no identity
changes.

---

### Search messages {#get-apiv1search}

**Endpoint:** `GET /api/v1/search`

Searches messages. The default mode is full-text search (FTS5 with LIKE
fallback). When vector search is configured on the server, `mode=vector` runs
semantic-only search and `mode=hybrid` fuses BM25 and vector ranking with
Reciprocal Rank Fusion.

Both `mode=vector` and `mode=hybrid` require at least one free-text term in
`q`, because the free text is what gets embedded as the query vector.
Operator-only queries such as `q=from:alice` have nothing to embed and return
`400 missing_free_text`; send filter-only requests to `mode=fts` instead.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `q` | string | (required) | Search query |
| `mode` | enum | `fts` | `fts`, `vector`, or `hybrid` |
| `page` | int | `1` | Page number (FTS only — vector/hybrid reject `page>1`) |
| `page_size` | int | `20` | Results per page (max 100 for FTS, max `[vector].search.max_page_size_hybrid` for vector/hybrid) |
| `message_type` | string | — | Message-type filter; repeat or comma-separate for multiple values |
| `account` | string | — | Restrict to one account/source |
| `collection` | string | — | Restrict to one collection |
| `explain` | 0/1 | `0` | When `1` and `mode=vector|hybrid`, include per-signal scores |

`message_type` uses the same values as local search: `email`,
`calendar_event`, `meeting_transcript`, `beeper`, `chatwoot`, `teams`, `discord`, `sms`,
`mms`, `whatsapp`, `imessage`, `fbmessenger`, `telegram`, `synctech_sms_call`,
`google_voice_text`, `google_voice_call`, and `google_voice_voicemail`. The
query string can also carry `message_type:` / `message_type=` operators inside
`q`.

**Response (mode=fts, default):**

```json
{
  "query": "quarterly report",
  "total": 23,
  "page": 1,
  "page_size": 20,
  "messages": [
    {
      "id": 12345,
      "subject": "Q4 Planning",
      "message_type": "email",
      "from": "alice@example.com",
      "to": ["bob@example.com"],
      "cc": ["carol@example.com"],
      "sent_at": "2024-10-15T09:30:00Z",
      "snippet": "Here's the draft for Q4...",
      "labels": ["INBOX", "IMPORTANT"],
      "has_attachments": true,
      "size_bytes": 52480
    }
  ]
}
```

**Response (mode=vector or mode=hybrid):**

```json
{
  "query": "when is the planning offsite",
  "mode": "hybrid",
  "returned": 12,
  "pool_saturated": false,
  "generation": {
      "id": 3,
      "model": "nomic-embed-text-v1.5",
      "dimension": 768,
      "fingerprint": "nomic-embed-text-v1.5:768:p1-111111:c32768:e1",
      "state": "active"
    },
  "took_ms": 84,
  "timings": {"query_embedding_ms": 12, "retrieval_ms": 41, "hydration_ms": 31},
  "results": [
    {
      "id": 12345,
      "subject": "Q2 planning offsite agenda",
      "message_type": "email",
      "from": "alice@example.com",
      "to": ["team@example.com"],
      "sent_at": "2024-01-15T10:30:00Z",
      "snippet": "Proposed agenda for the offsite on...",
      "labels": ["INBOX"],
      "has_attachments": false,
      "size_bytes": 2048
    }
  ]
}
```

Vector and hybrid responses expose `returned` instead of `total`
(ANN search does not have a meaningful total count), add a
`generation` sub-object naming the index generation that answered
the query, and include `took_ms` plus a `timings` breakdown
(`query_embedding_ms`, `retrieval_ms`, and `hydration_ms`). The top-level `results` array
replaces `messages`. `pool_saturated` is true when a vector or BM25
candidate pool hit its configured cap (or pure vector search returned
as many hits as requested), hinting that increasing the limit or
narrowing the query may expose more relevant results.

SQLite responses include `accelerator`: `vec1_ivf_opq` for approximate retrieval,
`exact-filter` for an exhaustive search of a small filtered population, `exact`
for exhaustive retrieval, or `exact-fallback` when an accelerator error caused
an exhaustive retry. Accelerator errors are also logged as warnings. Requests
larger than the accelerator's candidate ceiling use exact retrieval.

When `explain=1`, each element of `results` carries an extra `score`
object exposing the fused-score components:

```json
{
  "id": 12345,
  "subject": "...",
  "score": {
    "rrf": 0.032,
    "bm25": 7.4,
    "vector": 0.82,
    "subject_boosted": true
  }
}
```

`bm25` and `vector` are omitted when the message did not appear in
that signal (BM25 missed it or the ANN pool did not include it).
`rrf` is omitted in `mode=vector` because only one signal exists and there is
nothing to fuse. `subject_boosted` is `true` when the subject-line boost was
applied.

See [Searching](/docs/usage/searching/) for the full query syntax reference and
[Vector Search](/docs/usage/vector-search/) for vector and hybrid setup.

---

### Contact route discovery {#contact-route-discovery}

Find a saved person by name and then read that person's archived messaging
routes. These owner-only `GET` routes require API schema 3.20.0 or newer;
delegated agent tokens cannot call them. Each request reads one archive
snapshot without making provider calls or changing identities.

`GET /api/v1/people/contact-candidates?query=Avery%20Example` searches saved
names, current structured names, and display names of explicitly bound
participants, excluding names that appear only in message headers. Every
whitespace token must match a literal substring (`%`, `_`, and backslash are
literal, and SQLite folds ASCII case only). Queries must be 1–256 UTF-8 bytes
after trimming and at most 16 tokens. Each candidate returns canonical
`person_uid`, `person_id`, revision, and `match_kinds`. `ambiguous` reflects
the whole query even when `limit=1`, and your client must choose a person
explicitly.

`GET /api/v1/people/messaging-routes?person_uid=<selected-uid>` resolves
canonical and retired UIDs through current merge and split bindings. It returns
the requested and canonical UID, alias reason, person and identity revisions,
curated `contact_points`, bound `observations`, `unreviewed_suggestions`, and
`routes`. Suggestions summarize candidate or conflicted matches and accepted
matches awaiting application. Discovery never follows or accepts them.

Each route preserves `source_id`, `source_type`, exact `account_id`, network,
archive `conversation_id`, exact `provider_chat_id`, conversation type, bound
participant IDs, evidence and sync times, and reasons. Its status summarizes
the available proof:

| Status | Meaning |
| --- | --- |
| `archive_verified` | A direct chat with matching account/chat/type proof, a complete matching roster whose non-self members are all bound to the selected person, and evidence and source sync no older than seven days. |
| `unresolved` | Missing, invalid, incomplete, changed, stale, deleted, or failed evidence. Read `reasons` before using the identifiers. |
| `group_context` | A group or channel containing the person. Membership does not prove a direct endpoint. |
| `merged_container` | A Beeper container. Its member chat IDs require independent route evidence; `missing_member_chat_ids` identifies missing or ambiguous archived members. |

For Beeper, normal sync captures authoritative `Account.bridge.type` and the
exact roster. Bridge types that correspond to a native importer use that
importer's source type as `network` (`discordgo` becomes `discord`, and
`slackgo` becomes `slack`), while other bridge types such as `whatsapp` or
`telegram` stay as reported. Display labels, names, phone shapes, Matrix IDs,
and curated contact points never establish WhatsApp, and older archives stay
unresolved until normal sync captures this metadata. `network_label` is a
display value. `freshness=archive_only` and `checked_at` describe the lookup;
archive verification does not prove live reachability or authorize sending.

Both endpoints accept `limit` (default 20, range 1–100). Candidate pages use
`after_id`, while each route section returns `items`, `has_more`, and
`next_after_id`, which you pass back as `after_conversation_id`,
`after_contact_point_id`, `after_observation_id`, and `after_suggestion_id`.
Because the route cursor advances over inspected conversations, a
network-filtered page can be empty with `has_more=true`; keep following it
until exhausted. Optional `network` accepts a lowercase service name such as
`whatsapp` or `discord`. Native sources from other networks are skipped without
consuming page items, whereas Beeper routes are filtered after reading their
route metadata and unknown-network routes stay visible as unresolved. Optional
`source_id` narrows route sources; neither filter narrows the contact or
suggestion sections.

Paging is live across requests, so restart after a person or identity revision
changes and reconcile the first page when observations or rosters change, since
not all of those changes advance the identity revision. Every response sets
`Cache-Control: no-store`. Invalid input returns `400 invalid_contact_lookup`,
unknown UIDs return `404 person_not_found`, retired tombstones return
`410 person_uid_gone`, and unavailable storage or backends return
`503 contact_lookup_unavailable`. Failures never return successful empty
matches.

### Accounts summary {#get-apiv1accounts}

**Endpoint:** `GET /api/v1/accounts`

Lists configured accounts with their sync status.

**Response:**

```json
{
  "accounts": [
    {
      "email": "you@gmail.com",
      "display_name": "Your Name",
      "last_sync_at": "2024-10-15T08:00:00Z",
      "next_sync_at": "2024-10-15T09:00:00Z",
      "schedule": "0 * * * *",
      "enabled": true
    }
  ]
}
```

---

### Source sync status {#get-apiv1sourcesstatus}

**Endpoint:** `GET /api/v1/sources/status`

Read sync status for all sources, or filter to one source type with
`source_type`. This endpoint is useful for dashboards and remote
deployments because it exposes active, latest, and last-successful
sync runs without triggering a sync.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `source_type` | string | — | Optional source-type filter, for example `gmail`, `imap`, or `synctech_sms` |

**Response:**

```json
{
  "sources": [
    {
      "id": 1,
      "source_type": "gmail",
      "identifier": "you@gmail.com",
      "display_name": "Personal Gmail",
      "last_sync_at": "2026-06-18T13:02:11Z",
      "updated_at": "2026-06-18T13:02:11Z",
      "active_sync": null,
      "latest_sync": {
        "id": 42,
        "source_id": 1,
        "started_at": "2026-06-18T13:00:00Z",
        "completed_at": "2026-06-18T13:02:11Z",
        "status": "completed",
        "messages_processed": 250,
        "messages_added": 12,
        "messages_updated": 3,
        "errors_count": 1,
        "error_message": null,
        "skipped_count": 2,
        "item_errors": [
          {
            "source_message_id": "18fedcba12345678",
            "phase": "ingest",
            "error_kind": "ingest_error",
            "error_message": "parse MIME: malformed header",
            "created_at": "2026-06-18T13:01:44Z"
          }
        ]
      },
      "last_successful_sync": {
        "id": 41,
        "source_id": 1,
        "started_at": "2026-06-18T12:00:00Z",
        "completed_at": "2026-06-18T12:01:18Z",
        "status": "completed",
        "messages_processed": 33,
        "messages_added": 0,
        "messages_updated": 1,
        "errors_count": 0,
        "error_message": null
      }
    }
  ]
}
```

`active_sync`, `latest_sync`, and `last_successful_sync` are `null`
when no matching run exists. `item_errors` contains up to the 10 most
recent per-item errors for that run. `skipped_count` counts expected
per-item skips, such as Gmail messages that disappeared between list
and fetch. `error_message` is `null` unless the sync run itself failed
with a run-level error.

---

### Meeting intelligence {#meeting-intelligence}

These authenticated read operations require daemon API schema 2.27.0 or newer
and read archived provider evidence without making an AI call or upstream
mutation. See the
[meeting guide](usage/meetings.md#export-context-and-read-follow-ups) for the
user workflow and coverage meanings.

| Endpoint | Request and result |
|---|---|
| `POST /api/v1/meetings/context` | Exactly one of `message_ids` or an Explore `selection`; returns a context packet envelope |
| `POST /api/v1/meetings/actions` | Optional `scope` or `explore`, plus action filters; returns rows, coverage, count, and cursor |
| `POST /api/v1/meetings/metrics` | Optional `scope` or `explore`; returns totals, duration bases, monthly rows, and undated count |

Call `GET /api/v1/meetings/suggestions?ref=...` (API schema 5.11.0) to rank
overlapping meeting notes and calendar events, and post to
`POST /api/v1/meetings/links/decisions` to link a suggested pair. Without
`verification.read`, withheld titles contribute no title words and withheld
partner titles are masked.

Call `GET /api/v1/meetings/record?ref=...` (API schema 5.9.0) to fetch the
notes and calendar copies for a `msg:<id>` or `evt:<id>[@occurrence key]`
reference. A `message.read` grant returns notes and calendar copies from its
authorized sources.

Post owner requests to `POST /api/v1/meetings/links/decisions` (API schema
5.10.0) with `left`, `right`, and `decision` (`link`, `unlink`, or `clear`).
It returns the canonical refs and `state` (`link`, `split`, or `automatic`).
`link` and `unlink` replace the saved choice, while `clear` restores automatic
matching; a conflicting choice returns 409.

`POST /api/v1/meetings/context` accepts 1–100 meetings, `format: "json"` or
`"markdown"`, `include_transcript` (default `false`), and `max_bytes` (default
131072, range 4096–1048576). The byte budget applies to the UTF-8 bytes of
`content`, not the HTTP envelope. Save `content` directly; `content_bytes`,
`truncated`, and `omitted_message_ids` describe that exact payload. Mixed
selections fail with `selection_not_all_meetings`.

A direct `scope` supports `message_ids`, `source_ids`, `participant_id` or
`participant_ids`, `person_id`, `domains`, `after`, `before`, and `deletion`.
You cannot combine person and participant scopes. Different filter groups
intersect, while values inside one group act as alternatives. An explicit
`message_ids: []` matches nothing, and omitting `scope` selects all archived
meetings. Pass RFC3339 timestamps for dates (`after` is inclusive, `before` is
exclusive). `deletion` defaults to `any`; `active` and `deleted` refer to
source deletion, and locally deleted records never participate.

```json
{
  "scope": {"domains": ["example.com"], "after": "2026-01-01T00:00:00Z", "before": "2026-03-01T00:00:00Z"},
  "status": "pending",
  "assignee_email": "alex@example.com",
  "limit": 50
}
```

The actions request above also supports `query` (literal title/description
substring, at most 256 characters) and opaque `cursor`. `limit` defaults to 50
and accepts 1–200. Status accepts `pending`, `completed`, `cancelled`, or
`unknown`; omission includes all. Pagination reads current archived snapshots,
so it is not a retained snapshot across edits. Coverage describes the selected
meetings even when action filters return no rows.

An `explore` scope carries the complete `predicate`, `cache_revision`,
`search_provenance`, and `candidate_snapshot_id` where applicable. It is mutually
exclusive with direct `scope`. The server resolves the full matching population,
with a 10000-ID transfer ceiling. A stale authority requires reloading; an
oversized scope must be narrowed. Neither case widens the request.

### Import a recorder-local Muesli meeting

**Endpoint:** `POST /api/v1/import/muesli`

The native client uses this owner-authenticated endpoint for remote Muesli sync.
Agent-delegated credentials and read-only remote client keys are denied. Prefer the native [Muesli commands](cli-reference.md#sync-muesli);
applications do not need an export script.

The strict JSON contract accepts `action` (`register`, `upsert`, or `refresh`),
`source` (`identifier`, optional `display_name`, and `account_email`), an optional
normalized `meeting`, and optional `full`, `build_cache`, and `no_build_cache`
flags. The client probes its local database before sending `register` to create the stable source.
`upsert` requires a registered source and a nonempty meeting that is not
still recording or processing.
`build_cache` requires the `refresh` action, which permits an explicit cache
request even when a scan contains no eligible meetings. The recorder email must match the source's existing primary identity.
Database paths and raw Contacts identifiers are not accepted.

Requests are limited to 16 MiB, 200 participants, and 50 distinct identities per
participant. Unknown/duplicate JSON members, trailing input, and invalid UTF-8
are rejected. Invalid JSON returns `400`, validation failures `422`, oversized
requests `413`, and a missing source `404`. Validation errors omit field values.
When combined new and archived Contacts evidence would exceed these limits, the
daemon drops that attendee's review-only phone suggestions and imports the
meeting. A meeting the daemon cannot archive returns `422` /
`record_validation_failed`; native clients skip it and continue the scan. Source or request errors use `validation_failed`
and stop the scan so the owner can correct registration or configuration.
The body is bounded and validated before acquiring the operation gate. A busy
gate returns `503` / `operation_in_progress`, and the off-host rate limit
returns `429` with `Retry-After`. Native clients wait and retry the same body
until cancelled.

Responses contain `status`, `source_id`, optional `message_id`, and `changed`.
Statuses are `registered`, `created`, `updated`, `unchanged`, and `refreshed`.
Creation returns `201`; other successful actions return `200`. Meetings preserve
the existing Muesli key derived from row ID and creation time, so lost responses
can be retried safely and later edits update the same archive message. Each
new or edited meeting records one `muesli_push` sync run; an unchanged
retransmission records none. Routine changed writes request an automatic
background refresh; unchanged writes do not.

### Import a meeting {#post-apiv1importmeeting}

**Endpoint:** `POST /api/v1/import/meeting`

Import one provider-neutral meeting without configuring a provider account.
The authenticated, on-demand endpoint accepts at most 16 MiB of JSON and is
idempotent on `source.identifier` plus `meeting.external_id`: the first import
returns `201` / `created`, and retries return `200` / `updated`.

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
      "external_id": "planning-2026-07-29",
      "title": "Planning",
      "started_at": "2026-07-29T09:00:00-04:00",
      "summary_text": "Reviewed the launch plan.",
      "organizer": {"name": "You", "email": "you@example.com"},
      "attendees": [{"name": "Teammate", "email": "teammate@example.com"}]
    }
  }'
```

```json
{
  "status": "created",
  "source_id": 12,
  "message_id": 901,
  "source_message_id": "meeting:planning-2026-07-29"
}
```

Timestamps must be RFC 3339 values with explicit offsets. A meeting must
contain at least one non-empty `summary_markdown`, `summary_text`, `transcript`,
or `transcript_segments` value; plain and segmented transcripts are mutually
exclusive. Segment offsets must be finite, non-negative, and non-decreasing.
`meeting.action_items` accepts up to 1000 structured actions. Each needs a
nonblank title; optional fields preserve explicit assignee, source status, due
date, description, and source ID. An empty array means supported with no actions;
omission means unsupported; `null` is rejected. See the
[complete import example](usage/meetings.md#import-from-any-meeting-source).
Unknown fields are rejected except within `meeting.metadata`.

Each organizer or attendee needs an `email`, a `phone`, or both. `phone` must be
international (a leading `+` or `00`) and is normalized to E.164. An optional
`id` (up to 200 characters) identifies the person within the import source;
msgvault links the person's email and phone through it, including across
meetings. See [how meetings connect to people](usage/meetings.md#how-meetings-connect-to-people).
Validation errors name the failing field without echoing its value.

### Import a message {#post-apiv1importmessage}

**Endpoint:** `POST /api/v1/import/message`

Import one message from a source msgvault has no connector for, such as a feed
item. The authenticated, on-demand endpoint accepts at most 16 MiB of JSON and
is idempotent on `source.identifier` plus `message.external_id`. The first call
returns `201` / `created`; retries and replacements return `200` / `updated`
for the same archived message. An unchanged retry writes nothing.

```bash
curl http://localhost:8080/api/v1/import/message \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  --data '{
    "source": {
      "identifier": "feeds",
      "display_name": "Feeds",
      "account_email": "you@example.com"
    },
    "message": {
      "external_id": "entry-12345",
      "message_type": "feed_item",
      "conversation": {"external_id": "feed-42", "title": "Example Project releases"},
      "sender": {"name": "Example Project", "url": "https://example.com"},
      "title": "Example Project 1.2.0",
      "url": "https://example.com/releases/1.2.0",
      "sent_at": "2026-11-07T10:00:00-05:00",
      "body_markdown": "## Changes\n\n- Faster sync."
    }
  }'
```

```json
{
  "status": "created",
  "source_id": 14,
  "message_id": 902,
  "source_message_id": "entry-12345"
}
```

| Field | Rule |
|---|---|
| `source.identifier` | Required, up to 128 characters |
| `source.account_email` | Required; becomes a confirmed identity for the source |
| `message.external_id` | Required, up to 256 characters |
| `message.message_type` | Required, up to 64 lowercase letters, digits, and underscores, starting with a letter |
| `message.conversation.external_id` | Required, up to 256 characters; groups messages within the source |
| `message.sender` | Requires at least one of `name`, `url`, `email`, `phone`, or `id` |
| `message.title` | Up to 4096 characters |
| `message.url`, `message.sender.url` | Absolute URLs, up to 4096 characters |
| `message.sent_at` | Required RFC 3339 timestamp with an explicit offset |
| `body_markdown`, `body_html`, `body_text` | At most one; the message needs a title or a body |
| `message.metadata` | Any JSON object, stored with the message |

Imported conversations have the conversation type `channel`. The
[usage guide](usage/message-import.md) explains how a sender becomes a
participant and how bodies are stored. Unknown fields are rejected except
within `message.metadata`. Validation errors name the failing field without
echoing its value.

---

### OAuth token exchange {#post-apiv1authtokenemail}

**Endpoint:** `POST /api/v1/auth/token/{email}`

Upload an OAuth token JSON file generated by a local `msgvault` client.

This endpoint is used by `msgvault export-token` during remote/headless deployment workflows.

**Request headers:**

- `X-API-Key: <api-key>` (or any supported auth header)
- `Content-Type: application/json`

**Example request body (`/api/v1/auth/token/you@gmail.com`):**

```json
{
  "access_token": "ya29...",
  "token_type": "Bearer",
  "refresh_token": "1//0g...",
  "expiry": "2024-12-31T23:59:59Z",
  "scopes": ["https://www.googleapis.com/auth/gmail.modify"]
}
```

**Successful response (`201 Created`):**

```json
{
  "status": "created",
  "message": "Token saved for you@gmail.com"
}
```

---

### Create account {#post-apiv1accounts}

**Endpoint:** `POST /api/v1/accounts`

Registers an account for scheduled sync on the remote server.

`msgvault export-token` posts to this endpoint automatically after uploading a token. With `--upload-only`, it skips this request and does not create a Gmail sync schedule.

```json
{
  "email": "you@gmail.com",
  "schedule": "0 2 * * *"
}
```

The `enabled` field is always set to `true` server-side.

**If the account already exists (200 OK):**

```json
{
  "status": "exists",
  "message": "Account already configured for you@gmail.com"
}
```

**On success (201 Created):**

```json
{
  "status": "created",
  "message": "Account added for you@gmail.com"
}
```

---

### Start account sync {#post-apiv1syncaccount}

**Endpoint:** `POST /api/v1/sync/{account}`

Trigger a manual sync for an account. Returns immediately with a 202 status while the sync runs in the background.

**Response (202 Accepted):**

```json
{
  "status": "accepted",
  "message": "Sync started for you@gmail.com"
}
```

For sources scheduled as generic jobs (`source_type` set), the body also carries `disposition`:

| Disposition | Meaning |
|---|---|
| `started` | The job was idle and a run started. |
| `pending` | The job was already running; one follow-up run is recorded. Scheduler status reports it as `pending`. |
| `coalesced` | The request was merged into a run still waiting to start or into a follow-up already pending; no additional run was added. |

Repeated triggers never run the job concurrently. A generic-job trigger does not wait on the daemon's operation gate, so it is answered even while a long import of the same job holds it.

---

### Scheduler status {#get-apiv1schedulerstatus}

**Endpoint:** `GET /api/v1/scheduler/status`

Scheduler state and per-account schedule details.

**Response:**

```json
{
  "running": true,
  "accounts": [
    {
      "email": "you@gmail.com",
      "running": false,
      "last_run": "2024-10-15T08:00:00Z",
      "next_run": "2024-10-15T09:00:00Z",
      "schedule": "0 * * * *"
    }
  ]
}
```

The daemon runs scheduled work one job at a time. While a sync waits for
another job to finish, its entry reports `"queued": true` and `"running": false`.
`"running": true` means it acquired the operation gate; `started_at` gives when
it began. The top-level `running` field reports whether the scheduler is active.
A schedule tick that fires during a run
sets `"pending": true`, and the scheduler runs the sync once more when the
current run ends. Resumable account syncs (Gmail, Teams, and Discord), Slack, and
Beeper support preemption: after holding the gate for a minute while others
are queued, they are asked to stop at their next safe point. If they are
still running five seconds later, the scheduler cancels their context. An
interrupted run goes back behind waiting jobs immediately; it does not wait
for another schedule tick. Activity projection also supports preemption: it
stops after its current batch, limits each pass to ten batches, and stops at
two minutes regardless. It saves its reconciliation progress. Attachment
packing and daily attachment maintenance stop after one minute and resume
behind waiting work. IMAP full passes do not support scheduled preemption.
Other jobs keep their own runtime budgets. Waiting API requests can still
interrupt scheduled work. `GET /api/v1/sources/status` reports the same
state for every scheduled source as `scheduler_queued`, `scheduler_pending`,
and `scheduler_started_at`. Compare queued state and the last successful sync
to detect a source that is waiting too long. Health's `operation.label` names
the job holding the gate, such as `activity-projection` or `attachment-pack`.

---

### Messages during cache rebuilds

In auto mode on SQLite, `POST /api/v1/explore` serves live archive rows while
the analytical cache rebuilds and after a failed build. Date, source, account,
identity, domain, mailing-list, kind, and full-text filters stay available, and
responses include a warning about operations that need the cache along with
recovery instructions after a failed build.

Grouping, person filters, semantic and hybrid search, per-row exact-match
counts, bulk actions, **Files**, **People**, and the reader's **Open person**
action still require the cache. A live cursor expires when the archive changes
or the replacement cache publishes; restart pagination after
`409 archive_revision_changed`.

### Participant and domain timelines

`POST /api/v1/participants/{id}/timeline` and `POST /api/v1/domains/{domain}/timeline` omit drafts and unsent items. Participant timelines include events and meetings only when the archive owner participated. These rules apply to every caller, including CLI and MCP clients.

### Group analytical results

**Endpoint:** `POST /api/v1/explore/groups`

`grouping` takes one dimension: `source`, `participant`, `domain`,
`message_type`, `mailing_list`, `kind`, `year`, `month`, or `day`.
`day` returns `YYYY-MM-DD` keys and counts individual messages; other
dimensions count entries, with chats collapsed into conversations.
`timezone` sets the IANA time zone for day, month, and year grouping and defaults to `UTC`. It also sets the calendar dates in a keyed group's activity overview.
Day rows include `level` for calendar shading only when the page contains every day row. It ranks each count across that complete day set; partial pages and keyed lookups omit `level`.
`sent_only: true` excludes drafts and unsent items from the counts.
Day grouping is request-only and cannot be stored in a saved view.
Files grouping (`POST /api/v1/files/groups`) uses UTC days and has no `timezone` parameter.

Month and year rows include `total_items`, the number of entries across all matching time groups before pagination. A `group_key` lookup for a matching non-day group adds `overview` with its `first_at` timestamp, an `activity` series, and up to three `co_participants`. Activity buckets are days for month groups and months for other groups.

### Preflight an analytical selection {#post-apiv1explorepreflight}

**Endpoint:** `POST /api/v1/explore/preflight`

Validates a revision-pinned selection of canonical archive entries and issues
the single-use `operation_token` required to stage that selection for deletion.
Build selections from the analytical explore endpoints (`POST /api/v1/explore`
and `POST /api/v1/explore/groups`), whose responses return the `cache_revision`,
`search_provenance`, and (for semantic or hybrid search)
`candidate_snapshot_id` values you must echo back. See `/openapi.json` for the
full explore contract.

**Request:**

```json
{
  "selection": {
    "mode": "all_matching",
    "predicate": {
      "filters": [
        { "dimension": "domain", "values": ["example.com"] },
        { "dimension": "before", "values": ["2020-01-01"] }
      ]
    },
    "cache_revision": "<cache_revision from the explore response>",
    "search_provenance": {}
  }
}
```

`selection` fields:

| Field | Description |
|---|---|
| `mode` | `all_matching` selects everything the predicate matches; `explicit` selects only the listed `row_keys` |
| `predicate` | The explore request being acted on: `filters` (dimensions `source`, `participant`, `domain`, `message_type`, `kind`, `after`, `before`, `deletion`), plus `query` / `search_mode` for search-backed selections. Cursors are rejected |
| `row_keys` | Explore row keys (the `key` field of explore rows) to include; required when `mode` is `explicit` |
| `exclusions` | Row keys to exclude from an `all_matching` selection |
| `cache_revision` | Required. The `cache_revision` from the explore response being reviewed |
| `search_provenance` | The `search_provenance` from the explore response; checked when the predicate has a `search_mode` |
| `candidate_snapshot_id` | The snapshot ID from the explore response; required for `semantic` / `hybrid` predicates (`400 candidate_snapshot_required` otherwise) |

**Response (200):**

```json
{
  "count": 1234,
  "deletable_count": 1200,
  "estimated_bytes": 52428800,
  "cache_revision": "<current cache revision>",
  "search_provenance": {},
  "unavailable_actions": [
    { "action": "export", "reason": "browser_export_requires_single_message" },
    { "action": "open_in_source", "reason": "trusted_source_link_unavailable" }
  ],
  "action_targets": [],
  "operation_token": "<operation_token>",
  "expires_at": "2026-07-06T15:35:00Z"
}
```

`count` includes all selected items after exclusions. `deletable_count` is the
Gmail and Microsoft Graph mail subset that can be staged, and the difference is
the number of items staging skips. A chat conversation counts as one item.

`unavailable_actions` lists actions this selection does not support. A
`stage_deletion` entry means nothing in the selection can be deleted from its
source, so staging fails with `409 selection_not_deletable`. A selection that
mixes deletable and non-deletable items omits that entry: staging takes the
deletable subset and reports the rest as skipped.

The `operation_token` binds to this exact selection, its match count, and the
current cache revision. It expires at `expires_at` (five minutes after issue)
and is single-use: staging a manifest consumes it, while a staging attempt that
fails before the manifest persists leaves it valid for retry. Reusing or
sending an expired token returns `409 operation_token_invalid`.

Malformed selections return `400` with `invalid_selection` (bad `mode`,
missing `cache_revision`, or `mode: "explicit"` without `row_keys`) or
`invalid_selection_predicate`, and malformed search operators return
`400 invalid_query`. If the analytical cache or search index changes after the
explore response was produced, preflight fails with
`409 archive_revision_changed` or `409 search_revision_changed`; re-run explore
and preflight against the new revision. Staging repeats all of these checks.

---

### Saved Views {#saved-views}

Saved Views are persistent, shared analytical definitions: a query, an
explicit search mode, filters, a grouping chain, a presentation, a sort, the
visible columns, and the inspector preference. Each record carries a schema
version and a revision. The Web UI, the MCP server, and any API client read,
edit, and run the same records, so a view saved in the browser can be executed
by an agent without rebuilding its query.

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/v1/saved-views` | List every Saved View, ordered by name. |
| `POST` | `/api/v1/saved-views` | Create a view. Returns `201` with an `ETag` and `Location` header. |
| `GET` | `/api/v1/saved-views/{id}` | Read one view. Returns its `ETag`. |
| `PATCH` | `/api/v1/saved-views/{id}` | Update only the supplied fields. Requires `If-Match`. |
| `DELETE` | `/api/v1/saved-views/{id}` | Delete a view definition. Requires `If-Match`. Never touches archive messages. |
| `POST` | `/api/v1/saved-views/{id}/run` | Execute a view through its canonical Explore definition. |

Definitions with local-calendar date bounds use `schema_version: 3`; UTC-only definitions use version 2. Numeric offsets in schema 3 preserve local calendar dates; offsets in older definitions retain their UTC meaning. The daemon reads schema 1 with pipes as literal text in full-text queries. `canonical_state` is a closed object. The daemon rejects a
definition it could not execute, so every stored view can be opened by the
Web UI and run by the API:

| Field | Accepted values |
|-------|-----------------|
| `query` | Free text. Empty means no text search. |
| `search_mode` | `full_text`, `semantic`, `hybrid`. Defaults to `full_text` when a query is present. |
| `filters[].field` | An Explore filter dimension: `source`, `identity`, `participant`, `domain`, `mailing_list`, `message_type`, `kind`, `after`, `before`, `deletion`. The legacy aliases `source_id` and `participant_id` are accepted and executed as `source` and `participant`. |
| `filters[].operator` | `eq` or `in`. |
| `filters[].values` | One or more non-empty strings. Numeric identifiers are decimal strings so they survive JavaScript clients unchanged. |
| `grouping` | A drill-down chain of Explore grouping dimensions: `source`, `participant`, `domain`, `message_type`, `mailing_list`, `kind`, `year`, `month`. |
| `presentation` | `table`, `timeline`, or `files`. |
| `sort` | Only `{"field": "occurred_at", "direction": "desc"}`. |
| `columns` | `kind`, `people`, `title`, `excerpt`, `time`, `attachments`, `size`. Presentation only; never sent to Explore. |
| `inspector_pinned` | Optional inspector pin metadata. API and MCP clients preserve explicit `true` and `false` values. The current Web UI keeps the inspector pinned and ignores this field. |

Every field is optional, an explicit `null` is rejected, and unknown or
transient workspace state such as selections and result rows is rejected.
Values are checked with the rules Explore applies when the view runs: `source`
and `participant` take positive integer IDs, `after` and `before` take one
RFC3339 timestamp each and appear at most once, `deletion` takes `active` or
`deleted`, an `identity` filter carries source ID, identifier, and direction
next to a matching single-value `source` filter, and at most one `sort` entry
is allowed. The same vocabulary is published as enums in the OpenAPI document,
so generated clients validate it before a request is sent.

Read responses preserve `canonical_state` as the original JSON, including
definitions from older or unsupported schema versions. Check
`incompatibility_reason` before treating that JSON as an executable definition.
An incompatible view remains readable through the API and MCP so clients can
explain the problem and offer removal.

Mutations use the record revision as an optimistic-concurrency guard. Send
the latest `ETag` (for example `"saved-view-7-r3"`) as `If-Match`; a stale tag
returns `409 saved_view_revision_conflict`, so reload the view and review the
latest revision instead of overwriting it. Other errors are
`400 invalid_saved_view` for a definition outside the vocabulary or an
unsupported schema version, `404 saved_view_not_found`,
`409 saved_view_name_conflict`, `428 if_match_required`, and
`400 invalid_if_match`.

---

### Run a Saved View {#post-apiv1saved-viewsidrun}

**Endpoint:** `POST /api/v1/saved-views/{id}/run`

Executes the stored definition through the Explore endpoint its definition
selects, with the same validation, search resolution, and cursor pagination
that endpoint applies:

- A view with a `grouping` chain runs `POST /api/v1/explore/groups` at the
  first level of the chain, the level the Web UI shows when it opens the view.
- A view with `presentation: "files"` runs `POST /api/v1/explore/files`.
- Every other view runs `POST /api/v1/explore` as a table. Timeline is a
  client-side rendering of the same entry rows.

Grouped runs use UTC by default. Month and Year groups sort newest first; other groups sort by descending count. The saved sort applies to entry results.

```json
{
  "limit": 20,
  "cursor": "eyJvZmZzZXQiOjIwLC...",
  "timezone": "America/New_York"
}
```

`limit` is `1` to `100` and defaults to `100`. `cursor` is the opaque
`next_cursor` from the previous page and is omitted for the first page. An
empty object `{}` is a valid body. `timezone` is an optional IANA time zone for calendar groups; use the same zone for each page.

```json
{
  "saved_view": {
    "id": 7,
    "name": "Invoices",
    "canonical_state": {
      "query": "invoice",
      "search_mode": "full_text",
      "filters": [{"field": "source", "operator": "in", "values": ["1"]}],
      "presentation": "table"
    },
    "schema_version": 2,
    "revision": 3,
    "created_at": "2026-07-19T10:00:00Z",
    "updated_at": "2026-07-19T11:00:00Z"
  },
  "result_kind": "entries",
  "rows": [
    {"key": "message:42", "kind": "email", "title": "Quarterly invoice", "occurred_at": "2026-07-18T11:00:00Z"}
  ],
  "total_count": 2,
  "next_cursor": "eyJvZmZzZXQiOjEsLi4u",
  "cache_revision": "cache-9",
  "search_provenance": {"lexical_index_revision": "fts-4"}
}
```

`result_kind` names the populated array: `entries` carries `rows` in the
`POST /api/v1/explore` row shape, `groups` carries `groups`, and `files`
carries `files`. For semantic and hybrid searches, entry pages omit
`total_count` and report `candidate_snapshot_id` and `candidate_pool_saturated`.
Group and file pages retain Explore's `total_count`: the number of matching
groups or files within the resolved candidate set, not an archive-wide semantic
match count. These pages report `candidate_snapshot_id` and reject an incomplete
candidate pool. `search_deletion_scope` is `active` when a semantic search
narrowed an unrestricted deletion context. `next_cursor` is present while more
results remain.

A missing view returns `404 saved_view_not_found`, and a stored definition the
current daemon cannot execute, such as one written under another schema
version, returns `400 invalid_saved_view`. Every other failure is the Explore
endpoint's own response passed through unchanged: `400 invalid_cursor` or
`invalid_limit`, `409 archive_revision_changed` or `search_revision_changed`
when pagination must restart, `503 vector_not_enabled` or another vector
readiness error for semantic and hybrid views, and the analytical cache
readiness response when the cache is unavailable. Semantic and hybrid views
never fall back to full-text search.

---

### Stage messages for deletion {#post-apiv1deletions}

**Endpoint:** `POST /api/v1/deletions`

Stages messages for deletion by writing a pending deletion manifest. Staging
never touches Gmail; execution stays exclusively in the `delete-staged`
command. You can send one of three request shapes:

- **Dry run.** `"dry_run": true` with a `filter`, `message_ids`, or both
  resolves and counts matches without staging anything.
- **Explicit message IDs.** `message_ids` alone (internal IDs returned by
  `/messages` and `/search`) stages directly.
- **Preflighted selection.** A `selection` plus the `operation_token` issued by
  [`/api/v1/explore/preflight`](#post-apiv1explorepreflight). This is the only
  way to stage a filter-based deletion; a non-dry-run request with a `filter`
  returns `428 preflight_required`.

In every shape, resolution is limited to live Gmail-source messages, and a
staged manifest executes against a single mailbox, so your selection must
resolve to exactly one Gmail source. The daemon stamps the durable source tuple (`type`,
`identifier`, and local numeric `id`) on the manifest and returns it as
`source` in dry-run and created responses; `delete-staged` resolves that tuple
again before claiming the manifest. The response also reports the account
identifier. Selections spanning multiple accounts fail with
`400 multi_account_selection`; narrow the request (for example with
`filter.source_id` or a `source` filter dimension) or stage per account.
Unknown JSON fields return `400 invalid_request` so a misspelled filter key
cannot widen the selection silently, and requests with no criteria return
`400 empty_filter` so the entire archive cannot be staged.

#### Dry run

```json
{
  "filter": {
    "sender": "newsletter@example.com",
    "source_id": 1,
    "after": "2019-01-01",
    "before": "2020-01-01"
  },
  "dry_run": true
}
```

Supported `filter` fields are `sender`, `sender_name`, `recipient`,
`recipient_name`, `domain`, `label` (strings), `source_id` (integer), and
`after` / `before` (RFC3339 or `YYYY-MM-DD` dates). You can combine `filter`
with `message_ids`; matches are unioned and deduplicated. The server resolves
the request and returns `200` without writing anything:

```json
{
  "dry_run": true,
  "message_count": 1234,
  "account": "you@gmail.com",
  "source": {"id": 1, "type": "gmail", "identifier": "you@gmail.com"},
  "sample_gmail_ids": ["18c2f5a1b2c3d4e5", "..."]
}
```

#### Staging explicit message IDs

A non-dry-run request whose only criterion is `message_ids` stages directly,
because the IDs are already an explicit, reviewed list:

```json
{
  "message_ids": [123, 456],
  "description": "old newsletters"
}
```

IDs that do not resolve to live deletable Gmail or Microsoft Graph mail
messages with provider message IDs are omitted, and `message_count` reports the
number of targets the daemon resolved.

The server writes a pending manifest and returns `201`:

```json
{
  "dry_run": false,
  "message_count": 2,
  "account": "you@gmail.com",
  "source": {"id": 1, "type": "gmail", "identifier": "you@gmail.com"},
  "id": "20260706-153000-old-newsletters-a1b2",
  "status": "pending"
}
```

#### Staging a preflighted selection

Filter-based staging goes through preflight so the reviewed selection, not a
re-evaluated filter, is what gets staged:

1. Call `POST /api/v1/explore` with your predicate, review the rows, and note
   `cache_revision` (plus `search_provenance` and `candidate_snapshot_id` for
   search-backed predicates).
2. Call `POST /api/v1/explore/preflight` with the `selection`, review `count`,
   `deletable_count`, and `estimated_bytes`, and save the `operation_token`.
3. Call `POST /api/v1/deletions` with the same `selection` and the token:

```json
{
  "selection": {
    "mode": "all_matching",
    "predicate": {
      "filters": [
        { "dimension": "domain", "values": ["example.com"] },
        { "dimension": "before", "values": ["2020-01-01"] }
      ]
    },
    "cache_revision": "<cache_revision from the explore response>",
    "search_provenance": {}
  },
  "operation_token": "<operation_token from preflight>",
  "description": "old example.com mail"
}
```

The response is the same `201` manifest shape as above, plus `matched_count`
and `skipped_count`:

```json
{
  "dry_run": false,
  "message_count": 2,
  "matched_count": 3,
  "skipped_count": 1,
  "account": "you@gmail.com",
  "source": {"id": 1, "type": "gmail", "identifier": "you@gmail.com"},
  "id": "20260706-153000-old-example-com-mail-a1b2",
  "status": "pending"
}
```

`message_count` is the staged subset, `matched_count` the reviewed match set,
and `skipped_count` the items no source supports deleting. Deletion covers
Gmail and Microsoft Graph mail email, so a mixed selection stages those rows
and reports the rest as skipped rather than failing; only a selection with nothing deletable
returns `409 selection_not_deletable`. Legacy Gmail rows with a blank
`message_type` count as email.

`selection` cannot be combined with `filter` or `message_ids`
(`400 invalid_request`). The server re-validates the selection against the
preflight grant before staging: the selection, its match count, and the
cache/search revisions must be unchanged. `"dry_run": true` may be combined
with a selection to preview the same staged subset, counts, and sample; the
token is validated but not consumed.

#### Errors

| Status | Code | When |
|---|---|---|
| `400` | `empty_filter` | No filter criterion, `message_ids` entry, or `selection` |
| `400` | `invalid_request` | Unknown JSON fields, or `selection` combined with `filter` / `message_ids` |
| `400` | `invalid_date` | `after` / `before` is not RFC3339 or `YYYY-MM-DD` |
| `400` | `no_messages_matched` | The criteria or reviewed selection match nothing |
| `400` | `multi_account_selection` | The selection spans more than one Gmail account |
| `428` | `preflight_required` | Non-dry-run `filter` request without a preflighted selection, or `selection` without `operation_token` |
| `409` | `operation_token_invalid` | Token expired, already used, or does not match the selection, count, and revision |
| `409` | `archive_revision_changed` | The analytical cache changed since preflight |
| `409` | `search_revision_changed` | The search index revision changed since preflight |
| `400` | `invalid_selection_predicate` | The predicate query carries an empty `from` / `to` / `cc` / `bcc` value |
| `409` | `selection_not_deletable` | No item in the selection can be deleted from its source |
| `409` | `selection_changed` | The matching messages changed between preflight and staging |

---

### List staged deletions {#get-apiv1deletions}

**Endpoint:** `GET /api/v1/deletions`

Lists deletion manifests, newest first. The optional `status` parameter
filters by one of `pending`, `in_progress`, `completed`, `failed`, or
`cancelled`; omitting it returns all statuses.

**Response:**

```json
{
  "manifests": [
    {
      "id": "20260706-153000-old-newsletters-a1b2",
      "status": "pending",
      "created_at": "2026-07-06T15:30:00Z",
      "created_by": "api",
      "description": "old newsletters",
      "account": "user@example.com",
      "message_count": 1234
    }
  ]
}
```

---

### Cancel a staged deletion {#delete-apiv1deletionsid}

**Endpoint:** `DELETE /api/v1/deletions/{id}`

Cancels (unstages) a pending or in-progress deletion manifest. Returns `404`
for an unknown manifest ID and `409 not_cancellable` for manifests that are
already completed, failed, or cancelled.

**Response:**

```json
{
  "id": "20260706-153000-old-newsletters-a1b2",
  "status": "cancelled"
}
```

## Rate Limiting

The API enforces a rate limit of 10 requests per second per client IP, with a
burst of 20 requests. If you exceed the limit, the server responds with HTTP 429
and a `Retry-After` header telling you how long to wait before retrying.

Trusted loopback requests skip this limit when no API key is configured, or
when the request carries a valid API key, remote client key, or browser session
cookie. Credential-free loopback `GET /api/ping` and `GET /api/daemon/identity`
requests also skip it so local clients can find a busy daemon. Loopback
requests with a wrong or missing credential, along with session login requests,
stay rate-limited.

## CORS

Cross-Origin Resource Sharing is disabled by default. To allow browser-based
clients, list allowed origins in your `config.toml`:

```toml
[server]
cors_origins = ["http://localhost:3000", "https://my-dashboard.example.com"]
cors_credentials = true
cors_max_age = 3600
```

## Scheduled sync

The server can sync Gmail, IMAP, Microsoft Teams, and registered Discord guild
sources automatically on a cron schedule. Add `[[accounts]]` sections to your
config:

```toml
[[accounts]]
email = "you@gmail.com"
schedule = "0 * * * *"    # every hour
enabled = true

[[accounts]]
email = "user@example.com"
schedule = "*/15 * * * *" # every 15 minutes
enabled = true

[[accounts]]
email = "123456789012345678" # exact registered Discord guild ID
schedule = "*/30 * * * *"
enabled = true
```

When account schedules are configured, `msgvault serve` starts the scheduler
automatically. Discord schedules require the exact guild ID because display
names can change and collide. Poll `/api/v1/scheduler/status` to monitor
schedule state and call `/api/v1/sync/{account}` to trigger supported account
syncs off-schedule. For manual Discord syncs, run `msgvault sync-discord`.

The same HTTP server backs configured remote CLI access and the local
background daemon used by archive-access CLI commands. Starting in API schema
2.4.0, the CLI sync transport accepts `source_id`, letting `sync` and
`sync-full` target one exact source even when several source types share an
identifier.

!!! note
    Gmail accounts must have completed an initial `msgvault sync-full` before
    scheduled incremental sync. IMAP schedules scan the mailbox and skip known
    messages, while Teams and Discord importers detect and checkpoint their own
    first-run history backfills.

`msgvault serve` also runs scheduled SyncTech SMS Backup & Restore Drive
sources configured under `[[synctech_sms.sources]]`; see
[Configuration](/docs/configuration/#synctech-sms-sources).

## Security model

The server is built for local use by default:

- **Loopback-only by default.** The default bind address is `127.0.0.1`,
  limiting access to the local machine.
- **API key required for non-loopback.** Binding to a non-loopback address
  requires an effective key. `msgvault serve` creates a persisted key when no
  credential source is configured, and fails startup if a selected source is
  invalid. See [server credentials](configuration.md#server).
- **Opt-in for insecure binding.** To bind to a non-loopback address without an
  API key (not recommended), set `allow_insecure = true`.

!!! warning
    Exposing the server on a network without authentication gives anyone on that network access to your entire email archive. Keep authentication enabled when binding to non-loopback addresses.

### Restricted agent reads

Agent requests authenticate with `X-Msgvault-Agent-Token` instead of the owner
key. Before serving supported archive reads, the daemon checks `search.read`,
`message.read`, `attachment.read`, or `stats.read` alongside the live source
identity. Unscoped reads return only granted sources, and explicit accounts,
collections, and source ID sets must fit entirely inside the grant. A missing
permission or an account selector outside the grant returns
`403 permission_denied` naming the permission, while a message, thread, or
attachment outside the grant returns the same `404` as a missing one. At most
two agent reads run at once; others wait, and a request that ends while waiting
returns `503 agent_read_busy`. Unsupported routes stay owner-only, including
SQL, exports, writes, configuration, and grant management.

Delegated reads use the daemon's ordinary request deadline, normally 60 seconds.

See [agent-token](cli-reference.md#agent-token) for grant lifetime, expiry,
revocation, issuance, remote CLI and MCP usage, and supported read capabilities.

## Configuration reference

Server settings go in the `[server]` section of `config.toml`, and account
schedules go in `[[accounts]]` sections.

### `[server]`

| Key | Default | Description |
|---|---|---|
| `api_port` | `0` (auto-select) | Port the server listens on; `0` picks an open port at startup and clients discover it automatically. Set a fixed port for remote/NAS deployments. |
| `bind_addr` | `127.0.0.1` | Bind address |
| `api_key` | — | API key for authentication |
| `agent_access` | `false` | Enable restricted agent grants; requires an effective API key and a daemon restart after changes |
| `allow_insecure` | `false` | Allow non-loopback binding without `api_key` |
| `cors_origins` | `[]` | Allowed CORS origins |
| `cors_credentials` | `false` | Allow credentials in CORS requests |
| `cors_max_age` | `0` | CORS preflight cache duration in seconds (defaults to `86400` when `cors_origins` is set) |
| `trusted_proxies` | `[]` | IP addresses or CIDRs allowed to supply forwarded HTTPS and host headers |
| `daemon_idle_timeout` | `20m` | Idle timeout for automatically started daemons; set to `"0s"` to disable |
| `daemon_auto_restart` | `newer` | Local daemon restart policy when the CLI finds a different daemon binary version: `newer`, `never`, or `always` |
| `daemon_auto_start` | `true` | Let CLI, TUI, and MCP commands start a local background daemon when none is running; set `false` when a supervisor runs `msgvault serve` |

`daemon_idle_timeout` applies to automatically started daemons. `msgvault daemon start` and `msgvault daemon restart` launch daemons that stay running until stopped; foreground `msgvault serve` runs until interrupted. `MSGVAULT_DAEMON_IDLE_TIMEOUT` overrides the configured timeout for automatic starts.

`daemon_auto_restart` only affects local lifecycle-managed daemons. The default `newer` replaces older compatible daemons with the current CLI binary. A source-build CLI also replaces the release daemon it was built from, or one from an earlier release. A daemon from another source build of the same release keeps running, and the command prints the restart command. `never` leaves restarts to an external supervisor, and `always` restarts on any safe version mismatch. Remote servers are never restarted by CLI clients. When automatic replacement is suppressed, a local command that keeps a daemon eligible for replacement prints both versions and `msgvault daemon restart` on stderr.

### `[analytics]`

| Key | Default | Description |
|---|---|---|
| `engine` | `auto` | Aggregate engine for Web UI, TUI, and aggregate HTTP views: `auto`, `sql`, or `duckdb` |
| `auto_build_cache` | `true` | Refresh a stale or missing cache at startup, after scheduled or manual syncs, and when a query finds it due; `false` skips automatic builds |
| `min_rebuild_interval` | `0s` | Minimum age of a usable cache before a sync, query, or daemon restart may queue an automatic rebuild |
| `builder_memory_limit` | Adaptive | DuckDB buffer-manager budget for cache builds; see [builder sizing and overrides](configuration.md#analytics) |
| `builder_threads` | min(CPUs, 2) | DuckDB threads for cache builds; zero keeps the default |
| `builder_temp_limit` | `32GB` | Maximum spill-to-disk size for cache builds |
| `query_memory_limit` | `512MB` | DuckDB memory limit for daemon aggregate queries; raise it on a large archive |
| `query_threads` | min(CPUs, 4) | DuckDB threads for daemon aggregate queries; zero keeps the default |
| `query_temp_limit` | `2GB` | Maximum spill-to-disk size for daemon aggregate queries; a query that spills past it fails with a DuckDB out-of-memory error |

`engine = "sql"` forces live SQL for aggregate views. `engine = "duckdb"`
requires a usable Parquet cache and keeps analytics unavailable until it is
ready. Startup fails if no usable cache can be built or opened. A failed
automatic refresh keeps the last usable publication available.
`auto_build_cache = false` leaves cache rebuilds to explicit
`msgvault build-cache`, `query --fresh`, or sync `--build-cache` requests.
These settings replace the TUI/MCP analytics flags deprecated in 0.17.0; see [Configuration: analytics](/docs/configuration/#analytics).

`min_rebuild_interval` applies to automatic refreshes requested by syncs,
queries, and daemon startup, including usable partial snapshots. Explicit
refreshes and unusable-cache recovery are not delayed by it. See
[Configuration: analytics](configuration.md#analytics) for the cache freshness
policy and deletion visibility. Changes under
`[analytics]` take effect after the daemon restarts.

### `[[accounts]]`

| Key | Default | Description |
|---|---|---|
| `email` | (required) | Gmail/IMAP/Teams identifier or display name, or exact Discord guild ID |
| `schedule` | — | Cron expression for sync schedule |
| `enabled` | `false` | Whether scheduled sync is active |

See the [Configuration](/docs/configuration/) page for the full config file reference.

## Local embedding setup

Call `GET /api/v1/embeddings/local` to inspect the selected provider, download
size, and setup progress. `host` names the daemon host for remote browsers and
is empty on loopback addresses. `running` indicates an active setup, `phase` is
`idle`, `downloading`, or `indexing`, `done_bytes` and `total_bytes` track
download progress, and `error` reports a failed setup's explanation for the
current daemon session.

Call `POST /api/v1/embeddings/local` with no body to turn on local message
search. If Ollama already serves `nomic-embed-text`, setup reuses it;
otherwise, setup downloads the local model when needed, saves the
message-search configuration, publishes it to the running daemon, and builds
the message index. Poll `GET` for completion; setup continues after the HTTP
connection closes. Existing configured providers require a
configuration change before activation, and document and people consent stay
separate from message activation.

Both routes require owner access. `POST` returns `202` for a new or
already-running setup, or `503 unavailable` when setup cannot be admitted.
`GET` returns `503 unavailable` when inspection fails or local setup is
unavailable. Failures after admission appear in the polled `error` field. For
index coverage and throughput, use
[message embedding status](#message-embedding-status).

## Message embedding status

Call `GET /api/v1/embeddings/status` to get the same generation and coverage
shape returned by `msgvault embeddings status --json`, along with live job and
scheduler state, recent throughput, nullable ETA, and a bounded per-batch
timing sample. Pass an optional positive integer `source_id` to filter
`eligible`, `current`, and `pending`. Diagnostics, throughput, job state, and
failures still describe the full run across sources, and filtered responses set
ETA to `null`. Because run diagnostics and scheduler state span sources outside
a scoped agent grant, this route requires owner access.

The endpoint is read-only and bypasses the scheduler slot. It returns `400` for
invalid, repeated, unknown, or out-of-scope source IDs, and `503` with
`embeddings_status_unavailable` when vector search is disabled, required
archive state cannot be read, or the read takes longer than five seconds. If
only diagnostics fail to read or decode, the response still returns coverage
and job state with `diagnostics=null`. `coverage_sampled_at` records when
counts were taken, and stale counts suppress ETA. `diagnostics_stale` flags an
unfinished snapshot older than a minute, setting the phase of an active run to
`diagnostics_stale`. This endpoint never sends message content to a provider.
See the [CLI field semantics](cli-reference.md#embeddings-status) and the
[published OpenAPI schema](https://github.com/kenn-io/msgvault/blob/main/api/openapi.yaml).
MCP exposes the same response through `get_embeddings_status` on API 3.21.0 or
newer when vector search is enabled.

## Verification content

When a delegated HTTP, CLI, or MCP message read lacks `verification.read`,
msgvault replaces detected verification messages' subjects, snippets, excerpts,
bodies, conversation titles, and attachment filenames with
`[verification content masked]` and clears their HTML. Sender, date, labels,
and IDs stay available. The modifier requires the operation's existing read
permission and source grant, and owner reads and stored messages never change.

Detection evaluates subject, snippet, conversation title, attachment filenames,
and both body formats together. It folds fullwidth digits to ASCII and looks
for 4–10 digits (including spaced digits) or 6–10-character codes with at least
three digits tied to a code cue. Codes of 5–10 characters mixing ASCII letters
and digits, or 6–10 uppercase letters only (except `INVALID` and `DISABLED`),
also match directly after a verification, security, login, sign-in,
authentication, or one-time code, PIN, or password cue, or an OTP, passcode, or
2FA cue. Confirmation cues keep the three-digit rule. A cue in one field can
describe a value in another, whereas years from 1900 through 2099 count only
after a cue in the same field. Sign-in links require an opaque token or UUID
plus a sign-in cue or credential query key such as `token` or `oobCode`,
excluding unsubscribe and preferences paths. A sign-in cue anywhere in the
message applies to every link with an opaque token, including click-tracked
links, so newsletters that mention signing in and carry tracked links are
withheld. Fields of 1 MiB or more are also withheld.

Without `verification.read`, downloads declared `message/*` or named `.eml` on
any authorized record of the hash return `403 verification_masked`. Other
text-class downloads, declared `text/*` or sniffed as text, inspect their bytes
and every authorized parent message of the hash. They return `403` if any
parent is withheld, the bytes contain a NUL, invalid UTF-8, or a
`Content-Transfer-Encoding:` header line in any letter case, or the detector
withholds the text or filenames. Text of 1 MiB or more is withheld, while
allowed text and binary files download unchanged. Withheld parents mask
filenames in metadata and download headers. Parent bodies and attachment
metadata load in batches, so a shared hash can be checked across more than
32,766 messages.

Because agent grants live in daemon memory, tokens issued before an upgrade
stop working when the daemon restarts. Replacement tokens with the same
permissions see masked verification messages and the attachment refusals above
unless you include `verification.read` in the grant.

A cue can precede its value by up to 60 runes, including blank lines, and a
period can end the cue line or separate the cue from a code ending the same
line. Up to six words, with one short parenthetical, can sit between a cue and
its value when the value follows `is`, `:`, or `=`, so ordinary phrases such as
`Error code from the processor: 50213` are withheld too.

These heuristics can miss unfamiliar wording and layouts, lowercase letter-only
codes, short link tokens, credentials inside binary files, or text stored under
a binary type that sniffing misses, and they do not detect non-English text
such as `Tu código es 482913`. Lists and threads classify a message whose body
exists only in its original MIME using subject, snippet, title, and filenames.

Search rows are masked in place without changing membership or counts.

Search can still confirm a withheld value through exact or prefix queries, and
draft output can include verification content; these known limits are covered
by later slices for search confirmation and draft quotes. Grant
`verification.read` only when the delegated reader should receive verification
content.
