# Relationships Analytical Index

Status: approved after two adversarial-review rounds.

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
- narrow message facts plus origin-aware direct and conversation participant
  edges for filtered analysis;
- unfiltered people and domain rollups;
- an unfiltered relationship rollup containing sufficient statistics for
  daily time decay.

Deriving datasets from the staged Parquet snapshot is a new cache-builder
mechanism. It keeps this work set-wise and inside one bounded DuckDB process
without constructing a cross-engine SQLite join. Interactive identity-summary
endpoints do not scan `messages`, `message_recipients`, or the generic
`analytical_entries` view. The daemon also runs DuckDB with explicit memory,
thread, spill, and concurrency budgets.

The cache schema advances from version 14 to 15. Existing installations perform
one full cache rebuild after upgrade on SQLite-backed archives. PostgreSQL
backends do not build or serve the Parquet analytical cache today and are not
changed by this design.

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
memory limit, and exposes two analytical semaphore slots despite using a
single database connection. The duckdb-go driver does interrupt native
execution when its request context is canceled, and the engine passes that
context through. The relationships memo has a narrower cancellation defect:
its `singleflight` computation captures the leader's context, so followers
cannot cancel their own wait and inherit a leader cancellation. Cancellation
is still too late a defense for a query whose normal completion can allocate
tens of gigabytes. Slow requests can retain the only connection, queue later
requests, and create unacceptable allocator pressure before interruption.

## Goals

- Load the unfiltered Relationships landing page in at most 250 ms on the
  2.5-million-message reference archive after daemon startup.
- Return an unfiltered person or domain text search in at most 250 ms.
- Target at most 1 second for a filtered person, domain, or relationship query
  with ordinary predicates on the reference archive. This target is
  provisional until measured with the new narrow datasets at four threads.
- Keep interactive analytical queries independent of the width and join
  complexity of `analytical_entries`.
- Bound interactive DuckDB buffer-manager memory to DuckDB's decimal
  `512MB` setting and execution to at most four worker threads.
- Keep daemon RSS growth below 1.5 GiB during an interactive analytical query
  and below 256 MiB above the pre-query baseline after the query settles.
- Keep a full cache rebuild for the reference archive at or below 25 seconds
  and below 3 GiB peak RSS.
- Preserve current identity-cluster, owner, chat-grouping, filter, scoring,
  pagination, and cache-revision semantics.
- Preserve atomic cache publication and SQLite authority. Leave the current
  PostgreSQL analytical-cache behavior unchanged.
- Make identity link/unlink refreshes bounded and avoid re-exporting base
  messages.

The latency budgets are cold-process budgets. They do not depend on an
in-process memo populated by an earlier request.

## Non-goals

- Replacing the generic Explore, Everything, Files, or timeline read models.
- Moving the analytical system of record out of SQLite or PostgreSQL.
- Adding approximate person counts or approximate relationship scores.
- Making request timeout the primary resource-control mechanism.
- Moving interactive queries into a separate DuckDB worker process. Cache
  building and derived refreshes already use, or will use, short-lived child
  processes so native allocations are reclaimed on exit.
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
4. **Retain exact filtered semantics.** A normalized activity model remains
   available when date, source, modality, deletion, participant, or domain
   filters require recomputation.
5. **Canonicalization is refreshable.** Activity edges store durable raw
   participant IDs. Canonical directories and rollups can be rebuilt after
   link/unlink without rewriting message facts or raw direct edges.
6. **Resource safety is layered.** Small query shapes are the primary defense;
   DuckDB memory, threads, and concurrency are independent backstops.

## Cache datasets

### Origin-aware activity model

One undifferentiated message/person table cannot reproduce the current
participant and domain rules. The index therefore separates scalar message
facts, message-level participant edges, and conversation-level participant
edges. This also lets conversation membership remain fresh without duplicating
it across every historical message.

#### `identity_entry_facts`

`identity_entry_facts` contains exactly one row per exportable message:

