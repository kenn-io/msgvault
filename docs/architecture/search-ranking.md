---
title: Search Ranking Across Backends
description: Why SQLite and PostgreSQL can order the same matching messages differently.
---

`msgvault search`, the Web UI, the TUI, the HTTP API, and the MCP server can rank results
with different database engines depending on your archive backend and search
mode. Result sets usually match, but result order can differ because each
backend uses a different scoring model.

## Full-Text Ranking

### SQLite

SQLite uses FTS5 `bm25()` over the `messages_fts` virtual table. msgvault passes
column weights so subject and sender hits outrank body and recipient hits:

```sql
bm25(messages_fts, 1.0, 10.0, 1.0, 4.0, 1.0, 1.0)
```

The important weights are:

| Field | Weight |
|---|---|
| Subject | 10 |
| From address | 4 |
| Body, To, Cc | 1 |

The first `1.0` is the slot for the unindexed `message_id` column. Lower BM25
scores are more relevant.

BM25 also applies document length normalization. A query term in a short body
can score better than the same term in a long quoted thread because the long
document is penalized.

### PostgreSQL

PostgreSQL ranks the `search_fts` `tsvector` with `ts_rank()`. msgvault assigns
weights with PostgreSQL `setweight` labels:

| Field | Weight class |
|---|---|
| Subject | `A` |
| From address | `B` |
| To, Cc | `C` |
| Body | `D` |

msgvault passes rank weights of `A=1.0`, `B=0.4`, `C=0.1`, and `D=0.1`, which
matches SQLite's 10:4:1 field priority while retaining distinct PostgreSQL
weight classes for recipients and body text. Unlike BM25, `ts_rank()` without
a normalization flag does not penalize long documents.

## Where Ordering Can Diverge

The field weights make ordinary searches feel consistent. A subject-only match
usually outranks a body-only match on both backends.

The clearest divergence is a long subject-hit message versus a short body-hit
message:

- Message A has the query in the subject, but its body contains thousands of
  words of quoted history.
- Message B has the query once in a short body, but not in the subject.

PostgreSQL usually ranks Message A first because the subject field dominates.
SQLite BM25 can rank Message B first because the long body attached to Message A
reduces its BM25 score.

This is expected. PostgreSQL prioritizes email field structure more strongly.
SQLite BM25 blends field priority with document length, which is useful for web
search but can be surprising when re-finding known emails.

Use `subject:` when subject recall matters more than broad recall:

```bash
msgvault search 'subject:"quarterly review"'
```

## Query Grammar Differences

Full-text query parsing also differs:

| Path | Query grammar |
|---|---|
| SQLite FTS | FTS5 `MATCH` |
| PostgreSQL FTS | `to_tsquery` with prefix matching |
| PostgreSQL hybrid FTS signal | `to_tsquery` with prefix matching |

PostgreSQL FTS and PostgreSQL hybrid mode use the same prefix-matching query
grammar, so a prefix that matches in FTS mode should also contribute to the FTS
signal in hybrid mode. The ranking function differs: plain PostgreSQL FTS orders
by `ts_rank()`, while the hybrid PostgreSQL FTS signal uses cover-density
`ts_rank_cd(..., 32)` before combining it with vector similarity through RRF.
That can change ordering even when the matching FTS document set is the same.

## Vector Ranking

Vector search has a separate ranking caveat:

| Backend | Metric |
|---|---|
| SQLite sqlite-vec | L2 distance |
| PostgreSQL pgvector | Cosine distance |

For unit-normalized embeddings, L2 and cosine produce the same nearest-neighbor
ordering. Most modern embedding endpoints return normalized vectors, so the two
backends usually agree in practice.

For non-normalized vectors, L2 and cosine can order neighbors differently. Full
metric parity would require switching sqlite-vec tables to cosine distance and
rebuilding existing vector tables. That migration is not implemented today.

## Practical Guidance

- Expect the same matching messages for most normal queries.
- Expect occasional order differences at the top when long quoted threads are
  involved.
- Use field operators such as `subject:` and `from:` when you remember a
  specific message field.
- Use `--explain` with vector or hybrid search to inspect per-signal scores.

## Filename suggestions

