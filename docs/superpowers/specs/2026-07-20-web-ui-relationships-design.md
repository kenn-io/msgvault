# Web UI: Relationships-Centric Redesign

Date: 2026-07-20
Status: Approved design, pending implementation plan

## Problem

The current web UI is a first cut with quality, performance, and product
problems:

- The product foregrounds the haystack: the landing view is an "Everything"
  table of 2.5M rows, dominated by mailing lists, bots, and notifications.
  There is no way to see the important relationships in one's life, or the
  full story of one relationship across email, chat, and meetings.
- The dark theme visibly breaks when the inspector opens: the unpinned
  inspector renders as a kit-ui `DetailDrawer` with a full-viewport 62% black
  modal scrim (`--overlay-bg`, theme-dark.css), collapsing contrast behind it.
- Meeting-transcript excerpts show raw `###` markdown in table rows.
- Performance: slow initial load, slow search/filter, sluggish table
  interaction (root causes identified below).
- `web/src/lib/components/shell/AppShell.svelte` is a 1,907-line god
  component with ~10 overlapping `$effect`s; `PeopleWorkspace.svelte` and
  `DomainWorkspace.svelte` duplicate ~300 lines.

## Goals

1. Reshape the UI around relationships: a ranked view of the most important
   people, and for each person a unified timeline of email, chat, and
   meetings plus associated files.
2. Visual clarity on par with Fastmail/Gmail: crisp, clean, professional,
   office-application feel. Buttery-smooth interaction. Improvements land at
   the token/typography level in kit-ui where appropriate (kit-ui work is
   in-bounds).
