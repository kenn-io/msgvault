# Relationships Analytical Index

Status: proposed for adversarial review.

## Summary

The Relationships web workspace currently derives people, domains, and
relationship scores from the generic `analytical_entries` DuckDB view at
request time. On a 2.5-million-message archive, the first relationships
request takes about 40 seconds and a person search takes 29–61 seconds. A
single request can drive the daemon above 80 GB resident memory and leave more
than 100 GB of fragmented native allocations behind.

This design replaces request-time reconstruction with cache-built analytical
read models:

- a small canonical identity directory for labels and text matching;
- a narrow message/person fact table for filtered analysis;
- unfiltered people and domain rollups;
- an unfiltered relationship rollup containing sufficient statistics for
  daily time decay.

The cache builder derives these datasets set-wise from the existing staged
Parquet snapshot. Interactive endpoints do not scan `messages`,
`message_recipients`, or the generic `analytical_entries` view. The daemon
also runs DuckDB with explicit memory and thread budgets.

The cache schema advances from version 14 to 15. Existing installations perform
one full cache rebuild after upgrade.

## Incident evidence

The live archive used for diagnosis contained:

- 2,514,589 messages;
- 5,940,115 message-recipient rows;
- 70,810 participants;
- 834,091 conversations;
- 204 MB of compressed analytical Parquet.

Observed behavior from the daemon log and direct production-path requests:

| Operation | Observed time |
|---|---:|
| First `POST /api/v1/relationships` | 41.46 s |
| Warm memoized relationships request | 1–3 ms |
| `POST /api/v1/people/search` | 29.08–61.51 s |
| Simplified message/person aggregation | 1.16 s |
| Canonical display-name correlated expression | 32.20 s |
| Canonical identity-match correlated expression | 19.45 s |

During an isolated person search, DuckDB used 5–18 cores and raised process
RSS from approximately 60 GB to approximately 86 GB. A Go CPU profile placed
96.3% of sampled CPU in native code. The live Go heap was approximately 15 MB.
`vmmap` reported:

- 125.7 GB physical footprint;
- 77.0 GB swapped;
- 133.1 GB of `MALLOC_SMALL (empty)` regions;
- only 168.9 MB of live allocations in the default malloc zone.

The problem is therefore not the amount of stored Parquet data or Go heap
retention. It is the shape of the native DuckDB query and the allocator churn
created by its intermediate results.

## Root cause

`analytical_entries` is a view, not a materialized dataset. The current people
and relationships queries expand it for every cold request:

1. Scan messages and message recipients.
2. Aggregate participant IDs, labels, and domains into lists per message.
3. Join conversations, sources, senders, and attachment aggregates.
4. Classify messages and group chat messages into logical conversations.
5. Unnest participant lists back into one row per logical entry/person.
6. Resolve identity clusters and deduplicate aliases.
7. Aggregate activity and relationship signals across the archive.
8. Evaluate nested correlated subqueries for canonical labels and identity
   text matching.
9. Sort the complete population.
10. Apply the HTTP page limit.

The UI requests at most 500 rows, but that limit cannot reduce any of the
preceding work. Typing a selective identity query still constructs the entire
person population before applying the text predicate.

The generic view also carries wide fields that these endpoints do not need,
including subjects, snippets, participant-label lists, and participant-domain
lists. DuckDB may prune some projections, but list aggregation, repeated CTE
materialization, unnesting, distinct operations, correlated joins, and sorting
still create large parallel hash-table and vector-allocation workloads.

Finally, the engine currently uses `GOMAXPROCS` DuckDB threads, has no DuckDB
memory limit, exposes two analytical semaphore slots despite using a single
database connection, and cannot rely on request cancellation to promptly stop
native execution. Slow requests can therefore retain the only connection,
queue later requests, and keep allocating after the HTTP caller has gone away.

## Goals

- Load the unfiltered Relationships landing page in at most 250 ms on the
  2.5-million-message reference archive after daemon startup.
- Return an unfiltered person or domain text search in at most 250 ms.
- Return a filtered person, domain, or relationship query in at most 1 second
  for ordinary predicates on the reference archive.
- Keep interactive analytical queries independent of the width and join
  complexity of `analytical_entries`.
