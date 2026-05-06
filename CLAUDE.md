# CLAUDE.md

## General Workflow

When a task involves multiple steps (e.g., implement + commit + PR), complete ALL steps in sequence without stopping. If creating a branch, committing, and opening a PR, finish the entire chain.

Always commit after every turn. Don't wait for the user to ask — if you made changes, commit them before responding. Do not ask "shall I commit?" or "want me to commit?" — just commit. Committing is not a destructive or risky action; it is the expected default after every change.

PR descriptions should be concise and changelog-oriented: what changed, why, and how to use it. Do not include test plans, design decisions, or implementation details — those belong in specs and commit messages.

## Project Overview

msgvault is an offline Gmail archive tool that exports and stores email data locally with full-text search capabilities. The goal is to archive 20+ years of Gmail data from multiple accounts, make it searchable, and eventually delete emails from Gmail once safely archived.

## Architecture (Go)

Single-binary Go application:

```
msgvault/
├── cmd/msgvault/            # CLI entrypoint
│   └── cmd/                 # Cobra commands
├── internal/                # Core packages
│   ├── tui/                 # Bubble Tea TUI
│   ├── query/               # DuckDB query engine over Parquet
│   ├── store/               # SQLite database access
│   ├── deletion/            # Deletion staging and manifest
│   ├── gmail/               # Gmail API client
│   ├── sync/                # Sync orchestration
│   ├── oauth/               # OAuth2 flows (browser + device)
│   └── mime/                # MIME parsing
│
├── go.mod                   # Go module
└── Makefile                 # Build targets
```

## Quick Commands

```bash
# Build
make build                    # Debug build
make build-release            # Release build (optimized)
make install                  # Install to ~/.local/bin or GOPATH
make test                     # Run tests
make lint                     # Run linter

# CLI usage
./msgvault init-db                                    # Initialize database
./msgvault add-account you@gmail.com                  # Browser OAuth
./msgvault add-account you@gmail.com --headless       # Device flow
./msgvault add-account you@acme.com --oauth-app acme  # Named OAuth app
./msgvault sync-full you@gmail.com --limit 100        # Sync with limit
./msgvault sync-full you@gmail.com --after 2024-01-01 # Sync date range
./msgvault sync-incremental you@gmail.com             # Incremental sync

# TUI and analytics
./msgvault tui                                        # Launch TUI
./msgvault tui --account you@gmail.com                # Filter by account
./msgvault tui --local                                # Force local (override remote config)
./msgvault build-cache                                # Build Parquet cache
./msgvault build-cache --full-rebuild                 # Full rebuild
./msgvault stats                                      # Show archive stats

# Apple Mail import
./msgvault import-messenger --me you@facebook.messenger ~/facebook-dyi   # Import Facebook Messenger DYI
./msgvault import-emlx                                # Auto-discover accounts
./msgvault import-emlx ~/Library/Mail                 # Explicit mail directory
./msgvault import-emlx --account me@gmail.com         # Specific account(s)
./msgvault import-emlx /path/to/dir --identifier me@gmail.com  # Manual fallback

# Daemon mode (NAS/server deployment)
./msgvault serve                                      # Start HTTP API + scheduled syncs

# Maintenance
./msgvault repair-encoding                            # Fix UTF-8 encoding issues
```

## Key Files

### CLI (`cmd/msgvault/cmd/`)
- `root.go` - Cobra root command, config loading
- `syncfull.go` - Full sync command implementation
- `syncincremental.go` - Incremental sync command
- `tui.go` - TUI command, cache auto-build
- `build_cache.go` - Parquet cache builder (DuckDB)
- `repair_encoding.go` - UTF-8 encoding repair

- `import_emlx.go` - Apple Mail .emlx import command

