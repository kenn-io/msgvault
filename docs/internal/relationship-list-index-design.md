# Relationship list index

## Problem

The Relationships workspace currently derives people, domains, and
relationship scores from wide message rows and participant joins at request
time. On an archive with roughly 2.5 million messages, the default page can
take about a minute and a filtered person search can consume tens of gigabytes
of memory.

DuckDB is executing these requests, but the query shape is the problem. It
repeatedly expands message and conversation participants, canonicalizes
identities, groups chats, and computes labels and scores while serving an HTTP
request. More data makes the intermediate joins grow much faster than the
small result set.

The fix is a narrow Parquet index for the three sidebar list endpoints:

- `POST /api/v1/relationships`
- `POST /api/v1/participants/search`
- `POST /api/v1/domains/search`

Person and domain details, summaries, timelines, and file views keep their
existing query paths. This change is not a general replacement for the Explore
query engine.

## Goals

- Make the default Relationships page and people/domain searches feel
  immediate on a 2.5-million-message archive.
- Keep every list filter exact, including source, date, message type,
  participant, domain, and deletion filters.
- Preserve the current chat grouping, participant, domain, authorship, owner,
  scoring, sorting, and pagination behavior.
- Bound DuckDB memory, CPU, concurrency, and spill space in the daemon and
  cache builder.
- Append new-message activity incrementally.
- Keep the implementation limited to code that directly supports these three
  endpoints and their cache lifecycle.

Approximate scores, stale-schema fallbacks to the expensive query, and a
PostgreSQL Parquet cache are not goals.

## Measured activity grain

The activity index bakes conversation membership onto each message so filtered
queries do not join a separate conversation-edge table. This was measured
against the reference archive before choosing the grain:

| Measure | Rows |
| --- | ---: |
| Messages | 2,514,676 |
| Conversations | 834,079 |
| Distinct direct message/participant edges | 5,916,177 |
| Conversation-participant pairs | 662 |
| Expanded conversation-member message rows | 819,722 |
| Final `(message, canonical identity, domain)` rows | 6,703,190 |

Conversation membership therefore adds 787,013 net rows, or 13.3% over the
direct-edge population. The largest single conversation contributes 627,675
expanded rows, but total fan-out is well below the 2× cutoff that would justify
retaining a normalized conversation-edge dataset.

That ratio is evidence for this archive, not a universal bound. Every build
logs direct rows, expanded conversation-member rows, final activity rows, and
the final/direct ratio, and warns above 2×. The implementation does not switch
schemas per archive. If real chat-heavy archives miss the build-time or storage
gates because of fan-out, the follow-up design moves conversation membership
back to a normalized edge dataset.

## Datasets

The relationship index has four datasets.

### `relationship_activity`

This is the only large dataset. Its ordinary grain is one row per
`(message_id, canonical_id, participant_domain)`. Duplicate aliases at that
grain merge every Boolean flag with `bool_or`. A message with no valid direct
or conversation-member edge receives one nullable-identity sentinel row.
That row retains the message facts needed for filter-before-chat-grouping and
is excluded from identity and domain fan-out.

Message columns:

- `message_id`
- `conversation_id`
- `source_id`
- `source_type`
- `occurred_at`
- `message_type`
- `conversation_type`
- `entry_kind`
- `is_chat`
- `is_from_me`
- `attachment_count`
- deletion state

Identity and edge columns:

- `canonical_id`
- `participant_domain`
- `is_direct`
- `is_conversation_member`
- `is_sender`
- `is_author`
- `is_owner`

The dataset is partitioned by occurrence year. An empty archive still publishes
a schema-correct `year=0/empty.parquet` shard so the production glob remains
readable.

Owner rows remain in this dataset. They are excluded from returned people but
are required to decide whether a meeting includes the owner.

The query layer excludes nullable-identity sentinels, then performs a second
deduplication to `(message_id, canonical_id)` when computing people. It again
uses `bool_or` for edge flags. If an authored alias and a co-recipient alias
have been linked, the merged canonical identity is the author and receives
exactly one incoming unit, not zero and not two.

### `relationship_people`

One row per canonical identity with:

- display label, member IDs, normalized search values, and owner state;
- unfiltered activity/file totals and first/last timestamps;
- compact per-source and per-source-type rollups.

This dataset serves unfiltered text search, default people ordering, and
source-only requests that can be answered exactly from the compact rollups.

### `relationship_domains`