Newer than the latest release, explicit filename suggestions use one bounded
Store snapshot on SQLite and PostgreSQL. They preserve ordinary filename
substring search, message search, Directory ranking and identity behavior.
The [API reference](../api-server.md#filename-suggestions) owns request and
response fields; the [CLI reference](../cli-reference.md#typo-tolerant-filenames)
owns the opt-in flag and output.

### Matching and order

Names and queries use NFC normalization, case folding, then NFC again. Tokens
start with a Unicode letter or number; attached combining marks stay in the
token. Punctuation, slashes and isolated combining marks separate tokens.
Filenames are metadata, never filesystem paths. This tokenizer is separate
from Directory's existing rules.

Every query token must match. Rank classes, in order, are exact normalized full
filename, all-exact tokens, clean token prefixes, then one typo. A typo permits
one insertion, deletion, substitution or adjacent transposition in at most one
query token of at least five runes. All-number tokens on either side match
exactly. Mixed tokens permit clean prefixes, such as `invoice2` to `invoice20`,
but an edit cannot add, remove, replace or transpose a Unicode number.
There is no accent stripping, transliteration or synonym expansion.

Within each class, the newest attachment comes first. Both exact classes have
`filename_match: exact`. Separate occurrences remain separate even when names
or content hashes agree. The candidate pool selects the best matches across
all postings read, using the same rank and recency order. It cannot guarantee
the globally best results when the posting budget leaves some ranges unread.

### Projection and readiness

Schema setup creates an empty filename projection. Each daemon maintenance
batch bootstraps at most 500 attachments or replays at most 500 journal events,
rechecking each event's old and new occurrence IDs. Bootstrap starts from a
durable maximum attachment ID and catches up to a finite journal fence. It indexes every
extant attachment within the name limits, including attachments whose messages
are currently hidden or whose content is unavailable. Visibility and every
scope filter apply at request time, so a visibility change needs no filename
rebuild.

Each pass first checks, in a read-only query, whether journal events or old
generations remain. An idle pass ends there and takes no writer slot or table
lock. PostgreSQL maintenance fences attachment and message writers before
taking its first batch snapshot. Its two-second lock timeout leaves the checkpoint intact
for a later retry. SQLite reserves the writer before reading batch state.
Projection updates, acknowledgement and pruning commit atomically. Interrupted
bootstrap resumes; a reset replaces only this consumer's generation and
garbage-collects old generations in bounded batches. Other journal consumers
keep their state. Read-only archive readers perform no DDL or maintenance.

Suggestions return a warming-index error until finite bootstrap and catchup
finish. The daemon then runs bounded maintenance on a five-second cadence.
A pass that waits behind a long sync or import does not ask that work to
yield; it runs when the work finishes.
Current filenames, message visibility, scope and provenance are rechecked in
the same snapshot as candidate generation and person resolution. Stale names
cannot appear as current matches, but new or renamed files can be omitted until
maintenance catches up. An exhausted candidate stream does not prove that the
index has caught up with every recent write.

### Work and population limits

Each request reads strong exact streams before weaker ones. Exact streams
visit newest attachments first; prefix and deletion streams visit token keys
in descending order, then newest attachments within each key. Every posting
row is checked against message visibility and the request's source, account,
date, MIME-family and person filters with primary-key and indexed relation
probes. The projected name must match every query token before competing for
the candidate pool. After reading within the posting budget, the request
selects the best 500 distinct attachment IDs by rank and recency, verifies
their current filenames and authority, and returns at most 100 rows.

Every posting row read counts toward a work budget, including duplicates,
rejected rows and page lookahead. Unscoped requests read at most 2,048 rows.
Requests with any filter read at most 8,192 rows, because filters can reject
most postings of a common word. A filtered request that exhausts its streams
within that budget is complete, including when it is empty. The response
exposes candidate exhaustion separately from readiness and freshness; an empty
limited result is incomplete.

Queries reject more than 128 normalized runes, eight tokens or 64 runes per
token. Projection skips an entire filename exceeding 512 normalized runes,
64 tokens before deduplication, or 64 runes in any token. It does not index a
truncated prefix of an oversized name. Punctuation-only names remain valid
metadata but contribute no token postings. The maintenance state tracks
skipped names; filename suggestions do not cover that excluded population.
Deletion keys retain all Unicode numbers, since a typo may only edit other
characters. All-number tokens need only exact postings.

Person resolution reads at most 65 bindings to detect a 64-participant overflow.
Provenance uses bounded indexed relation probes and rejects more than 4,096
unique edges. No request repairs person bindings, publishes contacts, scans
message bodies or backfills the projection. Directory's existing four-rune
typo threshold and numeric behavior remain unchanged; its one-edit verifier now
uses linear work instead of a distance matrix.
