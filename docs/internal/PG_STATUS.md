# PostgreSQL Backend Implementation Status

This is a repository-local engineering tracker for PostgreSQL backend work. It
is intentionally not user documentation. Public technical docs belong in the
docs website:

- PostgreSQL setup and operations: `msgvault-docs/src/content/docs/architecture/postgresql.mdx`
- Backend ranking behavior: `msgvault-docs/src/content/docs/architecture/search-ranking.mdx`
- TUI keybindings: `msgvault-docs/src/content/docs/usage/tui.mdx`

## Current State

The PostgreSQL path is functionally implemented for the core archive workflow:

- Schema initialization against PostgreSQL with native types and legacy column
  migrations.
- Store-layer CRUD, sync state, FTS, attachment metadata, deletion metadata, and
  source removal paths.
- Dialect-aware query engine for aggregate, search, and message-detail paths.
- TUI, HTTP API, MCP, `serve`, `search`, and `embeddings` command wiring.
- pgvector backend for semantic and hybrid search.
- Portable embedding queue, enqueuer, and worker loop.
- Live PostgreSQL and pgvector CI lanes.

SQLite remains the default backend. PostgreSQL is opt-in via
`[data].database_url`.

## Implemented In This PR

- PostgreSQL query/store fixes needed after the initial dialect extraction.
- pgvector backend under `internal/vector/pgvector/`.
- Runtime vector backend selection from the archive DSN.
- PostgreSQL embedding queue and worker support using rebinding and
  PostgreSQL-safe claim semantics.
- Native pgvector fused search through `vector.FusingBackend`.
- Tests for PostgreSQL deletion execution, attachment lifecycle, search
  filters, pagination, FTS, queue/worker behavior, and pgvector generation
  lifecycle.
- CI lanes for live PostgreSQL and pgvector coverage.

## Remaining Implementation Work

These are real follow-ups, not blockers for the current branch:

1. **SQLite to PostgreSQL migration.** There is no command to copy an existing
   SQLite archive into PostgreSQL. A migrator must handle identity columns with
   `OVERRIDING SYSTEM VALUE`, reset sequences with `setval()`, and rebuild FTS
   and embedding indexes rather than blindly copying derived state.

2. **PostgreSQL aggregate acceleration.** SQLite archives use DuckDB over
   Parquet for fast TUI aggregates. PostgreSQL archives currently use live SQL
   through the dialect-aware query engine. Large archives need a benchmarked
   plan, likely materialized views, cached aggregates, or a PostgreSQL-side
   equivalent to the Parquet projection.

3. **Scale validation.** Live PostgreSQL tests use small corpora. Before
   recommending PostgreSQL as the primary backend for 1M+ message archives, run
   a seeded large-corpus benchmark and capture `EXPLAIN ANALYZE` for fused
   hybrid search and common TUI aggregate queries.

4. **TextEngine parity.** Features exposed only through `query.TextEngine`
   remain SQLite-only. Either implement PostgreSQL equivalents or keep those UI
   paths explicitly gated off for PostgreSQL.

5. **Subset export on PostgreSQL.** `CopySubset` still targets a SQLite
   destination. PostgreSQL callers should continue to gate this off until a
   backend-aware export path exists.

6. **Deletion manifest source scoping.** Deletion updates match Gmail message
   IDs by `source_message_id` without carrying `source_id` per item in the
   manifest. A correct fix needs a manifest schema/version change so multi-source
   deletion batches can scope each remote ID precisely.

7. **FTS storage layout.** PostgreSQL stores `search_fts` inline on `messages`.
   This works, but bulk FTS updates rewrite message rows and can create GIN/MVCC
   bloat. A future schema can move FTS into a side table if write amplification
   becomes an issue.

8. **FTS grammar parity.** SQLite FTS5, PostgreSQL FTS, and PostgreSQL hybrid
   search do not parse every query the same way. The most visible difference is
   prefix matching in PostgreSQL FTS versus PostgreSQL hybrid search.

9. **Vector metric parity.** sqlite-vec uses L2 distance today while pgvector
   uses cosine distance. Unit-normalized embeddings rank the same, but full
   parity would require switching sqlite-vec tables to cosine and rebuilding
   existing vector indexes.

10. **Direct TUI-on-PostgreSQL smoke coverage.** PostgreSQL coverage currently
    exercises the query engine that the TUI depends on. A thin Bubble Tea smoke
    test against PostgreSQL would reduce integration risk.

## What Should Stay In This Repository

Keep implementation-adjacent material here:

- Current backend status and follow-up list in this file.
- PostgreSQL schema and migration code under `internal/store/`.
- pgvector schema and backend notes under `internal/vector/pgvector/`.
- Build-tag and CI notes in `Makefile`, `.github/workflows/`, and inline test
  comments.
- Test commands and contributor workflow in `CLAUDE.md` and `AGENTS.md`.

Keep user-facing setup, operational guidance, ranking explanations, and TUI
reference material in the docs website. Do not add new public docs pages under
this repo's `docs/` directory unless they are explicitly codebase-internal.

## Test Commands

```bash
make test
MSGVAULT_TEST_DB=postgres://user:pass@localhost:5432/msgvault_test make test-pg-both
go test -tags "fts5 sqlite_vec pgvector" -count=1 ./internal/vector/... ./internal/scheduler/... ./cmd/msgvault/cmd/...
```

All the `test-pg*` targets require a live PostgreSQL database, and the
pgvector-tagged ones require the `vector` extension installed.

`test-pg-both` is the one to run. The shipped binary carries no `pgvector` tag,
so that build has to be exercised against PostgreSQL as well as the pgvector
build does — but the tag changes the test binary of only nine packages out of
the full transitive test closure, so running both lanes in full reproves
roughly a thousand seconds of identical work. `test-pg-both` runs the pgvector
lane in full and then only the packages that differ, and its
`pg-shipped-only-check` prerequisite fails if that set ever drifts. That set is
the reverse dependency closure of the tag-sensitive packages, not just the
packages carrying pgvector-gated files: a package linking one of those builds a
different test binary even though its own sources are identical. The
single-lane targets `test-pg` and `test-pg-shipped` remain for narrowing down a
failure; neither covers PostgreSQL on its own.