One row per domain with searchable domain text, unfiltered activity/person/file
totals, first/last timestamps, and compact source counts. It serves the
unfiltered domain list and text candidate selection.

### `relationship_daily`

One row per `(canonical_id, UTC event date)`, derived from the exact unfiltered
logical entries. It stores sent, received, and meeting units; modality bits;
and the full-precision last timestamp. The units are undecayed per-day counts,
so the raw reciprocity-gate totals are their plain sums.

This dataset has one consumer: the unfiltered default relationship ranking.
At request time DuckDB applies exact day-difference decay, clamping negative
day differences to zero, then aggregates and scores the identities.

Date-filtered ranking does **not** use this dataset. Chat conversations are
grouped after filters, so a date window can change a conversation's newest
message, direction, and decay date. Every filtered relationship request,
including a date-only request, reads `relationship_activity`.

All three compact datasets are derived from a logical reduction that sees
every base-cache message, including participantless chat anchors, and uses
`DeletionAny`. The compact fast paths run only when the request also uses
`DeletionAny`; explicit `active` and `deleted` requests route to
`relationship_activity`. An equivalence fixture compares compact and activity
results for `DeletionAny`, then checks the activity path against legacy results
for `active` and `deleted`.

## Exact query semantics

Filters select messages before logical entries are formed. Participant filters
match direct participants and conversation members for chat and non-chat
messages.

Resolved lexical, semantic, and hybrid message-search candidates are applied
at this same message-selection stage. Their provenance and candidate snapshot
remain cursor-pinned by the existing request contract.

After filtering:

- A non-chat message is one logical entry.
- A chat conversation is one logical entry.
- A chat entry's timestamp is the newest filtered message timestamp.
- Its direction comes from the newest filtered message, with `message_id` as
  the deterministic tie-breaker.
- Non-chat people fan-out uses direct participants only.
- Chat people fan-out uses direct participants plus conversation members.
- Domain fan-out uses direct and conversation-member domains for both chat and
  non-chat entries.
- Multiple aliases of one canonical identity receive one contribution.
- A non-chat incoming email/item credits only the canonical author.
- An incoming chat credits each qualifying non-owner canonical participant;
  it does not consult `is_author`.
- Meeting/event credit requires an owner among the logical entry's people.

Multiple values inside one filter group use OR; different filter groups use
AND. Sorting keeps the existing canonical-ID tie-breaker so cursor pagination
has a total order.

## Request paths

`POST /relationships` uses `relationship_daily` only when no filter is active.
Any filter routes to `relationship_activity`.

`POST /participants/search` uses `relationship_people` for unfiltered and exact
source-rollup requests. Text search selects candidate canonical IDs from
`relationship_people` before a filtered activity scan.

`POST /domains/search` uses `relationship_domains` for unfiltered requests.
Text search selects candidate domains there before a filtered activity scan.

The filtered activity query first identifies qualifying message IDs, then
forms chat/non-chat logical entries and applies the people or domain fan-out
rules above. Resolved message-search candidate IDs are part of that qualifying
message predicate; identity text candidates are an additional, separate
restriction on returned identities. The aggregation does not join raw
participant, identity-link, or conversation-participant datasets during
aggregation. After limiting the page to at most 500 rows, response shaping may
read participants and participant identifiers to return the existing
identifier fields.

If the relationship index is missing, stale, or invalid, these three endpoints
return a cache-unavailable response. They never fall back to the query that
caused the resource incident. Other endpoints retain their existing paths.

## Build and refresh lifecycle

A full relationship-index build reads committed base Parquet for messages,
recipients, conversations, participants, and sources. It exports only the
small identity/owner/conversation-membership inputs needed to reflect current
SQLite state, then builds all four relationship datasets in a bounded child
process. It does not re-export the base message cache.

Ordinary new-message cache updates:

1. append base message data up to a fixed source snapshot;
2. append `relationship_activity` rows for that same message-ID interval;
3. rebuild the three compact datasets from committed plus staged activity;
4. publish the base and relationship generations together.

If new messages arrive in the same staleness window as participant-link or
conversation-membership drift, the build does not append under mixed
relationship dimensions. It performs a full base rebuild and rebuilds the
relationship index with one coherent snapshot.

The watermark, snapshot upper bound, and destination-collision checks remain.

Participant link/unlink and conversation-membership changes cannot patch baked
canonical or membership rows. They trigger a full relationship-index build
from committed base Parquet. The conversation-participant SHA fingerprint is
retained so a membership change on an old conversation is detected even when
no message was added.

