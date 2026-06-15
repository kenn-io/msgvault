# PostgreSQL Backend Status

This document tracks the state of PostgreSQL backend support in msgvault.

## Summary

PR1 (`pr1-dialect-extraction`) extracted SQLite-specific behavior behind a
`Dialect` interface (zero functional change).

PR2 (`pr2-postgresql-dialect`) added the foundational scaffolding:
`PostgreSQLDialect`, pgx driver wiring, `schema_pg.sql` stub,
`PostgreSQLEngine` scaffold, and the dual-backend test harness via
`MSGVAULT_TEST_DB`.

**PR3 (this branch) makes the store layer functional against PostgreSQL.**
A PostgreSQL connection can now initialize the schema, insert rows, run FTS
queries, and serve the TUI / HTTP / MCP aggregate paths. The SQLite path is
unchanged.

PR4 (in progress on `pr4-upstream`) addresses remaining functional gaps in
deletion execution, attachment storage on PG, and end-to-end coverage under
`MSGVAULT_TEST_DB=postgres://...`. The first PR4 item to land is the
pgvector backend (`pr4a-vector`); see "Resolved in PR4" below.

## What Works

- `PostgreSQLDialect.Rebind()` correctly converts `?` → `$1, $2, ...`
  (including quoted-string safety)
- `PostgreSQLDialect.Now()`, `InsertOrIgnore()` (complete + prefix),
  `InsertOrIgnoreSuffix()`, `FTSSearchClause()`
- `PostgreSQLDialect.LegacyColumnMigrations()` returns the same logical list
  as SQLite, translated to PG types (`JSONB`, `TIMESTAMPTZ`, `BIGINT`) and
  using `ADD COLUMN IF NOT EXISTS` for idempotency. Existing PG databases
  pick up newly added columns on the next `InitSchema()` call
- `PostgreSQLDialect.DatabaseSize()` reports `pg_database_size(...)`
- `PostgreSQLDialect` error-code classification (23505, 42701, 42P01)
- `Open("postgres://...")` establishes a connection with pool settings
- `OpenReadOnly` for PostgreSQL enforces `default_transaction_read_only=on`
  via pgx `RuntimeParams` (set on every pooled connection at startup)
- `schema_pg.sql` is loaded by the dialect and contains PostgreSQL-native
  DDL: `BIGINT GENERATED ALWAYS AS IDENTITY`, `TIMESTAMPTZ`, `BYTEA`,
  `JSONB`, tsvector column + GIN index for FTS
- `Rebind()` is threaded through every store-layer query via the
  `loggedDB` / `loggedTx` wrapper — call sites can emit portable `?`
  placeholders and the wrapper applies the dialect-specific rewrite
- `RETURNING id` replaces `LastInsertId()` at every insert call site
  (`messages.go`, `sync.go`)
- `queryInChunks` / `insertInChunks` use `loggedTx` (auto-rebind); chunked
  `INSERT OR IGNORE` builders use `dialect.InsertOrIgnorePrefix/Suffix`
- `SearchMessages` / `SearchMessagesQuery` use uniform `?` placeholders
  through `FTSSearchClause()`, then the whole statement is rebound by
  `loggedDB` — no mixed `?` / `$N` styles
- `FTSBackfillBatchSQL` uses `LEFT JOIN message_bodies` so messages
  without a body row are still indexed (header-only FTS for that row)
- `GetStats` uses `dialect.DatabaseSize()` instead of `os.Stat` on the DSN
- `PostgreSQLEngine` (now a dialect-parameterized `SQLiteEngine`)
  implements the full `Engine` surface for aggregates, search, and
  message detail using the query-layer `Dialect` interface
- `query.NewEngine(db, isPostgres)` factory is wired in every engine
  construction site under `cmd/msgvault/cmd/`
- `Store.IsPostgreSQL()` lets callers dispatch without an
  `internal/query` dependency
- Unit tests for dialect string methods pass without a live Postgres
- SQLite regression: all existing tests pass unmodified

## Resolved in PR3