| Column | Type | Meaning |
|---|---|---|
| `message_id` | `BIGINT` | Durable message identifier |
| `conversation_id` | `BIGINT` | Durable conversation identifier, nullable |
| `source_id` | `BIGINT` | Source filter dimension |
| `source_type` | `VARCHAR` | Source modality |
| `occurred_at` | `TIMESTAMP` | Full message timestamp |
| `message_type` | `VARCHAR` | Exact stored message type |
| `conversation_type` | `VARCHAR` | Conversation classification |
| `entry_kind` | `VARCHAR` | `email`, `event`, `meeting`, or `item` |
| `is_chat` | `BOOLEAN` | Shared chat-classification result |
| `is_from_me` | `BOOLEAN` | Message direction baked by the base cache |
| `has_attachments` | `BOOLEAN` | Message has files |
| `attachment_count` | `INTEGER` | File count |
| `deleted_from_source` | `BOOLEAN` | Deletion filter dimension |

Per-message rows are load-bearing: all context predicates apply before chat
messages are reduced to a logical conversation. The dataset is partitioned by
`year(occurred_at)` and contains no subject, snippet, label, identifier, or
participant arrays.

#### `identity_direct_edges`

`identity_direct_edges` contains one row per distinct
`(message_id, participant_id)` from message-level evidence:

| Column | Type | Meaning |
|---|---|---|
| `message_id` | `BIGINT` | Message fact foreign key |
| `occurred_year` | `SMALLINT` | Storage partition key copied from the message |
| `participant_id` | `BIGINT` | Raw durable participant |
| `participant_domain` | `VARCHAR` | Normalized participant domain |
| `is_sender` | `BOOLEAN` | Participant equals `messages.sender_id` |
| `is_author` | `BOOLEAN` | A sender or `recipient_type = 'from'` edge exists |

The input is the union of `message_recipients.participant_id` and
`messages.sender_id`. Deduplication groups by `(message_id, participant_id)`;
both flags are merged with `bool_or`. Conversation membership is deliberately
absent. `occurred_year` is not a semantic field; it colocates edge shards with
the matching fact partitions so date-filtered scans can prune both datasets.

#### `identity_conversation_edges`

`identity_conversation_edges` contains one row per distinct
`(conversation_id, participant_id)` from `conversation_participants`, with the
participant's normalized domain. It includes all conversation types, not only
chat conversations. It is fully replaced on every base or derived cache
publication rather than copied onto historical message rows.

These three datasets encode the current asymmetry explicitly:

| Use case | Direct edges | Conversation edges |
|---|---:|---:|
| Participant filter, chat or non-chat | yes | yes |
| Domain filter, chat or non-chat | yes | yes |
| Non-chat people fan-out | yes | no |
| Non-chat domain fan-out | yes | yes |
| Chat people fan-out | yes | yes |
| Chat domain fan-out | yes | yes |

Filter qualification is a semi-join from each message fact to either edge
source. For participant filters, sender matching is already represented by
`identity_direct_edges.is_sender`; it remains equivalent to the current
explicit `sender_id OR participant_ids OR conversation_participant_ids`
predicate. After filtering:

- a non-chat logical entry gets people from direct edges only and domains from
  the union of direct and conversation edges;
- a chat logical entry gets both people and domains from direct edges across
  its in-filter messages unioned with its conversation edges.

The three inputs remain separately addressable during filtered evaluation; an
edge is never flattened without retaining its origin.

### Exact logical-entry reduction

Scalar context predicates and participant/domain semi-joins first select
message facts. Only then are chat messages grouped by
`(source_id, conversation_id)`. For each chat logical entry:

- `occurred_at = max(occurred_at)`;
- `anchor_message_id = arg_max(message_id,
  struct_pack(occurred_at := occurred_at, message_id := message_id))`;
- `is_from_me = arg_max(is_from_me,
  struct_pack(occurred_at := occurred_at, message_id := message_id))`.

Thus direction is the direction of the newest in-filter message, with
`message_id` as the deterministic timestamp tie-breaker. It is not `bool_or`.
The maximum filtered timestamp also controls decay, future classification,
`last_at`, and ordering.

