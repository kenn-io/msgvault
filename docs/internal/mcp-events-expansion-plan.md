# MCP Events roadmap and follow-up implementation plans

> **Delivery order:** Submit original Phase 1 Events first. The later tasks below are a roadmap for independent PRs, not prerequisites for basic Events.

**Architecture:** Extend the daemon-owned transactional journal and receipt readers. Provider adapters establish live capture and complete evidence; a separate owner-authorized messaging service binds previews and durable outbound attempts to exact destinations.

**Tech stack:** Go, SQLite, PostgreSQL, native MCP Go SDK, Huma HTTP schemas, existing provider clients.

**Spec:** [Original Events design](mcp-events-design.md), supplemented by the frozen extension contracts below.

**Status:** On 2026-10-07, the delivery plan was split into independently useful PRs. This supersedes the earlier requirement to finish every expansion before the first PR. The extension contracts reviewed on 2026-10-06 remain design references for follow-up work; they do not claim implemented functionality or authorize a competing outbound implementation.

**Goal:** Let an MCP agent discover and subscribe to the communication sources msgvault supports, read new messages/calendar changes/ready meeting and call transcripts, and reply through providers with a real outbound path.

**Execution:** Finish and submit PR 1 in the existing Events worktree. Preserve the expansion implementation on a durable branch before isolating Phase 1. Let coherent source readers finish. Use TDD and small implementation commits, then publish each PR as one clean commit with its own tests and documentation. Advertise only its implemented capabilities. Later roadmap items do not block PR 1.

## Delivery sequence

| PR | User-visible capability | Dependencies |
|---|---|---|
| 1. Native MCP Events | Original Phase 1 message, calendar and draft events; subscriptions, durable signed delivery, receipts, authorization and default-off configuration. | None |
| 2. More native message sources | Verified live producers for Beeper, Slack, Teams, Discord, Microsoft mail and Matrix. Split by provider when needed for review. | 1 |
| 3. Source and inbox subscriptions | Source-wide subscriptions, exact folders/channels/threads, resource lifetimes and pinned Beeper member routes. Keep membership validation and atomic capture together. | 1 and relevant providers from 2 |
| 4. Explicit import notifications | Separately opted-in notifications for newly imported content, provenance and replay protection. | 1 |
| 5. Transcript and media readiness | Complete receipt-bound transcript pagination, version consistency and evidence-backed meeting/media readiness. | 1 |
| 6. Twilio phone events | Reuse the existing connector contribution; add owned-number scopes, verified callbacks and call/recording readiness. | 1 and 5 |
| 7. Safe outbound messaging | Owner-only preview/execute/status, durable idempotency and recovery, and one complete provider adapter. Build on upstream outbound work. | Separate maintainer agreement on sending |
| 8+. Additional sending providers | Small provider-specific changes, each with a complete synthetic receive-to-reply-to-confirmation lifecycle. | 7 |

Security checks, transaction ordering and receipt validity ship with the feature that needs them. Schema changes ship with their first working consumer. Preserve the existing default-off behavior and the original Phase 2 gates; unimplemented families remain unadvertised.