### Core (`internal/`)
- `tui/model.go` - Bubble Tea TUI model and update logic
- `tui/view.go` - View rendering with lipgloss styling
- `query/engine.go` - DuckDB query engine over Parquet files
- `query/sqlite.go` - SQLite query engine (fallback)
- `store/store.go` - SQLite database operations
- `store/schema.sql` - Core SQLite schema
- `store/schema_sqlite.sql` - FTS5 virtual table
- `deletion/manifest.go` - Deletion staging and manifest generation
- `gmail/client.go` - Gmail API client with rate limiting
- `oauth/oauth.go` - OAuth2 flows (browser + device)
- `sync/sync.go` - Sync orchestration, MIME parsing
- `mime/parse.go` - MIME message parsing

### TUI Keybindings
- `j/k` or `↑/↓` - Navigate rows
- `Enter` - Drill down into selection
- `Esc` or `Backspace` - Go back
- `Tab` - Cycle views (Senders → Sender Names → Recipients → Recipient Names → Domains → Labels → Time)
- `s` - Cycle sort field (Name → Count → Size)
- `r` - Reverse sort direction
- `t` - Jump to Time view (cycle granularity when already in Time)
- `a` - Filter by account
- `f` - Filter by attachments
- `Space` - Toggle selection
- `A` - Select all visible
- `x` - Clear selection
- `d` - Stage selected for deletion
- `D` - Stage all messages matching current filter
- `/` - Search
- `?` - Help
- `q` - Quit

## Database Schema

Core tables:
- `sources` - Gmail accounts with history_id for incremental sync
- `conversations` - Gmail thread abstraction
- `messages` - Message metadata, foreign key to conversation
- `message_raw` - Raw MIME blob (zlib compressed)
- `labels` / `message_labels` - Gmail labels (many-to-many)
- `participants` / `message_recipients` - From/To/Cc/Bcc addresses
- `attachments` - Attachment metadata with content-hash deduplication
- `messages_fts` - FTS5 virtual table
- `sync_runs` / `sync_checkpoints` - Sync state for resumability

Schema files in `internal/store/`:
- `schema.sql` - Core schema (SQLite; shared structure)
- `schema_sqlite.sql` - SQLite FTS5 virtual table
- `schema_pg.sql` - PostgreSQL tsvector column + GIN index (opt-in, scaffold)

**Database backend**: SQLite is the default. PostgreSQL support is
scaffolded behind a `Dialect` interface (`internal/store/dialect.go`);
see `docs/PG_STATUS.md` for the current state and follow-up
work required to make PostgreSQL functional end-to-end.

**Test env**: `MSGVAULT_TEST_DB=postgres://...` is scaffolded for the future
PostgreSQL store test suite (`make test-pg`). Until the blockers in
`docs/PG_STATUS.md` are resolved, PostgreSQL tests are expected to fail before
schema initialization completes.

## Parquet Analytics

The TUI uses denormalized Parquet files for fast aggregate queries (~3000x faster than SQLite JOINs).

```
~/.msgvault/
├── msgvault.db              # SQLite: System of record
└── analytics/               # Parquet: Aggregate analytics
    ├── messages/year=*/     # Partitioned by year
    └── _last_sync.json      # Incremental sync state
```

**Workflow:**
1. Sync emails: `./msgvault sync-full you@gmail.com`
2. Launch TUI: `./msgvault tui` (auto-builds cache if needed)

**Parquet schema:**
- Denormalized: `from_email`, `from_domain`, `to_emails[]`, `labels[]`, etc.
- Partitioned by `year` for efficient time-range queries
- Compact: small fraction of SQLite size (excludes message bodies)

The TUI automatically builds/updates the Parquet cache on launch when new messages are detected.

## Implementation Status

### Completed
- **Gmail Sync**: Full/incremental sync, OAuth (browser + headless), rate limiting, resumable checkpoints
- **MIME Parsing**: Subject, body (text/HTML), attachments, charset detection
- **Parquet ETL**: DuckDB-based SQLite → Parquet export with incremental updates
- **Query Engine**: DuckDB over Parquet for fast aggregate analytics
- **TUI**: Full-featured TUI with drill-down navigation, search, selection, deletion staging
- **UTF-8 Repair**: Comprehensive encoding repair for all string fields
- **Deletion Execution**: Execute staged deletions via Gmail API (trash or permanent delete)

