# PostgreSQL Backend

msgvault stores your archive in SQLite by default. You can instead use a PostgreSQL database — sync, full-text search, vector/hybrid semantic search, deletion staging, attachments, stats, and the TUI/HTTP/MCP read paths all run natively on PostgreSQL, with [pgvector](https://github.com/pgvector/pgvector) powering semantic search.

SQLite remains the default; PostgreSQL is opt-in. There is currently **no tool to migrate an existing SQLite archive into PostgreSQL** — use PostgreSQL for a new archive or a fresh re-sync. See [PG_STATUS.md](PG_STATUS.md) for the full done/not-done scope.

## Prerequisites

- PostgreSQL with the [pgvector](https://github.com/pgvector/pgvector) extension available (tested against PostgreSQL 16).
- msgvault runs `CREATE EXTENSION IF NOT EXISTS vector` on first connection. If your database role can't create extensions, have a DBA install pgvector and set `vector.skip_extension_create = true`.

## Build

Build with the PostgreSQL and vector build tags:

```bash
go build -tags "fts5 sqlite_vec pgvector" ./cmd/msgvault
# or: make build BUILD_TAGS="fts5 sqlite_vec pgvector"
```

## Select PostgreSQL

Set the database DSN in `~/.msgvault/config.toml`:

```toml
[data]
database_url = "postgres://user:pass@host:5432/msgvault?sslmode=require"
```

When `database_url` is unset, msgvault uses the default SQLite file at `~/.msgvault/msgvault.db`. Keep credentials out of a world-readable config — prefer `PGPASSWORD` / `~/.pgpass`, or `chmod 600` the file. The schema is created automatically on first connection:

```bash
msgvault sync-full you@gmail.com
```

## Embeddings (semantic & hybrid search)

Enable embeddings and point at an OpenAI-compatible embedding endpoint:

```toml
[vector]
enabled = true
backend = "pgvector"   # marker; the concrete backend is chosen from database_url

[vector.embeddings]
endpoint  = "http://localhost:11434/v1/embeddings"   # any OpenAI-compatible endpoint
model     = "your-embedding-model"
dimension = 768
```

Build the embedding index:

```bash
msgvault embeddings build                 # incremental
msgvault embeddings build --full-rebuild  # new generation
```

Embeddings live in the same PostgreSQL database as your messages (a per-dimension HNSW cosine index). To keep the index current automatically, run `msgvault serve` with a schedule:

```toml
[vector.embed.schedule]
cron = "*/15 * * * *"
run_after_sync = true
```

## Searching

```bash
msgvault search "quarterly review" --mode hybrid    # RRF-fused FTS + vector
msgvault search "renewal reminder"  --mode vector    # semantic only
msgvault search "from:alice invoice" --mode fts      # full-text (default)
msgvault search "..." --explain                       # per-signal scores
```

`serve` (HTTP API + MCP) and the TUI use the same search paths.

## Notes

- **Ranking differs slightly between backends.** Result *sets* match, but order can differ in adversarial cases (SQLite BM25 vs PostgreSQL `ts_rank`, plus differing FTS query grammars). See [search-ranking.md](search-ranking.md).
- **Status & scope** — what's covered and what's deferred (including the missing SQLite→PostgreSQL migration tool) is tracked in [PG_STATUS.md](PG_STATUS.md).