- Bound interactive DuckDB buffer-manager memory to 512 MiB and execution to
  at most four worker threads.
- Keep daemon RSS growth below 1.5 GiB during an interactive analytical query
  and below 256 MiB above the pre-query baseline after the query settles.
- Keep a full cache rebuild for the reference archive at or below 25 seconds
  and below 3 GiB peak RSS.
- Preserve current identity-cluster, owner, chat-grouping, filter, scoring,
  pagination, and cache-revision semantics.
- Preserve atomic cache publication and current SQLite/PostgreSQL authority.
- Make identity link/unlink refreshes cheap without re-exporting base messages.

The latency budgets are cold-process budgets. They do not depend on an
in-process memo populated by an earlier request.

## Non-goals

- Replacing the generic Explore, Everything, Files, or timeline read models.
- Moving the analytical system of record out of SQLite or PostgreSQL.
- Adding approximate person counts or approximate relationship scores.
- Making request timeout the primary resource-control mechanism.
- Introducing a separate DuckDB worker process in this change.
- Freezing relationship decay permanently at cache-build time.
- Optimizing message body, conversation detail, or attachment-content reads.

## Design principles

1. **Filter dimensions must be scalar and narrow.** Interactive identity
   queries must not construct participant, label, or text lists over the
   archive.
2. **Search the dimension before the facts.** Identity text matching belongs
   on a 70,000-row directory, never on a population created from millions of
   activity rows.
3. **Precompute the common case.** The unfiltered landing page and searches use
   one-row-per-identity rollups.
4. **Retain exact filtered semantics.** A normalized fact table remains
   available when date, source, modality, deletion, participant, or domain
   filters require recomputation.
5. **Canonicalization is refreshable.** Large activity facts store durable raw
   participant IDs. Small canonical directories and rollups can be rebuilt
   after link/unlink without rewriting the large fact table.
6. **Resource safety is layered.** Small query shapes are the primary defense;
   DuckDB memory, threads, and concurrency are independent backstops.

## Cache datasets

### `identity_activity`

`identity_activity` is the only large new dataset. It contains one row per
exportable message and involved raw participant after deduplication:

| Column | Type | Meaning |
|---|---|---|
| `message_id` | `BIGINT` | Durable message identifier |
| `conversation_id` | `BIGINT` | Durable conversation identifier, nullable |
| `source_id` | `BIGINT` | Source filter dimension |
| `source_type` | `VARCHAR` | Source modality |
| `occurred_at` | `TIMESTAMP` | Message timestamp |
| `message_type` | `VARCHAR` | Exact stored message type |
| `conversation_type` | `VARCHAR` | Conversation classification |
| `entry_kind` | `VARCHAR` | `email`, `event`, `meeting`, or `item` |
| `is_chat` | `BOOLEAN` | Shared chat-classification result |
| `participant_id` | `BIGINT` | Raw durable participant |
| `participant_domain` | `VARCHAR` | Normalized participant domain |
| `is_author` | `BOOLEAN` | Participant authored the message |
| `is_from_me` | `BOOLEAN` | Message direction baked by the base cache |
| `has_attachments` | `BOOLEAN` | Message has files |
| `attachment_count` | `INTEGER` | File count |
| `deleted_from_source` | `BOOLEAN` | Deletion filter dimension |

Participants are the union of:

- `message_recipients.participant_id`;
- `messages.sender_id`;
- `conversation_participants.participant_id` for chat conversations.

Rows are deduplicated by `(message_id, participant_id)`. `is_author` is
`bool_or(recipient_type = 'from' OR participant_id = sender_id)`.

The dataset is partitioned by `year(occurred_at)`. Within each output shard,
rows are ordered by `(participant_id, occurred_at, message_id)` to improve
Parquet zone-map pruning and compression. It contains no subjects, snippets,
display labels, identifier strings, or participant arrays.

Per-message rows are intentional. `Context` applies before chat messages are
grouped into a logical conversation, so storing only conversation rollups would
make date and source predicates incorrect.

### `identity_directory`

`identity_directory` contains one row per canonical identity:

