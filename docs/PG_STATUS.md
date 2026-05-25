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

PR4 (future) will address remaining functional gaps in deletion execution,
attachment storage on PG, and end-to-end coverage under
`MSGVAULT_TEST_DB=postgres://...`.

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

## Remaining for PR4

- **FTS rank ordering (partially resolved)**:
  `SQLiteDialect.FTSSearchClause()` now orders by
  `bm25(messages_fts, 1.0, 10.0, 1.0, 4.0, 1.0, 1.0)` — weights are
  positional over every declared FTS5 column (the leading 1.0 is the
  slot for `message_id UNINDEXED`; the rest map to subject, body,
  from, to, cc). The 10:4:1 ratio across subject/sender/body mirrors
  PostgreSQL's `setweight 'A'=1.0 / 'B'=0.4 / 'D'=0.1`, so
  subject-only matches outrank sender-only, which outrank body-only
  on both backends. Verified by `TestFTSRankWeightsAcrossBackends`.
  Note: bm25 (Okapi BM25) and `ts_rank` (cover-density) remain
  different scorer functions, so intra-class tie-breaking (e.g. two
  body-only matches) can still diverge. Top-N ordering is consistent
  for most queries; expect occasional reorderings deep in the result
  list.
- **Deletion execution path on PostgreSQL**: end-to-end testing of
  staged-deletion → Gmail delete → archive update.
- **Attachment storage paths** under PostgreSQL — content-hash dedup
  and orphan-cleanup paths haven't been exercised end-to-end yet.
- **Vector / hybrid search**: SQLite-only by construction —
  `internal/vector/sqlitevec` uses the sqlite-vec extension and
  `ATTACH DATABASE` to fuse `vectors.db` onto the main store, and the
  embed worker / fused search dispatch `?` placeholders straight to
  the main DB handle. `setupVectorFeatures` now refuses a PG DSN
  with a clear error and `[vector] enabled = false` is required to
  run msgvault against PostgreSQL. PG support (likely pgvector with
  an analogous fused-search wrapper) is deferred to PR4.

## Running Tests Against PostgreSQL

```bash
# Start a PostgreSQL instance, then:
export MSGVAULT_TEST_DB=postgres://user:pass@localhost:5432/msgvault_test
make test-pg
```

Each test creates and drops its own schema (`msgvault_test_<hex>`) for
isolation. The `testutil.NewTestStore()` helper detects the env var and
routes accordingly. If `MSGVAULT_TEST_DB` is unset, SQLite is used.