Direct edges are first mapped to canonical IDs per message. Deduplication
groups by `(message_id, canonical_id)` and merges `is_sender` and `is_author`
with `bool_or`. Consequently, if an authored alias and a co-recipient alias
become linked on a non-chat message, the canonical identity remains the author
and is credited as the author of the incoming entry: one received unit, not
zero and not two. Chat participant membership is unioned across the selected
messages after this canonicalization; chat received credit does not consult
`is_author` today. If an author flag is retained on a chat logical unit for
validation, it comes from the canonicalized `anchor_message_id`, not `bool_or`
across every message in the conversation.

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
- per-source-type counts;
- exact nested per-`(source_id, source_type)` activity count, file count,
  first-at, and last-at rollups.

Chat messages are grouped by `(source_id, conversation_id, canonical_id)` with
the exact logical-entry reduction above. Non-chat activity remains one logical
entry per message. An entry involving multiple aliases in the same identity
cluster counts once.

The nested source rollups are a reviewed implementation amendment prompted by
the retained broad-source benchmark. A pure source-filter people request
aggregates only the selected nested rows. Combined source/date/participant/
message-type/deletion/search-candidate predicates continue through the generic
logical reduction, so the optimization cannot change cross-filter semantics.
Validation requires the nested rows to decompose the identity totals exactly.

The same build pass produces unfiltered domain rollups keyed by normalized
domain. Domain results retain exact canonical person counts.

### `relationship_rollups`

`relationship_rollups` contains one row per non-owner canonical identity with
at least one qualifying interaction:

- `anchor_date`, the UTC decay anchor;
- decayed sent, received, and meeting sums at the anchor date;
- total raw sent and meeting counts, including future-dated logical entries;
- the total modality bit mask, including future-dated logical entries;
- `last_at` as the maximum full-precision interaction timestamp, including
  future-dated logical entries.

The builder or identity-refresh child captures `anchor_date` once, at operation
start, as a UTC date. It stores the anchor both in every rollup row and in the
version-15 `_last_sync.json` marker as `relationship_anchor_date`; cache
validation requires those values to agree. The marker is authoritative for an
empty rollup. An identity-only or conversation-edge refresh intentionally
re-anchors all relationship rollups to the refresh date and publishes the new
anchor atomically with the new cache revision. `CacheSyncState.Revision()`
folds the anchor into its payload in addition to the existing identity
revision, so a changed anchor invalidates outstanding cursors even if no base
message watermark changed.

For request date `D`, each past weighted sum is advanced with one scalar
factor:

```text
delta_days = max(0, date_diff('day', anchor_date, D))
sum(D) = sum(anchor) * exp(-lambda * delta_days)
```

This is algebraically identical to decaying every contribution independently
when the contribution occurred on or before the anchor and `D >= anchor`.
Clamping `delta_days` to zero gives defined, conservative behavior if the wall
clock moves backward; the engine does not back-multiply already-clamped
contributions.

Exact per-day signals are retained in a companion `relationship_daily`
dataset. It contains one row per canonical identity and UTC event date across
the complete qualifying history:

| Column | Type | Meaning |
|---|---|---|
| `canonical_id` | `BIGINT` | Non-owner canonical identity |
| `event_date` | `DATE` | UTC logical-entry date |
| `sent_units` | `BIGINT` | Sent signal contributions on this date |
| `received_units` | `BIGINT` | Received signal contributions on this date |
| `meeting_units` | `BIGINT` | Meeting signal contributions on this date |
| `sent_count` | `BIGINT` | Raw reciprocity sent count on this date |
| `meeting_count` | `BIGINT` | Raw reciprocity meeting count on this date |
| `modality_mask` | `UTINYINT` | Modalities present on this date |
| `last_at` | `TIMESTAMP` | Maximum full-precision timestamp on this date |

At request time its weighted signals contribute:

```text
units * exp(-lambda * max(0, date_diff('day', event_date, D)))
```