| Column | Type | Meaning |
|---|---|---|
| `canonical_id` | `BIGINT` | Smallest member ID or unlinked participant ID |
| `display_label` | `VARCHAR` | Existing best-name/fallback policy |
| `partial_label` | `BOOLEAN` | Label is identifier-derived |
| `member_ids` | `BIGINT[]` | Sorted cluster members |
| `search_values` | `VARCHAR[]` | Lower-cased normalized names and identifiers |
| `is_owner` | `BOOLEAN` | Canonical cluster contains an owner identity |

The builder computes labels set-wise with grouped joins and ordered aggregates.
It does not use a scalar correlated subquery. `search_values` contains each
member's non-empty display name, email address, phone number, identifier value,
and identifier display value as a separate normalized element. Searching
unnests this small directory-local list and applies case-insensitive substring
matching to each element. Keeping values separate prevents a query from
spuriously matching across the boundary between two identifiers.

The existing `participants` and `participant_identifiers` datasets remain the
source for response identifier details. Those details are joined only after
the result page is selected, so at most 500 identities are shaped.

### `identity_rollups`

`identity_rollups` contains unfiltered people metrics keyed by canonical ID:

- activity count using the current logical-entry semantics;
- file count;
- first and last activity timestamps;
- per-source-type counts.

Chat messages are grouped by `(source_id, conversation_id, canonical_id)`
before aggregation. Non-chat activity remains one logical entry per message.
An entry involving multiple aliases in the same identity cluster counts once.

The same build pass produces unfiltered domain rollups keyed by normalized
domain. Domain results retain exact canonical person counts.

### `relationship_rollups`

`relationship_rollups` contains one row per non-owner canonical identity:

- anchor UTC date;
- decayed sent, received, and meeting sums at the anchor date;
- raw sent and meeting counts;
- modality bit mask;
- last interaction timestamp.

The cache publication date is the decay anchor. For request date `D` on or
after the anchor, each past weighted sum is advanced with one scalar factor:

```text
sum(D) = sum(anchor) * exp(-lambda * days(D - anchor))
```

This is algebraically identical to decaying every contribution independently
when the contribution occurred on or before the anchor.

Future-dated source rows are handled exactly in a companion
`relationship_future_daily` dataset. It contains daily signal totals by
canonical identity and UTC date only for activity after the anchor. At request
time those rows contribute:

```text
exp(-lambda * max(0, days(D - event_date)))
```

This preserves the current clamp for malformed or legitimately future-dated
events without forcing ordinary requests to scan historical activity.

The final score continues to be calculated by the existing
`RelationshipScore` Go function. The default reciprocity gate continues to use
raw sent and meeting counts.

## Cache-build algorithm

The builder keeps the existing rule that base SQLite/PostgreSQL tables are
exported separately. The new derived datasets are then built from staged
Parquet, not through a large cross-engine SQLite join.

### Full rebuild

1. Export existing base datasets into the sibling staging directory.
2. Create `identity_activity` with one set-wise DuckDB `COPY`:
   - project the narrow scalar message columns;
   - union recipient, sender, and chat-conversation participant edges;
   - aggregate only `(message_id, participant_id)` to deduplicate and derive
     `is_author`;
   - join the small participants dataset once for domain;
   - partition and compress the output.
3. Create `identity_directory` from participants, participant identifiers,
   clusters, and owner participants using grouped joins and ordered
   aggregates.
4. Canonicalize `identity_activity` through `identity_directory`, deduplicate
   `(message_id, canonical_id)`, and produce logical chat units.
5. Produce people, domain, relationship, and future-daily rollups in grouped
   `COPY` operations over the narrow canonical activity stream.
6. Validate row counts, uniqueness, referential membership, schema, and
   required empty-dataset shards.
7. Include every new dataset in the staged fingerprint and publish atomically
   with the version-15 commit marker.

The expensive message/recipient join is executed once per cache publication,
projects only needed columns, and never builds list-valued participant labels
or domains. Rollups reuse a narrow temporary relation within the builder
connection rather than independently re-reading and re-canonicalizing the
facts.

### Incremental publication

For a genuinely append-only cache update:

1. Export `identity_activity` rows only for new message IDs.
2. Append year-partitioned activity shards alongside existing live shards at
   publication.
