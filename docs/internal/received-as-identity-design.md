---
last_edited: "2026-10-02"
---

# Received-account attribution design

**Historical design record; implemented in this change.** Current storage rules
are owned by [Data Storage](../architecture/storage.md), and user-facing
precedence and filters by [Searching](../usage/searching.md#forwarded-mail-and-virtual-accounts).
The [benchmark report](received-as-identity-benchmark.md) records measured
performance and its limits.

One physical inbox can receive mail for several confirmed addresses. Attribution
records the receiving address without changing source provenance or creating
duplicate ingest sources. Sent mail uses its From identity; calendar events use
the registered calendar rather than the sync credential or event creator.

## Attribution decision

The approved decision on 2026-10-01 was one attributed account per message:
nullable indexed `messages.account_address`. Conflicts stay in the unattributed
bucket, and identity totals plus unattributed equal the eligible source total.
Do not choose the first address or final inbox merely to fill the column.

Overlapping memberships were rejected because their totals cannot be added like
separately synced accounts. A scalar projection also supports indexed lists and
Parquet facts. Derived labels would mix attribution with provider/user labels;
virtual ingest sources would duplicate credentials, provenance and checkpoints.

No personal archive or provider inventory was inspected. Header research used
primary documentation, and ordinary fixtures use synthetic addresses. Historical
messages may lack the documented delivery fields.

## Delivery evidence and its limits

Only parse the outer message's header block. Preserve repeated values and
their order. Never mine a forwarded attachment, quoted body or arbitrary
address-shaped text for attribution. Parse mailboxes with the real address
parser; reject malformed values and bound recovery header input.

| Path | Verified semantics | Treatment |
|---|---|---|
| Gmail forwarding | Google recommends `X-Forwarded-For` or `X-Forwarded-To` to signal forwarding, but does not specify a universal value grammar or original-recipient ordering. | Preserve these as hints. A forward destination alone cannot identify the upstream account. Use validated delivery/original-recipient matches; keep ambiguous forwarding chains unresolved. |
| Repeated `Delivered-To` | RFC 9228 describes successive delivery addresses, newest first. A final inbox can differ from the original recipient. The RFC is experimental, not a universal provider guarantee. | Retain the entire chain. Do not let a final-inbox match discard an upstream confirmed alias. Multiple supported upstream identities require stronger evidence or remain ambiguous. |
| Workspace routing | Routing can add `X-Gm-Original-To` when changing the envelope recipient. The setting is optional. | Use a valid, confirmed original-recipient match ahead of final-delivery evidence. Include the actual Workspace field, not just `X-Original-To`. |
| Fastmail forwarding | `X-Delivered-To` is the original envelope recipient; `X-Resolved-to` is the address after alias rewriting. Fastmail documented retaining `X-Delivered-To` on forwarded mail. | Prefer matched `X-Delivered-To` over `X-Resolved-to` / the final Gmail inbox. Treat the historical forwarding post as evidence of behavior, not a guarantee for all present routes. |
| Fastmail received/fetched mail | `X-Original-Delivered-to` can reflect an older Received `for` address; it can be absent for multiple recipients. | Preserve as secondary historical evidence. Do not confuse it with `X-Delivered-To`. |
| Generic `X-Original-To` | Its semantics are server-specific; it is not the documented Fastmail original-envelope field. | Support only a valid address match as a hint. Do not assume every provider inserts it or that it authenticates ownership. |
| Gmail POP fetch | Google documents the feature's retirement, with existing imports retained. No authoritative original-account grammar for `X-Gmail-Fetch-Info` was found in this research. | Support archived POP evidence through valid delivery headers or unique To/Cc matches. Preserve opaque fetch information for future rules; do not guess an identity from a host or free-form token. |
| Alias visible only in To/Cc | Author headers express addressed recipients, not necessarily envelope delivery. | Use a single confirmed match only after stronger evidence fails to resolve an identity. Two matches are ambiguous. |
| Bcc or mailing list | Visible To/Cc can omit the actual recipient. Delivery fields may include list and forwarding addresses. | Exclude unrelated list addresses from the candidate set. Allow a unique delivery match; do not infer the user from List-Id or a missing Bcc header. |

Sources checked on 2026-10-01:

- [RFC 9228](https://www.rfc-editor.org/rfc/rfc9228.html) explains delivery chains and their order.
- [Google forwarding guidance](https://support.google.com/mail/answer/175365?hl=en) documents forwarding hints.
- [Workspace routing settings](https://knowledge.workspace.google.com/admin/gmail/advanced/add-gmail-routing-settings) documents `X-Gm-Original-To`.
- [Fastmail addressing](https://www.fastmail.help/hc/en-us/articles/360058753414-Email-addressing) distinguishes envelope and resolved delivery.
- [Fastmail delivery process](https://www.fastmail.help/hc/en-us/articles/1500000278262-The-email-delivery-process) describes additional delivery evidence.
- [Fastmail forwarding history](https://www.fastmail.com/blog/x-delivered-to-header-now-left-on-forwarded-email/) documents retention of the envelope recipient.
- [Gmail POP changes](https://support.google.com/mail/answer/16604719?hl=en) says existing users can continue until January 2027; this is a historical-archive path, not a prerequisite for new routing.

These headers are attribution hints, not authenticated ownership or permission
to send. When provider-specific original-recipient fields disagree, return
ambiguity. A single strongest-tier candidate wins; lower-tier conflicts remain
in the evidence report. When no original-recipient field resolves the address,
evaluate non-primary delivery-chain candidates, then To/Cc, then a matched
final inbox, then an explicit source-default fallback. A primary-only
`Delivered-To` is deferred until after To/Cc: otherwise the common case of
`To: work@example.com` and `Delivered-To: inbox@example.net` would always
attribute to the sink and hide the work alias. Conflicts at an evaluated tier
stop resolution; they must not fall through to a lower tier. Bcc recorded in
an archive is diagnostic evidence only and never supplies attribution, even
when it is the only confirmed recipient. A Bcc-delivered message needs a
delivery-header match or the labelled source-default fallback. The regression fixtures include a unique archived Bcc without delivery headers.

For IMAP sources, resolve the primary inbox from the configured username, or
the username in the connection identifier when configuration omits it. Validate
it as a mailbox and retain source-confirmed identity checks. Candidate lookup,
attribution and indexed fallback dependencies use this same address; the URL
remains physical source provenance. Authoritative mailbox reconciliation uses
the attribution-aware label helper inside its existing transaction so a Sent
to Archive transition changes the account path without a MIME rewrite.

The primary inbox is a final-delivery candidate. If one other confirmed alias
is supported by the delivery chain and the primary is only the final sink,
attribute to that alias. Two non-primary matches stay ambiguous. A fallback is
labelled `source-default`, never presented as proof of original delivery.
Conflicting evidence yields NULL and basis `ambiguous`; it must not
fall through to the source default. Missing evidence with a known default
can use the source default. A source that intentionally disabled default
identity confirmation must not silently regain it.

## Candidate contract

The pure helper in `internal/emailattribution` accepts normalized candidates
and parsed outer-header evidence. It returns a match, no-match or ambiguity
plus evidence basis. It does not call Gmail or mutate account identities.

Pass the known final-inbox address explicitly; do not derive it from candidate
ordering. Use the source identifier when it is a confirmed email identity, or
the primary provider identity after owner confirmation. If no sink is known,
evaluate all delivery candidates together and retain multiple matches as
ambiguous. Normalize case using the existing identity helper; do not silently
remove dots or plus suffixes from email local parts.

Archive candidates are the source's confirmed email identities. Gmail's
primary and accepted `sendAs` entries can be offered for owner confirmation by
sending workflows; delivery headers never confirm an identity. Pending aliases are excluded
from sender eligibility. Provider inventory failure leaves confirmed archive
identities usable and reports unavailable inventory; it must not require a
network call for an archive search or rebuild.

Draft sender selection intersects confirmed ownership with provider sender
eligibility and applies its explicit From/grant rules. A send-as entry does
not prove that incoming mail was delivered there. Different domains,
`treatAsAlias=false`, and SMTP relay use do not disqualify an otherwise accepted
sender. Current `gmail.SendAs` omits `treatAsAlias` and relay metadata; avoid
adding them unless the sibling's behavior needs them. Google's
[SendAs reference](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.settings.sendAs)
owns those provider fields.

Archive attribution owns extraction; sending workflows own provider sender
eligibility and identity bootstrap. Callers pass normalized candidates so the
helper does not import Store. Provider identity-refresh integration must call
`recomputeAccountIdentitiesWith` in its existing identity-locked transaction
with newly added addresses, while preserving masked-token-only invalidation.

## Account projection

SQLite and PostgreSQL messages carry nullable `account_address`, nullable
`account_path` and `account_attribution_basis` (default `not-derived`). Store
owns versioned repair progress. `account_address` covers inbound, sent and
calendar records. Source/address/date/ID and address/date/ID indexes support
scoped lists and cross-source address searches.

Preserve raw evidence and keep this projection recomputable. Do not change
`source_id`, recipient rows, provider-native authorship or confirmed ownership.
Provider-native sent labels/folders select the sent path; a unique
confirmed From match supplies the sent identity. Receiving a message whose
From happens to be an owned address must not by itself switch to the sent
path. `source_is_from_me` is authorship evidence, not Sent evidence: the shared
email importer historically infers it from From equality, including imports
into an existing Gmail/IMAP source. Several distinct From addresses remain
ambiguous; repeated copies of the same normalized address count as one sender.

For sent mail, zero confirmed From matches yield NULL with basis
`unconfirmed-sender`; an absent From yields `missing-sender`. The sent path
never uses the source-default fallback. This prevents old imports from being
assigned to the login address when their actual sending alias is unconfirmed.

For calendars, use the registered calendar ID. `primary` maps to the token
account; a confirmed email calendar ID maps to that identity. For opaque/group
IDs, use an explicit calendar-to-confirmed-identity mapping; do not infer from
the creator, organizer, access role or calendar title. Calendar selection does
not itself confirm personal ownership. The Google
[CalendarList reference](https://developers.google.com/workspace/calendar/api/v3/reference/calendarList)
offers `dataOwner` for secondary calendars, but the current client drops it
and owner metadata alone does not grant a source-scoped identity binding.
Persist mappings in source configuration, invalidate attribution when changed,
and include cancellation tombstones and recurrence instances in repair.
An opaque calendar ID without a mapping yields NULL with basis
`unmapped-calendar`. An email/primary ID without a source-confirmed identity
yields `unconfirmed-calendar`. Calendar attribution never falls back to the
authenticated account for a different calendar.

The classifier sets `account_path` to `inbound`, `sent` or `calendar`, including
when the result is unattributed. It never uses identity-derived `is_from_me`
to choose a path. A row not yet processed has NULL path and basis
`not-derived`. Successful bases are `original-recipient`, `delivery-chain`,
`recipient-headers` (To/Cc), `final-inbox`, `source-default`, `sent-from` and
`calendar`. Processed NULL results use `ambiguous`, `missing-evidence`,
`malformed-evidence`, `unconfirmed-sender`, `missing-sender`,
`unmapped-calendar` or `unconfirmed-calendar`. `missing-evidence` applies when
there is no candidate and no permitted fallback; `malformed-evidence` applies
when no usable evidence remains after parsing and no fallback is permitted.
Store validates path/basis combinations. Keep path separate from basis so a
sent ambiguity cannot be mistaken for an inbound ambiguity. Export all three
fields to the cache. Other message types retain their existing projection. Changing an attributed
message to another type clears its account with basis `not-applicable`. Repair
progress counts only eligible email/calendar rows, including legacy empty types.

Virtual-account selectors carry the physical source ID and normalized address.
They never invent numeric source IDs. Multi-address sources can show identity
children and an unattributed bucket; single-address sources keep their current
account presentation. A physical source selection still includes all its rows.
Collections continue to group physical sources. Selecting an identity within a
collection intersects the address predicate with that collection's source IDs.

Archived account listings and `query.AccountInfo` add an optional
`virtual_accounts` array to physical sources. Children carry `key`, `source_id`,
`account_address` or `group`, `unattributed`, `message_count`,
`source_deleted_count` and `pending_count`. Keys are
`identity:<source_id>:<base64url(address)>`,
`group:<source_id>:<base64url(group)>` and `unattributed:<source_id>`.
They identify read selections and never sources. The catalog includes zero-count
confirmed email identities and the unattributed bucket. Pickers keep the ordinary
single-primary-address presentation and show children for aliases or groups.

Counts cover eligible email/calendar messages, exclude `deleted_at` rows, and
separate active and source-deleted totals. SQL groups masks into one exclusive
bucket. Confirmed identities plus unattributed partition eligible physical rows.
The physical source still includes its chats and other message types.
`/cli/accounts`, MCP, TUI and Web UI consume the archive projection;
`/api/v1/accounts` remains physical scheduler configuration.

Pending legacy rows belong to the unattributed bucket and carry `pending_count`.
The Web UI displays that repair is incomplete. Exact identity filters return
only rows whose attribution has been derived; run the explicit repair before
comparing a legacy archive's identity counts. The implementation uses indexed maintenance
instead of blocking source generations: confirmations recompute indexed dependencies
atomically, while pending initial repair remains visible and additive.

`account:<address>` provides exact search for inbound, sent and calendar attribution;
`received:<address>` adds `message_type=email` and `account_path=inbound`.
It never uses `is_from_me` to exclude sent rows. The reserved
`account:unattributed` selects NULL attribution across these message types;
`received:unattributed` restricts that bucket to inbound email. Other address
values require a nonempty valid mailbox; no substring or domain wildcard.
Repeated `account:` values form one OR group; repeated `received:` values
form a separate OR group. AND those two groups with each other and with other
filters. This is an explicit new contract: existing recipient operators have
OR semantics in query engines but AND semantics in Store search. The new groups use OR on every path; existing recipient operator semantics
remain unchanged. A structured multi-select can OR addresses within its
group, then AND that group with source, collection,
deletion and other filters. Malformed values fail validation at every front
door. Expose the same account-address selector in API, MCP, CLI, TUI and Web UI
filters and stats. An explicit account selector supplies its email/calendar
scope to totals and aggregates, so the generic email default cannot exclude
mapped calendars. A received selector still restricts the scope to inbound
email. Source identifier and source-pinned key resolution use `SourceAccountLister`
and `/api/v1/cli/source-accounts` without message totals; unqualified aliases
load the virtual catalog to detect ambiguity. Older daemons can use the existing
physical catalog fallback. Keep transport fields additive, update OpenAPI/generated
clients, and never reinterpret an existing numeric account selector.

The structured selector uses `account_addresses` for normalized addresses or
`account_unattributed=true` for NULL attribution; these fields are mutually
exclusive. Both are optional and preserve the existing unfiltered behavior
when omitted. API and query-engine filters share these semantics; MCP accepts a source-pinned
virtual key or unique address/group in its account parameter;
CLI search uses the operators, while TUI/Web UI bucket selection sends the
structured unattributed selector. Source/collection restrictions still apply
to the bucket. This selects all NULL bases, including ambiguous, unmapped and
not-yet-derived rows; progress must distinguish incomplete repair from evidence
that was processed and remained unattributed.

Export attribution and basis into analytics message facts. Update cache schema
versioning, incremental change journals, identity-change invalidation, facets
and account statistics. Stale caches must use eligible live SQL or the existing
readiness error until rebuilt; they must not silently omit the filter. Carry
filters through keyword, fast search, hybrid and vector retrieval before
pagination so limits and totals are accurate. No list, aggregate or search path
may read MIME or scan `message_bodies`.

## Fastmail masks at scale

`account:fastmail-masked:<account>` selects a source's confirmed masked identities
as one group. `received:<address>` always selects one exact inbound identity.
The archive catalog collapses 1000+ masks into one group row. Queries use an
indexed membership lookup instead of expanding masks into parameters.

Provider identity records preserve `provider-alias` and add the exact signal
`fastmail-masked-email` for masked-email membership. Store exposes
`account_identity_group_memberships` and
`RecomputeAccountAttributionForIdentitiesContext(ctx, sourceID, addresses)`.
The provider refresh owns metadata and the membership stamp; integration status
is recorded in Kata. This does not change
the confirmed-identity ownership authority or draft authorization.

## Backfill and incremental maintenance

1. Initialize nullable indexed account facts, compact evidence, indexed mentions
   and a source-scoped repair cursor. Initialization does not parse the archive.
2. `repair-account-attribution` runs through the daemon. Each page takes the
   existing identity-mutation lock, captures or resumes a fixed high-water ID
   and rule version, and reads at most 500 eligible message IDs in key order.
   It loads MIME by primary key, or immutable recipient snapshots if MIME is
   absent. Calendar rows use source configuration and event metadata.
3. Projection updates, evidence/mentions and the cursor commit together.
   Cancellation rolls back the page. Checkpoints run after commit. Conditional
   writes make replay a no-op, including derived-cache revisions.
4. Confirmation/removal transactions hold the same identity lock and revisit
   only indexed mention matches. Source-default and calendar identities are
   dependencies too. Mapping changes recompute that source's calendar events.
   Provider config refreshes preserve explicit calendar mappings. A rule-version
   change restarts repair without an initial lower bound, including zero and
   negative message IDs. The shared lock replaces per-source candidate fingerprints and mapping
   generations.
5. Sync/import persistence, raw/recipient repairs, label changes and calendar
   metadata updates derive current attribution in their write transaction.
   A repair cannot overwrite newer evidence. New rows above the repair's fixed
   high-water mark are maintained by these write paths. A completed cursor is
   an idempotent no-op; targeted identity changes still apply after completion.

Calendar mapping changes commit the mapping and every affected event projection
together. A large calendar can hold the identity lock for a long transaction.
Paged mapping changes remain follow-up work: they need a visible pending mapping
generation so readers can distinguish old projections from the new mapping.

For 150k messages, initial repair costs O(messages + header bytes read).
Compressed MIME still requires decompression on initial repair. Compact outer
header evidence and an indexed mention table retain unconfirmed addresses.
Identity additions/removals revisit only messages with matching mentions.
Source-default and calendar mappings register their address dependencies too.
Identity changes reuse compact evidence without MIME decompression. Index construction and cache regeneration also cost
one archive pass; measure them separately from steady-state search latency.
