# GitHub Issue Draft: Database Backend Abstraction (PostgreSQL Support)

> To be filed at: https://github.com/kenn-io/msgvault/issues

---

## Title

Database backend abstraction: pluggable Dialect for PostgreSQL support

## Body

**TL;DR:** I want to add PostgreSQL as an opt-in database backend (SQLite stays the default). The approach: a thin `Dialect` interface that the existing `Store` delegates to for the ~50 database-specific call sites — FTS5 vs tsvector, `datetime()` vs `NOW()`, etc. The other ~90% of queries are standard SQL and only need `Rebind()`. I'd split it into two PRs: first extract `SQLiteDialect` (zero functional change), then add `PostgreSQLDialect`. Happy to do the work if you're open to it.

### Motivation

msgvault is excellent as a local email archive. I'm building a semantic search
and classification layer on top of it (pgvector embeddings, LLM-based labeling,
cross-entity graph connecting emails/documents/messages). The natural home for
that layer is PostgreSQL — and having email metadata in SQLite while embeddings
and intelligence live in PostgreSQL means cross-database joins that can't be
optimized by either query planner.

I suspect others building on msgvault's MCP server or HTTP API will hit similar
friction: the moment you want to enrich the archive with computed data
(embeddings, classifications, entity resolution, analytics beyond what
DuckDB/Parquet covers), you need the email metadata co-located with that data.

