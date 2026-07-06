---
title: Architecture Overview
description: Package structure and data flow.
---

msgvault syncs your Gmail, Google Calendar, IMAP, Microsoft 365 mail, and Microsoft Teams accounts to a local SQLite database by default and can import local PST/MBOX archives, Apple Mail exports, and chats/texts from WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup & Restore. PostgreSQL is available as an opt-in backend for new archives. Keyword search, analytics, the TUI, and the MCP server run against the configured archive database, Parquet metadata exports where available, and local attachment files. Optional vector search calls the embedding endpoint configured in `[vector.embeddings]` to build/query semantic vectors, then stores those vectors in `vectors.db` on SQLite or pgvector tables on PostgreSQL.

<img src="/assets/static/how-it-works.svg" alt="msgvault architecture: Gmail API syncs to SQLite, then offline Parquet analytics, FTS5 search, TUI, and MCP Server" style="width: 100%; max-width: 960px; margin: 1.5rem auto; display: block;" />

## Package Structure

```
msgvault/
├── cmd/msgvault/            # CLI entrypoint
│   └── cmd/                 # Cobra commands
├── internal/                # Core packages
│   ├── tui/                 # Bubble Tea TUI
│   ├── query/               # DuckDB/SQL query engines
│   ├── store/               # SQLite/PostgreSQL database access
│   ├── backupapp/           # msgvault's App seam for the kit backup engine
│   ├── deletion/            # Deletion staging and manifest
│   ├── gmail/               # Gmail API client
│   ├── gcal/                # Google Calendar API client
│   ├── calsync/             # Google Calendar sync orchestration
│   ├── sync/                # Sync orchestration
│   ├── imap/                # IMAP client (go-imap/v2)
│   ├── importer/            # Local email import orchestration
│   ├── mbox/                # MBOX format parser
│   ├── emlx/                # Apple Mail .emlx parser
│   ├── pst/                 # Outlook PST reader
│   ├── applemail/           # Apple Mail account discovery
│   ├── daemonclient/        # HTTP/OpenAPI client for local and remote daemons
│   ├── vector/              # Local vector index, embedding worker, hybrid search
│   ├── whatsapp/            # WhatsApp backup import
│   ├── imessage/            # iMessage import
│   ├── gvoice/              # Google Voice Takeout import
│   ├── fbmessenger/         # Facebook Messenger DYI import
│   ├── synctechsms/         # SMS Backup & Restore import
│   ├── microsoft/           # Microsoft 365 OAuth
│   ├── teams/               # Microsoft Teams Graph ingestion
│   ├── oauth/               # OAuth2 flows (browser + device)
│   └── mime/                # MIME parsing
├── go.mod
└── Makefile
```

## Key Packages

| Package | Responsibility |
|---|---|
| `cmd/` | Cobra CLI commands, config loading |
| `internal/store` | SQLite and PostgreSQL database operations, schema management |
| `internal/backupapp` | msgvault's `backup.App` implementation (stats, layout, exclusions) over `go.kenn.io/kit`'s backup/pack engine |
| `internal/sync` | Sync orchestration, MIME parsing, checkpoint management |
| `internal/gcal` / `internal/calsync` | Google Calendar API client and event sync |
| `internal/imap` | IMAP client, connection management, credential storage |
| `internal/importer` | Local email import orchestration and message ingestion |
| `internal/mbox` | MBOX format reader (mboxo/mboxrd) |
| `internal/emlx` | Apple Mail .emlx parser and mailbox discovery |
| `internal/pst` | Outlook PST archive reader |
| `internal/applemail` | Apple Mail account discovery via `Accounts4.sqlite` |
| `internal/daemonclient` | HTTP/OpenAPI client, CLI facades, and `query.Engine` adapter for local and remote daemons |
| `internal/vector` | Vector index backend, embedding client/worker, and semantic/hybrid search |
| `internal/gmail` | Gmail API client with token bucket rate limiting |
| `internal/oauth` | OAuth2 browser and device authorization flows |
| `internal/query` | DuckDB engine over Parquet files, SQL fallback, and PostgreSQL query paths |
| `internal/tui` | Bubble Tea model, lipgloss-styled views |
| `internal/deletion` | Deletion staging, manifest generation |
| `internal/whatsapp` | WhatsApp backup parsing and import |
| `internal/imessage` | iMessage database import |
| `internal/gvoice` | Google Voice Takeout parsing and import |
| `internal/fbmessenger` | Facebook Messenger Download Your Information parsing and import |
| `internal/synctechsms` | SMS Backup & Restore XML/ZIP parsing and Drive source sync |
| `internal/microsoft` | Microsoft 365 OAuth flow |
| `internal/teams` | Microsoft Teams Graph client mapping, sync cursors, and importer |
| `internal/mime` | MIME message parsing, charset detection |

## Design Decisions

- **Local-first by design**: The Gmail API and IMAP servers are only contacted during explicit `sync-full`, `sync`, and deletion commands. Keyword search, analytics, TUI views, and ordinary MCP reads run against local data with no mailbox network access. Optional vector search additionally calls only the embedding endpoint you configure, so a local/self-hosted endpoint keeps semantic search on your own machine or network.
- **SQLite by default, PostgreSQL opt-in**: SQLite is the default system of record. PostgreSQL can be selected with `[data].database_url` for new archives that should live in a server database.
- **DuckDB + Parquet for default analytics**: On SQLite archives, the TUI runs an embedded DuckDB engine over Parquet metadata exports, delivering aggregate queries hundreds of times faster than SQLite JOINs. The entire analytics cache for hundreds of thousands of messages fits in a few megabytes, making drill-down and re-aggregation feel instant. PostgreSQL archives currently use live SQL for aggregate views.
- **Content-addressed attachments**: Deduplicated by SHA-256 hash, stored on disk.
- **Resumable sync**: Checkpoints allow interrupted syncs to resume without re-downloading.
- **Token bucket rate limiting**: Respects Gmail API quotas without manual throttling.