The builder derives `relationship_rollups` from these flat daily rows. At
request time, the unfiltered path reads only daily rows after the stored anchor
as exceptions to anchored decay, while a pure UTC-midnight date-window request
aggregates the requested daily rows directly. Combined or non-midnight filters
retain the generic logical reduction. The total raw gate counts, modality mask,
and `last_at` remain present in `relationship_rollups`, and validation requires
the daily components to decompose those totals exactly. Day bucketing affects
only the decay exponent. It never truncates `last_at` or removes raw counts and
modalities.

The modality mask uses stable values:

- bit 0 (`1`): email or generic item;
- bit 1 (`2`): chat/conversation;
- bit 2 (`4`): meeting/event with an owner participant.

The API converts the mask to `RelationshipScore.Modalities` with a population
count over these three bits. Events without an owner participant remain
excluded from relationship signals exactly as today.

The final score continues to be calculated by the existing
`RelationshipScore` Go function. The default reciprocity gate continues to use
the total raw sent and meeting counts, including future-dated logical entries.

## Cache-build algorithm

Current cache exports are all `COPY (SELECT ... FROM sqlite_db.*)` operations;
staged Parquet is currently read only during validation. Building derived
datasets from staged Parquet is a new mechanism, not an existing builder
precedent. Base SQLite tables remain separately exported. Once those exports
are complete, one bounded DuckDB child derives the identity indexes from the
staged files rather than executing a large cross-engine join.

This mechanism applies only to SQLite-backed archives. `build-cache` currently
rejects PostgreSQL DSNs, PostgreSQL cache-staleness checks intentionally no-op,
and the PostgreSQL query engine does not implement these analytical people and
relationships interfaces. Version 15 does not add a PostgreSQL Parquet cache
or change those endpoints' present availability.

### Full rebuild

1. Export existing base datasets into the sibling staging directory.
2. Create `identity_entry_facts` with a narrow projection over staged
   messages, conversations, sources, and attachment counts.
3. Create `identity_direct_edges` by unioning message recipients and senders,
   grouping `(message_id, participant_id)`, `bool_or`-merging `is_sender` and
   `is_author`, and joining participants once for normalized domain.
4. Create `identity_conversation_edges` from all staged
   `conversation_participants` rows, independently of message or conversation
   type.
5. Create `identity_directory` from participants, participant identifiers,
   clusters, and owner participants using grouped joins and ordered
   aggregates.
6. Canonicalize direct edges per message with `bool_or(is_author)`, then apply
   the exact logical-entry reduction and edge-origin rules.
7. Produce people and domain rollups plus flat relationship-daily rows in
   grouped `COPY` operations over the narrow canonical activity stream, then
   derive compact anchored relationship rollups from the daily dataset.
8. Validate row counts, uniqueness, referential membership, the relationship
   anchor, schema, and required empty-dataset shards.
9. Compute and store a deterministic SHA-256 fingerprint over ordered
   `(conversation_id, participant_id)` pairs.
10. Include every new dataset and the anchor/fingerprint metadata in the
   staged fingerprint, then publish atomically with the version-15 commit
   marker.

`identity_entry_facts` is partitioned by `year(occurred_at)`.
`identity_direct_edges` is partitioned compatibly with its message facts.
`identity_conversation_edges` is normalized and fully replaced. The builder may
create bounded temporary relations, but must not materialize display labels,
identifier arrays, or duplicated conversation membership alongside millions
of message edges.

The expensive message/recipient join is executed once per cache publication,
projects only needed columns, and never builds list-valued participant labels
or domains. Rollups reuse a narrow temporary relation within the builder
connection rather than independently re-reading and re-canonicalizing the
facts.

### Incremental publication

For a genuinely append-only cache update:

1. Export `identity_entry_facts` and `identity_direct_edges` only for new
   message IDs, using the existing watermark and snapshot upper bound.
2. Append their partitioned shards alongside existing live shards at
   publication. The existing destination-collision check remains the
   duplicate guard.
