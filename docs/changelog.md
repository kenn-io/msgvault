---
title: Changelog
description: Release history for msgvault
---

All notable changes to msgvault, grouped by release.

## Unreleased

**New features**

- `msgvault skills install` installs agent skills (per the open
  agent-skills standard) for Claude Code and Codex, teaching coding
  agents the search, attachment, and analytics CLI workflows.
  `msgvault skills uninstall` removes them.
- MCP message search now separates metadata and body search:
  `search_metadata` searches metadata, `search_message_bodies` performs
  body-only full-text search with bounded context around each match, and
  `semantic_search_messages` owns explicit `vector` and `hybrid` modes.
  The former combined `search_messages` tool remains as a deprecated
  compatibility wrapper during migration.
- MCP `list_messages` accepts a `conversation_id` filter for listing one
  conversation or thread.
- The daemon HTTP API schema is now 1.3.0 and supports `scope=body` on deep
  search so daemon-backed clients can require an exact body-only response;
  bounded excerpts are returned in an ID-keyed `body_contexts` companion.

**Improvements**

- Backup restore now installs compatible attachment packs directly instead of
  recreating every blob as an individual file, substantially reducing file
  creation overhead on Windows and large archives. Selected content remains
  SHA-256 verified, incompatible or oversized entries fall back safely to
  loose files, and `--loose-attachments` preserves the explicit downgrade and
  recovery path.
- Metadata and body search scopes are enforced consistently across SQLite,
  PostgreSQL, DuckDB, and daemon-backed MCP sessions. Metadata result counts
  and aggregate statistics use the same predicate, and body-search snippets
  remain bounded, UTF-8-safe, and are selected by each backend's native FTS
  tokenizer.
- PostgreSQL full-text indexes use a versioned field layout so stale indexes
  are detected and must be backfilled before body-only search.

---

## 0.17.1
<small>2026-07-06</small>

**New features**

- Deletion staging endpoints on the HTTP API (schema 1.2.0):
  `POST /api/v1/deletions` stages messages by filter and/or message IDs with
  server-side Gmail-ID resolution and a `dry_run` preview,
  `GET /api/v1/deletions` lists staged manifests by status, and
  `DELETE /api/v1/deletions/{id}` cancels a pending or in-progress manifest.
  Execution still happens only through `delete-staged`.
- The generated Go client includes deletion staging support through
  `StageDeletion`, including both dry-run (`200`) and create (`201`)
  responses.
- `msgvault search` now reports full-text index readiness while the daemon is
  checking or rebuilding the CLI search index, with long-running searches
  showing the daemon activity they are waiting on.

**Improvements**

- CLI search no longer waits for the full-text index completeness probe or
  backfill before returning results. Index checks and backfills run in the
  background, and responses identify when results may be incomplete until the
  index catches up.
- SQLite archives now index deletion timestamps so daemon cold starts avoid
  full-table deletion checks while deciding whether the analytics cache is
  stale.
- Automated dependency updates now use Renovate, and project dependencies were
  refreshed.

**Bug fixes**

- IMAP UID enumeration now constrains searches to fetchable UID ranges and
  raw fetches no longer depend on fragile `ENVELOPE` parsing, improving
  robustness on servers such as iCloud IMAP.
- API-staged deletion manifests now reject empty or unknown filter input,
  enforce single-account selections, record the raw staging request, and avoid
  same-second manifest ID collisions.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for the deletion staging
  API and client support, non-blocking CLI search status reporting, deletion
  timestamp indexes, and dependency refresh.