| # | Blocker | Resolution |
|---|---------|-----------|
| 1 | Schema type translation | `schema_pg.sql` with PostgreSQL-native DDL |
| 2 | Rebind threading through store layer | `loggedDB` / `loggedTx` apply `Rebind` to every statement |
| 3 | `queryInChunks` / `insertInChunks` dialect-aware | Use `loggedTx` (auto-rebind) + `InsertOrIgnorePrefix/Suffix` |
| 4 | `LastInsertId` → `RETURNING id` | Done at every insert call site |
| 5 | Mixed placeholder styles in search | All placeholders are `?`, rebound at execution |
| 6 | FTS backfill LEFT JOIN | `FTSBackfillBatchSQL` uses LEFT JOIN |
| 7 | `statement_timeout` pool-wide | Set via pgx `RuntimeParams` (PR2) |
| 8 | `GetStats` for PostgreSQL | `dialect.DatabaseSize()` |
| 9 | `PostgreSQLEngine` method implementations | Dialect-parameterized `SQLiteEngine` |
| 10 | `PostgreSQLEngine` wired to factory | `query.NewEngine(db, isPostgres)` in cmd/ |
| 11 | Legacy column migrations on PG | `LegacyColumnMigrations()` returns the SQLite list translated to PG types, using `ADD COLUMN IF NOT EXISTS` for idempotency |

## Codex Review Fixes (Late PR3)

The codex multi-level review of `pr3-upstream` flagged four
release-blocking concurrency / search-parity issues plus follow-up
maintainability work. All blocking findings are now addressed in this
branch:

- **H1** — `UpsertAttachment` now backed by a partial unique index on
  `(message_id, content_hash)` and uses `INSERT … ON CONFLICT DO
  NOTHING`. Legacy duplicates are deduped on `InitSchema`.
- **H2** — `AddAccountIdentity` runs inside a writer-locked
  transaction (SQLite `BEGIN IMMEDIATE`; PostgreSQL `SELECT … FOR
  UPDATE`) so concurrent merges no longer drop signals.
- **H3** — `query.Engine`'s `subject:` and metadata fallback predicates
  are `LOWER(col) LIKE LOWER(?)` with proper escape, matching the
  store-layer search.
- **H4** — `.github/workflows/ci.yml` runs a `test-postgres` job
  against a live `postgres:16` service.
- **M1** — `EnsureConversation` / `EnsureConversationWithType` /
  `GetOrCreateSource` collapse into a single
  `INSERT … ON CONFLICT DO UPDATE RETURNING` statement; `StartSync`
  runs in a writer-locked transaction with a `sources` row lock on PG.
- **M2** — `FTSNeedsBackfill` counts `search_fts IS NULL` rows
  directly so missing intermediates surface; `FTSRebuildSchema` is
  implemented for PG (DROP index → clear column → re-CREATE index).
- **M3** — Shared `?`-rebind and tsquery-escape primitives live in
  `internal/sqldialect`; both store and query dialects delegate.

## Resolved in PR4 (so far)