3. Fully replace `identity_conversation_edges` from the staged
   `conversation_participants` dataset.
4. Rebuild the directory and rollup datasets from live facts/edges plus the
   staged deltas and replacement conversation edges.
5. Replace derived directories atomically; never append rollup rows.

Covered-message updates and deletions retain the current full-rebuild
behavior. Late direct-recipient changes retain the current sync-counter
contract; this design does not weaken it.

Conversation membership needs an explicit change detector because a
participant can be added to an old conversation without a new message. At
startup and after a store sync, cache staleness computes the same deterministic
SHA-256 over the ordered conversation-participant pairs and compares it with
the committed marker. A difference triggers a **derived-index refresh** even
when the message watermark did not move. That refresh:

- fully replaces `identity_conversation_edges`;
- leaves message facts and direct edges untouched;
- rebuilds all people, domain, relationship-daily, and anchored relationship
  rollups;
- re-anchors relationship decay;
- publishes atomically under a new cache revision.

The fingerprint scan is a cache-lifecycle operation, never a request-path
operation. Correctness tests specifically add and remove a conversation
participant on an old non-chat conversation with no new message and verify
that the cache becomes stale and the refreshed filter, people, and domain
results retain their respective edge-origin semantics.

### Identity-only and derived refresh processes

Linking or unlinking identities does not change message facts or raw edges.
The existing `RefreshIdentityDatasets` path must not be expanded into a
multi-million-row in-daemon aggregation. Instead, identity-only and
conversation-edge refreshes run through a hidden cache-builder child mode:

1. The daemon starts a short-lived `msgvault build-cache --derived-only`
   subprocess and waits for its status.
2. The child acquires the cache build lock itself, validates the current
   committed cache, and opens DuckDB with the bounded builder policy.
3. It stages the applicable raw edge replacement plus:
   - `participant_clusters`;
   - `owner_participants`;
   - `identity_directory`;
   - `identity_rollups`;
   - domain rollups;
   - `relationship_rollups`;
   - `relationship_daily`.
4. It reads committed scalar facts and raw edges, captures a fresh anchor,
   fingerprints the staged result, and publishes all affected datasets and
   marker state atomically. It carries the full builder's committed stats
   summary forward unchanged rather than recomputing it.
5. It exits, returning all DuckDB allocator state to the operating system.

An identity-only refresh skips the conversation-edge export. A
conversation-edge refresh replaces it and may also incorporate the current
identity revision. Base messages, recipients, scalar facts, and direct edges
are untouched by either mode. Account-identity changes that alter baked
`is_from_me` still require a full rebuild.

`--derived-only` requires a committed version-15 cache containing all scalar
facts and raw edge datasets. The child refuses version 14 or an incomplete
version-15 cache before staging any output; the daemon treats that result as a
full-rebuild requirement. Normal stale-schema detection should select the full
build first, but the child enforces the precondition independently.

The parent daemon does not hold the publication lock while waiting for the
child, because the child is the lock owner. Failure leaves the old committed
cache, identity revision, relationship anchor, and all derived datasets
untouched.

## Interactive query paths

### Default relationships

1. Read `relationship_rollups`.
2. Advance the three weighted sums from the anchor date.
3. Add `relationship_daily` contributions strictly after the anchor.
4. Join `identity_directory` for labels and member IDs.
5. Calculate scores in Go, apply the reciprocity gate, sort, and page.

This path is proportional to the number of identities, not messages. It does
not require the existing in-memory relationships memo for acceptable latency.
The memo and its leader-context `singleflight` are removed so each request
owns its cancellation context and cold behavior is the behavior under test.

### Person search

1. Scan `identity_directory`, unnest `search_values`, and retain identities
   where any value contains the normalized query.
2. With no analytical context, join candidates directly to
   `identity_rollups`.
3. With only source IDs, aggregate the selected per-source rows stored in
   `identity_rollups`.