3. Rebuild the small directory and rollup datasets from the union of live
   activity plus the staged delta.
4. Replace rollup directories atomically; do not append rollup rows.

The existing cache staleness rules already force a full rebuild when covered
messages are updated or removed. The new datasets do not add a second change
detection mechanism.

### Identity-only refresh

Linking or unlinking identities does not change `identity_activity`, because
it stores raw participant IDs. `RefreshIdentityDatasets` is extended to stage:

- `participant_clusters`;
- `owner_participants`;
- `identity_directory`;
- `identity_rollups`;
- domain rollups;
- `relationship_rollups`;
- `relationship_future_daily`.

It regenerates these small derived datasets from committed
`identity_activity`, fingerprints them, and publishes them under one identity
revision. Base messages, recipients, and activity facts are untouched.

Account-identity changes that alter baked `is_from_me` continue to require a
full rebuild.

## Interactive query paths

### Default relationships

1. Read `relationship_rollups`.
2. Advance the three weighted sums from the anchor date.
3. Add the normally empty future-daily contributions.
4. Join `identity_directory` for labels and member IDs.
5. Calculate scores in Go, apply the reciprocity gate, sort, and page.

This path is proportional to the number of identities, not messages. It does
not require the existing in-memory relationships memo for acceptable latency;
the memo may remain as a small optimization.

### Person search

1. Scan `identity_directory`, unnest `search_values`, and retain identities
   where any value contains the normalized query.
2. With no analytical context, join candidates directly to
   `identity_rollups`.
3. With context filters, semi-join the narrow `identity_activity` facts to the
   candidate IDs, apply scalar predicates, form logical chat units, and
   aggregate only the matched candidate population.
4. Sort and page.
5. Shape identifiers and source counts for the selected page.

An empty query may match the whole directory, but it still avoids identity
string work over activity facts.

### Domain search

Domain matching first selects the small normalized domain population.
Unfiltered results use domain rollups. Filtered results aggregate the narrow
facts after candidate selection.

### Filter semantics

Source, time, message type, and deletion predicates apply directly to scalar
fact columns.

Participant and domain groups use semi-joins on `(message_id)` before logical
chat grouping:

- values inside one group are ORed;
- primary and additional groups are ANDed;
- identity participant IDs are expanded through the current directory;
- linked aliases on one entry deduplicate to one canonical result.

This preserves the current `Context` semantics without reconstructing
participant arrays.

## DuckDB resource policy

The long-lived daemon query engine is configured at connection creation with
the following effective settings. Go computes the integer thread value before
issuing `SET threads = <value>`:

```sql
SET memory_limit = '512MB';
SET threads = <min(GOMAXPROCS, 4)>;
SET preserve_insertion_order = false;
```

The query semaphore is reduced from two slots to one, matching the single
DuckDB connection. Requests waiting for the connection remain
context-cancelable and cannot begin native execution after their caller has
left.

The cache builder uses a separate policy, again substituting the computed
integer before execution:

```sql
SET memory_limit = '2GB';
SET threads = <min(GOMAXPROCS, 8)>;
SET preserve_insertion_order = false;
SET temp_directory = '<staging-sibling>/duckdb-tmp';
```

The builder removes its temporary directory after success or failure. A 2 GiB
buffer-manager budget permits parallel scans while providing space to spill
large hash/sort operators. Eight threads are a ceiling, not a target; DuckDB
uses fewer when the machine exposes fewer CPUs.

DuckDB documents that `memory_limit` governs the buffer manager rather than
every native allocation. The design therefore does not describe it as a
process-level hard limit. Narrow SQL shapes, bounded thread counts, measured
RSS acceptance criteria, and removal of correlated archive-wide operations
provide the primary safety.

Request cancellation remains required, but correctness and safety do not
depend on prompt cancellation inside DuckDB native code. Every interactive
query must fit its latency and memory budget when allowed to complete.

## Failure handling

- A missing, partial, or wrong-schema identity dataset makes the whole cache
  unavailable; endpoints do not fall back to the unsafe legacy query.
- A derived-build failure leaves the prior committed cache untouched.
- An identity refresh failure leaves the prior identity revision and all its
  derived datasets untouched.
