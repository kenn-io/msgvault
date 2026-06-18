# msgvault Codebase Reference (for PostgreSQL Dialect Work)

## What Is msgvault?

[msgvault](https://github.com/kenn-io/msgvault) is an open-source, local-first
email archival tool by Wes McKinney (creator of pandas, Apache Arrow). Written
in Go, single binary. Archives a lifetime of email and chat with offline search,
analytics, and AI query via MCP.

**Project health (April 2026):** 1,700 GitHub stars, 97 forks, 23 releases,
active development (v0.12.1 released April 10, 2026). Has a dedicated site
(msgvault.io), Discord community, and conda-forge distribution.

## Tech Stack

| Component | Technology |
|-----------|-----------|
| Language | Go 1.25+ (single binary) |
| System of record | SQLite (WAL mode) |
| Analytics cache | DuckDB + Parquet (optional, ~3000x faster aggregates) |
| Full-text search | SQLite FTS5 (unicode61, diacritics removal) |
| TUI | Bubble Tea + Lipgloss (optional, `msgvault tui`) |
| MCP server | mark3labs/mcp-go |
| HTTP API | go-chi/v5 (`msgvault serve`) |
| MIME parsing | enmime |
| CLI | Cobra (63 commands) |

## Capabilities

- **Raw MIME storage** — original RFC822 in `message_raw` (zlib compressed,
  optional encryption). Enables re-processing, forensic recovery, EML export.
- **Multi-platform ingestion** — Gmail API, IMAP, MBOX, EMLX, iMessage,
  WhatsApp, Google Voice, Google Messages.
- **Content-addressed attachment storage** — SHA-256 dedup at
  `~/.msgvault/attachments/{first-2-chars}/{hash}`.
- **Deletion staging** — manifest-based workflow: stage, review, execute.
- **Gmail-like search syntax** — `from:`, `to:`, `subject:`, `label:`,
  `has:attachment`, `before:`, `after:`, etc.
- **Deduplication** — RFC822 Message-ID (cross-mailbox), SHA-256 (attachments),
  canonical_id (contacts).
- **Daemon mode** — `msgvault serve` with HTTP API, scheduled syncs, auth, CORS.

## The SQLite Problem

msgvault is tightly coupled to SQLite. The store layer has:

- 86 public methods on a concrete `Store` struct (no interface)
- 125+ raw SQL queries with `?` placeholders
- 51 FTS5 references across 6 files
- 35 `datetime()` calls (SQLite-specific)
- 9 `GROUP_CONCAT` calls (PostgreSQL uses `STRING_AGG`)
- 13 `rowid` references (mostly FTS5 join pattern)
- 11 PRAGMA statements
- 11 `isSQLiteError` calls (driver-specific error handling)

However, there are signs PostgreSQL was considered:

- `Rebind()` method exists as a no-op with TODO: "When PostgreSQL support is
  added, this will convert ? to $1, $2, etc."
- Schema is already split: `schema.sql` (portable) vs `schema_sqlite.sql`
  (FTS5-specific)
- 24 existing `Rebind()` call sites in the codebase

## Components Not Affected by Dialect Work

These are entirely optional and separable — no changes needed:

- **Bubble Tea TUI**: separate `msgvault tui` command. Uses `query.Engine`
  interface (already abstracted).
- **DuckDB + Parquet**: optional analytics cache via `msgvault build-cache`.
  Reads Parquet files regardless of which database backend is in use.
- **MCP server**: uses `query.Engine` interface and `Store` methods — no
  direct SQL.
- **HTTP API**: same abstraction path as MCP.
- **`subset.go`**: heavy PRAGMA introspection, deferred from this work.
  Leave SQLite-only.

## TODO: Post-PR1 Rebase Cascade

After any commit is added to PR1 (`pr1-branch-dialect-extraction`), the
downstream branches must be rebased to inherit the change:

```
pr1-branch-dialect-extraction
  └── pr2-branch-postgresql-dialect
        └── pr3-branch-postgresql-functional
              └── feat/postgresql-dialect  (PR4: migrate-db)
```

Steps:
```bash
git checkout pr2-branch-postgresql-dialect
git rebase pr1-branch-dialect-extraction

git checkout pr3-branch-postgresql-functional
git rebase pr2-branch-postgresql-dialect

git checkout feat/postgresql-dialect
git rebase pr3-branch-postgresql-functional

# Then force-push all four branches
git push --force-with-lease origin pr2-branch-postgresql-dialect pr3-branch-postgresql-functional feat/postgresql-dialect
```

**Current state (2026-04-17):** PR1 has a fix (`aa6ceab` — loggedDB type
mismatch) that PR2/PR3/PR4 have NOT yet inherited. The fix passes `s.db.DB`
instead of `s.db` to Dialect interface methods that expect `*sql.DB`. PR3+
also has additional `loggedDB` mismatches in `openPostgres`/`openPostgresReadOnly`
(they assign raw `*sql.DB` to `Store.db` which is now `*loggedDB`) — those
need fixing during the rebase.

Run `make build && make test` after each rebase to verify.