Account-identity add/remove changes and participant merges are different:
`is_from_me` is baked into base message Parquet during export, and a merge may
also repoint `messages.sender_id`. Account-identity revision drift therefore
keeps the existing full base-cache rebuild. The relationship index is rebuilt
as part of that publication. Covered-message updates, source deletions,
internal deletions, and other existing non-append staleness signals likewise
force a full base rebuild carrying a new relationship index.

Identity link/unlink requests keep the current synchronous cache-state
contract: the SQLite mutation commits first, then the request waits for the
child index build. Other clients continue reading the previous committed
generation while the child runs. Refreshes are serialized; after acquiring
the refresh lock, a waiter skips its build if the published participant-link
and conversation-participant revisions already cover its mutation. A failed
child leaves the mutation committed and reports the cache as stale.

The existing version-15 benchmark built the more complicated full cache in
13.54 seconds on the 2.5-million-message synthetic archive. That run already
used the same two-thread builder policy specified below, with a `1536MB`
memory cap. The final implementation raises that cap to `2GB` because the
reference archive exhausted `1536MB` while materializing relationship activity.
It is a full-cache baseline, not an index-only measurement. The simplified index-only
build must remain below the 25-second release gate, and its measured time must
be recorded before this change is approved for merge.

## Publication and recovery

The four relationship datasets are staged and validated as one immutable
generation. The cache marker is atomically replaced last and identifies the
generation and its source/identity/conversation-participant revisions.

There is no durable publication journal and no in-daemon repair workflow. An
interruption before marker replacement leaves readers on the old generation.
An incomplete or mismatched generation fails readiness checks and requires a
new cache build. Stale staging cleanup may remove a build directory only after
confirming that its recorded process is no longer alive.

## Resource policy

The long-lived daemon owns one configured DuckDB connection:

- memory limit: `512MB`;
- threads: at most 4;
- one concurrent heavy analytical query;
- a private temp directory under the msgvault home;
- spill limit: `2GB`.

Cache and relationship-index builds run in a child process:

- memory limit: `2GB`;
- threads: at most 2;
- private staging spill directory;
- spill limit: `8GB`.

All DuckDB work, including statistics collection, uses these configured
connections. Native builder allocations disappear when the child exits.

## Verification and release gates

Focused equivalence tests compare the new and legacy results for the three
migrated endpoints. Fixtures cover:

- direct versus conversation-only edges for chat and non-chat entries;
- filtering before chat grouping and newest-message direction;
- linked author/co-recipient aliases;
- owner presence and meeting credit;
- participant/domain filter-group logic;
- deletion filtering;
- deterministic pagination;
- compact/activity equivalence for `DeletionAny`, `active`, and `deleted`;
- incremental append and every index-only/full-base rebuild trigger.

The scale harness uses 2.5 million messages, 75,000 participants, six million
direct edges, and a group-chat-heavy profile with at least five million
conversation-member expansions. Every third conversation is a chat whose
messages expand across three members; the final fixture contains 5.83 million
conversation-member rows and 11.2 million activity rows. It measures the real
binary, builder, daemon, and HTTP routes and reports the same fan-out counters
as the production builder.

Release gates:

- cold unfiltered relationships: at most 250 ms;
- cold unfiltered people search: at most 250 ms;
- cold unfiltered domain search: at most 250 ms;
- each essential filtered mode: at most 2.5 seconds on the chat-heavy stress
  archive and at most 1 second on the reference archive;
- settled daemon RSS growth: at most 1 GiB on the stress archive;
- peak interactive RSS growth: at most 1.5 GiB;
- relationship-index build: at most 45 seconds and 4 GiB peak RSS on the
  stress archive, and at most 25 seconds and 3 GiB on the reference archive.

The cold relationship measurement explicitly exercises request-time decay over
`relationship_daily`. If that path misses 250 ms, the only additional dataset
allowed without another design review is a compact unfiltered per-identity
score rollup. It must be justified by the measured failure.

The benchmark retains a compact machine-readable summary. Full DuckDB operator
trees, benchmark dumps, generalized publication machinery, derived-only
partial refreshes, and migrations of unrelated endpoints are outside this
change.

As a review budget, production additions should stay below roughly 3,000 lines
and tests/harness code below roughly 2,500 lines. Any excess must map to a named
endpoint, exact semantic rule, cache-safety requirement, or release gate above.