| # | Item | Resolution |
|---|------|-----------|
| 1 | Vector backend on PostgreSQL | `internal/vector/pgvector/` implements `vector.Backend` against pgvector. Selection at runtime in `serve_vector.go` and `embed_vector.go` via `Store.IsPostgreSQL()` / DSN prefix. Build with `-tags "fts5 sqlite_vec pgvector"` to enable. |
| 2 | FTS weight parity (SQLite ↔ PG) | `SQLiteDialect.FTSSearchClause()` now orders by `bm25(messages_fts, 1.0, 10.0, 1.0, 4.0, 1.0, 1.0)` — weights are positional over every declared FTS5 column (the leading 1.0 is the slot for `message_id UNINDEXED`; the rest map to subject, body, from, to, cc). The 10:4:1 ratio across subject/sender/body mirrors PostgreSQL's `setweight 'A'=1.0 / 'B'=0.4 / 'D'=0.1`, so subject-only matches outrank sender-only, which outrank body-only on both backends. Verified by `TestFTSRankWeightsAcrossBackends` (runs on both SQLite and PG via `MSGVAULT_TEST_DB`). |
| 3 | Deletion execution path on PostgreSQL | `internal/deletion/executor_e2e_test.go` exercises the full staged-deletion → mock Gmail → store pipeline on a multi-source, multi-attachment corpus. Covered: trash-mode soft delete (`deleted_from_source_at` set, source isolation), permanent-mode row deletion with `ON DELETE CASCADE` of attachment rows, batch-mode cross-source `IN (...)` UPDATEs (`MarkMessagesDeletedByGmailIDBatch`), and post-delete `AttachmentPathsUniqueToSource` consistency. Runs unchanged on both backends via `MSGVAULT_TEST_DB`. |
| 4 | Attachment storage paths on PostgreSQL | `internal/store/attachment_e2e_test.go` exercises the multi-message / multi-source attachment lifecycle: intra-source dedup (idempotent `UpsertAttachment`), `ON DELETE CASCADE` from `messages` to `attachments`, cross-source `AttachmentPathsUniqueToSource` promotion when one source is removed, the full orphan-cleanup pipeline (`AttachmentPathsUniqueToSource` → `RemoveSourceSerialized` → `IsAttachmentPathReferenced`), and exclusion of NULL-hash / empty-path rows. The query helpers route through `Store.Rebind` so `?` placeholders are translated for PG. |
| 5 | embed.Queue / pipeline portability | `internal/vector/embed/queue.go` is portable: it uses `?` placeholders run through `rebind`, a chunked multi-row `VALUES` enqueue (`enqueue.go`; the old SQLite-only `json_each` path is gone), and `FOR UPDATE SKIP LOCKED` for safe concurrent claims on PG. The full worker loop (claim/complete) runs on PostgreSQL; covered by `internal/vector/embed/queue_pg_test.go` and `worker_pg_test.go` against a live DSN. |
| 6 | FusedSearch on pgvector | `pgvector.Backend` implements `vector.FusingBackend` (compile-time assertion at `internal/vector/pgvector/fused.go:20`) via a single-query hybrid CTE combining `ts_rank_cd` and the `<=>` cosine operator. `hybrid.NewEngine` selects it automatically, so PostgreSQL takes the native fused path — identical in *query shape* to sqlitevec. Note this is structural parity only: the vector *distance metric* still differs across backends — pgvector ranks by cosine (`<=>` / `vector_cosine_ops`) while the sqlitevec `vec0` tables use sqlite-vec's default L2 (Euclidean). For unit-normalized embeddings the two yield equivalent rankings, but not for non-normalized vectors; switching sqlitevec to cosine (plus a one-time vector-table rebuild migration) is **deferred** to a follow-up PR. See `docs/search-ranking.md` ("Vector ranking: SQLite L2 vs PostgreSQL cosine"). |

The pgvector backend covers `CreateGeneration`, `ActivateGeneration`,
`RetireGeneration`, `Active/BuildingGeneration`, `Upsert`, `Search`,
`Delete`, `Stats`, `EnsureSeeded`, `LoadVector`, and `Close`. Embeddings
live in the same Postgres database as messages (no separate `vectors.db`).
The per-dimension HNSW cosine index is created lazily by
`pgvector.EnsureVectorIndex(db, dim)` with a partial `WHERE dimension = N`
guard so generations of different dimensions can coexist in the same
`embeddings` table.

### Retiring a generation deletes its embedding rows (pgvector only)

Because the HNSW index is partial by **dimension only**, a single graph
indexes *every* generation of that dimension, and `Search`/`FusedSearch`
apply `generation_id` as a **post-filter**. If a retired generation's
vectors stayed in the table they would remain in the shared graph and
consume the `ef_search` candidate budget, eroding the active generation's
recall (the inner ANN scan can fill its budget with retired-generation
rows that the post-filter then discards, short-returning the active set).

To keep the shared graph generation-clean, retiring a pgvector generation
**deletes its embedding rows** — in both paths that retire a generation:
`RetireGeneration` (explicit) and `ActivateGeneration` (which auto-retires
the previously-active generation during the normal re-embed flow). The
`index_generations` row is preserved (`state = 'retired'`) so lifecycle and
history queries still see it; only the `embeddings` rows are removed, inside
the same transaction as the state flip.