4. With other or combined context filters, apply scalar predicates and
   origin-aware edge semi-joins to `identity_entry_facts`. Edge-filtered
   requests first materialize at most 10,000 exact fact IDs; larger candidate
   sets fall back unchanged. Form exact logical units, canonicalize, and
   aggregate only the matched population.
5. Sort and page.
6. Shape identifiers and source counts for the selected page.

An empty query may match the whole directory, but it still avoids identity
string work over activity facts.

### Domain search

Domain matching first selects the small normalized domain population.
Unfiltered results use domain rollups. Filtered results aggregate the narrow
facts after candidate selection.

### Filter semantics

Source, time, message type, and deletion predicates apply directly to scalar
fact columns.

Participant and domain groups use semi-joins through both direct and
conversation edges before logical chat grouping:

- values inside one group are ORed;
- primary and additional groups are ANDed;
- identity participant IDs are expanded through the current directory;
- a conversation edge qualifies every message in the conversation, including
  non-chat messages;
- linked aliases on one logical entry deduplicate to one canonical result with
  `bool_or(is_author)`.

Fan-out then follows the direct/conversation matrix defined above. This
preserves current `Context`, people, and domain semantics without
reconstructing participant arrays.

### Endpoint scope

Version 15 replaces the identity-summary query path for exactly:

- `POST /api/v1/relationships`;
- `POST /api/v1/people/search`;
- `GET /api/v1/people/{id}`;
- `POST /api/v1/people/{id}/summary`;
- `POST /api/v1/domains/search`;
- `GET /api/v1/domains/{domain}`;
- `POST /api/v1/domains/{domain}/summary`.

It does not migrate:

- `POST /api/v1/people/{id}/timeline`;
- `POST /api/v1/domains/{domain}/timeline`;
- `POST /api/v1/relationships/{id}/timeline`;
- global, person, and domain file searches;
- generic Explore, Everything, Files, grouping, or timeline queries.

Those endpoints retain their existing analytical views and shared
`buildExploreLogicalSQL` machinery. “No legacy fallback” in this proposal
means only that the seven migrated endpoints never fall back to their old
archive-wide people/relationship aggregation when a version-15 derived dataset
is absent or invalid. It does not remove the legacy views from the production
DuckDB connection.

## DuckDB resource policy

The long-lived daemon query engine is configured at connection creation with
the following effective settings. Go computes the integer thread value before
issuing `SET threads = <value>`:

```sql
SET memory_limit = '512MB';
SET threads = <min(GOMAXPROCS, 4)>;
SET preserve_insertion_order = false;
SET temp_directory = '<msgvault-home>/tmp/duckdb-query-<pid>';
SET max_temp_directory_size = '2GB';
```

The query semaphore is reduced from two slots to one, matching the single
DuckDB connection. Requests waiting for the connection remain
context-cancelable and cannot begin native execution after their caller has
left. The daemon creates its process-specific spill directory outside the
committed analytics directory so spill files cannot alter the cache
fingerprint. Startup removes only validated stale `duckdb-query-<pid>`
directories under that dedicated parent. “Validated stale” means the basename
strictly parses as the expected prefix plus a positive PID and the platform
process-liveness check confirms that PID is not running; malformed names and
live-PID directories are never removed. Normal shutdown removes the current
directory. A query that exhausts both its buffer and 2 GiB spill budgets fails
with DuckDB's out-of-memory error rather than consuming unbounded disk.

The cache builder uses a separate policy, again substituting the computed
integer before execution:

```sql
SET memory_limit = '1536MB';
SET threads = <min(GOMAXPROCS, 2)>;
SET preserve_insertion_order = false;
SET temp_directory = '<staging-sibling>/duckdb-tmp';
SET max_temp_directory_size = '8GB';
```

The full builder and every `--derived-only` refresh child use this same policy.
The child removes its temporary directory after success or failure. The
1,536 MB buffer-manager budget preserves enough aggregation memory for the
non-spillable domain rollup while two threads limit concurrent native
allocation pressure.