3. Fix the identified performance and quality defects in the same pass
   (redesign-led sequencing: don't polish screens the redesign replaces).

Non-goals for this cycle: relationship-score configuration UI, automatic
identity-merge suggestions, org inference from domains, any change to
deletion execution semantics.

## Information architecture

Navigation becomes:

**Relationships** (home) · Everything · Files · Saved Views · Sources ·
Deletions · Settings

- The People and Domains workspaces merge into Relationships as two facets
  (People | Domains toggle) of one shared hub component, eliminating the
  duplicated workspace pair.
- The Domains facet keeps its existing activity/latest/label ranking; the
  relationship score, reciprocity gate, and "show all senders" control are
  people-facet only this cycle. Domains remain exact facts, not inferred
  organizations.
- Relationships is the default landing view. Everything remains one click
  away and keeps its slice-and-dice role (filters, grouping, selection,
  deletion staging).
- Existing `workspace=people` and `workspace=domains` URL states are
  normalized at parse time to the corresponding Relationships facet, so
  old bookmarks and browser history keep working.

## Relationships workspace: three-pane mail layout

Fastmail's interaction shape — no page navigation; all three panes update in
place:

```
┌──────────────┬──────────────────────────────┬─────────────────────┐
│ RELATIONSHIPS│ Person header: name,         │ READING PANE        │
│ ⌕ search     │ identity chips, sparkline,   │ selected item       │
│ ranked list  │ [Link identity] [Files N]    │ rendered in place   │
│ of people    ├──────────────────────────────┤                     │
│ (or domains) │ Unified timeline: month      │ email → ContentFrame│
│              │ headers; email / chat-burst /│ chat → message run  │
│ [show all]   │ meeting rows, virtualized    │ meeting → formatted │
└──────────────┴──────────────────────────────┴─────────────────────┘
```

**Left pane — ranked people list.** Display name, modality/channel badges,
last-interaction date, small activity indicator. Search finds any person,
ranked or not. Newsletters/bots are excluded by the reciprocity gate; an
explicit "show all senders" control lifts the gate.

**Center pane — person header + unified timeline.**

- Header: display name, identity chips from `participant_identifiers`,
  activity sparkline over the relationship lifetime, item counts, `Link
  identity` action, `Files N` toggle.
- Timeline: one row per logical item under month headers. Emails and meeting
  notes are individual rows. Chat rolls up into bursts: consecutive messages
  in one conversation on one day render as a single "N messages in
  <conversation>" row. Kind is differentiated by icon plus a restrained color
  accent. Virtualized (kit-ui `virtualSlice`), `j`/`k` navigation, cursor
  pagination; no unbounded walk-to-end.
- `Files N` switches the center pane to a files table scoped to the person
  (chronological; same reading-pane preview on select). Timeline and files
  are two views of the same relationship, one keystroke apart.

**Right pane — reading pane.** Selecting a timeline row renders the item in
place: emails through the existing sanitized `ContentFrame`, chat bursts as a
message run, meeting notes with transcript markdown rendered properly.
Attachments listed at the bottom, opening in the existing viewers. Nothing in
the workspace is modal; no scrim, ever.

**Context carry.** Arriving from Everything with an active filter shows the
filter as a removable chip scoping both timeline and files (the same
context-scoping semantics docs/web-ui.md already defines for People).

**Entry from Everything.** Person/domain rows and the inspector gain an
"Open relationship" affordance that opens the hub with that person selected
and the current filter carried along.

**Narrow screens.** Panes collapse right-to-left: below the three-pane
breakpoint the reading pane stacks over the timeline (Esc returns);
below the two-pane breakpoint the people list becomes a slide-in drawer.
No new keyboard chords; `j`/`k` act on the pane that has focus and Esc
walks back one layer, consistent with the existing shell semantics.

## Relationship ranking (backend)

New endpoint `POST /api/v1/relationships`, computed server-side over the
DuckDB/Parquet analytical cache.

**Data contract.** Two cache additions, both covered by a cache schema
version bump and a one-time full rebuild:

- The messages Parquet dataset gains an `is_from_me` column, but it cannot
  simply re-export `messages.is_from_me` unchanged. That column is reliable
  for native chat/calendar importers, which set it from platform direction
  flags (iMessage `is_from_me`, Beeper `IsSender`, calendar organizer). It
  is *not* reliable for email: the Gmail/IMAP sync path (`internal/sync`)
  never sets it — rows keep the default `false` — and the MIME importer
  (`internal/importer/ingest.go`) sets it only by comparing the From
  address against a single source identifier string, not against every
  confirmed `account_identities` row for that source. Exporting the column
  as-is would make `sent_to_them` zero on a typical Gmail/IMAP archive,
  and the reciprocity gate would then hide legitimate relationships. The
  export instead derives `is_from_me` at cache-build time as
  `messages.is_from_me OR (sender's address matches, case-insensitively, a
  confirmed account_identities row for that message's source)` — the `OR`
  means correctly-set chat/calendar rows are unaffected. This derivation
  runs entirely at export time against data already in the archive, so it
  works for existing archives without any re-sync or re-import. Regression
  tests cover Gmail and IMAP fixture archives where `messages.is_from_me`
  is false but the sender is a confirmed account identity, asserting the
  exported column is `true`.
- A small `owner_participants` Parquet dataset maps each source to its
  owner participant IDs, derived from `account_identities` joined to
  `participant_identifiers` at export time. This is what makes
  `meetings_together` computable: for calendar events `is_from_me` only
  marks events the owner organized, while attendance is stored in
  participant junctions — an event organized by someone else with the
  owner attending is still "together." The dataset is regenerated on every
  cache build and additionally whenever the identity revision (below)
  changes, under the same refresh contract as the cluster mapping.

No other owner-identity injection into DuckDB at query time, and no
reply-graph signal in this cycle (`reply_to_message_id` is not exported;
"replies exchanged" is dropped as a distinct signal in favor of the
sent/received counts below).

**Per-person inputs** (person = canonical identity cluster, see below):

- `sent_to_them` — count of messages with `is_from_me` where the person is
  among the recipients or direct-conversation participants.
- `received_from_them` — count of messages authored by the person.
- `meetings_together` — calendar/meeting items whose participants include
  both an owner participant (via `owner_participants`) and the person.

**Gate.** `sent_to_them >= 1 OR meetings_together >= 1`; otherwise the
person is excluded from the default ranking (this filters newsletters,
bots, and lists). "Show all senders" lifts the gate.

**Score.** Each input is summed with exponential time decay
(half-life 365 days), combined with per-signal weights — default
`sent_to_them × 2.0`, `meetings_together × 3.0`,
`received_from_them × 1.0` (initiated contact and shared meetings signal a
relationship more strongly than inbound volume) — then multiplied by a
modality-breadth boost of `1 + 0.25 × (modalities − 1)` for
email/chat/meetings presence. Ties break by most-recent interaction, then
display name. Weights and half-life live in one documented Go function
with table-driven tests; no configuration surface initially.

**Engine policy.** Relationship ranking and the hub require the DuckDB
analytical engine (`query.PeopleAnalyzer` is implemented only there, by
design). Under PostgreSQL or the live-SQL fallback engine, the
Relationships workspace renders a named degraded state — consistent with
the cache-state philosophy of naming states rather than silently
substituting — and the default landing view falls back to Everything.
No PG/SQL ranking implementation this cycle.

## Identity linking

Builds on the existing identity tables (`participants`,
`participant_identifiers`). Collections group *sources*, not participants,
and are out of scope here.

**Model: reversible canonical clusters.** UI-asserted links do not mutate
participants, identifiers, or message junction rows. A new table
`participant_links(participant_a, participant_b, created_at)` stores
user-asserted edges between participant rows; connected components resolve
to a canonical cluster at read time. All hub queries (ranking, timeline,
files) group by cluster. `Unlink` deletes the edge — fully reversible
because nothing else moved.

**Graph invariants.** Edges are normalized `participant_a <
participant_b`; self-edges are rejected by constraint. The link graph is
kept a forest: a link request between two participants already in the
same cluster is rejected with a named `already_linked` (409) error,
*unless* it exactly re-submits an existing edge (same normalized pair),
which is an idempotent 200 no-op instead — see the response contract
below. `already_linked` is reserved for a genuinely new, redundant edge
between two distinct participants that are already indirectly connected
through the cluster. This split keeps the forest property intact: every
edge that is actually inserted joins two previously distinct components,
and every unlink deterministically splits one cluster in two.

**Cluster identity.** The canonical cluster ID is the smallest participant
ID in the cluster. Relationship APIs accept any member participant ID and
resolve it to the canonical cluster, echoing the canonical ID in the
response; the UI rewrites its URL to the canonical ID. After a link,
unlink, or importer merge changes cluster membership, an open relationship
URL re-resolves on next load — it never 404s while the referenced
participant exists; it simply lands on that participant's current cluster.

**Importer merges.** `MergeParticipants` (irreversible, evidence-based)
remains importer-only, but must become link-aware: in the same
transaction it rewrites `participant_links` endpoints from the absorbed
participant to the survivor, drops resulting self-edges, deduplicates
re-normalized edges, and bumps the identity revision. Endpoint rewriting
alone does not preserve the forest invariant: when the merged pair are
themselves the two ends of an existing multi-edge path, contraction turns
that path into a cycle. For example, links A–X, X–Y, Y–B form a path from
A to B; merging A and B (B absorbed into A) rewrites the Y–B edge to
Y–A, leaving A–X, X–Y, Y–A — a cycle, not a tree. After that, `Unlink`
can no longer deterministically split the cluster in two (removing any
one edge of the cycle still leaves the other two connecting every
member). So after endpoint rewriting and dedup, the merge recomputes a
deterministic spanning forest for every cluster touched by the merge:
rebuild that cluster's edges from scratch as a canonical spanning tree
over its current members, ordered by participant ID (e.g. a path linking
each member to the next-lowest-ID member already in the tree), discarding
whatever cyclic edge set the rewrite produced. Tests must cover an importer
merge involving user-linked participants connected via paths longer than
two edges, asserting the forest invariant holds afterward (edge count is
exactly member count minus one, no cycles) and that `Unlink` on any
surviving edge still splits the cluster. The UI never re-parents
identifiers via `SetParticipantIdentifier`, which would strand historical
message references on the old participant.

- The person header shows all identifiers across the cluster as chips,
  annotated by source participant.
- `Link identity` opens a search over participants/identifiers; confirming
  inserts a link edge via a new API. `Unlink` removes it.
- Automatic merging stays conservative (explicit archive evidence only, per
  docs/web-ui.md); this feature is purely user-asserted linking.

**Cache consistency.** Message Parquet never changes on plain link/unlink
(message participant IDs are untouched by the cluster model). The
participant→canonical-cluster mapping is exported as its own small Parquet
dataset (alongside `owner_participants`), versioned by an identity
revision counter (`identity_revision` in `archive_metadata`). Link, unlink,
and a link-aware importer merge that touches no link edges all bump only
this counter, and the API attempts the cheap identity-dataset refresh
(`owner_participants` + the cluster mapping) synchronously, without
touching message Parquet.

`owner_participants` is also derived from `account_identities`, and
`account_identities` is what backs the `is_from_me` flag *baked into every
message Parquet shard at export time* — not just the identity datasets.
Two mutation classes therefore reach further than a plain link/unlink:
confirming or removing an account identity (`AddAccountIdentity` /
`RemoveAccountIdentity`, exposed via the identity CLI/API) changes which
participants are owners for a source, and a participant merge
(`MergeParticipants` / the phone-dedupe merge path) repoints
`messages.sender_id`, which can leave a stale `is_from_me` on the merged
sender's messages. Both bump a second counter, `account_identity_revision`,
in the same transaction as `identity_revision`; the mutation's own
synchronous refresh is still the cheap identity-dataset one (it cannot
rewrite already-exported message shards). Cache staleness compares both
counters: `identity_revision` drift alone is satisfied by that cheap
refresh, but any drift in `account_identity_revision` forces the *next*
scheduled or on-demand cache build to be a full rebuild rather than a
skip or the lightweight identity-only path — that is the only path that
re-derives `is_from_me`. Without the second counter, an identity
addition/removal or a participant merge would leave `is_from_me` (and
everything downstream of it — `sent_to_them`, `meetings_together`)
silently stale until an unrelated full rebuild happened to refresh it.

**Link/unlink response contract.** Link/unlink and account-identity
add/remove are all idempotent:
re-submitting the exact same edge that already exists (same normalized
`participant_a < participant_b` pair) and re-deleting an edge that is
already missing are both no-ops returning current state, not errors — this
is what makes retries after a `stale` cache response or a dropped
connection safe. A *new* edge between two participants already connected
through the cluster (not the same pair) still hits `already_linked`
per the graph invariants above. The archive mutation commits first, then the API
attempts the identity-dataset refresh synchronously. The response is
`200` whenever the mutation is durable, with body
`{identity_revision, cache_state: "ready" | "stale"}`. `stale` means the
refresh failed; the UI shows the named `identity_cache_stale` state
(with the standard rebuild guidance) rather than silently serving pre-link
groupings, and retrying is safe — repeating the same link/unlink call
re-attempts the refresh without re-mutating.

## Relationship timeline contract

The hub timeline is a new endpoint (`POST /api/v1/relationships/{id}/timeline`),
not a reuse of the person-timeline forwarding into Explore — Everything's
one-row-per-conversation-lifetime chat semantics are unchanged.

- **Row identity**: emails and meeting notes one row per message, as
  today. Chat rows are day bursts keyed
  `(source_id, conversation_id, local_day)`; the row carries message
  count, first/last message time, and the latest snippet.
- **Day bucketing** uses a client-supplied IANA timezone (default UTC).
- **Cursor**: ordered by `(occurred_at DESC, entry_key)` where a burst's
  `occurred_at` is its latest message time. The opaque cursor binds the
  canonical cluster ID, timezone, active filters, message-cache revision,
  and identity revision (mirroring the request/revision binding of the
  existing explore cursors). A mismatch on any of these — e.g. a
  link/unlink between pages — returns a named `cursor_invalidated`
  conflict and the client restarts pagination from the top.
- **Reading pane**: selecting a burst loads its messages through the
  conversation endpoint extended with optional UTC `start`/`end` bounds,
  paginated within that window (the current anchor-plus-counts form caps
  at 50 messages each way and has no time constraint, so it can neither
  complete a large burst nor exclude adjacent-day messages).

## Shell architecture changes

- **Inspector**: the pinned side panel becomes the only inspector mode in
  Everything. The modal `DetailDrawer` + scrim usage is removed (fixes the
  dark-theme washout). `DetailDrawer` remains for true modal flows only.
  The dead `relationships`/`summary` extension-slot scaffolding in
  `Inspector.svelte` is removed; the inspector gains "Open relationship".
- **AppShell decomposition**: split into per-workspace container components
  plus a shared data-loading controller. Replace `JSON.stringify(...)`
  reactive-dependency hacks (10 occurrences) with explicit `$derived`
  fingerprints. Collapse the overlapping per-predicate `$effect`s so one
  state change triggers one coordinated load.

## Performance fixes

| Symptom | Root cause | Fix |
|---|---|---|
| Slow search/filter | People/Domains/Files search issues a `limit: 500` DuckDB query per keystroke, each written as committed URL history | Debounce ~250 ms; keep typing in draft state; commit on settle/submit, matching Everything's submit-only semantics |
| Table interaction | `End` (`loadThroughEnd`) walks the entire result set in 500-row round-trips; overlapping effects fire main query + coverage + preflight + match-counts together | Cap walk-to-end or jump to a server-computed last page; coordinate loads in the shared controller |
| Initial load | AppShell effect fan-out on first render; coverage poll at a fixed 1s interval while initializing | Consolidated first load; backoff on the coverage poll |
| Excerpt noise | Granola/Circleback importers persist raw body prefixes (including `###` markdown) as stored snippets, and Explore emits the stored snippet directly | Flatten markup at query/render time in the explore row path, which covers all existing stored snippets without a migration or re-import; cleaning at import time is an optional follow-up |

Already sound, keep as-is: Everything-table virtualization, cursor
pagination with cycle detection, dynamic pdfjs import, bundle splitting.

## Visual system

A Fastmail-grade crispness pass done at the design-token level so the whole
app inherits it; kit-ui changes are made upstream in kit-ui:

- Full dark-token coverage: fix the fragile `:root, :root[data-theme='light']`
  base-layer pattern in `theme-light.css` so a token added to light cannot
  silently leak its light value into dark mode; same for `color-scheme`.
- One type scale, quieter table chrome (lighter borders/row separation),
  consistent density between panes, professional office-application tone.
- No modal scrims over analytical surfaces.

## Testing

- Colocated vitest component tests (existing convention) for the hub panes,
  chat-burst rollup, and identity-link flows.
- Playwright flows: three-pane navigation (list → timeline → reading pane),
  files toggle, identity linking, context carry from Everything.
- Table-driven Go tests for the ranking function and the new
  relationships/identity-link endpoints.
- Tests exercise real behavior through production paths per project policy.

## Decisions log

- Sequencing: redesign-led (fixes land with the redesign, not before).
- Ranking signals: reciprocity-weighted + recency-weighted volume +
  modality breadth, all three combined; reciprocity acts as the default gate.
- Identity: build on the existing participants/identifiers tables; expose
  user-asserted linking in the UI (collections group sources and are out
  of scope).
- Landing view: Relationships home (falls back to Everything when the
  analytical engine cannot serve people analysis).
- Layout: three-pane mail style (Fastmail shape).
- Visual bar: crisp, clean, professional; kit-ui improvements in-bounds.
- Identity model: reversible canonical clusters via user-asserted link
  edges kept as a forest; canonical ID = smallest member participant ID;
  no re-parenting, no destructive merges from the UI; importer
  `MergeParticipants` becomes link-aware and recomputes a canonical
  spanning forest for any cluster it touches (contraction can otherwise
  create a cycle). Re-submitting an existing edge is an idempotent 200
  no-op; `already_linked` (409) is reserved for a new edge between
  distinct, already-connected participants.
- Ranking data: `is_from_me` is derived at cache-export time (existing
  `messages.is_from_me` OR sender matches a confirmed `account_identities`
  row for that source) rather than exported unchanged, because the
  Gmail/IMAP sync and import paths do not populate it reliably; covered by
  regression tests against Gmail/IMAP fixture archives. Plus an
  `owner_participants` source→owner mapping exported to the cache
  (one-time full rebuild); reply-graph signals deferred; signal weights
  sent 2.0 / meetings 3.0 / received 1.0.
- Identity-revision scope: link, unlink, and link-aware importer merge all
  bump `identity_revision` and synchronously attempt the cheap
  identity-dataset refresh. Account-identity confirm/remove and participant
  merges (which repoint `messages.sender_id`) additionally bump a second
  counter, `account_identity_revision`, since `owner_participants` and the
  baked `is_from_me` flag both depend on `account_identities` — a drift
  there forces the next cache build to be a full rebuild instead of a skip
  or the lightweight identity-only path.
- Chat bursts: new relationship-timeline endpoint keyed by
  (source, conversation, local day); Everything semantics unchanged.
- Domains facet: activity-ranked, no reciprocity gate this cycle.
- Excerpt cleanup: query-time flattening (covers existing stored
  snippets); import-time cleanup optional follow-up.
- Defaults recorded: decay half-life 365 days, breadth boost
  `1 + 0.25 × (modalities − 1)`, tie-break recency then name.
