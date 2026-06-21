---
title: Search Ranking Across Backends
description: Why SQLite and PostgreSQL can order the same matching messages differently.
---

`msgvault search`, the TUI, the HTTP API, and the MCP server can rank results
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
| Body, To, Cc | `D` |

PostgreSQL's default weights are roughly `A=1.0`, `B=0.4`, and `D=0.1`, which
matches SQLite's 10:4:1 field priority. Unlike BM25, default `ts_rank()` does
not penalize long documents.

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
