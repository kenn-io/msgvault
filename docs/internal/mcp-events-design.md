---
title: MCP Events design
description: Approved native MCP Events design, implementation contracts, and rollout boundaries.
last_edited: "2026-10-06"
---

# MCP Events — Design

Status: READY, approved 2026-10-05 after four pre-implementation design reviews.
The source baseline below is `aa411818`; reconciliation at `9938aa951` adds the
owner HTTP managed-draft reader recorded in the capability matrix. This document
preserves the native Events design and unbuilt Phase 2 gates; the
[MCP guide](../usage/chat.md#events) owns current operating instructions.
It describes native support for the MCP Events webhook profile that ChatGPT shipped on
2026-09-29 ([OpenAI guide](https://developers.openai.com/plugins/build/mcp-events)),
built on the draft Triggers & Events extension. Source facts cite `main` at
`aa411818` plus the open pull requests cited below. The protocol choices
section records the reusable patterns evaluated for this design. Where an
exact guarantee cannot be made from today's data, the design simplifies
rather than adds machinery.

## Summary

An agent host (ChatGPT first) subscribes to one exactly-scoped slice of the
archive — one conversation, one calendar — and is woken by a signed webhook
each time msgvault durably commits an occurrence in that scope: a new message
or reaction, a calendar transition, a managed draft changing, and later a
Kata issue created from the archive or an attachment whose extracted text or
transcript became readable. Payloads carry identifiers only; the woken run
reads content, including attachments in chunks, with the tools it already
has plus the small read tools this design adds.

The daemon owns everything durable: a commit-ordered journal written inside
the persistence transaction, one subscriptions table with generation fences,
and a delivery service with one worker per active subscription. The
`msgvault mcp --http` process stays a stateless protocol adapter: it
advertises the `events` capability, validates the protocol surface, and
proxies `events/list`, `events/subscribe`, and `events/unsubscribe` to new
daemon routes through `daemonclient`, as it already does for every other
capability.

Work is phased. Phase 1 ships the families whose commit points and read
paths exist today: `msgvault.message_archived`,
`msgvault.calendar_event_changed`, and `msgvault.draft_changed` limited to
confirmed create, update, and delete transitions. Phase 2 ships
`msgvault.kata_issue_filed` and `msgvault.attachment_processed` behind
explicit readiness, occurrence, and recoverability gates. The feature is
off by default and adds a new egress class, so it needs the consents listed
under [Consent and rollout](#consent-and-rollout).

## Goals

- Wake a subscriber within seconds of the archive commit of an occurrence in
  one conversation or one calendar.
- Never emit for historical backfill, re-sync, label, reindex, attachment
  download, or projection work on existing rows.
- Keep every protocol promise honest: subscriptions survive restarts, event
  IDs and bytes are stable across retries, a cursor never skips an
  unsettled occurrence, and `truncated: true` is returned whenever
  continuity cannot be proven.
- Work identically on SQLite and PostgreSQL.
- Bound the cost of an agent waking itself, using only facts the archive can
  prove.
- Add no new content surface: webhooks carry IDs, timestamps, and flags only.

## Non-goals

- Account-wide or inbox-wide subscriptions, filtered or not.
- Polling, push streaming, `gap`/`terminated` control envelopes, or terminal
  callbacks. ChatGPT supports none of them and they are not advertised.
- An OAuth authorization server inside msgvault, or treating a forwarded
  bearer as a verified subject.
- Sending messages. msgvault never sends (`docs/usage/chat.md` in #1092).
- A `sent` draft event or deterministic exclusion of an agent's own replies.
  Neither can be proven from today's data: a Message-ID match does not show
  that a draft was sent, and every Phase 1 caller shares the owner key.
  Both wait for #666's send path (which appends its own occurrences) or a
  provider-confirmed sent predicate plus authenticated agent identities.
- RSVP changes (no normalized attendee projection exists) and Kata issue
  state changes (Kata holds the state and has no feed to msgvault).

## Existing behavior and invariants

What the design relies on, verified on `main` unless a PR is named:

- **Transport.** `internal/mcp/server.go` serves `/mcp` with go-sdk v1.7.0's
  `NewStreamableHTTPHandler` in `Stateless` + `JSONResponse` mode behind
  `bearerAuthHandler`, which captures one static token at construction. The
  inbound token defaults to the daemon's owner API key; `--http-token-file`
  and `--http-token-env` substitute an independent one
  (`cmd/msgvault/cmd/mcp.go`). Protocol `2026-07-28` and `server/discover`
  have been served since #566; `cachePolicyMiddleware` type-switches on
  `*sdkmcp.DiscoverResult` to set `TTLMs` and `CacheScope`. Requests on an
  older protocol lose the write tools. The SDK validates `Mcp-Method` and
  the other `Mcp-*` headers (`-32020`), rejects unsupported versions with
  `-32022`, and provides `AddReceivingCustomMethod`. `ServerCapabilities`
  has no `events` field (go-sdk#1325), so the top-level key must be injected.
- **Process split.** `cmd/msgvault/cmd/mcp.go` builds `ServeOptions` from a
  `daemonclient.Client`, gating each backend on `APISchemaVersionAtLeast`.
  `daemonclient` authenticates the second hop with its configured daemon API
  key or agent token, never with the inbound MCP bearer. The daemon's
  `humaAuthMiddleware` admits API-key, keyless-loopback, browser-session,
  and delegated (`X-Msgvault-Agent-Token`, `AuthModeDelegated`) callers
  (`internal/api/middleware.go`, `routes.go`).
- **Agent grants.** `internal/agentgrant`: `Grant{ID, Label, Permissions
  (draft.create|edit|delete, calendar.*), Sources []SourceRef{Type,
  Identifier, SenderKeys}}`, issued and revoked through
  `/api/v1/agent-tokens`. The registry is in-memory and process-scoped:
  grants do not survive a daemon restart. #1092 (slice 9a of #666) serves
  delegated MCP sessions over stdio only and refuses `--http`; #1023 is a
  competing stacked wiring (`getMCPCapabilities`, typed receipts,
  `--allow-draft-writes`). Both rely on the merged daemon-side grant model.
- **Persistence.** The low-level upsert is `upsertMessageWith`
  (`internal/store/messages.go`), but there is no universal call chain.
  Composite persistence (`persistMessageWith` with `MessagePersistData`)
  writes header, metadata, body, raw, recipients, and final attribution in
  one transaction; the Beeper importer commits the header first and body,
  raw, attachments, mentions, and reactions in later calls
  (`internal/beeper/importer.go`). Final `from_me` attribution can change
  within composite persistence. `UpsertReaction` is not transactional and
  `ReplaceReactions` deletes the whole set before reinserting. The
  `embedding_change_clock` singleton row is the repository's commit-ordering
  pattern (`schema.sql`, `dialect_pg.go`, `embedding_changes.go`).
- **Identity.** `messages.id` and `conversations.id` are integer primary
  keys; conversations are unique per `(source_id, source_conversation_id)`
  with `conversation_type` in `email_thread`, `group_chat`, `direct_chat`,
  `channel`, `calendar`. Reactions are rows in `reactions`, unique per
  `(message_id, participant_id, reaction_type, reaction_value)`, with
  `removed_at`; Beeper and Slack borrow the target message's timestamp for
  embedded reactions.
- **Calendar.** `internal/calsync` stores each Google Calendar event as a
  `messages` row (`message_type = calendar_event`, one source per calendar,
  `sent_at` = event start) and writes status, sequence, `ical_uid`, and
  series linkage to `messages.metadata`; `ingestEvent` persists message,
  metadata, body, raw event, recipients, and FTS in separate calls, and
  `flagCancelled` merges `status: cancelled` into existing metadata while
  preserving the other fields. `gcal.Event` carries `Created` and `Updated`.
  Attendee `responseStatus` is parsed but not persisted. Calendar
  write-through (`internal/calcontrol`) has no sync run. Emailed `.ics`
  invites are ordinary email attachments.
- **Provenance.** `sync_runs.sync_type` distinguishes `full`, `import-*`, and
  incremental runs, but Beeper and Slack runs contain mixed phases under one
  type and `Message` carries no provenance.
- **Drafts** (#880, #882, #1003, #1006, #1008 merged). Four tables share one
  lifecycle (`internal/store/draft_lifecycle.go`): `gmail_drafts`,
  `imap_drafts`, `beeper_drafts`, `chat_drafts` (local Slack/Teams/Discord
  text). Each has a stable opaque `draft_id`, `revision`, `discarded_at`,
  `pending_operation ∈ {edit, delete}`, and `pending_code`. Gmail and IMAP
  drafts are persisted after the provider accepted them (`CreateDraft`,
  `AppendDraft`) and archived immediately as `messages` rows
  (`current_message_id`, `is_from_me`, label `DRAFT`); `Publish*Replacement`
  and `Finish*Delete/Removal` advance `revision`; `Record*Outcome` records
  provider evidence codes without advancing it, and those codes
  (`append_rejected`, `cleanup_pending`, `removed`, `remote_unknown`, …) are
  not all uncertainty. Beeper drafts are inserted with a pending first write
  and confirmed by `FinishBeeperDraft` at revision 1; an aborted first write
  is discarded with a revision bump; the table stores `(source_id,
  chat_id)` and no `conversation_id`. `chat_drafts` carry `conversation_id`
  and are physically deleted. No table records who created a draft. There
  is no send path: `draft-send-as` only lists Gmail aliases, and the
  operator sends from their own client.
- **Kata.** The task-link integration (`[integrations.tasks]`,
  `internal/taskclient`, `/api/v1/messages/{id}/tasks`) writes `mail_links`
  metadata onto the Kata task after the remote call, keeps no local record,
  and its reverse index is an explicitly disposable cache. #1104 (open) adds
  `[integrations.kata]`: `POST /api/v1/integrations/kata/issues` with an
  `Idempotency-Key`, a request digest, and an action marker written into the
  issue metadata that `FindActionTask` finds on retry; `…/issues/{ref}/evidence`
  (up to 32 references, possibly from several conversations, no marker of
  its own); `…/evidence/prepare` with stable `PassageID`s; MCP tools behind
  `--allow-kata-writes`. msgvault keeps no local record of the issues.
- **Docbank** (#876, #939, #999 merged; #1077, #1094 open). Document text
  extraction is shared per canonical blob and profile:
  `ListDocumentExtractionCandidates` picks one representative occurrence and
  skips blobs with a compatible head, `PublishDocumentExtraction` publishes
  atomically (derivatives, `document_extractions.state = ready`, head
  switch, search revision), `FailDocumentExtraction` records a terminal or
  retried failure, and `document_occurrences` maps attachment → message →
  source. Stored audio is routed to Docbank through `beeper_media_occurrences`
  and `beeper_media_deliveries` keyed by `(destination_key, processing_key)`;
  `FinishBeeperMediaOperation` moves a delivery to `done` on any terminal
  result, success or failure. msgvault stores no transcript text; #1077's
  `GET /api/v1/messages/{id}/recordings` reads live from Docbank and treats
  only `EvidenceState == "ready"` as a transcript. `search_document_attachments`
  requires a nonempty query; no tool returns a whole extracted text.
- **Read tools.** `get_message` returns the message and its attachment list
  but neither `source_id`, `is_from_me`, nor calendar metadata
  (`internal/mcp/handlers.go`); `get_attachment {id, offset, length ≤ 4 MiB,
  sha256}` streams chunks (sha256 from the first chunk required for every
  later offset; whole objects up to 50 MiB); `list_thread` resolves a
  conversation from a message ID or a provider `thread_id` + `account`;
  `list_messages {conversation_id, …}` pages one conversation newest-first.
  Calendar control tools take Google calendar IDs, not archive sources.
- **Reusable pieces.** `internal/netguard` (`ProhibitedIP`,
  `ProhibitedHostname`, `ValidateTrustedDestination`: one origin paired with
  its pins), the pinned-IP dialer in `internal/remoteimage/fetch.go` (which
  follows redirects, so it is a pattern, not a drop-in),
  `internal/httpretry.RetryAfter`, HMAC use in `internal/daemonauth`, the
  owner-only key-file creation in `providercredentials`, the media scheduler
  that keeps network work outside the archive gate, and the modern-HTTP
  protocol tests in `internal/mcp/protocol_test.go`. `Scheduler.runJob`
  holds its work tracker — which includes the archive operation gate — for
  a whole job, so a delivery loop cannot be an ordinary scheduler job.

## Architecture and package ownership

```text
ChatGPT ──events/subscribe──▶ msgvault mcp --http ──daemonclient──▶ daemon API
   ▲                           (stateless adapter)                /api/v1/mcp/events/*
   │                                                                     │
   │                      persistence tx ─▶ mcp_event_log ─▶ mcpevents.Service (workers)
   └────────── signed webhook (direct outbound HTTPS from the daemon) ◀───┘
ChatGPT run ──get_mcp_event / get_message / get_attachment──▶ msgvault mcp ──▶ daemon
```

Dependency direction, kept narrow so Store never imports the service:

```text
cmd / api wiring -> mcpevents -> store, netguard, httpretry
internal/mcp -> EventsBackend interface <- daemonclient
store -> archive values and SQL only; never mcpevents, the MCP SDK, or HTTP
```

- `internal/store/mcp_events.go` owns journal append, the clock, subscription
  transitions, pruning, and backend-specific SQL. Journal collection and
  finalization are explicit steps of the shared persistence transaction;
  the transaction returns the appended sequences and the caller issues one
  nonblocking after-commit wake. No Store method opens a second transaction
  after a mutation to append an event.
- `internal/mcpevents` (a few files, not a package per helper) owns
  orchestration, callback transport and signing, retry decisions, secret
  handling, and the one place that canonicalizes closed arguments (the
  daemon revalidates at its trust boundary). It exposes a blocking
  `Run(ctx) error`.
- API handlers call the service; they assemble no transactions. Wire DTOs
  go through the API schema and generated client (`Makefile` generate
  target); protocol adaptation lives in `internal/mcp`.
- Startup passes narrow options (enabled flag, retention, sources, trusted
  callbacks, key path), never the whole config.
- No extraction to `go.kenn.io/kit` now. After two real consumers exist,
  extract only pure signing and verification or a narrowly specified
  callback transport. Test vectors and synthetic conformance fixtures are
  shared immediately.

## Principal and authorization

v1 is single-owner and the daemon is the only authority:

- Events requires the MCP inbound key to be the daemon's owner API key (the
  default). When `--http-token-file` or `--http-token-env` is set, the MCP
  process serves its existing tools but registers no `events/*` methods and
  advertises no `events` capability. The MCP process forwards no principal;
  the daemon derives it from the credential it authenticated.
- The Events routes require `AuthModeAPIKey` explicitly. Keyless-loopback,
  browser-session, and delegated callers receive `403` from the daemon and
  `-32012` from MCP. Delegated calendar authority is not permission to
  subscribe to the owner's archive.
- `principal_id = "owner:" + hex(SHA-256(owner key))[:16]`, computed by the
  daemon from the key it loaded at startup. Rotating the owner key is a
  daemon restart; after it, a stale MCP process fails with `401` at the
  daemon, every subscription of the old principal fails its next recheck
  and ends with reason `principal_revoked`, and its pending delivery is
  dropped.
- Authorization is rechecked at subscribe and refresh, before every dial,
  and before recording success: the principal matches the current owner
  key, the scope row exists and its source is not removed, the scope's
  `(family, source type)` is still enabled, and the feature is still on.

Agent principals (`agent:<grant.ID>`) use the same column and vocabulary but
are not served in Phase 1. Prerequisites, each a separate change outside
this design: a persisted grant registry, an HTTP delegated session (neither
#1092 nor #1023 provides one), and an `events.subscribe` permission checked
with `Grant.Allows` against the scope's source. No choice between the two
delegated MCP wirings is needed to ship the owner-key slice. Until then
delegated callers get `-32012`.

A gateway or tunnel that forwards a user bearer does not create a principal:
the static bearer checker rejects a different bearer, and hashing arbitrary
bytes authenticates nothing. A verified OAuth subject needs a trusted
verifier or a signed gateway assertion, which is additional scope.

## Event families

Names are prefixed `msgvault.`. Argument schemas are closed objects. Every
`events/list` description names the read tools for its payload and says
that `get_mcp_event` recovers a lost payload from the `eventId`.

### Scope: one conversation or one calendar

`conversation_id` is the msgvault `conversations.id`; a subscriber resolves
it with `list_thread` or reads it from any `get_message` / `list_messages`
result. `calendar_source_id` is a calendar's `sources.id`, listed by the new
`list_calendar_sources` tool. Subscribe validates existence, type, and that
the scope's `(family, source type)` is enabled and produces every
explicitly requested kind (`-32602`, `data.reason` one of `unknown_scope`,
`wrong_scope_type`, `source_not_capable`, `kind_not_capable`). Subject,
participant names, and bare provider IDs are never accepted. Account-wide
scope is rejected: every mailbox message would wake the run, label and
sender filters mean different things per source, and the run can filter
after a scoped wake.

### Wire rules shared by all families

- Archive IDs (messages, conversations, sources, attachments, participants)
  are decimal strings. Draft IDs are the opaque prefixed strings the store
  assigns. Kata issue references and Docbank identifiers are opaque strings.
- Set-valued arguments (`kinds`) are canonicalized before hashing: sorted,
  deduplicated, empty rejected (`-32602`), unknown member rejected. An
  omitted `kinds` canonicalizes to the literal `["*"]`, meaning every kind
  enabled for the scope's `(family, source type)` at delivery time, so
  catalog growth never changes a subscription's identity on refresh or
  unsubscribe.
- Every payload field is either required, nullable (`null` when the family
  has the field but this occurrence lacks a value), or omitted (field not
  defined for this kind); each family's payload table below says which.
- Journal selection for a subscription is by `(epoch, family, scope_kind,
  scope_id, seq > cursor_seq)` before the subscription's argument filters
  apply; several families share one conversation scope.

### Runtime capability matrix

The daemon exposes, and `events/list` reflects, an explicit matrix of
`(family, source type, kind, read tool)` entries enabled on this build and
configuration. A family is advertised only with the kinds whose commit point
and read path exist for at least one enabled source type; subscribe rejects
a scope whose source type is absent from the matrix for that family. Phase 1
entries:

| Family | Source types | Kinds | Read path |
|---|---|---|---|
| `message_archived` | `gmail`, `imap` first; others as their persistence meets the ready boundary | `message`, `reaction` | `get_message`, `list_thread`, `get_attachment` |
| `calendar_event_changed` | `gcal` | `created`, `updated`, `cancelled` | `get_message` with the calendar projection |
| `draft_changed` | `gmail`, `imap` | `created`, `updated`, `deleted` | `get_message` on the archived draft row; `draft_get` on the managed draft |
| `draft_changed` | `beeper`; local chat drafts (`draft_kind: chat`) on `slack`, `slackdump`, `teams`, `discord` | `created`, `updated`, `deleted` | `draft_get` on owner HTTP MCP; enabled only where the confirmed producer and reader both exist |

Current `main` includes the owner HTTP `draft_get` reader for these managed
drafts. Their Phase 1 read gate is met; when their source type is enabled in
`[mcp.events].sources`, advertise the implemented draft kinds. This does not
enable Beeper message capture, its media-ready boundary, or any Phase 2 family.

A Gmail message capability says nothing about drafts; a Beeper message
boundary says nothing about transcripts. Validation uses the actual source
and the selected kinds: subscribe requires every explicitly requested kind
to exist for the scope's `(family, source type)` and otherwise returns
`-32602` with `data.reason: kind_not_capable`; the all-kinds marker means
the kinds enabled for that scope's source, not every kind enabled anywhere;
delivery and the coverage fingerprint apply the same interpretation. The
matrix is ordinary code and configuration, not a schema.

### Phase 1: `msgvault.message_archived`

A message or reaction newly committed in one conversation. "Archived", not
"received": archive time lags provider arrival by the sync interval.

Arguments: `conversation_id` (required), `include_from_me` (default
`false`), `include_reactions` (default `true`).

| Field | `message` | `reaction` |
|---|---|---|
| `kind`, `conversation_id`, `source_id`, `from_me` | required | required |
| `message_id`, `sent_at`, `archived_at` | required (`sent_at` nullable) | omitted |
| `target_message_id`, `reaction_key`, `reactor_participant_id`, `reacted_at` | omitted | required |

- `kind: message` is appended once per archived message, at the ready
  boundary below. Edits, labels, read flags, soft deletes, attachment
  downloads, FTS, and projections never emit. A message deleted before
  delivery still yields its retained occurrence. Event-bound reads return
  the referenced message's current state or report it unavailable; they
  never resolve a reused archive ID to another message. The occurrence is
  not a historical content snapshot.
- `kind: reaction` has no `message_id` because a reaction row is not a
  message. `reaction_key` is the emoji or tapback name. One occurrence per
  `(target, reactor, type, value)` within retention; a removal and re-add
  inside that window emits nothing again, and removals never emit.
  `reacted_at` is the real reaction time when the source gives one, else
  the observation time; borrowed target timestamps are never used.
- `from_me` is the message's final `is_from_me` or, for reactions, whether
  the reactor is a confirmed owner identity of the source (the
  `identity_is_from_me` predicate). `archived_at` is the timestamp the
  persistence transaction recorded, not an exact database commit time.

### Phase 1: `msgvault.calendar_event_changed`

A calendar object created, updated, or cancelled in one synced calendar.

Arguments: `calendar_source_id` (required).

Payload (all required unless noted): `kind` (`created` | `updated` |
`cancelled`), `message_id`, `conversation_id` (the series conversation),
`source_id`, `ical_uid` (nullable), `sequence` (nullable), `starts_at`
(nullable: RFC 3339, or `YYYY-MM-DD` when `all_day` is true; `null` for a
cancellation of an object whose start was never seen — the observation
time is never substituted for an unknown scheduled start, and a known
start is preserved on cancellation), `all_day` (boolean; `null` when
`starts_at` is `null`), `changed_at` (provider `updated` when present, else
observation time), `from_me` (the owner is the organizer; it says nothing
about who made this change, so the own-message filter and guard do not
apply to this family). The current-state calendar projection on
`get_message` uses the same nullability.

Entity identity and occurrence identity are separate: the message ID names
the object, each transition is a new journal sequence, and there is no
uniqueness constraint on calendar rows. One Store operation,
`PersistCalendarEvent`, owns the classification after an atomic old/new
comparison under the object's row lock, and calendar rows are excluded from
the generic message producer:

- `created`: no prior row.
- `updated`: prior row exists and at least one of status (other than to
  `cancelled`), start, end, all-day flag, or time zone differs, or
  `sequence` increased. A sequence decrease or reset alone is not an update.
  Identical redelivery (incremental sync, write-through followed by sync)
  emits nothing because the persisted state already equals the new state.
- `cancelled`: status becomes `cancelled`. A cancellation for an object
  never seen produces one `cancelled` occurrence, not `created` plus
  `cancelled`, through the sparse-tombstone path that preserves prior
  details. Cancel → restore → cancel produces three occurrences.
- Full syncs are muted by provenance. RSVP is outside the comparison and
  outside the advertised enum; "invites from or to a person" is a later,
  person-scoped family.

### Phase 1: `msgvault.draft_changed`

A managed draft in one conversation was confirmed created, updated, or
deleted. This is what msgvault can prove about outbound work today: drafts
prepared through the CLI, Web UI, or the MCP draft tools (#1092 or #1023),
and sent by the operator from their own client.

Arguments: `conversation_id` (required), `kinds` (set of `created`,
`updated`, `deleted`; default all).

| Field | Rule |
|---|---|
| `kind`, `draft_id`, `revision`, `draft_kind` (`gmail` \| `imap` \| `beeper` \| `chat`), `conversation_id`, `source_id`, `changed_at` | required |
| `message_id` (the archived draft row) | required for `gmail`/`imap`; omitted for `beeper`/`chat` |
| `created_by` (`owner` \| `session` \| `agent:<grant id>` \| `unknown`) | required; creator of the draft, never the actor of this transition |

Transition matrix. Only confirmed transitions emit; every emitting
transition advances `revision`, so the occurrence key
`draft:<draft_id>:<revision>:<kind>` is unique per transition.

| Draft kind | `created` | `updated` | `deleted` | Never emits |
|---|---|---|---|---|
| Gmail | `PersistGmailDraft` (after `CreateDraft` succeeded; archived row exists) | `PublishGmailDraftReplacement` (revision + 1) | `FinishGmailDraftDelete` (`discarded_at`, revision + 1) | claims, aborts that prove no effect, `RecordGmailDraftOutcome` codes, sync observations (`AdoptGmailDraftObservation`) |
| IMAP | `PersistIMAPDraft` (after `AppendDraft` succeeded) | `PublishIMAPDraftReplacement` | `FinishIMAPDraftRemoval` | claims, aborts, `RecordIMAPDraftOutcome` codes, recovery steps that publish nothing new |
| Beeper | first `FinishBeeperDraft` confirming text at revision 1 | later `FinishBeeperDraft` for an edit (revision + 1) | `FinishBeeperDraft` for a delete (`discarded_at`, revision + 1) | the pending first write (`CreateBeeperDraft`), its abort (discarded without ever being confirmed), claims |
| Local chat | `CreateChatDraft` | `UpdateChatDraft` | `DeleteChatDraft`: scope, creator, and last revision are copied into the occurrence inside the delete transaction; a later `draft_get` returns not-found while `get_mcp_event` still resolves the occurrence | — |

- Uncertain provider outcomes are not a Phase 1 kind. `Record*Outcome`
  codes mix definitive rejections, cleanup markers, and unknown outcomes and
  do not advance the revision, so a kind built on them would either
  misreport known outcomes or collapse repeated attempts. If an
  `uncertain` kind is wanted later, it follows the calendar pattern: a
  locked transition into a defined uncertainty state with its own sequence
  and no revision/code uniqueness.
- Beeper scope resolution: `(source_id, chat_id)` → `conversations WHERE
  source_id = ? AND source_conversation_id = ?`. When no archived
  conversation exists the transition is not journaled; no phantom
  conversation is created. Beeper and local-chat kinds stay unadvertised
  until an owner HTTP MCP draft reader exists.
- `created_by` comes from a new nullable `created_by_principal` column on
  the four draft tables, set by the daemon from the authenticated mode of
  the creating call (`owner` for API-key callers, `session` for the Web UI,
  `agent:<grant id>` for delegated tokens); legacy `NULL` is reported as
  `unknown` and never backfilled by inference. It is provenance for the
  run to read, not an authorization or filtering input.
- The sent transition is not observed. A live `from_me` email with a
  draft's Message-ID may be the still-unsent draft (Gmail persists drafts
  with the `DRAFT` label) or an edited copy; a provider-confirmed sent
  predicate (Gmail label state, IMAP sent-mailbox observation with draft
  state excluded) is a separate design, and #666's send path appends its
  own occurrences when it exists.

### Phase 2: `msgvault.kata_issue_filed`

A Kata issue created from one conversation through msgvault (#1104). A
remote write followed by a local append is not a transaction, so the family
gets a durable action record with recovery defined per kind, and advertises
only the kind whose recovery is sound today.

Arguments: `conversation_id` (required), `kinds` (set; default all).

Payload (required unless noted): `kind`, `action_id`, `issue_ref`,
`project`, `conversation_id`, `references` (the bounded list of
`{message_id, attachment_id (nullable), passage_id}` entries of this
conversation), `created_by`, `changed_at`.

- `mcp_action_records(id, kind, idempotency_key, request_digest,
  principal_id, state ∈ {pending, confirmed, unknown, abandoned},
  issue_ref, project, references JSON, created_at, settled_at)`, with
  `UNIQUE(kind, idempotency_key)`. The handler inserts the record as
  `pending` before the remote call, reusing #1104's archive-scoped
  idempotency key and request digest; a reuse with a different digest is
  rejected locally as #1104 does remotely.
- `created`: after Kata confirms, one transaction marks the record
  `confirmed` and appends one occurrence per affected conversation,
  keyed `action:<action_id>:<conversation_id>` (a request citing several
  conversations fans out, each occurrence carrying that conversation's
  references). Recovery is lookup-only; the record stores no original
  create input (`title`, `brief`, `list`, `person_id`) and the daemon never
  retries a create in the background. A crash between the remote call and
  the confirming transaction leaves `pending`; the maintenance pass looks
  the marker up with `FindActionTask` and, exactly as #1104's `replay`
  does, compares the issue's recorded request digest with the record's:
  marker found and digest equal → `confirmed` plus the append; marker
  found with a different digest → `unknown`, never confirmed against that
  issue; marker absent, issue deleted, or invisible → `unknown` (the issue
  may have existed, or the call may still be in flight); `abandoned` only
  when Kata's answer to the original call was an explicit request
  rejection that produced no issue. An `unknown` record is surfaced by the
  status command; the caller retries explicitly through #1104 with the
  original input, whose idempotency returns the existing issue, and that
  call confirms the record. Recovery limit, stated plainly: a remote
  success whose only receipt (the marked issue) disappears before
  confirmation is not reconstructible and emits nothing.
- `evidence_added` (#1104's link route) and the legacy task-link
  `linked`/`unlinked` have no operation receipt or marker upstream, and the
  reverse index cannot say which cycle completed. They are not advertised
  until the upstream path gains a recoverable receipt; the family and its
  record are designed to add them then.
- Read path: the issue lives in Kata; the cited passage is read with
  `get_message` or the Phase 2 text reader. Issue state changes remain a
  Kata-side events dependency.

### Phase 2: `msgvault.attachment_processed`

An attachment of a message in one conversation whose derived content became
readable, or whose processing failed terminally. Use case: a voice note
arrives (`message_archived`), and later its transcript exists
(`attachment_processed`), so the agent reacts to the content.

Arguments: `conversation_id` (required), `kinds` (set; default all
advertised).

Payload (required unless noted): `kind`, `message_id`, `attachment_id`,
`conversation_id`, `source_id`, `profile`, `changed_at`, plus
`extraction_id` (text kinds, else omitted) or `docbank_occurrence_id` and
`content_version` (media kinds, else omitted).

Durable live eligibility: a message admitted under live provenance while
its `(family, source type)` is enabled gets a row in
`mcp_live_admissions(message_id PK, source_id, message_reference_seq, epoch,
admitted_at)` inside its persistence transaction. Admission persists the
current message reference. Asynchronous processing emits for an occurrence
only when its message has an admission row in the current epoch whose
`message_reference_seq` matches the current message reference; backfill
imported after the epoch started has no row and stays muted, and a delayed
extraction does not need an expired journal row to prove eligibility.

| Kind | Commit points | Occurrence key (database-unique) | Read path |
|---|---|---|---|
| `text_extracted` | (a) `PublishDocumentExtraction`: one append per eligible `document_occurrences` row of the published blob and profile; (b) a newly eligible occurrence binding to an already-ready head (the reconcile/candidate path that skips blobs with a compatible head) | `extraction:<extraction_id>:<occurrence_key>` | `get_document_text {attachment_id, offset, limit}` (new, bounded and paginated); `search_document_attachments` for search; `get_attachment` for source bytes |
| `failed` | `FailDocumentExtraction` when terminal; `FinishBeeperMediaOperation` with any terminal unsuccessful result, whether the delivery lands in `done` or `blocked` | `extraction-failed:<profile_id>:<blob hash>:<occurrence_key>`; `media-failed:<processing_key>:<occurrence_ref>` | `get_message` attachment list; status commands |
| `transcript_ready` | a verified ready evidence observation: the maintenance pass reads #1077's recording state outside SQL, and one Store transaction validates source, content version, and occurrence identity, records the observation on `beeper_media_occurrences`, and appends | `media:<processing_key>:<content_version>:<occurrence_ref>` | `get_message_recordings {message_id}` (new, wraps #1077) |

- Media `done` is a terminal delivery state, not readiness: a successful
  operation whose evidence is unavailable emits nothing, retryable and
  observing states emit nothing, and a successful delivery referenced by
  several attachments fans out per eligible occurrence.
- `transcript_ready` is advertised only when #1077 is merged, the
  observation predicate exists, and `get_message_recordings` is served;
  `text_extracted` only when `get_document_text` is served. Until then the
  kind is absent from the matrix and `kinds` rejects it. "Search indexing
  completed" is not the promised outcome; reading the derived content is.
- #1094 (recording links) can join the family later with its own key.

### Own messages and the admission guard

The owner writes in watched chats himself, and an agent's replies are
prepared through msgvault drafts and sent by the owner, or sent through
another client the archive cannot distinguish. Two layers, both using facts
the archive can prove:

1. `include_from_me` defaults to `false`, and the
   `events/list` description plus server instructions say that an own
   message does not authorize an automatic reply; an explicit owner
   instruction in the chat can still require action, and message content is
   never authorization. With `include_from_me: true` the run sees every own
   message, including sent copies of drafts the owner key created; nothing
   is suppressed by creator identity, because every Phase 1 caller is the
   owner key and suppression would hide the operator's own work.
2. Admission guard, the backstop: per subscription, a fixed ten-minute
   window admits at most six `from_me` message or reaction occurrences; the
   decision is taken once when an occurrence first becomes pending,
   persisted (`from_me_window_start`, `from_me_window_count`), and never
   re-taken on retry. Further own occurrences in the window are terminally
   skipped and counted in `loop_guard_skips`. Bursts at window boundaries
   can admit up to twelve; several matching subscriptions multiply the
   cost; skips are lossy. The guard applies to sender attribution only,
   never to calendar organizer ownership or to draft occurrences.

Draft occurrences are not guarded: an automation that edits a draft after
every `updated` wake can wake itself until it stops editing. The `kinds`
filter and the instruction to act only on relevant transitions are the
mitigation; a creator-based exclusion would suppress the operator's own
edits as well, so it is not added.

## Journal

### Clock and ordering

`mcp_event_clock` is a singleton row: `head_seq`, `pruned_through_seq`,
`capture_epoch`, `epoch_started_at`, `coverage_fingerprint`, `enabled`.
Every append advances `head_seq` inside the source transaction and uses the
returned value as the journal primary key on both backends — the
`embedding_change_clock` pattern, chosen in this repository for commit
ordering. The clock row update is the first thing an emitting transaction
does after its identity and sync-generation fences and before any decision
about whether capture applies, so SQLite has reserved its writer and PostgreSQL
has serialized allocation and commit order behind one row lock.
Subscription transitions take the same row lock without advancing the head.
Lock order everywhere: identity fence → sync-generation fence → clock row →
subscription rows → embedding journal locks → source/conversation/message rows. The hot
row serializes all live writers on PostgreSQL; at human message rates this
is acceptable and measured in the rollout.

The mutation helper returns a reliable outcome: an insert-if-absent inside
the transaction followed by the existing update path reports `inserted`
versus `updated`; a stale prior read is never treated as the insertion
result. Calendar and reaction comparisons run under the same transaction and
row locks. All of this stays in Store.

### Log schema

`mcp_event_log`: `seq` (PK from the clock), `epoch`, `family`, `kind`,
`scope_kind`, `scope_id`, `item_key`, `message_id`, `conversation_id`,
`source_id`, `attachment_id`, `message_reference_seq`, `from_me`, `occurred_at`, `recorded_at`,
`data` (payload JSON encoded once). No foreign keys and no cascade: a
retained occurrence outlives its message. Indexes: `(epoch, family,
scope_kind, scope_id, seq)`, `(source_id)` for source removal, and a
partial unique index on `(family, scope_kind, scope_id, item_key)` for the
`reaction` kind and the `draft`, `kata`, and `attachment` families, which
makes every repeatable publication path idempotent at the database.
Messages are deduplicated by the insert outcome; calendar transitions are
intentionally repeatable and excluded from the index.

All eligible live occurrences are journaled while Events is enabled,
filtered at delivery. This removes subscriber-dependent capture gaps and the
per-write subscription lookup, at the cost of journaling every live message
for seven days, which the consent list names.

Archive message IDs may be reused after physical deletion. For every
message-backed occurrence, Store records an Events-owned reference in
`mcp_event_message_refs`: `message_id` is a foreign key to `messages` with
`ON DELETE CASCADE`, and `reference_seq` is the message's first captured
journal sequence. The reference survives edits but cannot survive physical
deletion or recur for a replacement row: the journal clock never reuses a
committed sequence. The log retains `message_reference_seq` without an
archive foreign key. Reaction deduplication includes this reference, and
asynchronous live-admission checks match it before processing a message.
Reactions reference their target message; calendar and archived email
draft occurrences reference their archived message when one exists.

Events recipes pass `{id, event_id}` to `get_message` on every page. This
optional event-bound form authorizes the receipt and requires its message
ID and reference to match the current archive row. Store checks the
reference and reads the message, body, recipients, labels, and attachments
in one consistent database snapshot. Missing or mismatched references
return unavailable. The internal reference is not a payload field; callers
use the signed receipt. Reads without `event_id` retain their existing
behavior.

### Provenance and ready boundary

Every emitting mutation receives an explicit immutable
`IngestContext{Mode ∈ {live, backfill, unknown}, ObservedAt}` carried
through each source phase and composite persistence; `unknown` is muted. It
is never inferred from the currently running sync: Beeper and Slack runs
mix history and live phases, calendar write-through has no run, and
reconciliation inside an incremental run is not its live delta. `ScopedToSync`
views keep their generation fence and may carry the context immutably. Live
admission is also persisted (`mcp_live_admissions`) so asynchronous Phase 2
work inherits it after the context is gone.

The ready boundary is the commit of the core archived message — header,
body, relevant metadata, recipients, final attribution — in one
transaction, with the journal append after those writes. Each source family
becomes Events-capable only when its persistence meets that boundary:
composite `MessagePersistData` sources first (Gmail incremental, IMAP
incremental), calendar after `PersistCalendarEvent` exists (full projection
in one transaction, sparse-cancellation merge under the same transaction,
Google parsing kept in `calsync`), Beeper only after its header-first
persistence is moved onto the composite boundary, which follows the email
slice. Network requests and media downloads stay outside transactions;
attachments are readable only when already archived, with the existing
unavailable/retry behavior, and the design does not promise every
attachment byte at the first wake.

Reactions: the old and new sets are diffed inside one transaction before
replacement; only additions under live provenance emit, and a newly
backfilled target gets a muted baseline. `UpsertReaction` becomes
transactional with the same context.

Event time per family (the envelope `timestamp`, always present): message
`sent_at` when the provider supplied one, otherwise `archived_at` (the
recorded observation); reaction real time or observation time; calendar
provider `updated` or observation time; drafts and Phase 2 families the
transaction's observation time. Payload time fields keep their own
nullability. No age cap is applied: provenance is the rule.

### Retention, epochs, and coverage

- Retention is seven days of `recorded_at`, hard. `recorded_at` is not
  monotonic in `seq` (a transaction can start earlier and take the clock
  lock later), so pruning may delete holes. In the pruning transaction,
  rows with `recorded_at` older than the cutoff are deleted and
  `pruned_through_seq = max(previous floor, highest seq deleted)`, even when
  the final row is deleted. Rows at or below the floor that survive remain
  physically retained for `get_mcp_event`, but they never make an older
  cursor safe: any cursor with `seq < pruned_through_seq` truncates. This
  can truncate a scope that lost nothing, which the design permits; it
  never crosses a deleted hole silently. An old provider `occurred_at`
  under live provenance never expires immediately.
- Cursors are bound to `(subscription_id, epoch, seq)`. A cursor from an
  older epoch, or with `seq < pruned_through_seq`, truncates: the
  subscription restarts at `head_seq` and the result says `truncated: true`.
  A conservative global boundary may report an unnecessary gap; it never
  conceals one.
- `coverage_fingerprint` is a hash of the enabled `(family, source type,
  kind)` matrix plus the enabled flag. At daemon startup the configured
  fingerprint is compared with the persisted one under the clock lock: a
  difference increments `capture_epoch` once, records the new fingerprint,
  stops every active subscription with reason `capture_gap` (generation
  advanced, pending cleared, settled cursor and epoch preserved for a
  truncating refresh); an identical fingerprint changes nothing. Starting
  with Events disabled after a run with it enabled is such a difference and
  is the only administrative transaction that takes the Events clock lock
  while the flag is off.
  Ordinary same-configuration restarts never change the epoch.
- Delivery is epoch-fenced: `PrepareMCPDelivery` and `FinishMCPDelivery`
  require `cursor_epoch == capture_epoch` and select journal rows of that
  epoch only; a pending delivery from an older epoch is invalid and an old
  worker's success cannot cross the boundary.
- `PrepareMCPDelivery` re-reads the current floor before selecting each
  occurrence; a subscription whose settled cursor is below
  `pruned_through_seq` is stopped with reason `retention`, its cursor
  preserved, and pruning stops such subscriptions under the same clock
  fence. The next refresh returns `truncated: true` and the head. The
  worker never skips to the head on its own.
- Source removal deletes that source's log and admission rows and ends
  subscriptions in its scope with reason `scope_removed`. This cleanup also
  runs inside the existing source-removal transaction when capture is
  disabled; it takes no Events clock lock in that case. Expired and ended
  subscriptions are purged after the 24-hour read grace.

## Subscriptions and cursors

### Identity and validation

`events/subscribe` validates arguments against the closed schema (`-32602`),
requires `delivery.mode: "webhook"` (`-32014`), an `https` URL on port 443
or 8443 with no credentials or fragment, and a `whsec_` secret decoding to
24–64 bytes, then authorizes the principal and scope. The subscription ID
is `sub_` + hex SHA-256 over length-prefixed `(principal, name, canonical
arguments, url)`. A `cursor` that is omitted or JSON `null` means "no
cursor". TTL: default and maximum 24 h; `ttlMs: null` is granted 24 h with a
finite `refreshBefore`. The result is `{id, refreshBefore, cursor, truncated}`.

Verification: when no durable verification exists for exactly
`(subscription_id, secret_revision)`, the callback is challenged outside any
transaction — POST `{"type":"verification","challenge":<32 random bytes,
base64url>}` with `webhook-id: msg_verification_<random>`, the Standard
Webhooks headers, and `X-MCP-Subscription-Id`; 2xx, a body of at most 4 KiB,
and a constant-time match within 10 s are required. Failure returns `-32015`
with `data.reason` in `connection_refused`, `timeout`, `tls_error`,
`http_4xx`, `http_5xx`, `challenge_failed` and leaves any existing
subscription untouched. There is no cross-subscription verification cache.
Capture and replay start at activation, never at challenge time.

### Transition rules

All transitions are Store commands with private transactions
(`ActivateMCPSubscription`, `PrepareMCPDelivery`, `FinishMCPDelivery`,
`EndMCPSubscription`). Each activation runs under the clock row lock, reads
`head_seq`, `capture_epoch`, and `pruned_through_seq` from the same
snapshot, and counts the principal's active rows there; two concurrent
activations therefore serialize, and the 64-active quota (`-32013`) cannot
be exceeded by a race. A candidate verified against generation G whose row
is no longer at G fails with a retryable `-32013`, `data.reason:
concurrent_update`; it never overwrites a newer rotation.

| # | Existing row | Request | Result |
|---|---|---|---|
Generation invariant: `generation` is monotonically increasing for the
lifetime of a retained row and is never reset or reused. It is 1 only when
no row exists for the ID. Rotations, replays, and reactivations set
`generation = previous + 1`; an active renewal with the same secret and no
cursor preserves the generation under rule 3. A challenged candidate records the
predecessor it was verified against — `(no row)` or `(state, generation,
secret_revision)` — and activation requires that exact predecessor to still
hold; otherwise `concurrent_update`. A candidate lives only for its subscribe
request (seconds) and workers are cancelled and joined when their
generation ends, while purge happens at least 24 h after a row ends, so no
candidate or worker can span a purge and meet a recreated row at
generation 1.

| # | Existing row | Request | Result |
|---|---|---|---|
| 1 | none | no cursor | fresh activation: `generation = 1`, `cursor_epoch = capture_epoch`, `cursor_seq = head_seq`, pending empty, verification required |
| 2 | none | cursor | as 1, then the cursor is validated (MAC, same subscription, epoch, floor, `seq ≤ head_seq`); invalid → `-32602`; below floor or old epoch → start at head with `truncated: true`; else `cursor_seq` = cursor |
| 3 | `active`, same secret | no cursor (TTL renewal) | `expires_at` extended; generation, pending snapshot, attempts, due time, and worker untouched, even mid-send |
| 4 | `active`, new secret | no cursor (rotation) | candidate verified against `(active, G, R)`; atomically `secret_revision = R + 1`, previous secret kept until +60 s, `generation = G + 1`, pending snapshot rebound to the new generation with identical envelope bytes, event ID, attempts, and due time; the old worker is cancelled and its completion is stale; only signatures change |
| 5 | `active` | cursor (explicit replay) | validated as in 2; `generation + 1`, pending cleared, `cursor_seq` = cursor, worker restarted; combined with a new secret, rule 4's secret steps apply in the same transaction |
| 6 | `expired` within the 24 h grace | no cursor or cursor | reactivation: `generation + 1`, pending cleared, `expires_at` reset, settled cursor resumed subject to epoch and floor checks (truncation possible); a supplied cursor is validated as in 2; attempts restart; the receiver sees the same bytes again because envelopes are deterministic. A new secret is verified for this candidate and installed atomically with `secret_revision + 1` in the same transaction, and it signs the next request; an unchanged secret reuses its durable verification |
| 7 | `stopped`, reason `retention` or `capture_gap` | no cursor or cursor | reactivation: `generation + 1`, pending cleared, `expires_at` reset, start at `head_seq` in the current epoch with `truncated: true`; a supplied cursor is ignored because it is known-invalid; secret handling as in rule 6 |
| 7a | `unsubscribed` or `gone` (retained, within the 24 h grace) | no cursor or cursor | re-subscribe: `generation + 1`, pending cleared, `expires_at` reset, verification required for the supplied secret (a `410` receiver is revived only this way); the cursor is validated as in 2, else start at head |
| 8 | `stopped`, reason `principal_revoked` or `scope_removed` | any | `-32012` or `-32602 unknown_scope`; a differently keyed, authorized request is a fresh activation under rule 1 or 2 |
| 9 | any | `events/unsubscribe` | matches `(principal, name, arguments, url)`; ends the row (`unsubscribed`, `generation + 1`, pending cleared), cancels future attempts and an in-flight request, returns `{}`; succeeds when nothing matches |

Completion rule: `FinishMCPDelivery` applies only when the row is `active`,
`generation` equals the worker's generation and `pending_generation`,
`pending_seq` matches, and `cursor_epoch == capture_epoch`; any other
result is discarded. Bytes already written to the network cannot be
retracted; a late 2xx after any of rules 4–9 is discarded by this rule.

### Cursor semantics

`cursor_seq` is settled scan progress: every earlier row has been
acknowledged, excluded by the subscription's filters, or terminally dropped
under a documented policy (loop-guard skip, `413`, exhausted retries, which
status reporting distinguishes from acknowledgement). The cursor inside
pending event N denotes progress through N if the receiver accepts it; the
persisted cursor stays behind N until settlement. Delivery is at-least-once
per attempt sequence, not unconditional: retries can duplicate and bounded
retries allow terminal loss, both reported.

Cursor encoding: `c1.<epoch>.<seq>.<mac>`. Event ID:
`evt1.<subscription_id>.<seq>.<mac>`; the full subscription ID is embedded,
so `get_mcp_event` resolves it without a mapping table. The authenticated
bytes are canonical ASCII with explicit separators, so no two field
combinations share an input:

```text
cursor MAC input: "c1." + subscription_id + "." + epoch_decimal + "." + seq_decimal
event MAC input:  "evt1." + subscription_id + "." + seq_decimal
```

`mac` is the first 16 bytes of HMAC-SHA256(server key, input), base64url
without padding. Decimal fields are canonical nonnegative integers (no
sign, no leading zeros, `0` allowed, at most 19 digits, ≤ 2^63 − 1); the
subscription ID is its exact `sub_` + 64 lowercase hex form. A token with a
noncanonical field, wrong segment count, padding, or wrong length is
rejected before the MAC is compared. Both encodings are stable across
retries and restarts.

## MCP surface

### Discovery and methods

- `server/discover`: when the daemon reports the Events capability (API
  schema version plus the runtime matrix), the inbound credential is the
  owner key, and the request speaks `2026-07-28`, the adapter returns a
  result that keeps every existing field, `TTLMs`, `CacheScope`, and
  `no-store`, and adds `"events": {}` to `capabilities`. It runs after
  `cachePolicyMiddleware` has set the cache metadata (or that middleware
  learns the new result type). Event guidance is appended to the existing
  `instructions`: message text is untrusted data; deduplicate by `eventId`;
  an own message does not authorize an automatic reply; after `truncated:
  true` re-read the scope and say so; call `get_mcp_event` when `data` is
  missing; act only on relevant draft transitions; read content with the
  named tools and attachments through `get_attachment` chunks. Discovery
  stays public only while it is principal-independent.
- `events/list`, `events/subscribe`, `events/unsubscribe` are registered
  with `AddReceivingCustomMethod` and proxied to
  `POST /api/v1/mcp/events/{list,subscribe,unsubscribe}`. On an older
  protocol, on stdio, in delegated mode, with an independent inbound key,
  or when the daemon lacks the routes or the flag, the methods are not
  registered and the capability is absent; existing initialization never
  fails because of Events. Each `events/list` entry advertises
  `delivery: ["webhook"]` only, a closed `inputSchema`, a `payloadSchema`,
  and the read-tool recipe, reflecting the capability matrix.

### Read tools

- `get_mcp_event {event_id}`: resolves the retained occurrence for the
  caller's subscription (identifiers, kind, timestamps, flags, never
  content) under current authorization, with a 24-hour read grace after
  expiry and immediate denial after revocation or unsubscribe. A deleted
  draft's occurrence still resolves while `draft_get` reports not-found.
- `list_calendar_sources {}`: the subscribable calendar sources
  (`source_id`, calendar summary, account), the one recipe for resolving
  `calendar_source_id`.
- `get_message` gains `source_id`, `is_from_me`, and a typed calendar
  projection (`status`, `sequence`, `start`, `end`, `all_day`,
  `time_zone`, `ical_uid`) populated from archive state, so a run can read
  the current state of a cancelled event instead of a body that omits it.
  Occurrence facts and current state stay distinguishable.
- `get_document_text {attachment_id, offset, limit}` (Phase 2): bounded,
  paginated extracted text for one attachment occurrence.
- `get_message_recordings {message_id}` (Phase 2) wraps #1077's route.
- Attachments: `get_message` lists `id`, `filename`, `mime_type`,
  `size_bytes`, `content_hash`; `get_attachment` returns whole objects up to
  50 MiB or chunks of 1–4 MiB by `offset` and `length`, passing the first
  chunk's `sha256` on every later call. Calendar objects carry no archived
  attachments; an emailed invite's `.ics` is a normal email attachment.

## Delivery

`mcpevents.Service.Run(ctx) error` is started and joined by daemon lifecycle
wiring, not registered as a scheduler job. A supervisor reconciles the set
of active subscriptions, starts one worker goroutine per active
subscription with one in-flight event each, cancels workers whose
generation ended, and runs a bounded maintenance pass (expiry sweep every
minute, pruning hourly, Phase 2 action-record reconciliation and readiness
observation). Workers drain ready occurrences until a retry deadline,
expiry, or an empty queue, then wait on a coalesced wake or timer; one
subscriber's I/O never stalls another. Wakes are nonblocking hints sent
after a successful outer commit; startup and the one-second reconciliation
scan recover lost hints. Callback verification and delivery run outside
Store transactions and outside the archive operation gate, which is taken
only around the short Store steps. Workers are cancelled and joined before
Store closes. The runbook preserves the single-daemon-per-archive invariant
that makes leases unnecessary.

Per occurrence:

1. `PrepareMCPDelivery`: recheck authorization and the epoch, read the next
   journal row by `(epoch, family, scope_kind, scope_id, seq > cursor_seq)`,
   apply the subscription's filters and the admission guard (settling
   skipped rows), build the envelope `{eventId, name, timestamp, data,
   cursor}` with `timestamp` = the family's event time, and persist its
   bytes as the pending event with the current generation. Every retry
   sends identical bytes.
2. POST at most 262144 bytes with `Content-Type: application/json`,
   `webhook-id` = `eventId`, `webhook-timestamp` in Unix seconds,
   `webhook-signature` = `v1,` + base64 HMAC-SHA256 over
   `id.timestamp.body` under the decoded `whsec_` key (two space-separated
   signatures for 60 s after rotation), and `X-MCP-Subscription-Id`.
   Receipt is decided from the status line: any 2xx is success.
3. `FinishMCPDelivery` under the completion rule above. Success: recheck
   authorization, settle the cursor, clear the pending event, reset
   attempts. `410`: end the subscription (`gone`). `413`: terminal drop of
   that event (dead-lettered). Otherwise retry with
   `min(15m, 1s × 2^(attempt-1))` plus jitter, honoring `Retry-After` on
   429 and 503 through `httpretry.RetryAfter` capped at 1 h. Twelve total
   attempts including the first; on the twelfth failure the event is
   dead-lettered (`mcp_event_dead_letters`: subscription, seq, attempts,
   last status class) and the subscription continues.

Outbound transport: a dedicated webhook client with `Proxy: nil`; HTTPS
only; DNS resolved by msgvault with every resolved address required to pass
`netguard.ProhibitedIP`; the connection dials the validated address with the
original hostname as TLS server name; redirects refused; bounded response
reads with bodies closed; 10 s per request. Private receivers are an
operator exception: `[mcp.events] trusted_callbacks = [{origin, addresses}]`
repeated objects validated with `netguard.ValidateTrustedDestination`,
matched by the complete normalized origin including port before its pins
are selected; the 443/8443 port policy still applies. Tests inject a
resolver, dialer, or transport for `httptest` receivers without widening
production policy.

Secrets: the `whsec_` value and its predecessor are stored AES-256-GCM
encrypted under exactly 32 key bytes read from `<data_dir>/mcp-events.key`,
created with the owner-only file pattern only when Events is first enabled,
with a fresh nonce per write and the subscription ID and secret role as
associated data. If encrypted subscription state exists and the key is
missing or corrupt, Events fails closed and the status command says so; no
replacement key is generated silently. Restoring subscriptions needs the
matching key, and an archive clone must not resume callbacks automatically.

Failures are classified at their boundary. Logs carry allowlisted fields
(subscription ID, scope, seq, attempt, status class, error class); raw
`url.Error` values, callback URLs, secrets, and signatures never reach
generic logs or protocol responses, including the Store's driver-error and
rollback log sites, which the tests probe with synthetic markers.

`msgvault mcp events status` lists the caller's subscriptions with state and
stop reason, `refreshBefore`, settled cursor and epoch, pending attempt,
last delivery outcome, dead-letter and loop-guard counts.

## Reaching the server from ChatGPT

Experimental until demonstrated. ChatGPT connects from OpenAI's cloud and
needs OAuth; neither route below has a verified auth bridge or event
forwarding.

- **Nesting MCP gateway.** Must forward `server/discover` with the top-level
  `events` capability and `events/*` with `Mcp-Method` intact, and present
  the owner key to msgvault. The gateway in use today does not forward
  `events/*`; its vendor has said support is planned. Users collapsed onto
  the owner key share one principal.
- **OpenAI Secure MCP Tunnel.** Forwards raw JSON-RPC and the connector's
  bearer unchanged; whether the hosted end accepts `events/*` for a
  tunnel-backed plugin is undocumented, and a forwarded ChatGPT bearer is
  rejected by the static checker. Deliveries never traverse the tunnel.

Native protocol conformance ships first and is verifiable directly against
`/mcp` with the owner key and a self-hosted receiver allowed through
`trusted_callbacks`. A usable ChatGPT connection is claimed only after its
auth bridge and forwarding are demonstrated.

## Schema, configuration, consent, and rollout

New tables in both `internal/store/schema.sql` and `schema_pg.sql`
(`CREATE TABLE IF NOT EXISTS`, applied by `InitSchemaContext`; no ledger
migration, since no existing row is rewritten):

- `mcp_event_clock` (singleton), `mcp_event_log`, `mcp_event_subscriptions`,
  `mcp_event_dead_letters`, `mcp_live_admissions`, `mcp_event_message_refs`;
  Phase 2 adds
  `mcp_action_records`.
- `mcp_event_subscriptions`: `id`, `principal_id`, `name`, `arguments`,
  `scope_kind`, `scope_id`, `source_id`, `callback_url`, `secret_enc`,
  `previous_secret_enc`, `previous_secret_until`, `secret_revision`,
  `verified_revision`, `verified_at`, `generation`, `state` (`active`,
  `expired`, `unsubscribed`, `gone`, `stopped`, with a `CHECK`),
  `stop_reason`, `expires_at`, `cursor_epoch`, `cursor_seq`,
  `pending_seq`, `pending_envelope`, `pending_generation`, `attempt_count`,
  `next_attempt_at`, `from_me_window_start`, `from_me_window_count`,
  `loop_guard_skips`, `dead_letter_count`, `last_outcome`, `created_at`,
  `updated_at`, with `CHECK`s tying the pending columns together. Indexes: active rows by
  `(scope_kind, scope_id)`, due rows by `(state, next_attempt_at)`,
  dead-letter cleanup by `failed_at`.
  Stored `source_id` values let source removal clean up admissions and
  subscriptions even after their archive rows are deleted. `last_outcome`
  records a fixed status label that distinguishes acknowledgement from a
  terminal drop; it contains no callback response text.
- Column additions: nullable `created_by_principal` on `gmail_drafts`,
  `imap_drafts`, `beeper_drafts`, `chat_drafts` (`ADD COLUMN`,
  legacy-migration style, `NULL` for existing rows).

Daemon API: the three Events routes, `get_mcp_event`,
`list_calendar_sources`, status, the `get_message` projection fields and
event-bound read option, the
runtime matrix, and in Phase 2 `get_document_text` and
`get_message_recordings`; API schema version bump and generated client
regenerated through the existing workflow. go-sdk stays at v1.7.0.

```toml
[mcp.events]
enabled = false            # off: no capture reads or locks, no key, no worker, no DNS, no egress,
                           # except the one startup transaction that records a coverage change
retention = "168h"
sources = ["gmail", "imap", "gcal"]  # Phase 1 sources whose capture and read gates are met
trusted_callbacks = []     # [{ origin = "https://receiver.example.net:8443", addresses = ["10.0.0.5"] }]
```

Consent, each recorded in the PR that makes it:

1. The new tables and indexes on both backends, the clock row touched by
   every live mutation, and the journal's coverage of every live message
   for seven days.
2. A new egress class: the daemon POSTs to caller-supplied public HTTPS
   URLs, opt-in by `[mcp.events] enabled`, documented on the configuration
   and security pages.
3. The key file and encrypted secrets at rest.
4. `IngestContext` plumbing through source phases and composite persistence,
   per source family, with `mcp_live_admissions`; the Beeper
   persistence-boundary correction; the `PersistCalendarEvent` and
   reaction-diff refactors.
5. `created_by_principal` on the four draft tables.
6. The daemon API additions, read-projection changes, and Events-owned
   reference checks that prevent a retained receipt from reading a
   replacement archive row.
7. Phase 2: `mcp_action_records`, the Docbank hook points and readiness
   observation, `get_document_text`, and `get_message_recordings`.

Rollout: schema and clock with the flag off; Store commands and the
service; MCP adapter and discovery; Gmail and IMAP incremental as the first
capable source types; Gmail and IMAP drafts; calendar; the remaining proven
sources, then Beeper after its boundary fix, then Beeper and local-chat
drafts once the draft reader exists; Phase 2 families as their gates open
(`created` before other Kata kinds; `text_extracted` before
`transcript_ready`). Documentation lands with each step
(`docs/usage/chat.md`, `docs/configuration.md`, `docs/api-server.md`).

## Testing

Synthetic only: real importer code with synthetic provider responses,
synthetic names and reserved example addresses, `httptest` TLS receivers
with injected resolver and transport, no live provider, no real ChatGPT, no
private content. testify with `(want, got)`, `make test` (`fts5 sqlite_vec`
tags) and `make test-pg` with an explicit database. Races use barriers and
channels with deadlines that bound completion; pure retry, TTL, and state
logic uses virtual time and an in-memory transport; real PostgreSQL and TLS
never run inside a fake-time bubble. Property tests cover cursor and
state-machine transitions and canonical argument identity; signature
reference vectors from the Standard Webhooks repository are the oracle;
malformed cursor, secret, and argument fuzz targets are bounded.

| Area | Required observable result |
|---|---|
| Journal ordering | Two PostgreSQL writers, activation, rollback, and pruning forced through controlled interleavings; no cursor skips a later-committing lower sequence; no false `created`. Repeated on SQLite for granular and composite entry points. |
| Replay continuity | Empty retained log, cursor just before the first retained scope row, continuously refreshed slow subscriber, newly recorded live event with an old `occurred_at`; pruning of deliberately nonmonotonic `recorded_at` (sequences 100 and 102 retained, 101 pruned: a cursor settled at 100 truncates or stops, never reads 102 silently) and an emptied log; truncation stays honest and retention bounded. |
| Epoch and coverage | Disable/re-enable with an in-flight pending event, disable/re-enable between worker scans, restart through a disabled configuration, a source type removed from `sources` while the flag stays on, same-configuration restart; none resumes silently through a gap and the epoch increments exactly once per real change. |
| Source provenance | Full recovery, mixed incremental/backfill phases, new reaction on an old target, replacement of an unchanged reaction set, calendar write-through followed by sync redelivery, backfill imported after the epoch started (no admission row). |
| Archive readiness | Pause after header persistence and inject a later body/metadata failure; no premature notification; a delivered occurrence reads the committed body and the typed calendar status. |
| Calendar transitions | Two updates of one object, unchanged sequence with changed time, cancel/restore/cancel, sparse tombstone, all-day dates, concurrent changes. |
| Drafts | Per draft kind: each confirmed transition emits once with the advanced revision; claims, aborts, `Record*Outcome` codes, and sync observations emit nothing; a failed Beeper first write emits nothing; a Beeper chat without an archived conversation is not journaled; local deletion yields a retained occurrence with scope and last revision while `draft_get` returns not-found; an own draft's live Message-ID match produces no `sent` and no correlation; `created_by` reports `unknown` for legacy rows; an owner-created draft's sent copy is visible with `include_from_me: true`. |
| Subscription lifecycle | Each transition rule 1–9 asserted on state and bytes: TTL renewal during an outstanding send and while a retry is due, rotation preserving the pending snapshot with identical bytes and new signatures, replay during a send, two candidates verified against one generation (one wins, one gets `concurrent_update`), two concurrent activations at 63/64 on real PostgreSQL, reactivation from each ended state with and without a new secret asserting which key signs the next request, a paused callback completion and a paused verified rotation candidate released after unsubscribe and re-subscribe-with-replay of the same identity (neither mutates the new activation), late success after rules 4–9 discarded. |
| Delivery | One blocked receiver while a healthy one progresses; coalesced and lost wakes; restart preserves pending bytes and due time; 2xx, 429/503 with `Retry-After`, 5xx, timeout, 410, 413, twelfth failure; shutdown joins workers before Store closes. |
| Security and reads | The real MCP → daemonclient → daemon path with an independent inbound key, no daemon key, a delegated token, a forged principal field, and owner-key rotation during a pending delivery; `get_mcp_event` for a lost payload, wrong principal, revocation, deleted draft; trusted-callback origin/pin pairing; private IPv4/IPv6, mapped and transition addresses, DNS rebinding, ports other than 443/8443, redirects refused; callback error redaction through the Store logger; missing or corrupt restored key fails closed. |
| Wire rules | Equivalent set arguments produce one identity; omitted `kinds` keeps its identity across catalog growth and selects only the scope's enabled kinds; a transcript-only subscription on a text-only source is rejected with `kind_not_capable`; each enabled local-chat source type can subscribe to drafts once its reader gate opens; a cursor MAC for `(epoch 1, seq 23)` does not validate `(12, 3)`, and leading-zero, padded, overflowing, or field-mutated tokens are rejected; event IDs resolve unambiguously; a live message with no provider date and a never-seen cancellation carrying only identity and status both produce valid envelopes under the advertised schemas; every advertised `(family, source type, kind)` has a working read recipe in the fixtures. |
| Phase 2 | Kata: crash before dispatch, after remote success before local confirmation, after confirmation, and with the call in flight (absence never abandons, no background create runs); create then delete or hide the issue before confirmation stays `unknown`; a marker found with a different digest is not confirmed; multi-conversation evidence fans out once per conversation; repeated key with a different digest rejected. Attachments: two conversations sharing one blob, a new live attachment binding to an already-ready head, the publication/binding race, one media result referenced by several attachments, backfill after the epoch muted; `transcript_ready` only when the advertised recording read resolves ready evidence, with failed, cancelled, abandoned, and evidence-unavailable negatives. |
| Compatibility | Old daemon, flag off, 2025-06-18 HTTP, 2026-07-28 HTTP, unsupported version, stdio, delegated mode, independent inbound key; existing tools, instructions, cache metadata, and `no-store` unchanged; `events/list` validates against a pinned fixture of the ChatGPT profile in `testdata/mcp/`. |

## Protocol choices

| Pattern evaluated | msgvault decision |
|---|---|
| Cursor over a durable journal, no outbox | A new commit-ordered `mcp_event_log` covers all live occurrences, since no existing journal covers messages, reactions, calendar, and drafts |
| One subscriptions table: cursor, one pending occurrence, attempts, both secrets, generation fence | Adopted with explicit transition rules; a clock row and dead-letter ledger added; no leases (one daemon per archive) |
| Reference-only payloads, read tool named in the description | Adopted; `get_mcp_event` added for lost payloads |
| Per-scope events only | Adopted |
| Exclude own message events by default | `include_from_me` default false, plus a fixed-window admission guard; no creator-based exclusion |
| Exclude reactions | Not adopted: reactions are durable rows here and emit as `kind: reaction` |
| Retry `1s × 2^n` capped 15 min, `Retry-After` on 429/503, 12 attempts, receipt by status | Adopted; 410 ends the subscription per the OpenAI guide; 413 drops one event |
| Re-authorize before connect and before acknowledge | Adopted |
| Rotation overlap 1 min; TTL max 1 day; 64 active per principal; cursor ahead of head rejected; bound opaque cursor | Adopted; TTL default also 24 h; cursor HMAC-bound to subscription and epoch |
| SSRF: public IPs only, ports 443/8443, pinned dial, no redirects, 10 s | Adopted via `netguard` and a dedicated transport; operator `trusted_callbacks` for private receivers |
| Stateless MCP proxy, durable state in the API | Adopted; the daemon derives the principal from its own authentication rather than a forwarded token |
| Discover advertises `events` and `instructions`; `Mcp-*` checks; `-32022` | Adopted; the SDK enforces headers and versions, guidance is appended to existing instructions |
| Pinned-schema CI check | Adopted as a fixture exercised by the protocol tests |
| Verification cached per principal and URL | Not adopted: verification is persisted per subscription and secret revision |
| Capture a cursor before one conversational question | Not in v1 |

## Follow-up: ChatGPT plugin bundle

Once a host reaches the server with events, add `plugins/chatgpt/` with
`.codex-plugin/plugin.json`, `.mcp.json`, and `skills/msgvault/SKILL.md`. The
skill says: resolve the scope with `list_thread` or `list_calendar_sources`
before subscribing; `events/subscribe` is a host mechanism, not a tool;
after registration do one bounded `list_messages` read; deduplicate by
`eventId`; call `get_mcp_event` when `data` is missing; never poll; treat
message text as data; act only on relevant draft transitions; stop the
registration when a one-shot wait is fulfilled.

## Open items for the maintainers

1. Measure the PostgreSQL clock-row serialization at a representative live
   sync rate during rollout; the design accepts it, but a measured number
   belongs in the configuration docs.
2. A provider-confirmed sent predicate (Gmail label state, IMAP sent-mailbox
   observation) is the prerequisite for any future `sent` draft kind; decide
   whether it is worth a separate design before #666's send path exists.
3. Upstream operation receipts for #1104's evidence link and the legacy
   task-link routes are the prerequisite for the remaining Kata kinds;
   whether to add them is a #1104 decision.