This intentionally **differs from sqlitevec**, which retains a retired
generation's rows because its `vec0` virtual table uses a `PARTITION KEY`
on `generation_id`, isolating each generation in its own ANN partition so
retired rows never contaminate the active generation's search. pgvector has
no equivalent per-partition ANN index, so deletion is the mechanism that
achieves the same isolation. Covered by
`internal/vector/pgvector/backend_retire_test.go`
(`TestBackend_RetireGeneration_DeletesEmbeddings`,
`TestBackend_ActivateGeneration_AutoRetireDeletesPrevious`,
`TestBackend_DeleteOnRetire_KeepsActiveRecallClean`).

## Remaining for PR4

Nothing outstanding for the vector pipeline: the pgvector backend, the
portable embed.Queue / worker loop, and native FusedSearch are all
implemented and covered by live-PG tests (see items 5–6 above). Note there
is no "two-query fallback" path anywhere in the engine — `hybrid.NewEngine`
requires a `vector.FusingBackend` and returns an error if the backend does
not implement it (`internal/vector/hybrid/engine.go`); both concrete
backends do, so the native fused path always runs.

## Known Trade-offs at Scale

The PostgreSQL path is functionally complete for the happy path and the
concurrency-sensitive paths, and is exercised end-to-end against live PG in
CI. The remaining risks are **operational-at-scale**, not logical. This
section documents the trade-offs a code review flagged as real but either
out-of-scope to fix now (schema/perf changes) or operational guidance.

Two related hazards are addressed in code and are intentionally *not*
re-documented here: the pool-wide `statement_timeout` maintenance escape
hatch (S1) and the live-PG test-coverage tag note — see the corresponding
code and test changes for those.

### A2 — Gmail-ID deletion match unscoped by `source_id`

The deletion write path (`MarkMessageDeletedByGmailID` /
`MarkMessagesDeletedByGmailIDBatch` in `internal/store/messages.go`, plus the
read-side `GetMessageBySourceID`) matches on `source_message_id` **without a
`source_id` scope**, so a Gmail-ID collision across two accounts would
soft-delete/permanent-delete the wrong account's row (blast radius: one row).
Scoping is **deferred**, not fixed: the deletion `Manifest`
(`internal/deletion/manifest.go`) carries only a flat `GmailIDs []string` with
no per-id `source_id`, and a single manifest can legitimately span multiple
accounts (the account filter is optional in both `internal/tui/actions.go`
`resolveGmailIDs` and `internal/mcp/handlers.go`), so a single
`Filters.Account` cannot scope every id correctly. A correct fix needs a
manifest schema/version change, which is out of scope. Gmail message IDs are
random enough that a cross-account collision is astronomically unlikely. This
behaves identically on SQLite and PG.

### A3 — No Parquet acceleration on PG

On SQLite, the TUI and aggregate analytics run over denormalized DuckDB /
Parquet files (~3000× faster than SQLite JOINs, per `CLAUDE.md`). The PG
path has **no counterpart to that acceleration layer**: the TUI / aggregate
surface runs live relational SQL through the dialect-parameterized query
engine. At the stated target (20+ years, 1M+ messages), drill-down
aggregates such as `buildAggregateSQL` (which `LEFT JOIN`s an
`attachments GROUP BY message_id` derived table) execute live on every
navigation. These live plans have **not yet been validated with
`EXPLAIN ANALYZE` on a realistic corpus**, and there is no
materialized-view or caching story on PG yet. PG's planner is far stronger
than SQLite's, but expect live aggregation latency rather than
Parquet-instant results until this is measured and (if needed) cached.

### A4 — `TextEngine` feature gap on PG

Features exposed only through `query.TextEngine` — the conversation / text
views built on FTS5 `MATCH` and `strftime` — are **SQLite-only**. On PG
these surfaces are intentionally unavailable: the type assertion to
`TextEngine` fails cleanly rather than emitting SQLite-specific SQL against
PostgreSQL. UI surfaces that depend on `TextEngine` therefore lose those
features on the PG backend by design.

### A5 — `CopySubset` is SQLite-only

Subset export (`internal/store/subset.go` / `CopySubset`) always targets a
SQLite destination and is **not part of the PG path**; callers gate it off
PostgreSQL. This ties into the missing SQLite→PG migration tool — see S6
below.

### P1 — Inline `tsvector` write amplification