- An interactive DuckDB out-of-memory error returns the existing analytical
  error response and is logged with the endpoint and cache revision.
- Cache validation verifies that every canonical ID in a rollup exists in the
  directory and that owner IDs do not appear in relationship rollups.
- Empty archives publish schema-correct empty shards for every required
  dataset.

## Verification

### Correctness

Production-path tests build version-15 Parquet from real test stores and compare
new and legacy results for:

- linked and unlinked identities;
- multiple aliases on one message;
- owner aliases across sources;
- email, chat, calendar, meeting, and generic item activity;
- incoming authors versus co-recipients;
- participant and domain intersection groups;
- source, date, message-type, and deletion filters;
- future-dated events;
- pagination and cache/identity revision drift;
- empty archives and identities without names.

Tests use `testify` and invoke the real cache builder and DuckDB engine. They do
not inspect SQL source text or substitute fake commands.

### Query-path guard

An integration test opens a built cache, makes the legacy message and recipient
views unavailable to the identity query connection, and verifies that default
relationships plus unfiltered people/domain search still succeed. Filtered
queries retain access only to `identity_activity` and the small identity
datasets. This proves behavior through the production path rather than grepping
SQL strings.

### Scale and resource regression

An opt-in local performance test generates a deterministic synthetic Parquet
archive with:

- 2.5 million messages;
- 6 million participant edges;
- 75,000 participants;
- a mix of email, chat, and meeting activity;
- linked aliases and owner identities.

It runs in a fresh subprocess so peak RSS and post-exit reclamation are
measurable. The test records:

- full cache-build wall time and peak RSS;
- cold relationships latency;
- cold unfiltered and filtered identity-search latency;
- peak and settled daemon RSS;
- DuckDB operator profile and rows scanned per dataset.

The acceptance budgets are the Goals above. CI continues to run smaller
correctness fixtures; the production-scale test is a release/performance gate
on the reference Apple-silicon machine because shared CI timing and memory
measurements are not stable enough for sub-second assertions.

## Rollout

1. Add the version-15 datasets and builder validation.
2. Add the new query paths behind the version-15 cache requirement.
3. Run correctness equivalence and the production-scale resource benchmark.
4. Remove the legacy people/relationship SQL only after equivalence passes.
5. Ship the schema bump. Daemon startup reports the stale cache and rebuilds it
   through the existing cache lifecycle.

There is no compatibility fallback to the version-14 identity queries. A
version-14 cache is rebuilt before the Relationships web workspace is served.

## Alternatives rejected

### Rewrite the current SQL only

Selecting identity candidates before activity aggregation and replacing
correlated subqueries with joins would materially improve latency. It would
still rebuild participant edges, logical chats, and canonical activity from
base messages on every filtered request. It also leaves memory safety dependent
on optimizer behavior over a complex generic view.

### Cache only HTTP responses

The existing relationships memo makes repeat requests fast but does nothing for
the first request after restart, person search terms, filtered contexts, or
native memory use. It also hides the cost rather than fixing the data flow.

### Store only one row per person

A single unfiltered aggregate cannot answer date, source, modality, deletion,
participant-intersection, or domain-intersection predicates correctly. The
narrow activity fact table is required for exact filtered behavior.

### Run every query in a worker process

A worker process would guarantee reclamation after termination, but process
startup, connection setup, cache validation, IPC, and lifecycle management are
substantial complexity. It does not make inappropriate SQL efficient. Worker
isolation remains a possible later defense-in-depth measure if bounded queries
still reveal native allocator defects.

## Review focus

Adversarial review should concentrate on:

- whether `identity_activity` preserves every current pre-chat filter semantic;
- whether incremental publication can ever duplicate an activity edge;
- whether identity-only refresh publishes all canonical datasets atomically;
- whether anchored decay plus future-daily exceptions is algebraically
  equivalent to current scoring;
- whether the activity build performs only one large edge aggregation;
- whether any interactive path can reach `analytical_entries`, `messages`, or
  `message_recipients`;
- whether the stated DuckDB settings are effective in both daemon and builder
  connections;
- whether the resource benchmark measures native RSS rather than Go heap only.