### Not Yet Implemented
- **App-level encryption**: Encrypt database and attachments at rest
- **Web UI**: Browser-based interface

## Testing with Real Gmail Data

```bash
./msgvault init-db
./msgvault add-account you@gmail.com
./msgvault sync-full you@gmail.com --after 2024-12-01 --before 2024-12-15
./msgvault tui
```

Sync is **read-only** - no modifications to Gmail.

## Test Data

Never use real people's names, email addresses, or identifiers in test fixtures. Use obviously synthetic names: `alice`, `bob`, `Test User`, `user@example.com`. Before committing test data, verify no real PII is present.

## Go Development

After making any Go code changes, always run `go fmt ./...` and `go vet ./...` before committing. Stage ALL resulting changes, including formatting-only files.

## Git Workflow

When committing changes, always stage ALL modified files (including formatting, generated files, and ancillary changes). Run `git diff` and `git status` before committing to ensure nothing is left unstaged.

## Code Style & Linting

All code must pass formatting and linting checks before commit. A pre-commit
hook is available via [prek](https://prek.j178.dev/) to enforce this
automatically:

```bash
make install-hooks             # Install pre-commit hook via prek
make test                      # Run tests (SQLite default)
make test-pg                   # PostgreSQL scaffold check; expected to fail until PG_STATUS blockers are fixed
make fmt                       # Format code (go fmt)
make lint                      # Run linter (auto-fix)
make lint-ci                   # Run linter (CI, no auto-fix)
go vet ./...                   # Check for issues
```

**Standards:**
- Default gofmt configuration
- Use `error` return values, wrap with context using `fmt.Errorf`
- Table-driven tests

## Code Conventions

- Use Bubble Tea for TUI, lipgloss for styling
- DuckDB for Parquet queries, go-duckdb driver
- SQLite via marcboeker/go-duckdb for cache building, mattn/go-sqlite3 for store
- Context-based cancellation for long operations
- Route all DB operations through `Store` struct
- Charset detection via gogs/chardet, encoding via golang.org/x/text/encoding

## SQL Guidelines

- **Never use SELECT DISTINCT with JOINs** - Use EXISTS subqueries instead (becomes semi-joins)
- EXISTS is faster (stops at first match) and avoids duplicates at the source
- Example - instead of:
  ```sql
  SELECT DISTINCT m.id FROM messages m
  JOIN message_recipients mr ON mr.message_id = m.id
  WHERE mr.recipient_type = 'from' AND ...
  ```
  Use:
  ```sql
  SELECT m.id FROM messages m
  WHERE EXISTS (
      SELECT 1 FROM message_recipients mr
      WHERE mr.message_id = m.id AND mr.recipient_type = 'from' AND ...
  )
  ```

- **Never JOIN or scan `message_bodies` in list/aggregate/search queries** — this table is separated from `messages` specifically to keep the messages B-tree small for fast scans. Only access `message_bodies` via direct PK lookup (`WHERE message_id = ?`) when displaying a single message detail view. For text search, use FTS5 (`messages_fts`); if FTS is unavailable, search `subject`/`snippet` only.

## Configuration

All data defaults to `~/.msgvault/`:
- `~/.msgvault/config.toml` - Configuration file
- `~/.msgvault/msgvault.db` - SQLite database
- `~/.msgvault/attachments/` - Content-addressed attachment storage
- `~/.msgvault/tokens/` - OAuth tokens per account
- `~/.msgvault/analytics/` - Parquet cache files

Override with `MSGVAULT_HOME` environment variable.

```toml
[data]
# data_dir = "~/custom/path"

[oauth]
client_secrets = "/path/to/client_secret.json"

# Named OAuth apps for Google Workspace orgs
# [oauth.apps.acme]
# client_secrets = "/path/to/acme_secret.json"

[sync]
rate_limit_qps = 5
```