- Thanks to [Tim Kersten](https://github.com/io41) for the IMAP UID
  enumeration and raw-fetch robustness fixes.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for
  migrating automated dependency updates to Renovate.

---

## 0.17.0
<small>2026-07-04</small>

**New features**

- `msgvault backup` adds incremental, verifiable archive snapshots with
  `init`, `create`, `list`, `verify`, and `restore`. Snapshots capture the
  SQLite database, attachments, deletion audit files, and optional config/token
  extras into an append-only repository with byte-level verification.
- Google Calendar archive support via `msgvault add-calendar` and
  `msgvault sync-calendar`, including read-only event sync, recurring series,
  cancellations, scheduled `[[gcal]]` sync, and search with
  `--message-type calendar_event`.
- Microsoft Teams archive support via delegated Microsoft Graph sync.
  `msgvault add-teams` authorizes Graph access, `msgvault sync-teams` imports
  chats and channels, Teams messages are stored with `message_type = teams`,
  and `backfill-teams-media` can re-fetch hosted inline media for already
  imported messages.
- Search and query paths now understand message-type scoping. Local search
  accepts `message_type:` / `message_type=` query operators and the
  `--message-type` flag; HTTP, MCP, aggregate, and SQL-backed query surfaces
  expose the stored `message_type` so email, calendar events, text messages,
  Teams messages, and other imported records can be separated cleanly.
- Scoped embedding builds can restrict a vector generation to selected message
  types through `[vector.embed.scope].message_types`, with vector/hybrid search
  rejecting incompatible unscoped queries instead of treating a partial index as
  complete.
- The HTTP API is now generated through Huma with a checked-in OpenAPI contract
  (`msgvault openapi`, `/openapi.json`) and a generated Go client under
  `pkg/client`.
- Archive-access CLI commands now route through a msgvault daemon (the
  configured remote server, or a local background daemon that starts on
  demand and idles out after `[server].daemon_idle_timeout`). The daemon is
  the single archive writer: concurrent operations queue with a visible
  `Waiting:` message, read-only commands run immediately, and scheduled
  syncs yield to interactive commands. See the
  [Daemon Migration Guide](/guides/daemon-migration/).
- Daemon lifecycle management via `msgvault serve start|status|stop|restart`,
  with automatic restart of older local daemons on binary upgrade
  (`[server].daemon_auto_restart`).

**Improvements**

- IMAP resyncs skip unchanged folders using the mailbox `UIDVALIDITY` and
  `UIDNEXT` values captured after the previous completed sync.
- `msgvault serve stop` now explains long shutdown waits by reporting the
  daemon operation it is waiting for and periodically printing elapsed wait
  updates.
- MCP `get_message` returns large message bodies in windows instead of
  returning the whole body in one response.
- Vector embedding maintenance no longer uses a separate pending queue.
  Coverage is tracked per message with `embed_gen`, so rebuilds, repairs, and
  daemon top-ups all share the same scan-and-fill path.
- Full-sync batch errors are counted and persisted on the sync run, making
  source status and diagnostics more accurate after partial failures.
- The documentation site now lives in the repository, including CLI, API,
  setup, backup, calendar, daemon, PostgreSQL, and vector-search docs plus the
  docs build/check scripts.
- macOS users can install through Homebrew with `brew install msgvault`.
- The TUI migration to Bubble Tea v2 is complete.

**Deprecations**

- `tui --force-sql`, `tui --no-cache-build`, `tui --no-sqlite-scanner`,
  `mcp --force-sql`, and `mcp --no-sqlite-scanner` are deprecated (removal
  planned for a later release); engine and cache selection moved to the
  `[analytics]` config section.

**Bug fixes**

- MCP list/search requests now reject Gmail-only `list:` / `List-ID` search
  operators instead of treating them as local full-text terms.
- DuckDB query engines re-probe Parquet schema columns when the cache is
  rebuilt or replaced underneath a running process.
- Empty `subject:` and empty text search terms no longer match every message.
- Facebook Messenger imports preserve multiple attachments on one message
  instead of collapsing them into a single row.
- Imported message and reaction timestamps are normalized to UTC.
- TUI startup avoids mixed SQLite access while the daemon owns archive writes.
- API server documentation no longer overflows its sidebar on narrow layouts.
- Release publishing and the Nix update flow were corrected for the new module
  and release packaging.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for the backup repository
  commands, daemon-only CLI routing, daemon lifecycle/status improvements,
  IMAP folder-state skipping, the in-repo documentation site, OpenAPI/client
  generation, API docs polish, TUI startup fix, and release/Nix publishing
  fixes.
- Thanks to [danshapiro](https://github.com/danshapiro) for Google Calendar
  sync, message-type filters, scoped embedding builds, and persisted full-sync
  batch error counts.
- Thanks to [Nat Torkington](https://github.com/njt) for Microsoft Teams
  ingestion and the Facebook Messenger multi-attachment fix.
- Thanks to [Yuriy Grinberg](https://github.com/webgress) for replacing the
  pending embedding queue with per-message embedding generations.
- Thanks to [endolith](https://github.com/endolith) for windowed MCP message
  body reads.
- Thanks to [Lazare Rossillon](https://github.com/Lazare-42) for re-probing
  Parquet schemas when the analytics cache changes underneath a running query
  engine.
- Thanks to [Matthew Sweeney](https://github.com/sweenzor) for the Homebrew
  installation instructions.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for
  finishing the Bubble Tea v2 TUI migration.

---

## 0.16.0
<small>2026-06-18</small>

**New features**

- PostgreSQL backend with pgvector support for semantic and hybrid search.
- Source sync status endpoint in the HTTP API.
- Pagination for MCP `search_messages` and `list_messages`.

**Improvements**

- Record per-item sync errors so failed imports and fetch/ingest/delete failures are visible without treating an entire run as opaque.
- Upgrade DuckDB support to DuckDB 1.5.4 via `duckdb-go/v2`.
- Improve search/query behavior across MCP pagination, full-text query sanitization, and backend compatibility.
- Update packaging and release metadata for 0.16.0.
- Update stale project URLs to the `kenn-io/msgvault` location.

**Bug fixes**

- Prevent WAL corruption by isolating DuckDB's SQLite usage from the daemon.
- Fix Linux release builds by correcting the DuckDB link.
- Harden SyncTech SMS Backup & Restore Drive sync lifecycle handling.
- Tolerate null source import checksums.

**Acknowledgements**

- Thanks to [Yuriy Grinberg](https://github.com/webgress) for the PostgreSQL backend and pgvector semantic/hybrid search support.
- Thanks to [danshapiro](https://github.com/danshapiro) for source sync status, per-item sync diagnostics, SyncTech Drive lifecycle hardening, and null-checksum handling.
- Thanks to [Matthew Sweeney](https://github.com/sweenzor) for the DuckDB 1.5.4 / `duckdb-go/v2` migration and stale URL cleanup.
- Thanks to [endolith](https://github.com/endolith) for MCP pagination on `search_messages` and `list_messages`.
- Thanks to [Jesse Robbins](https://github.com/jesserobbins) for improving the AFM vector-search docs and correcting the accounts, identities, collections, and deduplication documentation for the shipped 0.16.0 behavior.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for isolating DuckDB's SQLite usage from the daemon to prevent WAL corruption.
- Thanks to [Wes McKinney](https://github.com/wesm) for the Linux release-build fix and 0.16.0 packaging/release metadata updates.

---

## 0.15.2
<small>2026-06-10</small>

**New features**

- Add `msgvault verify --json` for machine-readable verification results.

**Bug fixes**

- Escape FTS5 metacharacters in hybrid search queries to prevent search failures.
- Fix Windows installer redirects in PowerShell 5.x and improve arm64 fallback handling.

**Improvements**

- Update minor and patch dependencies.

**Acknowledgements**

- Thanks to [Carlos de la Lama-Noriega](https://github.com/cdelalama) for adding machine-readable `verify` output.
- Thanks to [Frederic Masi](https://github.com/fmasi) for fixing hybrid search failures with FTS5 metacharacters.
- Thanks to [Wes McKinney](https://github.com/wesm) for fixing Windows installer redirects and arm64 fallback handling.

---

## 0.15.0
<small>2026-05-28</small>

**New features**

- Microsoft Outlook PST archive import via `msgvault import-pst`, including folder labels, attachments, resumable checkpoints, and automatic skipping of non-email PST items.
- Facebook Messenger Download Your Information import via `msgvault import-messenger`, with JSON and HTML export support.
- SyncTech SMS Backup & Restore import via `msgvault import-synctech-sms`, plus Google Drive source configuration and one-shot sync commands for scheduled Android backups.
- Per-account identities, named collections, scoped search/stats, and reversible deduplication workflows across accounts and collections.
- Google service account support for Workspace domain-wide delegation, including per-app service account keys.
- Message detail API responses now expose `body_html`, and inline image MIME parts can be fetched through a dedicated inline endpoint.
- MCP StreamableHTTP transport with `msgvault mcp --http`.
- MCP `search_by_domains` tool for finding messages where any participant belongs to one of several domains.

**Improvements**

- Vector embedding management is consolidated under a single `msgvault embeddings` command, with `build`, `resume`, `list`, `activate`, and `retire` subcommands covering the full index-generation lifecycle. `msgvault build-embeddings` still works as a deprecated alias for `msgvault embeddings build`.
- Long messages are split into embedding chunks instead of being truncated to a single input.
- Embedding preprocessing now handles HTML bodies, base64/data blobs, URL tracking parameters, and whitespace cleanup more aggressively.
- Embedding progress reporting has steadier ETA handling, per-character timing, and better behavior when failed batches are downshifted.
- SQLite full-text search ranking better matches the weighting used by PostgreSQL-backed paths.
- iMessage imports can backfill participant display names from vCard contacts.
- Scheduled sync dispatch now resolves source type and supports IMAP sources as well as Gmail. `msgvault serve` can also schedule SyncTech SMS Backup & Restore Drive sources.
- SQLite sync paths are more durable and treat transient network failures as retryable scheduled-sync skips.
- Routine CLI command errors no longer print the full help output.
- Update checks avoid unnecessary GitHub API rate-limit pressure.
- Switch the Go module path to `go.kenn.io/msgvault`.

**Bug fixes**

- Domain-based search results now hide locally deleted rows.
- SQL slow/error logging includes query arguments and reports accurate streaming query durations.
- Embedding skips rows that become empty after preprocessing and surfaces API 4xx response bodies for easier troubleshooting.
- PST imports namespace source message IDs per archive so messages from different PST files no longer collide.

**Acknowledgements**

- Thanks to [Matthew C Roberts](https://github.com/YourEconProf) for the Microsoft Outlook PST importer, including attachment handling, folder labels, checkpoints, and PST item filtering.
- Thanks to [Jesse Robbins](https://github.com/jesserobbins) for Facebook Messenger import support, the accounts/identities/collections/deduplication work, and several embedding progress and logging improvements.
- Thanks to [danshapiro](https://github.com/danshapiro) for the SyncTech SMS Backup & Restore importer and Google Drive source workflow.
- Thanks to [hansn74](https://github.com/hansn74) for Google service account support, MCP StreamableHTTP transport, multi-domain participant search, long-message embedding chunking, and expanded embedding preprocessing.
- Thanks to [Yuriy Grinberg](https://github.com/webgress) for the PostgreSQL dialect refactor work and SQLite FTS5 ranking improvements.
- Thanks to [Rob Elkin](https://github.com/robelkin) for SQLite sync durability hardening and better handling of transient scheduled-sync network failures.
- Thanks to [Franklin](https://github.com/franklintra) for scheduled-sync dispatch by source type, including IMAP support.
- Thanks to [Boris Jabes](https://github.com/bjabes) for iMessage display-name backfill from vCard contacts.
- Thanks to [sarcasticbird](https://github.com/sarcasticbird) for exposing HTML email bodies and inline MIME parts through the API.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for the golangci-lint v2 migration, broad linter cleanup, command error-output polish, and Nix flake restructuring.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the `go.kenn.io/msgvault` module path migration and embedding-generation lifecycle command work.
- Thanks to [Wes McKinney](https://github.com/wesm) for PST source-message ID namespacing, update-check rate-limit avoidance, Docker CI speedups, and the Go test-suite migration to testify.

---

## 0.14
<small>2026-04-21</small>

**New features**

- **Vector search (semantic and hybrid).** msgvault can now embed your archive using a configured OpenAI-compatible embedding endpoint (Ollama, llama.cpp `server`, LM Studio, etc.) and search it by meaning, not just keywords. `msgvault search --mode vector` runs pure semantic search; `--mode hybrid` fuses BM25 and vector similarity via Reciprocal Rank Fusion. Exposed through local CLI search (`msgvault search`), the HTTP API (`GET /api/v1/search?mode=vector|hybrid`), and the MCP server (`search_messages` mode argument plus a new `find_similar_messages` tool). See [Vector Search](/usage/vector-search/).
- `msgvault build-embeddings` command to generate and maintain the local vector index. Incremental by default; `--full-rebuild` creates a new generation and atomically activates it once coverage reaches zero. Same-model rebuilds keep answering against the previous active generation while the new one is built, with active-generation top-ups frozen until activation; model or dimension changes return `index_stale` until activation.
- Background embedding via the daemon scheduler. A new `[vector.embed.schedule]` config block drives the embed worker on cron and/or after every successful scheduled sync, so `msgvault serve` can keep the vector index current without manual intervention.
- `/api/v1/stats` gains a `vector_search` sub-object reporting the active generation, any in-flight rebuild, and the actionable missing embedding count for the generation the worker will target next.
- `msgvault rebuild-fts` command to rebuild the SQLite FTS5 shadow table after corruption.

**Improvements**

- `search` command gains `--mode fts|vector|hybrid` and `--explain` flags. `--explain` includes per-signal scores (RRF, BM25, vector) in table and JSON output for ranking inspection.
- Configuration gains a full `[vector]` block with sub-tables for the embedding endpoint, message preprocessing, hybrid ranking, and the embed scheduler. See [Configuration: vector](/configuration/#vector).
- `remove-account` deletes attachment files from disk when they were unique to the removed account. Files shared across multiple accounts are preserved automatically, and an in-progress sync on any account skips file deletion to avoid racing new attachment writes.

**Bug fixes**

- `remove-account` no longer leaves orphaned attachment files on disk after an account's database rows are removed.

**Acknowledgements**

- Thanks to [Yuriy Grinberg](https://github.com/webgress) for the first PostgreSQL dialect refactor, which laid groundwork for alternative storage backends.
- Thanks to [Matthew C Roberts](https://github.com/YourEconProf) for making `remove-account` clean up unshared attachment files safely.
- Thanks to [Wes McKinney](https://github.com/wesm) for semantic and hybrid vector search, the embedding-generation workflow, background embedding scheduler, and the FTS5 rebuild recovery command.

---

## 0.13.1
<small>2026-04-15</small>

**Bug fixes**

- Fix importing older WhatsApp `msgstore.db` backups.

---

## 0.13
<small>2026-04-14</small>

**New features**

- Structured file logging with per-run correlation IDs. Every CLI invocation gets a unique `run_id` on every log line, making it easy to trace a single run across shared log files. New `msgvault logs` command for viewing and tailing logs. File logging is opt-in; see [Configuration: Log](/configuration/#log) for setup.

**Improvements**

- The terminal UI inherits your terminal background colors instead of forcing its own, so custom terminal themes (Dracula, Solarized, Nord, etc.) work naturally.

**Bug fixes**

- Improve full-text search performance across local archives.
- Improve terminal UI stability during rapid interactions (fix race condition where switching views could briefly show stale data).

**Acknowledgements**

- Thanks to [Jesse Robbins](https://github.com/jesserobbins) for structured file logging with per-run correlation IDs, plus the full-text search performance fix and TUI race-condition fix.
- Thanks to [Wes McKinney](https://github.com/wesm) for making the TUI inherit terminal background colors.

---

## 0.12.1
<small>2026-04-10</small>

**New features**

- Shell completion via `msgvault completion` for Bash, Zsh, Fish, and PowerShell.
- `MSGVAULT_IMAP_PASSWORD` environment variable and stdin piping for non-interactive `add-imap` (Docker, CI).
- Advanced search: word-boundary regex matching replaces ILIKE substring matching across all search paths, and FTS5 prefix search for the SQLite full-text index.
- Expanded store API with structured query parsing for search (`SearchMessagesQuery`).

**Improvements**

- Search result quality: text matching switched from ILIKE to word-boundary regex, reducing false positives from substring matches. SQLite aggregate sort ties are broken deterministically by key.
- Nix flake packaging metadata updated for 0.12.1.
- Docker image switched to `wolfi-base` with `libstdc++` for CGO/DuckDB compatibility, non-root user, and health check.

**Bug fixes**

- IMAP label handling: standard folders (Sent, Drafts, Trash, Junk, etc.) are now classified as system labels via RFC 6154 attributes and fallback name matching.
- `import-mbox` accepts plain mbox files with any extension (ZIP entries still require `.mbox`/`.mbx`), `--label` is repeatable/comma-separated, and re-imports update labels on existing messages instead of silently skipping.
- API search now uses the full structured query parser (operators like `from:`, `subject:`, date/size filters) instead of plain-text matching.
- `completion` command registered correctly in the CLI command tree.

**Acknowledgements**

- Thanks to [Jesse Robbins](https://github.com/jesserobbins) for the advanced search work across regex matching, FTS5 prefix search, snippets, deterministic sorting, and related search-quality improvements.
- Thanks to [Wes McKinney](https://github.com/wesm) for the completion-command registration fix, IMAP label handling fixes, MBOX import fixes, API structured-query search fix, and Docker image update.

---

## 0.12
<small>2026-04-09</small>

**New features**

- SQL query interface via `msgvault query`. Run arbitrary SQL against DuckDB over Parquet with `--format json|csv|table`. See [SQL Queries](/usage/querying/).
- Microsoft 365 OAuth2 support via `msgvault add-o365` for Outlook.com and organizational accounts. Auto-detects personal vs. org IMAP hosts.
- Text message import: `import-whatsapp`, `import-imessage`, and `import-gvoice` for WhatsApp, iMessage, and Google Voice. See [Text Messages](/usage/text-messages/).
- TUI text mode: press `m` to toggle between Email and Texts for browsing imported text conversations.
- `--after` and `--before` date filters for `sync-full` with IMAP accounts.
- CC and BCC recipients exposed in the message API responses.
- Claude Code skill for querying the archive via SQL views.

**Improvements**

- `delete-staged` now supports IMAP accounts (uses `UID STORE \Deleted` + `UID EXPUNGE`).
- Analytics cache is automatically rebuilt after write operations (sync, import, delete-staged) so stats stay current.
- `msgvault query` auto-rebuilds a stale cache before executing SQL.
- Improved archive query views and text-message search support.

**Bug fixes**

- `from:domain.com` search now matches domain patterns automatically for common TLDs. Uncommon TLDs still require the explicit `@` prefix (`from:@brand.pizza`).
- Wait for the IMAP server greeting before authenticating, fixing `unexpected EOF` errors with OAuth proxies.
- Fix label name conflict handling when ensuring Gmail labels (use `ON CONFLICT` upsert).
- Open the MCP database in read-only mode to prevent concurrent session hangs when multiple AI sessions query the archive.

**Acknowledgements**

- Thanks to [Matthew C Roberts](https://github.com/YourEconProf) for Microsoft 365 OAuth2 IMAP support.
- Thanks to [dominic](https://github.com/DominicHolmes) for adding IMAP support to `delete-staged`.
- Thanks to [danshapiro](https://github.com/danshapiro) for exposing CC and BCC recipients in message API responses.
- Thanks to [arunim1](https://github.com/arunim1) for adding `--after` and `--before` date filtering to IMAP sync.
- Thanks to [hansn74](https://github.com/hansn74) for the `from:domain.com` domain-pattern search fix.
- Thanks to [Shantanu Singh](https://github.com/shntnu) for Nix dev-shell setup improvements.
- Thanks to [Wes McKinney](https://github.com/wesm) for the SQL query interface, Claude Code skill, text-message imports, cache rebuilds after writes, MCP read-only database mode, IMAP greeting handling, and label-conflict fix.

---

## 0.11
<small>2026-03-24</small>

**New features**

- Support multiple Google OAuth apps for Google Workspace organizations.
- Add `source_conversation_id` to `search` and `show-message` JSON output.

**Improvements**

- Show masked IMAP passwords with `*` while typing during account setup.
- Better protect local data and cache handling when SQLite state is corrupted or analytics cache data is empty.

**Bug fixes**

- Fix IMAP host parsing for IPv6 addresses in `add-imap`.
- Improve IMAP compatibility by removing `ESEARCH RETURN (ALL)` for IMAP4rev1 servers.

**Acknowledgements**

- Thanks to [Rob Elkin](https://github.com/robelkin) for protecting local data paths when SQLite state is corrupted or analytics cache data is empty.
- Thanks to [Jason Kuhrt](https://github.com/jasonkuhrt) for exposing `source_conversation_id` in CLI JSON output.
- Thanks to [Alexander Mangel](https://github.com/Cygnusfear) for improving IMAP4rev1 compatibility by removing `ESEARCH RETURN (ALL)`.
- Thanks to [endolith](https://github.com/endolith) for fixing IPv6 host parsing in `add-imap`.
- Thanks to [Wes McKinney](https://github.com/wesm) for multiple Google OAuth app support and masked IMAP password entry.

---

## 0.10
<small>2026-03-15</small>

**New features**

- IMAP account support via `add-imap` command for syncing mail from any standard IMAP server.
- Remote TUI support: `msgvault tui` can connect to a remote server when `[remote]` is configured.
- `--account` flag on `search` to limit results to a specific account.

**Improvements**

- Auto-discover Apple Mail accounts during `import-emlx` by reading macOS `Accounts4.sqlite`.
- Shorten overly long MIME parse error messages to keep terminal output readable.

**Bug fixes**

- Fix Apple Mail V10 import to discover `.emlx` files in partition subdirectories.
- Prevent sync re-authentication from mixing tokens between accounts by adding `login_hint` and post-auth email validation.

**Acknowledgements**

- Thanks to [Ben Labaschin](https://github.com/EconoBen) for TUI remote-server support.
- Thanks to [David GG](https://github.com/davidggphy) for adding the `--account` search flag.
- Thanks to [Wes McKinney](https://github.com/wesm) for IMAP account support, Apple Mail V10 import fixes and account auto-discovery, MIME parse error truncation, and sync re-auth token isolation.

---

## 0.9
<small>2026-02-26</small>

**New features**

- `create-subset` command to generate smaller subset databases for testing or sharing.
- `remove-account` command to delete an account and all its local data.

**Improvements**

- Support modern Apple Mail V10 directory layouts during `import-emlx`.
- Handle expired or revoked OAuth tokens with automatic re-authentication and a `--force` flag.

**Bug fixes**

- Fix a foreign key constraint failure during message ingest.

**Acknowledgements**

- Thanks to [Hugh Brown](https://github.com/hughdbrown) for expired/revoked OAuth-token recovery with automatic re-authentication and `--force`.
- Thanks to [Hugh Brown](https://github.com/hughdbrown) for query-layer refactoring that reduced duplication between DuckDB and SQLite paths.
- Thanks to [Wes McKinney](https://github.com/wesm) for `create-subset`, `remove-account`, Apple Mail V10 layout support, and the ingest foreign-key fix.

---

## 0.8
<small>2026-02-24</small>

**New features**

- `import-mbox` command to import local MBOX archives.
- `import-emlx` command to import Apple Mail `.emlx` exports.
- MCP `stage_deletion` tool for Claude-assisted staged email cleanup.
- NAS/Docker deployment support with updated Compose templates.

**Improvements**

- Installation instructions for conda-forge and additional package managers.

**Bug fixes**

- Fix TUI label search and aggregate search behavior.

**Acknowledgements**

- Thanks to [Riccardo Iaconelli](https://github.com/ruphy) for the local mail import commands, `import-mbox` and `import-emlx`.
- Thanks to [Rob Elkin](https://github.com/robelkin) for the `stage_deletion` MCP tool.
- Thanks to [Ben Labaschin](https://github.com/EconoBen) for NAS and Docker deployment support.
- Thanks to [Pavel Zwerschke](https://github.com/pavelzw) for additional package-manager installation instructions.
- Thanks to [bchoor](https://github.com/bchoor) for store time-parsing unit tests.
- Thanks to [Wes McKinney](https://github.com/wesm) for TUI label and aggregate search fixes and Docker Compose template cleanup.

---

## 0.7
<small>2026-02-09</small>

**New features**

- HTTP API server with daemon mode and scheduled background syncs.
- Account filters for MCP `search`, `list`, and `aggregate` tools.
- Hide-deleted message filter with a revamped filter modal in the TUI.
- Gmail thread ID support in query results.
- Nix flake for reproducible builds.

**Improvements**

- Optimize incremental sync to reduce sync time.
- Harden cache validation and handling.

**Bug fixes**

- Fix CPU pinning behavior during batch deletion.
- Fix batch deletion terminal UI workflow issues.

**Acknowledgements**

- Thanks to [Ben Labaschin](https://github.com/EconoBen) for the HTTP API server, daemon mode, and scheduled sync foundation.
- Thanks to [Rob Elkin](https://github.com/robelkin) for account filters on MCP search, list, and aggregate tools.
- Thanks to [Ben Lovell](https://github.com/socksy) for the initial Nix flake and automated vendor-hash maintenance.
- Thanks to [Wes McKinney](https://github.com/wesm) for optimizing incremental sync, hardening cache handling, adding Gmail thread IDs to query results, improving batch-deletion UX, and adding the TUI hide-deleted filter.

---

## 0.6
<small>2026-02-05</small>

**New features**

- Secure file permissions with Windows DACL support.
- Windows update support with `.zip` archives and `.exe` binaries.
- `--home` CLI flag to set the base directory for archives.
- FTS5 full-text search index built and updated during sync, with automatic backfill for existing databases.

**Improvements**

- Strip surrounding quotes from CLI paths for Windows CMD compatibility.
- Suggest running `repair-encoding` when encoding errors are detected during sync.

**Bug fixes**

- Fix TUI search and navigation issues (pagination, scrolling, stats, zero-result handling).
- Fix silent error handling in encoding repair.
- Fix command-injection risk when launching OAuth browser.
- Fix Windows TOML parsing error hints for backslashes.
- Preserve cursor position when scrolling page up/down in the message list.
- Fix invalid UTF-8 handling during sync to prevent failures.

**Acknowledgements**

- Thanks to [Hugh Brown](https://github.com/hughdbrown) for cross-platform secure file permissions with Windows DACL support.
- Thanks to [Hugh Brown](https://github.com/hughdbrown) for several security and reliability fixes, including OAuth browser command-injection hardening, attachment path-traversal fixes, MCP bounds checks, panic handling, and encoding repair error handling.
- Thanks to [Wes McKinney](https://github.com/wesm) for FTS5 indexing and backfill, Windows update/build support, `--home`, Windows path diagnostics, invalid UTF-8 handling, and TUI search/navigation fixes.

---

## 0.5
<small>2026-02-04</small>

**New features**

- `export-attachment` and `export-attachments` CLI commands.
- Windows support with installer, config path fixes, and `--config` flag.
- MCP attachment support with embedded resources and `export_attachment` tool.
- Account management CLI with `add`, `list`, and `update` commands.

**Improvements**

- Improve Windows installer, remove `sqlite_scanner` dependency, and harden test reliability.

**Bug fixes**

- Fix cache consistency after deletions.
- Fix incremental export losing junction table data.
- Fix SQL injection vulnerability in query handling.
- Fix MIME date parsing issues.
- Fix path traversal risk in attachment export, including symlink traversal.
- Fix non-functional `sync-full` limit argument.
- Fix DuckDB type handling errors.
- Prevent crash when rethrowing panics during export.
- Add missing bounds checks in MCP handlers.

**Acknowledgements**

- Thanks to [Ethan Byrd](https://github.com/etbyrd) for account CLI updates and the `sync-full --limit` fix.
- Thanks to [Hugh Brown](https://github.com/hughdbrown) for security fixes across SQL query handling, attachment path traversal, symlink traversal, MIME date parsing, and panic handling.
- Thanks to [Rob Elkin](https://github.com/robelkin) for fixing cache consistency after deletions.
- Thanks to [Wes McKinney](https://github.com/wesm) for Windows installer/config support, attachment export commands, MCP attachment resources, `export_attachment`, DuckDB type fixes, MIME date fixes, and incremental export fixes.

---

## 0.4
<small>2026-02-03</small>

**New features**

- `--list` flag on `delete-staged` to preview staged deletions before executing.

**Improvements**

- Replace broken `--headless` device flow with clearer setup instructions.

---

## 0.3
<small>2026-02-02</small>

**New features**

- `sync` and `sync-full` run without arguments to sync all accounts.

**Improvements**

- Tighten private file permissions to `600` for better local data security.
- Improve deletion progress display and recovery behavior.

**Bug fixes**

- Fix deletion issues around scope escalation and checkpoint recovery.
- Fix missing `rows.Err()` handling when batching participants.

**Acknowledgements**

- Thanks to [Hugh Brown](https://github.com/hughdbrown) for tightening private-resource file permissions.
- Thanks to [Ethan Byrd](https://github.com/etbyrd) for fixing missing `rows.Err()` handling in participant batching.
- Thanks to [Matt Galligan](https://github.com/galligan) for fixing broken README documentation links.
- Thanks to [Wes McKinney](https://github.com/wesm) for making `sync` and `sync-full` run across all accounts, and for deletion workflow fixes around scope escalation, checkpoint recovery, and progress reporting.

---

## 0.2
<small>2026-02-02</small>

**Improvements**

- Use MCP server for chat instead of the built-in chat command.
- Reduce memory use during string joins for better performance.

**Acknowledgements**

- Thanks to [Hugh Brown](https://github.com/hughdbrown) for the memory-efficient string-join improvement.
- Thanks to [Wes McKinney](https://github.com/wesm) for replacing the built-in chat command with the MCP server.

---

## 0.1
<small>2026-02-02</small>

**New features**

- MCP server for AI-assisted email exploration.

**Improvements**

- Improve Linux compatibility by building against Ubuntu 20.04 (glibc 2.31).
- Rename `sync-incremental` command to `sync` for a simpler workflow.
- Show full version tag in the TUI title bar.
- Show helpful OAuth setup instructions when `client_secrets` is missing.

**Bug fixes**

- Fix TUI update notification to show commit and date info in release builds.
- Fix incorrect elapsed time reporting during sync.
- Fix recipient name filters to include BCC recipients.

**Acknowledgements**

- Thanks to [Ethan Byrd](https://github.com/etbyrd) for fixing recipient-name filters to include BCC recipients.
- Thanks to [Wes McKinney](https://github.com/wesm) for Linux build compatibility, the `sync` rename, OAuth setup guidance, TUI release/update polish, and sync elapsed-time fixes.

---

## 0.0
<small>2026-02-01</small>

Initial public release.