`search_fts` lives **inline on the `messages` table** (GIN-indexed) rather
than in a separate FTS table the way SQLite uses `messages_fts`. As a
result, each FTS upsert is a non-HOT update of a GIN-indexed column, and
`FTSDelete` / `FTSClear` rewrite full message rows. The effect is write
amplification and MVCC bloat during bulk sync, relative to SQLite's
separate-table design. A future schema change (moving FTS to a separate
table) would mitigate this, but it is **deferred** — schema/migration
changes are out of scope for the current work.

### V4 — Retire churn and vacuum expectations

To keep the shared per-dimension HNSW graph clean, retiring a pgvector
generation **deletes that generation's embedding rows** (see "Retiring a
generation deletes its embedding rows" above). Each rebuild cycle therefore
churns roughly corpus-size rows (delete + re-insert). Until autovacuum
catches up, dead tuples linger in the heap and in the HNSW graph and degrade
scan performance. **Tune autovacuum on the `embeddings` table** (or run a
manual `VACUUM` after a retire) for archives where rebuilds are frequent.

### V6 — Three FTS query grammars

Full-text query parsing differs across mode and backend, so the *same query
string can match different documents*:

- **Non-fused PG search** uses `to_tsquery` with `:*` prefix matching.
- **Fused (hybrid) PG search** uses `websearch_to_tsquery`, which has **no**
  prefix matching.
- **SQLite** uses FTS5 `MATCH`.

The most visible consequence: prefix matching that works in non-fused PG
search is absent in fused/hybrid PG search.

### S6 — `IDENTITY` columns and a future migrator

PG tables use `GENERATED ALWAYS AS IDENTITY` for primary keys. Any future
SQLite→PG migrator that needs to **preserve existing message ids** must
insert with `INSERT ... OVERRIDING SYSTEM VALUE` and then `setval()` the
identity sequences afterward so server-generated ids continue from the right
point. (FTS and embeddings can be rebuilt rather than copied.)

### T1 — No scale / plan validation yet

All live-PG tests run against **small corpora**. Small-corpus tests cannot
catch the at-scale risks above — the `statement_timeout` maintenance hazard
(S1), live-aggregate latency (A3), and the fused-query plan shape (V1 / V2).
Before trusting PG as the primary backend, run a **one-off seeded 1M-row
benchmark** and capture `EXPLAIN ANALYZE` for both fused hybrid search and
the TUI aggregates. **Record the results here when done.**

### T4 — TUI is not driven against PG directly

The TUI is exercised against PG only at the **query-engine layer**
(`pg_compat_test.go`). The Bubble Tea layer is engine-agnostic and is not
driven against PostgreSQL directly. This is acceptable given the TUI's
engine independence, but note that "TUI on PG" coverage is engine-level, not
UI-level.

### Security / deployment

- **SEC1 — DSN / password handling.** The PostgreSQL DSN, including the
  password, lives in `~/.msgvault/config.toml` and is held in memory as
  `Store.dbPath`. Recommendations:
  - `chmod 600 ~/.msgvault/config.toml`.
  - To keep the password out of the file entirely, use pgx's native
    `PGPASSWORD` environment variable or a `~/.pgpass` entry combined with a
    password-less DSN.
  - Set `sslmode` explicitly for a LAN deployment (`require` or
    `verify-full`). CI uses `sslmode=disable`, which is fine for CI but
    should **not** be cargo-culted into a real deployment.
- **SEC2 — Read-only is a session default, not a role.** Read-only
  enforcement is `default_transaction_read_only=on`, applied per pooled
  connection as a **session default** — in-session SQL could flip it back.
  This is sufficient for the single-user trust model. However, if `serve` /
  MCP is ever exposed beyond localhost, provision a **dedicated read-only
  PostgreSQL role** as the real security boundary instead of relying on the
  session default.

## Running Tests Against PostgreSQL

```bash
# Start a PostgreSQL instance, then:
export MSGVAULT_TEST_DB=postgres://user:pass@localhost:5432/msgvault_test
make test-pg
```

Each test creates and drops its own schema (`msgvault_test_<hex>`) for
isolation. The `testutil.NewTestStore()` helper detects the env var and
routes accordingly. If `MSGVAULT_TEST_DB` is unset, SQLite is used.
