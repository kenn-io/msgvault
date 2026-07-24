---
title: PostgreSQL Backend
description: Run msgvault on PostgreSQL with native full-text search and optional pgvector semantic search.
---

SQLite remains the default msgvault database. PostgreSQL is an opt-in backend for
new archives or fresh re-syncs. There is currently no SQLite to PostgreSQL
migration command.

On PostgreSQL, sync, full-text search, vector and hybrid semantic search,
deletion staging, attachment metadata, stats, and the Web UI, TUI, HTTP, and MCP read
paths all run against PostgreSQL. This covers the basic message-listing API
(`GET /api/v1/messages`), aggregate views (Senders/Domains/Labels/Time), and
search — the paths served by the dialect-aware query engine directly. The
Web UI's cache-backed analytical surfaces — Explore (its base listing as
well as the grouping/coverage/selection endpoints), Files, the People and
domains workspaces, and Relationships (ranking, timeline, and the identity
link/unlink cache refresh) — require the SQLite + DuckDB/Parquet analytics
cache and are unavailable on PostgreSQL; see Storage Differences below.
Optional semantic search uses the
[pgvector](https://github.com/pgvector/pgvector) extension and
stores embeddings in the same database as the message archive.

## Prerequisites

- PostgreSQL 16 is the tested target.
- The `pgvector` extension must be available if `[vector].enabled = true`.
- The msgvault database role needs normal DDL privileges so msgvault can create
  its schema on first connection.

msgvault runs `CREATE EXTENSION IF NOT EXISTS vector` when the pgvector backend
initializes. If your role cannot create extensions, have an administrator install
pgvector in the database and set:

```toml
[vector]
skip_extension_create = true
```

## Build

Standard builds include SQLite, FTS5, and sqlite-vec support:

```bash
make build
```

To include the pgvector backend, add the `pgvector` build tag:

```bash
go build -tags "fts5 sqlite_vec pgvector" ./cmd/msgvault
# or
make build BUILD_TAGS="fts5 sqlite_vec pgvector"
```

The `sqlite_vec` tag is still useful in PostgreSQL builds because some parity
tests and mixed backend code paths compile with both vector backends available.

## Configure PostgreSQL

Set `database_url` in `~/.msgvault/config.toml`:

```toml
[data]
database_url = "postgres://user:pass@host:5432/msgvault?sslmode=require"
```

When `database_url` is unset, msgvault uses `~/.msgvault/msgvault.db`.

Protect the DSN because it can contain credentials:

```bash
chmod 600 ~/.msgvault/config.toml
```

For production or LAN deployments, set `sslmode` deliberately, for example
`require` or `verify-full`. CI and local throwaway databases may use
`sslmode=disable`, but that should not be copied into a real deployment.

The schema is created automatically on first use:

```bash
msgvault sync-full you@example.com
```

## Enable pgvector Search

With a PostgreSQL `database_url`, msgvault selects the pgvector backend at
runtime. `[vector].backend` is a marker accepted by config validation, not the
primary selector.

```toml
[vector]
enabled = true
backend = "pgvector"

[vector.embeddings]
endpoint = "http://localhost:11434/v1"
model = "nomic-embed-text"
dimension = 768
```

The embedding endpoint is an OpenAI-compatible base URL. msgvault appends
`/embeddings`.

Build the index:

```bash
msgvault embeddings build --full-rebuild --yes
```

Keep it current manually:

```bash
msgvault sync you@example.com
msgvault embeddings build
```

Or use `msgvault serve` to drain the embedding queue after scheduled syncs:

```toml
[vector.embed.schedule]
cron = "*/15 * * * *"
run_after_sync = true
```

## Storage Differences

| Area | SQLite backend | PostgreSQL backend |
|---|---|---|
| Message store | `~/.msgvault/msgvault.db` | PostgreSQL database |
| Full-text index | FTS5 virtual table | `tsvector` column with GIN index |
| Vector store | `vectors.db` via sqlite-vec | pgvector tables in the main database |
| Vector metric | sqlite-vec default L2 | pgvector cosine distance |
| Analytics cache | DuckDB over Parquet | Live SQL through the query engine |

PostgreSQL does not currently have a Parquet acceleration layer equivalent to
the default SQLite TUI path. The TUI's Senders/Domains/Labels/Time aggregate
views run as live SQL, so very large archives should be validated with
realistic data before PostgreSQL becomes your primary backend.

`msgvault build-cache` is SQLite-only — it refuses to run against a
PostgreSQL `database_url`. This means the Web UI's cache-backed analytical
surfaces (Explore, including its base listing and its
grouping/coverage/selection endpoints; Files; the People and domains
workspaces; and Relationships' ranking, timeline, and identity link/unlink
cache refresh) have no PostgreSQL equivalent: those endpoints detect the
missing DuckDB/Parquet cache and return a named unavailable-cache state
rather than falling back to live SQL. If you see that state on a PostgreSQL
backend, it is expected — the [cache troubleshooting guidance](/web-ui/#cache-states)
applies to SQLite archives only.

## Current Scope

Implemented:

- PostgreSQL schema initialization and legacy column migrations.
- Store, query, Web UI, TUI, HTTP, and MCP read paths through a dialect-aware
  query layer, covering the basic message-listing API, aggregate views,
  search, and stats. The cache-only analytical surfaces (Explore, Files,
  People and domains, Relationships) are not part of this layer — see Not
  implemented below.
- Full-text search with PostgreSQL `tsvector` and `ts_rank`.
- Deletion staging and execution metadata updates.
- Attachment metadata and cleanup paths.
- pgvector semantic search and hybrid search.
- Embedding queue and worker support on PostgreSQL.
- Live PostgreSQL and pgvector test lanes in CI.

Not implemented:

- SQLite to PostgreSQL archive migration.
- PostgreSQL Parquet or materialized aggregate acceleration.
- The Web UI's cache-backed analytical surfaces (Explore, including its
  base listing and its grouping/coverage/selection endpoints; Files; the
  People and domains workspaces; and Relationships' ranking, timeline, and
  identity link/unlink cache refresh): these require the DuckDB/Parquet
  cache, which `build-cache` refuses to build against PostgreSQL, so they
  return an unavailable-cache state rather than running as live SQL.
- PostgreSQL corruption checks inside `msgvault verify`; use PostgreSQL
  operational tooling such as `pg_amcheck`.

## Operational Notes

If you expose `serve` or MCP beyond localhost, use a dedicated PostgreSQL role
with the minimum privileges needed for that process. msgvault's read-only mode
uses a per-session `default_transaction_read_only` setting, which is a useful
guardrail but not a replacement for database role permissions.

pgvector HNSW indexes are per dimension. Retiring a pgvector generation deletes
that generation's embedding rows so old vectors do not consume search candidate
budget for the active generation. Frequent full rebuilds can create dead tuples,
so monitor autovacuum on the embedding tables and run maintenance when needed.

See [Search Ranking Across Backends](/architecture/search-ranking/) for
ranking differences between SQLite, PostgreSQL, sqlite-vec, and pgvector.