Sending is a separate product decision because it changes the existing never-sends position. Follow [upstream outbound issue #666](https://github.com/kenn-io/msgvault/issues/666), inspect the maintainer's current implementation before proposing changes, and contribute only agreed additions. Outbound work is not part of PRs 1–6.

The detailed tasks below map to these delivery units: Tasks 5–6 primarily support PR 2; Tasks 2–3 and 7 support PR 3; Task 8 supports PR 4; Tasks 10–11 support PR 5; Task 12 supports PR 6. Tasks 13–15 are outbound design references to reconcile with upstream, not an instruction to build a duplicate service. Apply Task 16's delivery checks separately to each PR. Keep original Phase 1 calendar correctness in PR 1; later calendar capability additions remain separate.

**Baseline inspected:** feat/mcp-events, fc4a83e1993f62b6a6aaf1bd9096e2ecc20d8725, incorporating main 057386225. Native Events/API minimum 3.3, ISO8601 refreshBefore adapter, durable signed webhook delivery and replay already exist. The working tree was clean. Ongoing tests were only inspected, not interrupted or restarted.

## Baseline capabilities and missing work

- internal/mcpevents/catalog.go currently advertises message events for gmail/imap, calendar changes for gcal, and draft transitions for gmail/imap plus explicitly enabled beeper/slack/slackdump/teams/discord. This is not general inbound chat support.
- Message subscriptions currently require conversation_id; calendar subscriptions require calendar_source_id. Source/inbox-wide message and phone-number subscriptions are not implemented.
- internal/beeper/types.go models AccountID, Network and ChatID; beeper sources use account IDs. Preserve these boundaries for every underlying Beeper network and login. A friendly network name is not a unique account identity.
- internal/beeper/importer.go persists row/body/raw/auxiliary state in granular calls. Advertising its message events before a coherent readable commit boundary would be incorrect.
- Current msgvault documentation says it never sends. internal/beeper/client.go documents SetDraft as its only write; existing Slack/Teams/Discord clients are archive readers. Actual conversation replies require outbound implementation and a deliberate documented policy change.
- Meeting MCP reads exist in internal/mcp/meetings.go, including opt-in transcripts. The current proposed attachment_processed transcript_ready read recipe and observation gate are not implemented in the native Events catalog.
- The current Events/main production tree has no Twilio connector, but upstream PR #1081 already contains a recorded-call meeting integration (public head 732084eb7ffeceb0d7abbf81bbb7896edb525c99; original Twilio unit 03c1e407c). Reuse that implementation where applicable and add the callback/owned-number Events delta; do not build a duplicate connector or imply the unmerged provider already exists on main.

## Design decisions

1. Extend the existing durable journal, replay, signing and MCP adapter. Do not build a parallel event service or advertise sources by appending names to a list before their producers and readers work.
2. Introduce a source capability inventory covering every native sync/import registration. For each actual source expose archive reading, live message capture, explicit new-import notifications, reactions, drafts, sending, calendars, meetings/transcripts, transport mode, operational prerequisites and an honest unsupported reason. Capability evidence comes from adapters and source configuration, not a test-only table. Catalog discovery remains authorization-filtered.
3. Preserve exact-conversation subscriptions. Add an explicitly selected source/inbox subscription for all new conversations/messages in that source; never widen an existing conversation subscription automatically. Resolve scopes through discovery, using local IDs bound to opaque resource-lifetime references plus source/provider identity. Beeper member accounts, workspaces, tenants, guilds, channels, threads and phone numbers remain isolated. Provider folder/label scopes require a real persisted ID and proven membership predicate; a UI-only inbox is reported unsupported rather than treated as the entire account.
4. Preserve existing event names and canonical subscription identities. Use the frozen source, inbox, import, meeting and phone contracts below. Source-wide subscriptions and real inbox subscriptions are distinct. Retain msgvault.attachment_processed kind transcript_ready for conversation-bound voice/media evidence; open its gate only after the complete receipt-bound reader works.
5. Incremental live sync emits only after a message and mandatory read state commit together. Keep identity -> sync generation -> Events clock ordering, atomic rollback, stable message references and post-outer-commit wakes. Emit media/transcript readiness separately when evidence becomes readable. Do not block a text message indefinitely waiting for asynchronous transcription.
6. Historical imports and recovery remain muted by default. For import-only sources, implement explicit new-import notifications with ingestion provenance and archive timing, using a distinct documented mode/family; do not label an old imported record as a live incoming message. Every source is represented in discovery even when it inherently cannot receive or send live traffic.
7. Reuse calendar_source_id subscriptions for Google Calendar and extend any additional calendar adapter only when both its current-state read and transition persistence exist. An emailed invitation is a message; an archived calendar transition is a calendar event. Recurrence/cancellation and write-through dedupe must remain correct.
8. Separate meeting arrival, recording availability and transcript readiness. Prefer existing get_meeting_context for bounded meeting transcripts; implement missing receipt-bound/paginated recording reads for media. Publish a transcript-ready event only for a confirmed readable version. Missing/failed/partial evidence never implies a complete transcript.
9. Twilio phone_number_id identifies an owned account+number resource, not a caller's number or an unqualified string. Add verified callback ingestion for call identity/state and final transcript evidence. Persist event provenance, dedupe callback IDs/session+sequence and handle callbacks out of order. Call-start, recording-ready and transcript-ready are different events. Full transcript readiness is required here; streaming partial speech and a live audio answering agent are separate interfaces, not promises of this webhook feature.
10. Add explicit text sending/reply capabilities for providers that can send; drafts alone do not satisfy two-way chat. Proposed daemon tools: message_reply, message_send_status and a conversation capability read. message_reply is bound to source_id, conversation_id, optional reply_to_message_id, body, expected sender and idempotency_key. Its preview identifies provider, sender, exact recipient/thread, effective write policy and request digest. Execution requires explicit write authorization through existing owner/agent boundaries. A subscription or receipt grants no permission to send. Never provision live credentials or widen grants during implementation/testing.
11. Persist outbound attempts before contacting providers. Use the frozen send state machine below; Beeper pending IDs are not confirmation. Pin the selected member chat before sending. An ambiguous timeout/crash remains unknown until authoritative reconciliation and never triggers an automatic resend. Preserve existing include_from_me behavior: false suppresses all own-message deliveries by default; true includes manual and agent messages alike, subject to the existing loop guard. Correlation uses provable provider identities and can begin before confirmation; text/time matching is never authority.
12. Support near-real-time receive/read/reply where a provider supplies push or a configured frequent incremental sync. Advertise the transport and expected latency source. MCP webhook delivery and host task batching are asynchronous; do not claim hard instantaneous or exactly-once action semantics.

## Frozen extension contracts (reviewed READY)

### Configured capabilities and resource lifetimes

Discovery returns every production-registered sync/import kind and each configured source/destination, with archive reads, capture provenance, actual subscribed scopes, sender identity, transport and limits. Effective flags depend on the actual adapter, configuration, credentials/permission metadata and destination state. Missing permission evidence is not permission. Import-only sources, disconnected accounts, encrypted Matrix messages that the native importer cannot decrypt, and unsupported destination modes carry stable machine-readable reasons. Production registration and dispatch consume the same descriptors; tests do not supply a second authoritative list.

Keep Events disabled by default and preserve existing source selections. Add an explicit all-eligible-source selection; neither discovery nor a subscription changes configuration. Outbound messaging is independently disabled by default, with explicit daemon send enablement and the existing owner HTTP MCP write opt-in both required. Provider-specific send credentials/permissions must already be deliberately configured. Development uses synthetic fixtures and fake provider endpoints. It does not provision credentials or widen scopes. Delegated sending is unadvertised and denied; persisted delegated send grants remain outside this implementation.

New discovered source, conversation, inbox and phone resources have an opaque lifetime reference bound to source/provider/account identity and a random persistent generation. Reuse existing foreign-key removal and message-reference guards; add small resource-reference records rather than migrate every archive ID. Rename preserves the reference; deletion/recreation, account replacement or changed member routing changes it. All new subscriptions and previews require the corresponding reference. Existing canonical argument bytes remain unchanged. An existing subscription is stopped on resource removal and cannot refresh across a lifetime mismatch; existing-family clients can explicitly select the new discovered lifetime through an optional reference argument, which is canonically included only when supplied.

### Event and scope table

Local numeric scope/archive IDs use canonical positive decimal strings. Provider IDs, event IDs, attempt IDs and lifetime references retain their explicitly opaque formats. Every new argument object is closed. References are opaque authenticated strings; noncanonical, mismatched or stale references fail before callback I/O. Boolean defaults are explicitly persisted in canonical arguments; kind sets are sorted/deduplicated, and omitted kinds persist ["*"] rather than expanding with future catalog growth.

| Event | Required scope and optional filters | Matching and read |
|---|---|---|
| msgvault.source_message_archived | source_id, source_ref; include_from_me=false, include_reactions=true | All eligible new conversations/messages in exactly that connected source lifetime. Payload uses existing message IDs/kind/observation fields; get_message with event_id protects the reference. |
| msgvault.inbox_message_archived | inbox_id, inbox_ref; include_from_me=false, include_reactions=true | One real persisted label/mailbox/channel/thread resource. Capture-time membership is persisted from the actual provider identifier; delivery rechecks current source/inbox access. No display-name/prefix approximation and no fallback to account-wide matching. Payload also identifies inbox_id; receipt-bound get_message. |
| msgvault.import_message_archived | source_id, source_ref; include_from_me=false | Explicit newly imported archive rows, with ingestion_mode=import, import_run_id and archive-commit timestamp. Requires the separate import-notification opt-in and explicit import provenance. Does not call historical content live; receipt-bound get_message. |
| msgvault.meeting_changed | source_id, source_ref; kinds=*, include_imports=false | Kinds arrived, recording_ready, transcript_ready. Message_id identifies the meeting archive record; readiness payload includes evidence_version and recording_id where applicable. Complete transcript reads use get_mcp_transcript; get_meeting_context remains bounded context. |
| msgvault.call_changed | phone_number_id, phone_ref; kinds=* | Kinds started, completed, recording_ready. Phone identity is the owned account+number resource. Payload carries archive message_id, call/parent identifiers and declared call-leg coverage; get_message or the receipt-bound recording reader. |
| msgvault.call_transcript_ready | phone_number_id, phone_ref | Only the completed-call coverage contract below. Payload carries message_id and evidence_version; get_mcp_transcript reads that evidence to completion. |

Gmail labels and IMAP mailboxes resolve real persisted provider IDs. Slack, Teams and Discord channels/threads resolve exact source/account/channel/root-thread relationships; a thread is not its parent channel. Beeper member accounts/chats are isolated by their real account and chat identities, even when network/display names repeat. A folder or membership mode with no proven native predicate is explicitly unavailable until its required persistence/adapter is implemented, never advertised as a whole source. All buildable native scope implementations remain required here.

New source/inbox events use the same default own-message policy as the original conversation family. Message-body/raw/mandatory auxiliary completion and each matching occurrence commit together. Optional media does not delay readable text; readiness is a later versioned occurrence. Historical/full/recovery runs remain muted unless the distinct import mode is explicitly selected. Readiness requires a current-epoch live admission or an explicitly selected import admission, with import provenance disclosed. Polling an old archive alone creates no live admission. Scope membership is captured transactionally; later access revocation stops delivery and receipt reads, while a mere rename does not rewrite history.

### Explicit send preview and execution

Expose message_reply_preview and message_reply only through the enabled owner HTTP write surface. Expose message_send_status as an owner-authorized read independently of send enablement and HTTP write opt-in. Preview accepts source/conversation IDs and lifetime references, optional reply message ID, plain text body and expected sender. It resolves the concrete provider account/member recipient/thread and reply target reference, and returns those exact destinations, effective policy, request digest, expiry and an authenticated preview_token. A preview performs no send or durable dispatch. Rejected authority/configuration/lifetime checks precede provider I/O.

The preview token expires after five minutes and binds the current authenticated owner principal, source/account lifetime, conversation/member/thread, selected sender, reply reference, exact UTF-8 body digest and write-policy revision. Execute requires the unchanged request, token and idempotency_key. Revalidate all bindings and current authorization before claiming the attempt. A changed/reused reply ID, destination, member route, sender, policy, body or owner authority rejects with zero send requests. A Beeper merged destination must resolve a concrete member chat before preview; otherwise require explicit member selection. Send to the pinned member endpoint, not the provider's mutable merged-chat default. Record and verify the returned route; disagreement is unknown and never triggers a corrective resend.

Idempotency keys are nonempty opaque strings of at most 128 UTF-8 bytes and are scoped to the archive plus authenticated principal. The canonical request digest includes every destination/lifetime/sender/reply/body binding. Concurrent same-key/same-digest requests reuse one attempt; any changed digest conflicts. Persist key/digest tombstones for the lifetime of the archive so pruning operational details cannot silently allow a resend. An attempt ID is opaque; a receipt or draft permission grants no send permission.

message_send_status accepts exactly one of attempt_id or idempotency_key, with lookup scoped to the archive and authenticated owner principal. It reads only an already persisted attempt or retained key tombstone. It requires no valid preview, current send policy, provider connection or current target lifetime, and never creates, claims, retries or sends an attempt. Missing/pruned details are reported honestly. Another principal cannot discover the attempt; delegated status reads remain denied. Disabling sends or HTTP writes, disconnecting/removing a source, or restarting must not hide this recovery read.

Execute always rejects an expired preview, including an existing-key request; status-by-key is the recovery path when its response or attempt ID was lost. A matching still-prepared attempt can dispatch only under a fresh valid preview and current authorization. A matching already-claimed attempt returns its existing outcome only after the normal valid-preview/authorization checks; it never redispatches. Status lookup cannot turn a prepared attempt into a send.

| Durable state | Transition/evidence |
|---|---|
| prepared | Request and immutable route persisted, not dispatched. Only a matching, currently authorized execution can atomically claim it. |
| dispatching | One compare-and-swap claimant persisted the dispatch marker before provider I/O. No second caller dispatches. |
| accepted | Provider explicitly accepted a write but its contract does not prove final message confirmation. A pending Beeper ID or Graph mail 202 is acceptance. |
| confirmed | Authoritative provider/message identity and route evidence resolves the attempt; this does not promise recipient delivery/read. |
| failed | Proven rejection or cancellation before the first provider invocation; never use this for an ambiguous remote effect. |
| unknown | Dispatch may have occurred but confirmation is unavailable, malformed, truncated, cancelled after dispatch or otherwise ambiguous. Recovered dispatching attempts become unknown. |

Prepared work is never sent automatically on restart. An unknown or accepted attempt is never automatically resent. Cancel/revoke checks before provider invocation prevent writes; cancellation after invocation remains ambiguous unless success is proven. Network I/O runs outside SQL and the operation gate; shutdown joins owned send/reconciliation work before closing Store. Reconciliation performs authorized reads only and can advance accepted/unknown to confirmed without resending. Read retries remain intact.

| Adapter | Dispatch/dedupe and reconciliation contract |
|---|---|
| Beeper | One request to the pinned member chat; retain actual chatID/pendingMessageID immediately. Resolve through the documented message read or authoritative archive identity. Pending is not confirmed. No assumed POST idempotency. |
| Slack | One chat.postMessage to exact channel/thread with configured chat:write authority. Preserve returned channel/ts and success semantics; no undocumented unlimited dedupe assumption. Ambiguous writes are not retried. |
| Graph chat/channel | Separate delegated ChatMessage.Send and ChannelMessage.Send paths. A new response-preserving single-dispatch transport bypasses existing Post retry behavior; preserve returned message identity. Never substitute migration-only application permission. |
| Graph mail | One authorized send operation; 202 means accepted. Reconcile only a documented draft/immutable-message or request-marker identity when available; otherwise remain accepted/unknown. |
| Discord | Bot destination and SEND_MESSAGES permissions checked. Persist the documented nonce and response ID; the provider's short enforce_nonce window is not unlimited retry permission. No resend after its window or ambiguous dispatch. |
| Matrix | Stable transaction ID bound to exact endpoint/device/room and digest before dispatch. Preserve response event_id and verified echoes; changing the device cannot reuse dedupe authority. No automatic resend is needed even where dedupe is documented. |
| Gmail | Deliberately configured Gmail send authority; preserve returned provider message/thread identities and reconcile authoritative archive state. Readonly importer credentials do not imply send authority. |
| IMAP/SMTP | IMAP supplies no send operation. Require separately configured SMTP sender/identity, exact envelope and stable Message-ID. SMTP queue acceptance is accepted, not recipient delivery. Ambiguous DATA/response is never retried automatically. |

All adapters preserve successful response identity and use single-dispatch writes below the service, including after transport errors, body-read errors, 429/5xx and malformed success responses. No library can invisibly reissue an ambiguous write. Unsupported/read-only destinations reject before dispatch. Provider-specific text and permission limits are validated against their primary contracts before preview/execute.

### Complete transcript reads and readiness

Add get_mcp_transcript {event_id,cursor?,max_bytes?} for meeting, call and conversation-bound media evidence. Default page size is 32 KiB; allowed maximum is 64 KiB. It returns message_id, evidence_version, offset_bytes, text, evidence_complete=true, done and next_cursor. Cursors authenticate event/subscription, retained message reference, evidence/artifact version and byte offset. UTF-8 boundaries are preserved. The event's exact complete evidence version must still be current and readable on every page; a changed, deleted, unavailable or reused identity returns a stable unavailable/changed error, never newer text substituted into an old continuation. This current-version contract does not require indefinite historical transcript retention.

Observe provider/file evidence outside publication SQL, compute its canonical complete version, and revalidate persisted message reference/source epoch/version in the publication transaction. A stale observation cannot publish. Recording/transcript read descriptors and producer metadata commit coherently; failed/cancelled/partial evidence cannot become ready. Dedupe publication by legitimate binding, kind and evidence version independently of seven-day journal pruning; shared evidence fans out once to each admitted occurrence. Native restarts reconcile readable evidence without republishing completed versions. Keep get_meeting_context for bounded context; it is not the full reader and its message_ids/max_bytes contract stays unchanged.

### Twilio callback and whole-call coverage

Reuse PR #1081's client, recorded-call persistence and completed/usable transcript parsing. New ingress supports status notifications and recording-status notifications with an empty successful acknowledgment only after durable callback admission. It is not a Voice/TwiML call-control URL and never replaces an existing call-handling application. The callback route is explicitly bound to one configured owned phone resource/account. Validate every provider parameter and the exact configured public URL with that account's AuthToken; API-key read credentials alone do not enable callback admission. Bound payloads to 1 MiB and reject unsupported content types or unknown account/number identities.

Persist callback identity/type/sequence before acknowledgment; tolerate added signed parameters, duplicates, out-of-order delivery and replay. Reconciliation uses the authoritative reused client outside SQL, then revalidates resource lifetime/source epoch and persists before publication. A historical callback after reconnect is historical reconciliation, not a fresh live call. Parent/child legs keep distinct provider identities with a verified account+parent association; an unrelated leg or another owned number never inherits a scope by caller number or display name.

Whole-call completeness requires a completed authoritative call plus its declared connected interval, complete recording coverage of that interval and both relevant audio tracks, and complete usable transcripts for every required recording segment. Missing intervals, unknown coverage, incomplete pagination/manifests, unavailable media, partial recording sets or unaccounted call legs yield no whole-call-ready event. A recording-ready event may describe its actual partial coverage honestly. Final utterance, transcription-stopped, one recording's completed transcript, or the first completed child leg alone are never whole-call evidence. Call-leg scope/parent association and the coverage manifest are explicit in payload/read metadata; no aggregate parent completion is inferred from a child. Persist the verified complete manifest/version so paginated reads cannot mix late replacement evidence. Do not configure production webhooks, recording or transcription features during development.

### Review regression gates

Add native regressions for changed source/conversation lifetimes and reply IDs; two accounts on one Beeper network and merged-route changes before execute; body/sender/policy/expiry revocation with zero provider requests; concurrent idempotent claims, both sides of dispatch crashes and hidden transport request counts; Discord nonce-window expiry and Matrix device replacement; source-wide versus real inbox/thread SDK schemas and preserved legacy canonical bytes; a transcript larger than 1 MiB read fully and changed/deleted/reused identity between pages; stale/shared evidence publication and restart; Twilio final utterance/early stop/partial recording sets/parent-child/two-number/reconnect and external-URL signature cases; echo before response and unknown then authoritative echo, plus manual identical own text and both include_from_me values. Add recovery regressions for a lost execute response, expired preview, disabled send/HTTP write toggles, removed/disconnected source and restart to unknown: owner status-by-key still returns the same persisted attempt with zero provider requests; another principal is denied. A prepared attempt with an expired preview stays prepared under status lookup and cannot dispatch through expired-token execute. Each advertised provider/scope/read tuple must finish the real signed synthetic SDK lifecycle on SQLite and PostgreSQL.

## Approach comparison

Recommended: reusable provider capability inventory + existing transactional journal + provider-specific provenance/readiness and outbound adapters. It scales across sources while preserving their real limits.
A catalog-only expansion is rejected: it creates unsupported subscriptions and can wake an agent before content exists.
A separate per-provider event/send server is rejected: it duplicates daemon authorization, receipts, replay and configuration, and bypasses archive identity rules.

## Workstream A: all-source incoming events and scope discovery

### A1. Inventory and discovery
Files: internal/mcpevents/catalog.go; internal/config MCP Events configuration; internal/api/schema.go; internal/mcp Events/read handlers; existing source registration and sync dispatch; owning public docs. Add a small provider capability unit rather than growing catalog.go with unrelated transport code.
- [ ] Write a failing native discovery test covering every registered source type. Assert each has a verified implementation or a machine-readable inherent limitation; include msmail, matrix, Beeper member accounts, Slack, Teams, Discord, meeting sources and import-only formats, not just the original three defaults.
- [ ] Run it with go test -tags 'fts5 sqlite_vec' and confirm actual behavior fails.
- [ ] Implement authorization-filtered capability/scope discovery; distinguish a provider's theoretical API support from a configured connector's effective permissions.
- [ ] Verify zero archive content/credentials in discovery; test disabled/read-only sources and duplicate account display names.
- [ ] fmt, tagged vet, inspect diff and commit.

### A2. Coherent message producers
Files: internal/store/messages.go; internal/store/mcp_events.go; per-provider importer.go for beeper/slack/teams/discord/msmail/matrix and every other native live-sync producer established by A1. Tests belong beside each production producer, with SQLite and PostgreSQL Store coverage.
- [ ] RED: after a paused row insert before mandatory body/raw completion, no callback/readable event; rollback no event; successful complete commit yields one occurrence. Exercise existing production parsers/persistence, not direct synthetic journal inserts as the primary proof.
- [ ] RED: initial backfill/full/recovery muted; interleaved incremental tail emits; retries/dedup/edits/deletions/media download do not create duplicate new-message events.
- [ ] Implement transactional ready-boundary publication and explicit IngestContext for each proven live phase. Preserve outer identity and sync fences; isolate optional media processing from mandatory message readiness.
- [ ] GREEN on both databases, including source removal and concurrent identity/reclassification races. Commit each provider independently.

### A3. Inbox and sub-inbox subscriptions
Files: internal/mcpevents/catalog.go/service.go; internal/store/mcp_events.go; MCP scope discovery and owning schemas. Add migration only if a real inbox/folder identity cannot be expressed by current source/conversation models.
- [ ] RED: exact-conversation isolation and an explicitly selected inbox receiving a newly created conversation; identical provider chat/thread IDs in two accounts remain isolated.
- [ ] RED: Beeper network/login isolation, merged-chat member routing, Slack channel/thread, Teams chat/channel/replies and Discord channel/thread; no match by names.
- [ ] Implement the frozen source-wide family with source_id/source_ref and the distinct real inbox family with inbox_id/inbox_ref and proven membership predicates, keeping old subscription canonical bytes and IDs stable.
- [ ] GREEN: live membership/access changes, inbox deletion/rename, replay, unsubscribe/expiry/refresh, account disconnection and receipt-authorized reads. Add explicit import-only notification mode without changing live defaults. Commit.

## Workstream B: calendar, meetings, transcripts and phone-number subscriptions

### B1. Calendar coverage
Files: internal/calsync; internal/store/calendar_events.go; internal/mcpevents/catalog.go; existing calendar reads and public guides.
- [ ] Add native RED coverage for every existing calendar connector's proposed advertised transitions and readable projection; do not invent a CalDAV sync connector because contact CardDAV code exists.
- [ ] Preserve gcal create/update/cancel, recurring-instance identities, null times, initial full scan suppression and write-through+sync dedupe; implement any missing proven calendar producers/readers.
- [ ] Verify real owner HTTP SDK subscription -> signed callback -> get_message calendar read on both databases; commit.

### B2. Meeting and media transcript readiness
Files: internal/meetingimport; internal/granola/circleback/plaud/notionmeetings/muesli producer paths; internal/store/docbank_media.go and meeting persistence; internal/mcp/meetings.go; new receipt-bound recording read adapter where required.
- [ ] RED: meeting arrives without transcript, later a readable final version emits exactly once; a failed/cancelled/unavailable result never emits ready; unchanged retry emits nothing.
- [ ] RED: ready evidence bound to two legitimate occurrences fans out once each, stale/rebound/deleted message IDs cannot grant another record's read, restarted workers resume without redispatching completed versions.
- [ ] Implement missing observation/read dependencies; perform external evidence reads outside SQL, revalidate identity/version in the publication transaction. Use existing meeting read APIs rather than duplicating transcripts in the webhook.
- [ ] Verify source/meeting scopes, bounded transcript pagination and SQLite/PostgreSQL readiness races; commit. Do not advertise until this complete path passes.

### B3. Twilio connector and phone scope
Reuse the existing internal/twilio recorded-call implementation from upstream PR #1081, reconciling its source registration, meeting persistence and configuration with current main. Add callback.go and focused native tests, daemon/API callback routes, owned phone-number persistence/discovery and Events schemas. Recheck fresh main before integration; do not duplicate another owner's connector.
- [ ] RED against httptest provider callbacks: invalid signature, unknown account/number, duplicate/reordered call states, transcript before recording callback, missing transcript, retried callback and cancellation.
- [ ] Implement provider signature validation against the configured externally visible URL, bounded payloads, stable account/number/call/session identity and durable callbacks before acknowledgment. No production number/webhook setup.
- [ ] Implement distinct call_changed and final call_transcript_ready publication and receipt-bound reads, including subscription before the next call exists. Final utterances are not a whole-call transcript until completion is established; normalize the two concepts explicitly.
- [ ] GREEN on owned synthetic provider and both databases; verify two numbers/accounts with overlapping IDs and replay after crash. Commit.

## Workstream C: actual reply/send path

### C1. Outbound contract and durable attempts
New Store outbound-attempt unit/migration, a daemon-owned messaging service, API routes/schema and generated clients, internal/mcp message_reply/status adapters. Do not reuse event payloads as write authorization.
- [ ] RED: read-only subscription cannot send, wrong source/thread/sender denied, same idempotency key+digest reuses attempt, same key+different digest fails, cancellation before provider call causes no write.
- [ ] Implement preview-bound authorization, durable attempt states and message_send_status. Start with existing authenticated owner boundary; delegated support must use actual persisted grants if exposed, never forged arguments.
- [ ] RED then GREEN for crash before dispatch/after ambiguous remote success/before confirmation, double clicks/repeated MCP task runs and restart. Unknown outcomes remain unknown unless proven reconciled. Commit.

### C2. Provider reply adapters
New focused send.go plus tests beside internal/beeper/slack/teams/discord/matrix and native mail sender paths established by A1. Gmail and Microsoft mail use supported send APIs; IMAP needs an explicitly configured outbound SMTP service and cannot send through IMAP itself. Import-only archives have no send path without a separately configured live provider.
- [ ] Add native httptest raw-wire RED per adapter proving exact intended recipient/thread/reply relation and least necessary permissions; never assume importer credentials authorize writes.
- [ ] Implement Beeper send with actual routed member chat+pending ID reconciliation, Slack postMessage/thread reply, Microsoft Graph chat/channel reply with appropriate delegated sending permission, and Discord Create Message with documented nonce handling. Verify Matrix and mail send contracts against primary docs before implementing them.
- [ ] Reject unsupported sender identities, closed/readonly destinations and missing scope; expose provider limitations in capability reads. No real provider writes or credential changes.
- [ ] GREEN: 429/5xx/disconnection/timeout semantics, idempotency and confirmation versus acceptance. Keep unsafe automatic retries disabled where the provider cannot dedupe/reconcile. Commit per provider.

### C3. Synthetic conversation lifecycle
Files: real MCP SDK integration tests, daemon service tests and each native provider fake server.
- [ ] Per live supported provider: receive synthetic message -> coherent archive commit -> actual signed callback -> MCP receipt-bound content read -> explicitly authorized message_reply -> provider fake receives exact reply -> confirmation/status -> outgoing archive echo -> no unsolicited repeat reply.
- [ ] Include every Beeper account/network model with two accounts of the same network; native Slack/Teams/Discord/Matrix and mail, plus disconnect/revocation during callback/send and two simultaneous conversation scopes.
- [ ] Assert unsupported sources accurately discover their read/import-only limits. A simulated conversation cannot turn an offline export into a live transport.
- [ ] GREEN SQLite/PostgreSQL and raw SDK tests, with normal production code/parsers; commit.

## Review focus

- Mandatory message content readiness versus optional asynchronous media; callback must not race partial row publication.
- Sub-inbox/account/merged-chat identity isolation; sending must not select the wrong login or member destination.
- Callback acknowledgment versus task completion, send acceptance versus confirmation, and ambiguous timeout/crash handling.
- Old archived/imported media gaining transcripts must not silently become a live incoming-message event.
- Grants/disconnect/deletion races; receipt authority cannot authorize sending or another reused message identity.

Each of these has explicit RED/GREEN coverage in A2/A3/B2/C1/C2/C3. Self-review found no requirement delegated to a catalog-only promise. Twilio and outbound sending remain genuine new work, not already built components.

## Review and pre-PR gates

1. Reconcile each follow-up with the original Events design and the current source. Keep unchanged Phase 1 decisions out of scope unless new evidence requires revisiting them.
2. Obtain an independent design review before implementation. Cover protocol/profile compliance, truthful provider capabilities, source completeness, calendar/meeting/phone semantics, safe explicit sending, idempotency and lifecycle behavior. Address its findings, then verify the implementation separately.
3. Work from a stable source snapshot, preserve unrelated test infrastructure, and implement each workstream with TDD and small commits.
4. Treat this document as a roadmap, not authorization. Each expansion needs maintainer agreement and a separate PR. Keep unsupported or import-only provider limits explicit; do not claim every source is live or conversational.
5. Regenerate Go/browser schemas after source changes stabilize. Run the relevant SQLite and documented PostgreSQL matrices, real SDK wire tests, formatting, vet, lint, build and documentation checks. Investigate failures without weakening unrelated timeouts.
6. Document only implemented, explicitly authorized behavior, including import-only limits, latency and source-specific configuration. Scrub private data, inspect the final diff, review the exact final tree, and publish a focused follow-up above reconciled upstream main after its checks pass.
7. Native signed callback and MCP SDK lifecycle tests do not certify a separate gateway deployment.

## Primary references checked

- MCP Events profiles allow server-side resource filters; signed webhooks are acknowledged separately from asynchronous host processing: https://developers.openai.com/plugins/build/mcp-events
- Beeper send returns pendingMessageID and the actual routed member chat; confirmation is separate: https://developers.beeper.com/desktop-api-reference/resources/messages/methods/send/
- Slack outbound chat requires chat:write and an exact channel/thread: https://docs.slack.dev/reference/methods/chat.postMessage/
- Microsoft Graph chat sending uses delegated ChatMessage.Send; migration-only application permissions are not ordinary chat permission: https://learn.microsoft.com/en-us/graph/api/chat-post-messages?view=graph-rest-1.0
- Discord message creation documents nonce/enforce_nonce with a bounded dedupe window: https://docs.discord.com/developers/resources/message#create-message
- Twilio distinguishes incoming call, recording callbacks and real-time transcription utterances/finality: https://www.twilio.com/docs/usage/webhooks/voice-webhooks and https://www.twilio.com/docs/voice/twiml/transcription


## Global implementation constraints

- Preserve the Events/API 3.4 minimum after upstream telemetry reconciliation and ISO8601 MCP `refreshBefore`; advance the advertised API version for new schema capabilities according to current version policy.
- Keep Events and outbound messaging independently disabled by default. Existing source selections remain unchanged; `sources = ["*"]` explicitly selects all eligible implemented sources.
- Use identity → sync generation → Events clock ordering. Commit mandatory readable state and occurrence together; wake after the outer commit.
- All Go tests use `-tags "fts5 sqlite_vec"` and testify. Run fmt and tagged vet before each Go commit; include generated changes at regeneration boundaries.
- Sending is owner-only. Subscription/receipt/draft authority never grants send authority. Recovery status remains readable independently of sending or write opt-in.
- Use the existing worktree and implementation session. Finish frozen source readers before modifying Go; document-only commits do not change their source.
- Keep every requested buildable adapter in scope. Provider limitations are explicit; an unfinished adapter is not an inherent limitation.

## Implementation interfaces and task sequence

The nine workstreams above define acceptance. The tasks below fix their concrete units and order. Each task uses its own failing behavior test, minimal implementation, focused GREEN, diff inspection and commit. Keep an execution ledger outside published documentation with exact RED/GREEN commands and results; record any interface correction before using it downstream.

### Task 1: Production source inventory (A1)

**Files:** Create `internal/sourcecatalog/catalog.go` and `catalog_test.go`; modify `cmd/msgvault/cmd/constants.go`, `sync.go`, `serve.go` and actual import registrations; create `cmd/msgvault/cmd/source_catalog_test.go`.

**Interfaces:** `sourcecatalog.Descriptor{Type, Transport string; LiveMessages, Imports, Calendars, Meetings, Reactions, Drafts, Send bool; Limitations []string}`; `sourcecatalog.List() []Descriptor`; `sourcecatalog.Lookup(sourceType string) (Descriptor, bool)`. Descriptors are the production registration authority; sync/import dispatch consumes the same types and mode metadata. Capability flags become true only with completed producer/read/send paths.

- [ ] RED: `TestNativeSourceInventory` executes native registration/dispatch with synthetic sources and asserts descriptors cover every actual registered sync/import kind, custom import types are honestly classified, and encrypted Matrix capture is unavailable. Expected missing inventory/discovery behavior.
- [ ] Implement the registry and consume descriptors from real native registrations. Include Gmail, IMAP, Microsoft mail, Beeper, Slack, Teams, Discord, Matrix, Google Calendar, all meeting connectors, SyncTech SMS and actual offline import formats. Twilio is added with Task 12.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/sourcecatalog ./cmd/msgvault/cmd -run 'TestNativeSourceInventory|TestSourceCatalog'`; expected PASS. Inspect production dispatch and returned inventory, fmt/vet and commit.

### Task 2: Resource lifetime records and discovery (A1)

**Files:** Create `internal/store/mcp_resources.go`, `mcp_resources_test.go`, `internal/mcpevents/discovery.go`, `discovery_test.go`; modify Store schemas/migrations, `internal/api/mcp_events.go`, `internal/daemonclient/mcp_events.go`, `internal/mcp/events.go` and catalog tool definitions.

**Interfaces:** `store.MCPResource{Kind string; ID, SourceID int64; ProviderID, Reference string}`; `(*Store).ListMCPResources(ctx context.Context) ([]MCPResource,error)`; `(*Store).ValidateMCPResource(ctx context.Context, kind string, id int64, reference string) error`; `(*mcpevents.Service).Discover(ctx context.Context, principal string) (DiscoveryResult,error)`. `DiscoveryResult` contains provider descriptors, effective configured source capabilities and actual scope resources. Native MCP tool `list_mcp_sources` and HTTP GET `/mcp/events/sources` use the authenticated daemon boundary. Discovery is owner-authorized and available even with capture disabled.

- [ ] RED: discover two identically named accounts, rename a source, physically delete/recreate a reused numeric ID, replace Beeper member routing, and reject a stale reference. Require different generations on replacement, identical reference on rename, no tokens/content in results and no callback/provider I/O for invalid scope.
- [ ] Implement random persistent resource generations and authenticated opaque references, source/conversation/inbox FK cleanup, exact owner discovery and configuration/permission filtering. Missing configured authority carries a stable reason.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/store ./internal/mcpevents ./internal/api ./internal/mcp -run 'TestMCPResource|TestMCPDiscovery'`; expected PASS on SQLite and configured synthetic PostgreSQL. Inspect diff, fmt/vet and commit.

### Task 3: Source-wide capture and canonical compatibility (A2/A3)

**Files:** Modify `internal/mcpevents/catalog.go`, `service.go`, `workers.go`, `internal/store/mcp_events.go`, `mcp_event_subscriptions.go`; add focused source-scope tests beside each.

**Interfaces:** Extend canonical arguments with `source_ref`, `conversation_ref` and the frozen source family. Extend stored subscription scope lifetime and validation; source-wide message occurrences share the original message reference and atomic admission. Existing omitted-reference canonical bytes remain unchanged. `IngestContext` retains Mode/ObservedAt and adds explicit import-run provenance for Task 8.

- [ ] RED: `TestMCPSourceSubscriptionNewConversation`, `TestMCPLegacyCanonicalCompatibility` and `TestMCPResourceRefreshAfterReplacement` prove a subscription made before a conversation exists receives only its selected source, preserves legacy IDs, and cannot refresh into a new lifetime.
- [ ] Implement source-family catalog/schema, validation, matching, replay, revocation and receipt reads. Preserve own-message defaults and muted full/recovery behavior. Never publish a row-only upsert as coherent message readiness.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/mcpevents ./internal/store -run 'TestMCPSource|TestMCPLegacy|TestMCPResourceRefresh'`; expected PASS on both databases. Commit.

### Task 4: Beeper coherent ready boundary (A2)

**Files:** Modify `internal/beeper/importer.go`, `syncstate.go`, `mapping.go`; add `internal/beeper/mcp_events_test.go`. Reuse `store.MessagePersistData` and `PersistMessageWithParticipantsContext`.

**Interfaces:** One parsed message becomes one atomic `MessagePersistData` with conversation, recipients, body, provider raw and mandatory metadata. Per-message store views select explicit live versus historical provenance from native cursor/phase evidence. Optional media stays outside this mandatory boundary.

- [ ] RED: native fake-server import pauses before mandatory raw completion; no occurrence/read is available. Commit yields exactly one; rollback yields none. Test interleaved initial history/live tail, recovery mute, retries and two accounts on the same network.
- [ ] Convert persistence to the existing atomic Store snapshot and explicit phase provenance. Preserve attachment processing, identity/sync fences and route identities.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/beeper -run 'TestMCP|TestImporter'`; expected PASS with native fake endpoint on both databases. Commit.

### Task 5: Slack, Teams and Discord live boundaries (A2)

**Files:** Each provider's `importer.go` and new `mcp_events_test.go`; native sync-state files when needed.

**Interfaces:** Consume Task 3 families and Task 4's existing Store snapshot interface, not Beeper-specific code. Capture native account/channel/root/thread/reply identifiers in mandatory metadata and preserve provider attachment behavior.

- [ ] For each provider independently, RED on native incremental head/thread paths: initial walk muted, later new message emits after complete body/raw/recipients, failed mandatory write rolls back, edit/delete/reaction retries do not duplicate new-message events.
- [ ] Implement the provider's coherent snapshot and exact live phase. Slack rescan/history and thread recovery are not automatically live; Teams chat/channel cursors are distinct; Discord bot identity and native channel/thread relationships are retained.
- [ ] GREEN per provider: `go test -tags "fts5 sqlite_vec" ./internal/<provider> -run 'TestMCP|TestImporter'`; expected PASS on both databases. Commit each provider separately; activate its descriptor only after coverage passes.

### Task 6: Microsoft mail, Matrix and other registered live adapters (A2)

**Files:** `internal/msmail/importer.go`, `internal/matrix/importer.go`, matching native tests, and every additional live message adapter found in Task 1 such as `internal/synctechsms/importer.go`.

**Interfaces:** Existing `PersistMessageContext`/participant snapshot and Task 3 provenance. Matrix timeline after a validated `next_batch` is live; pagination/gap recovery is historical. Microsoft folder delta versus initial/restarted walk supplies per-message provenance.

- [ ] RED per native adapter on first full walk, valid incremental cursor, expired/restarted cursor, complete raw/body boundary, cancellation and account isolation. Matrix unsupported encrypted content remains unavailable and never becomes readable by advertising.
- [ ] Implement explicit live provenance and atomic mandatory persistence. Every descriptor marked live has a tested path; polling offline imports remains import mode.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/msmail ./internal/matrix ./internal/synctechsms -run 'TestMCP|TestImport'`; expected PASS on both databases. Commit each coherent provider change.

### Task 7: Real inbox and thread scopes (A3)

**Files:** Create `internal/store/mcp_inboxes.go` and tests; modify provider snapshot builders, Events catalog/Store matching/discovery and schemas.

**Interfaces:** Persist `MCPInboxMembership{InboxID int64; InboxReference string}` with `MessagePersistData`. Inboxes own exact source/provider folder/channel/root-thread identities and generation; `(*Store).ResolveMCPInbox(ctx context.Context, sourceID int64, kind, providerID string) (MCPResource,error)`. Capture-time membership is part of the coherent occurrence, while access validation uses current resource lifetime.

- [ ] RED: Gmail labels and IMAP/Graph folders isolate overlapping names; Slack/Teams/Discord channel versus thread, Beeper member chat and Matrix room use exact persisted relationships. New conversations in a source enter a selected real inbox only with provider evidence. Rename preserves scope; delete/recreate changes it.
- [ ] Implement the frozen inbox family with closed arguments, canonical references and actual membership predicates. No prefix/display-name or source-wide fallback.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/store ./internal/mcpevents ./internal/mcp -run 'TestMCPInbox|TestMCPThreadScope'`; expected PASS including SDK subscribe/replay/unsubscribe on both databases. Commit.

### Task 8: Explicit new-import notifications (A3)

**Files:** `internal/store/mcp_events.go`, actual import boundary and atomic provider/import snapshots; Events configuration/catalog; new native import-notification tests.

**Interfaces:** `IngestImport` with nonempty import-run ID and archive commit timing; separate `msgvault.import_message_archived` admission guarded by explicit import-notification configuration. Shared Store persistence creates notifications only for newly created archive rows; retries and existing updates remain quiet.

- [ ] RED: every actual offline importer represented in Task 1 publishes newly imported synthetic content only under both opt-ins; historical/default runs remain quiet. Assert disclosed `ingestion_mode=import`, run ID and archive time, restart/dedupe and own-message filter.
- [ ] Implement provenance at the real daemon import boundary and native snapshot persistence. Represent configurable custom import types honestly; do not imply their original provider is live or sendable.
- [ ] GREEN: native importer tests plus `go test -tags "fts5 sqlite_vec" ./internal/mcpevents ./internal/store -run 'TestMCPImport'`; expected PASS on both databases. Commit.

### Task 9: Calendar lifecycle completion (B1)

**Files:** `internal/calsync`, `internal/store/calendar_events.go`, MCP real SDK integration tests and source inventory.

**Interfaces:** Preserve existing `calendar_source_id`, original calendar family and receipt-bound current projection. Advertise only actual registered calendar connectors with transitions and reads; no invented CalDAV connector.

- [ ] RED for any missing create/update/cancel/recurring-instance/null-time transition or write-through+sync dedupe through the native producer. Assert initial full scan mute and real SDK receipt read after signed callback.
- [ ] Implement only observed gaps; retain existing proven behavior and scope references without changing omitted-reference canonical bytes.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/calsync ./internal/mcp -run 'TestMCP.*Calendar|TestCalendar.*Events'`; expected PASS on both databases. Commit changes, or record existing passing production coverage without an empty commit.

### Task 10: Complete receipt-bound transcript pages (B2)

**Files:** Create `internal/store/mcp_transcripts.go`, `internal/mcpevents/transcripts.go` and tests; modify meeting/media readers and daemon/API/MCP adapters.

**Interfaces:** `TranscriptPageRequest{EventID, Cursor string; MaxBytes int}`; `TranscriptPage{MessageID, EvidenceVersion string; OffsetBytes int64; Text string; EvidenceComplete, Done bool; NextCursor string}`. `(*Service).ReadTranscript(ctx context.Context, principal string, request TranscriptPageRequest) (TranscriptPage,error)`; native tool `get_mcp_transcript`. Default 32768 bytes, maximum 65536. Authenticated cursor binds the retained event, message reference, version and byte offset. Read complete source evidence through the native parser at primary key; `get_meeting_context` remains bounded.

- [ ] RED: >1 MiB synthetic complete transcript read to its exact end, UTF-8 boundaries, hostile/tampered cursor, changed/deleted/reused row between pages, cross-receipt access and zero silent version substitution.
- [ ] Implement current-version guarded complete reads and cursor authentication; unavailable/incomplete evidence fails honestly. Add receipt-bound chunked recording reads to the existing attachment/blob reader where meeting recordings lack such a path.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/store ./internal/mcpevents ./internal/mcp -run 'TestMCPTranscript|TestMCPRecording'`; expected PASS on both databases and actual SDK wire. Commit.

### Task 11: Meeting and media evidence publication (B2)

**Files:** Create `internal/store/mcp_evidence.go` and tests; modify existing Granola, Circleback, Plaud, Notion, Muesli, meetingimport and docbank media production persistence/observation paths; new meeting-family schemas.

**Interfaces:** `MCPEvidenceObservation{MessageID, MessageReferenceSeq, SourceID, Epoch int64; Kind, Version, RecordingID string; Complete bool}`; `(*Store).PublishMCPEvidence(ctx context.Context, observation MCPEvidenceObservation) error`. External observation computes a canonical readable version; Store revalidates identity/epoch/version under the normal fences. Durable `(message-reference,kind,version)` publication keys outlive journal pruning. Share one evidence version across each legitimately bound occurrence without conflating their read authority.

- [ ] RED per connector: arrival without transcript, later complete readable transcript/recording, partial/cancelled/unavailable result, changed evidence, stale observation, shared fanout and restart. Old archive scans do not create live admissions; explicit imports disclose provenance.
- [ ] Implement coherent metadata and version-bound readiness through actual native parsers. Expose the frozen meeting family and attachment transcript kind only after Task 10 reads pass.
- [ ] GREEN: provider evidence tests and `go test -tags "fts5 sqlite_vec" ./internal/store ./internal/mcpevents ./internal/docbankmedia ./internal/meetingimport -run 'TestMCP.*(Meeting|Evidence|Transcript)'`; expected PASS on both databases. Commit each complete connector unit.

### Task 12: Reuse Twilio and add owned-number callbacks (B3)

**Files:** Reconcile required public PR #1081 `internal/twilio` and shared meeting/CLI/config units; create native `callback.go` tests, Store callback/phone units, daemon callback routes and Events definitions. Do not merge unrelated branch changes.

**Interfaces:** `TwilioCallback{AccountSID, PhoneReference, CallbackKind string; Values url.Values}` admitted durably under configured owned account/phone and exact externally visible URL signature. Callback route acknowledges status/recording notifications only, not TwiML call-control. `(*Store).AdmitTwilioCallback(ctx context.Context, callback TwilioCallback) (bool,error)` dedupes before acknowledgment; reconciliation validates the authoritative completed coverage manifest before publishing.

- [ ] RED: invalid/modified signatures, added signed parameters, wrong public URL/account/number/content type, >1 MiB body, out-of-order and duplicate callbacks, crash after admission, reconnect replay, two numbers with overlapping call IDs.
- [ ] Reuse authoritative client/parsers and add bounded ingress plus durable callbacks. Parent/child legs remain distinct. A completed utterance, stopped stream, partial recording set, unknown coverage or missing transcript never satisfies whole-call completeness.
- [ ] GREEN: exact interval/both-track/required-segment coverage tests and signed SDK `call_changed`/`call_transcript_ready` receipt reads on both databases; native `internal/twilio` suite passes. Commit focused reuse and callback delta separately when useful.

### Task 13: Durable owner-authorized outbound attempts (C1)

**Files:** Create `internal/messaging/types.go`, `service.go`, `preview.go`, tests; `internal/store/outbound_attempts.go`, tests/migration; owner API routes, daemonclient and native MCP tools; explicit config.

**Interfaces:** `ReplyRequest` carries source/conversation IDs+refs, optional reply message ID, exact body, expected sender, preview token and idempotency key. `(*messaging.Service).Preview(ctx context.Context, principal string, request ReplyRequest) (ReplyPreview,error)`; `Reply(ctx context.Context, principal string, request ReplyRequest) (SendAttempt,error)`; `Status(ctx context.Context, principal string, request SendStatusRequest) (SendAttempt,error)`. `SendStatusRequest` accepts exactly one attempt ID or key. Adapter interface `Resolve(ctx, request) (SendTarget,error)`, `Send(ctx, target, request) (SendResult,error)` and `Reconcile(ctx, attempt) (SendResult,error)` uses immutable pinned destinations.

- [ ] RED: preview expires at five minutes; changing body/sender/reply/resource/member/policy/owner denies before provider I/O; receipt-only caller denied. Concurrent equal keys dispatch once; unequal digest conflicts. Both sides of dispatch crash yield durable prepared/unknown and never automatic resend.
- [ ] Implement authenticated preview binding, permanent key/digest tombstones, atomic claim and the six frozen states. Network is outside SQL/gate; shutdown joins owned work. Status-by-key survives lost response, expired preview, removed source, disabled writes/sends and restart without another provider request; expired-token execute always denies.
- [ ] GREEN: `go test -tags "fts5 sqlite_vec" ./internal/messaging ./internal/store ./internal/api ./internal/mcp -run 'TestOutbound|TestMessageReply|TestSendStatus'`; expected PASS on both databases and raw SDK read/write security classes. Commit Store and service/tool units with their tests.

### Task 14: Native provider send/reply adapters (C2)

**Files:** `send.go`/`send_test.go` beside Beeper, Slack, Teams, Discord, Matrix, Gmail and Microsoft mail; a narrowly scoped Graph single-dispatch response-preserving transport; separately configured SMTP sender. Wire adapters in daemon messaging service.

**Interfaces:** Implement Task 13 adapter interface. Single dispatch retains actual provider route/message/pending identities. Effective configuration/permission metadata comes from Task 2, never from archive receipt claims. Provider reads retain existing retry behavior; write transports never invisibly replay ambiguous operations.

- [ ] RED per adapter with native `httptest` provider: exact member/account/channel/thread/reply/envelope/sender, body/permission limits, response identity and request count on 429/5xx/transport/body-read/malformed success. Fake server is justified by the external provider protocol contract and avoids actual writes.
- [ ] Implement Beeper pinned member+pending resolution, Slack exact post/thread, Graph distinct chat/channel and mail acceptance, Discord bot+finite nonce, Matrix device/room transaction, Gmail authorized MIME send and explicit SMTP envelope. Ambiguous outcomes remain unknown; Graph mail/SMTP queue acceptance remains accepted.
- [ ] GREEN each full native adapter suite; changed source/member/device/dedupe window tests and reconciliation never resend. Commit each provider with its tests and accurate discovery limits.

### Task 15: Full native synthetic lifecycles (C3)

**Files:** Native SDK integration tests, daemon messaging tests and each provider's production fake endpoint fixtures. Use reserved addresses and synthetic account/content identities.

**Interfaces:** Consume Tasks 2–14, with actual production importer → Store → signed callback → MCP SDK receipt read → owner preview/execute → exact fake-provider request → status/reconciliation → outgoing archive echo. No special journal-insert bypass as the lifecycle proof.

- [ ] RED per live provider and real supported scope. Two accounts of one Beeper network, simultaneous threads, disconnect/revoke during callback/send, echo before response and authoritative echo after unknown all preserve isolation.
- [ ] Complete any integration gaps. `include_from_me=false` suppresses all own messages; true includes manual/agent messages under the unchanged loop guard. Identical text/time is never trusted correlation or a reason to send again.
- [ ] GREEN on SQLite and PostgreSQL through actual SDK requests and signed deliveries. Assert every advertised provider/scope/read/send tuple has a passing lifecycle; unsupported offline transports remain accurately discovered. Commit.

### Task 16: Owning docs, regeneration and publication gates

**Files:** Update owning MCP/provider/configuration/architecture guides and their website companions, generated Go/browser API clients. Keep this roadmap separate from Phase 1 and retain the original design rationale.

- [ ] Replace blanket no-send claims only for implemented explicit owner-authorized paths. Document configuration, scope discovery, transcript paging, imported versus live content, acceptance versus confirmation, recovery and provider-specific limits.
- [ ] At a stable source boundary regenerate Go/browser schemas, run api-check, build, fmt/vet, lint-ci, owning docs checks, full native SQLite and documented PostgreSQL/pgvector/shipped-only matrices, focused races and actual SDK contracts. Investigate every current check failure and report any unresolved limitation accurately.
- [ ] Inspect and scrub the full diff, PR text, fixtures and commit messages for private data. Review the exact final tree, then publish a focused follow-up above reconciled upstream main after agreement and checks.
- [ ] Monitor current-head CI and reviews, address actionable feedback, and record final evidence with the owning issue.

## Implementation self-review

Every A1–C3 acceptance item has a concrete task above. The registry, lifetime, coherent snapshot, evidence version and send-attempt interfaces are defined before their consumers. Mandatory content races belong to Tasks 3–6; scope replacement/membership to Tasks 2/7; ambiguous writes and recovery to Tasks 13/14; historic-media admission to Tasks 8/11; receipt/deletion races to Tasks 10/15. Provider-specific changes are committed independently. No configured provider capability is inferred from a catalog-only declaration.