PostgreSQL also unlocks:
- **pgvector** for semantic search (cf. #154)
- **True multi-client concurrency** for `msgvault serve` (SQLite WAL allows
  one writer at a time)
- **Server-mode deployments** where the archive lives on a NAS or home server
  and multiple clients connect (cf. #116)

SQLite should absolutely remain the default — it's the right choice for the
single-user, local-first use case. This proposal adds PostgreSQL as an
opt-in alternative, not a replacement.

### Prior art in the codebase

The codebase already anticipates this:

1. **`Rebind()` method** (`store.go:286`) with an explicit TODO:
   ```go
   // When PostgreSQL support is added, this will convert ? to $1, $2, etc.
   ```
   24 call sites already use `Rebind()`.

2. **Split schema files**: `schema.sql` (portable DDL) is already separated
   from `schema_sqlite.sql` (FTS5 virtual table). The base schema is
   valid PostgreSQL with minor adjustments.

3. **Standard SQL for most queries**: The vast majority of the 125+ queries
   use standard SQL. SQLite-specific constructs are concentrated in a
   well-defined set of locations.

### Inventory of SQLite-specific constructs

Scanning through the `internal/` tree, here's what I see:

| Construct | Count | PostgreSQL equivalent | Complexity |
|-----------|------:|----------------------|------------|
| FTS5 (`messages_fts`, `MATCH`, `rowid` joins) | 51 refs, 6 files | `tsvector` + `GIN` index + `ts_rank` | High (but well-contained) |
| `datetime('now')` | 35 refs | `NOW()` / `CURRENT_TIMESTAMP` | Mechanical |
| `GROUP_CONCAT(col, sep)` | 9 refs | `STRING_AGG(col, sep)` | Mechanical |
| `INSERT OR IGNORE` | 6 refs | `ON CONFLICT DO NOTHING` | Mechanical |
| `INSERT OR REPLACE` | 3 refs | FTS-specific, replaced by tsvector trigger | Goes away |
| `PRAGMA` statements | 11 refs | Connection pool config / `SET` | Straightforward |
| `isSQLiteError()` | 11 refs | Driver-agnostic error type | Straightforward |
| `rowid` references | 13 refs | Standard PK (mostly FTS-related) | Goes away with tsvector |
| `?` placeholders | All queries | `$1, $2, ...` via `Rebind()` | Already stubbed |

**~90% of queries need no changes beyond `Rebind()`.** The SQLite-specific
surface area is concentrated, not scattered.

### Proposed approach: `Dialect` interface

Rather than two separate `Store` implementations (high duplication, hard to
maintain), I propose a thin `Dialect` interface that the single `Store`
delegates to for database-specific behavior:

```go
// Dialect abstracts database-specific SQL generation and behavior.
type Dialect interface {
    // Driver and query rewriting
    DriverName() string                   // "sqlite3" vs "pgx"
    Rebind(query string) string           // ? -> $1,$2,... for PostgreSQL
    Now() string                          // "datetime('now')" vs "NOW()"
    GroupConcat(expr, sep string) string   // GROUP_CONCAT vs STRING_AGG
    InsertOrIgnore(sql string) string     // "INSERT OR IGNORE" vs rewrite to ON CONFLICT DO NOTHING

    // Full-text search (see FTS5 → tsvector section below for rationale)
    FTSUpsertSQL() string                 // SQL to insert/update search index for one message
    FTSSearchClause(paramIndex int) (join, where, orderBy string)
    FTSDeleteSQL() string                 // SQL to remove a message from the search index
    FTSBackfillBatchSQL() string          // SQL to populate index for a range of message IDs
    FTSAvailable(db *sql.DB) (bool, error)
    FTSNeedsBackfill(db *sql.DB) (bool, error)
    SchemaFTS() string                    // DDL for FTS setup

    // Connection lifecycle
    InitConn(db *sql.DB) error            // PRAGMAs vs SET/pool config
    SchemaFiles() []string                // ["schema.sql", "schema_sqlite.sql"] vs ["schema.sql", "schema_pg.sql"]
    CheckpointWAL(db *sql.DB) error       // WAL checkpoint (no-op for PostgreSQL)

    // Schema migration support
    SchemaStaleCheck() string             // SQL to detect whether migrations are needed
    IsDuplicateColumnError(err error) bool

    // Error handling
    IsConflictError(err error) bool
    IsNoSuchTableError(err error) bool
}
```

**Phase 1: Extract `SQLiteDialect`** — move all current SQLite-specific
behavior behind the interface. Zero functional change. All existing tests
pass unmodified.

**Phase 2: Implement `PostgreSQLDialect`** — `schema_pg.sql` with tsvector
columns + GIN index, `pgx` driver, `$N` placeholders, `STRING_AGG`, etc.

**Phase 3: Config-driven selection** — `config.toml` gains a
`database_url` option:

```toml
[data]
# SQLite (default, unchanged):
# database_url = "sqlite:///home/user/.msgvault/msgvault.db"

# PostgreSQL (opt-in):
database_url = "postgres://user:pass@localhost:5432/msgvault"
```

Driver is inferred from the URL scheme. No behavioral change for existing
SQLite users.

### FTS5 → tsvector migration detail

This is the most involved part, so here's the concrete mapping and the design
rationale.

**The data layout problem:**

The searchable text spans three tables: `messages.subject`, `message_bodies.body_text`,
and addresses from `message_recipients` + `participants`. The current FTS5 virtual
table is a **denormalized copy** that pulls from all three during backfill. Once
populated, search queries only touch `messages_fts` + `messages`, never `message_bodies`.

A PostgreSQL `GENERATED ALWAYS AS` column can only reference columns in its own
table, so it can't combine `messages.subject` with `message_bodies.body_text`.
A trigger-based approach adds complexity and slows batch inserts.

**Solution: application-populated tsvector column on `messages`.**

Same data flow as SQLite — the application populates the search index via
`UpsertFTS()` and `BackfillFTS()`. The dialect controls the SQL.

**SQLite (current):**
```sql
-- Schema: separate virtual table
CREATE VIRTUAL TABLE messages_fts USING fts5(
    message_id UNINDEXED,
    subject, body, from_addr, to_addr, cc_addr,
    tokenize='unicode61 remove_diacritics 1'
);

-- Upsert (per-message):
INSERT OR REPLACE INTO messages_fts(rowid, message_id, subject, body, ...)
VALUES (?, ?, ?, ?, ...);

-- Search:
SELECT m.* FROM messages_fts fts
JOIN messages m ON m.id = fts.rowid
WHERE messages_fts MATCH ?
ORDER BY rank;

-- Backfill (batch):
INSERT OR REPLACE INTO messages_fts(rowid, message_id, subject, body, ...)
SELECT m.id, m.id, m.subject, mb.body_text, ...
FROM messages m LEFT JOIN message_bodies mb ON mb.message_id = m.id
WHERE m.id >= ? AND m.id < ?;
```

**PostgreSQL equivalent:**
```sql
-- Schema: column on messages + GIN index (no separate table)
ALTER TABLE messages ADD COLUMN search_fts TSVECTOR;
CREATE INDEX messages_search_fts_idx ON messages USING GIN (search_fts);

-- Upsert (per-message):
UPDATE messages SET search_fts =
    setweight(to_tsvector('simple', COALESCE($1, '')), 'A') ||
    setweight(to_tsvector('simple', COALESCE($2, '')), 'B') ||
    to_tsvector('simple', COALESCE($3, '')) ||
    to_tsvector('simple', COALESCE($4, '')) ||
    to_tsvector('simple', COALESCE($5, ''))
WHERE id = $6;
-- $1=subject, $2=from_addr, $3=body, $4=to_addr, $5=cc_addr, $6=messageID

-- Search (no extra JOIN needed — column is on messages):
SELECT m.* FROM messages m
WHERE m.search_fts @@ plainto_tsquery('simple', $1)
ORDER BY ts_rank(m.search_fts, plainto_tsquery('simple', $1)) DESC;

-- Backfill (batch):
UPDATE messages m SET search_fts =
    setweight(to_tsvector('simple', COALESCE(m.subject, '')), 'A') ||
    to_tsvector('simple', COALESCE(mb.body_text, '')) ||
    ... (address subqueries same as SQLite backfill)
FROM message_bodies mb
WHERE mb.message_id = m.id AND m.id >= $1 AND m.id < $2;
```

**How the dialect absorbs this:**

The application code stays identical for both backends:
```
insert message → insert body → insert recipients → call UpsertFTS()
```

The `Dialect` interface provides the SQL for each operation:
- `FTSUpsertSQL()` — per-message index update
- `FTSSearchClause(paramIndex)` — returns `(join, where, orderBy)` fragments
- `FTSBackfillBatchSQL()` — batch population for a range of IDs
- `FTSDeleteSQL()` — remove a message from the index

SQLite returns JOIN-based queries against `messages_fts`; PostgreSQL returns
column-based queries against `messages.search_fts`. The store code just plugs
in the fragments — it doesn't know or care which strategy is in use.

**Why this is better than the generated-column approach:**
- No cross-table column reference problem
- No triggers to slow batch inserts
- Same application flow as SQLite (no new code paths)
- Search queries on PostgreSQL are actually simpler (no extra JOIN)
- Backfill is explicit and observable (same as FTS5 today)

### What I'm offering

I'm prepared to implement this end-to-end and submit a PR. I'd like to
align on the approach before writing code:

1. Is a `Dialect` interface the right abstraction level, or would you prefer
   a different pattern (e.g., separate `Store` implementations)?
2. Should `schema_pg.sql` live alongside `schema_sqlite.sql` in
   `internal/store/`, or in a separate package?
3. Any preferences on the PostgreSQL driver? I'd default to `pgx` (most
   popular, pure Go, good performance).
4. For the query layer: a new `PostgreSQLEngine` implementing the existing
   `Engine` interface (parallel to `SQLiteEngine`) vs threading `Dialect`
   through the existing engine. I'd lean toward a new engine since the
   `Engine` interface already provides the abstraction.
5. Should `subset.go` (heavy PRAGMA introspection) be in scope or deferred?

### Scope boundary

This PR (or 2) would cover:

**PR 1: Extract SQLiteDialect (zero functional change)**
- `Dialect` interface definition (`internal/store/dialect.go`)
- `SQLiteDialect` implementation (`internal/store/dialect_sqlite.go`)
- Wire `Store` to hold a `Dialect` field, set by `Open()`/`OpenReadOnly()`
- Replace ~50 SQLite-specific call sites with dialect method calls
- All existing tests pass unmodified

**PR 2: PostgreSQL dialect**
- `PostgreSQLDialect` implementation (`internal/store/dialect_pg.go`)
- `schema_pg.sql` — PostgreSQL DDL with `BIGSERIAL`, `TIMESTAMPTZ`, `BYTEA`,
  `search_fts TSVECTOR` + GIN index
- `PostgreSQLEngine` implementing `query.Engine` interface
  (`internal/query/postgres.go`)
- Driver selection: `Open()` dispatches on URL scheme (`postgres://` → pgx)
- `pgx` driver dependency in `go.mod`
- Config: `database_url` option
- Dual-backend test support via `MSGVAULT_TEST_DB` env var
- CI PostgreSQL service container

**These PRs would NOT cover:**
- pgvector / semantic search (separate feature, enabled by this foundation)
- Migration tooling (SQLite → PostgreSQL data migration)
- Changes to DuckDB/Parquet analytics layer
- Changes to the TUI, MCP server, or HTTP API
- `subset.go` PostgreSQL support (heavy PRAGMA introspection, deferred)

### Labels

`enhancement`, `database`