This is a reviewed implementation amendment to the initially proposed 2 GiB,
up-to-eight-thread builder policy. Production-scale trials showed that even a
2 GiB, three-thread configuration could exceed the 3 GiB process-RSS release
gate. Smaller memory pools could not complete the non-spillable domain
aggregation. The 1,536 MB, two-thread policy completed the
2.5-million-message, 6-million-edge reference build within both the 25-second
and 3 GiB gates, and subsequent retained benchmark results confirmed the same
safety margin. The outlier-prone 2 GiB, three-thread setting is therefore not
a fallback or performance-tuning option; changing this policy requires new
full-scale latency, RSS, and spill evidence plus another reviewed design
amendment.

`cacheops.CollectStats` is also an unconfigured in-daemon DuckDB open today.
Version 15 removes DuckDB from that path: the builder records the stats
summary currently computed by `CollectStats`—message/source/sender/domain
counts, year bounds, message size, and attachment size—in the committed marker.
`CollectStats` holds the cache read lock, validates readiness, and reads that
summary plus the existing sync fields. Thus the daemon has one production
DuckDB connection owner: the configured query engine.

Production-path tests instantiate the daemon engine, full builder, and derived
refresh child and query their effective thread, memory, temp-directory, and
spill-limit settings. They also verify that stats collection succeeds without
opening DuckDB.

DuckDB documents that `memory_limit` governs the buffer manager rather than
every native allocation. The design therefore does not describe it as a
process-level hard limit. Narrow SQL shapes, bounded thread counts, measured
RSS acceptance criteria, and removal of correlated archive-wide operations
provide the primary safety.

Request cancellation remains required and duckdb-go's native interrupt
behavior remains covered by `duckdb_cancel_test.go`. Resource safety does not
depend on callers canceling an otherwise valid query: every interactive query
must fit its latency and memory budget when allowed to complete.

## Failure handling

- A missing, partial, or wrong-schema identity dataset makes the whole cache
  unavailable; the seven migrated endpoints do not fall back to their unsafe
  legacy aggregation.
- A derived-build failure leaves the prior committed cache untouched.
- An identity or conversation-edge refresh failure leaves the prior revision,
  anchor, and all derived datasets untouched.
- An interactive DuckDB out-of-memory error returns the existing analytical
  error response and is logged with the endpoint and cache revision.
- Cache validation verifies that every canonical ID in a rollup exists in the
  directory, every canonical ID in `relationship_daily` has a
  `relationship_rollups` row, and owner IDs do not appear in either
  relationship dataset.
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
- authored and co-recipient aliases linked into the same canonical identity;
- non-chat conversation members that qualify participant filters but are
  excluded from people fan-out and included in domain fan-out;
- chat direction determined by the last in-filter message, including equal
  timestamp/message-ID tie-breaking;
- participant and domain intersection groups;
- source, date, message-type, and deletion filters;
- future-dated events contributing weighted signals, raw reciprocity counts,
  modalities, and full-precision `last_at`;
- a backward request clock relative to the stored anchor;
- conversation-participant changes on old conversations with no new messages;
- pagination and cache/identity revision drift;
- empty archives and identities without names.

Tests use `testify` and invoke the real cache builder and DuckDB engine. They do
not inspect SQL source text or substitute fake commands.

### Query-path guard

An integration test opens a built cache, makes the legacy message and recipient
views unavailable to the identity query connection, and verifies all seven
migrated endpoints, including filtered variants, through the production HTTP
path. Filtered queries retain access only to `identity_entry_facts`,
`identity_direct_edges`, `identity_conversation_edges`, and the small derived
datasets. Separate timeline and file-search tests keep the legacy views
available and prove those endpoints remain outside this migration. These tests
exercise behavior; they do not grep SQL source strings.

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
- cold broad source-only people-search latency;
- cold date-window relationships latency;
- peak and settled daemon RSS;
- DuckDB operator profile and rows scanned per dataset.

The harness preserves the parsed rows-scanned, rows-returned, and spill-byte
metrics for every measured operation as machine-readable profile evidence
inside the single JSON gate result. Human-formatted benchmark output remains
diagnostic only.

The 250 ms landing/unfiltered budgets and all memory/build budgets are release
gates. The filtered 1-second number is an explicit provisional target: the
existing 1.16-second measurement used the wide legacy path and full
parallelism, so it neither proves nor disproves the four-thread narrow-index
result. If the new benchmark misses one second, the implementation is not
silently accepted and the thread cap is not raised merely to improve the
number. The query/partitioning is optimized first; any change to the target or
thread cap requires a measured design amendment covering latency, peak RSS,
settled RSS, and spill.

CI continues to run smaller correctness fixtures. The production-scale test is
a release/performance gate on the reference Apple-silicon machine because
shared CI timing and memory measurements are not stable enough for sub-second
assertions.

## Rollout

1. Add the version-15 datasets and builder validation.
2. Add the new query paths behind the version-15 cache requirement.
3. Run correctness equivalence and the production-scale resource benchmark.
4. Remove only the old ranking/search/detail/summary aggregation used by the
   seven migrated endpoints after equivalence passes. Retain the shared
   Explore/timeline/file SQL and its views.
5. Ship the SQLite cache schema bump. Daemon startup reports the stale cache
   and rebuilds it through the existing cache lifecycle.

There is no compatibility fallback to the version-14 identity queries for a
SQLite DuckDB deployment. A version-14 cache is rebuilt before the
Relationships web workspace is served. PostgreSQL deployments have no Parquet
cache to rebuild and retain their current analytical endpoint availability.

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
narrow origin-aware activity model is required for exact filtered behavior.

### Run every query in a worker process

A worker process would guarantee reclamation after termination, but process
startup, connection setup, cache validation, IPC, and lifecycle management are
substantial complexity. It does not make inappropriate SQL efficient. Worker
isolation remains a possible later defense-in-depth measure if bounded queries
still reveal native allocator defects.

## Review focus

Adversarial review should concentrate on:

- whether the direct/conversation edge split preserves every current pre-chat
  filter and post-chat fan-out semantic;
- whether incremental publication can ever duplicate an activity edge;
- whether conversation-participant drift is detected without a new message;
- whether identity and conversation-edge refreshes publish all canonical
  datasets and the new anchor atomically from a bounded child process;
- whether anchored decay plus post-anchor daily exceptions is algebraically
  equivalent to current scoring, including raw counts, modalities, and
  full-precision `last_at`;
- whether chat `arg_max` direction and canonical `bool_or(is_author)` are
  preserved after every filter and identity-link case;
- whether the activity build performs only one large edge aggregation;
- whether any of the seven migrated paths can reach `analytical_entries`,
  `messages`, or `message_recipients`;
- whether the stated DuckDB settings are effective in daemon, stats, full
  builder, and derived-refresh connections;
- whether the resource benchmark measures native RSS rather than Go heap only.

## Adversarial-review disposition

The first review's findings are resolved in the specification as follows:

1. Split scalar facts, direct edges, and all-conversation edges; pin the filter
   and fan-out matrix.
2. Pin post-filter chat `max`/`arg_max` semantics and tie-breaking.
3. Canonicalize per message with `bool_or(is_author)` and credit an incoming
   authored/co-recipient cluster exactly once.
4. Give daily rows raw counts, modality masks, and full timestamps; define bit
   values, exact total decomposition, and marker-owned anchor behavior.
5. Move identity/derived refresh into a bounded subprocess and remove DuckDB
   from stats collection.
6. Describe staged-Parquet derivation as new and state the unchanged
   PostgreSQL behavior.
7. Clamp a backward request date at the stored anchor.
8. Correct the cancellation description and remove the memo's shared leader
   context.
9. Add conversation-participant fingerprint staleness and a replacement
   refresh path.
10. Enumerate the seven migrated endpoints and the timeline/file endpoints that
    retain legacy views.
11. Set daemon and builder temp directories and spill limits.
12. Mark the filtered one-second goal provisional and require a measured
    amendment rather than an unreviewed thread-cap increase.
