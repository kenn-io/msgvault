# msgvault

[![Go 1.25+](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Docs](https://img.shields.io/badge/Docs-msgvault.io-blue)](https://msgvault.io)
[![Discord](https://img.shields.io/badge/Discord-Join-5865F2?logo=discord&logoColor=white)](https://discord.gg/fDnmxB8Wkq)

[Documentation](https://msgvault.io) · [Setup Guide](https://msgvault.io/guides/oauth-setup/) · [Interactive TUI](https://msgvault.io/usage/tui/)

> **Alpha software.** APIs, storage format, and CLI flags may change without notice. Back up your data.

Archive a lifetime of email. Analytics and search in milliseconds, entirely offline.

## Why msgvault?

Your messages are yours. Decades of correspondence, attachments, and history shouldn't be locked behind a web interface or an API. By default, msgvault downloads a complete local copy and then everything runs offline. Search, analytics, and the MCP server all work against your msgvault archive with no mailbox network access required. If you configure a remote deployment, the archive lives on your own server rather than a hosted msgvault service.

Currently supports Gmail, Google Calendar, Microsoft Teams, and IMAP sync, plus
offline imports from MBOX exports, Apple Mail (.emlx) directories, PST archives,
and common chat/text export formats.

## Features

- **Full Gmail backup**: raw MIME, attachments, labels, and metadata
- **Google Calendar sync**: archive events, organizers, and attendees; searchable alongside email
- **Microsoft Teams sync**: archive delegated Graph chats, channels, replies, and inline media with `message_type = teams`
- **IMAP sync**: archive mail from any standard IMAP server
- **Incremental backup snapshots**: verifiable `msgvault backup` repositories for the SQLite archive and attachments
- **MBOX / Apple Mail / PST import**: import email from local export formats
- **Interactive TUI**: drill-down analytics over your entire message history, powered by DuckDB over Parquet — connects to a remote `msgvault serve` instance or runs locally
- **Full-text search**: FTS5 with Gmail-like query syntax (`from:`, `has:attachment`, date ranges)
- **MCP server**: access your full archive at the speed of thought in Claude Desktop and other MCP-capable AI agents
- **DuckDB analytics**: millisecond aggregate queries across hundreds of thousands of messages in the TUI, CLI, and MCP server
- **Incremental sync**: History API picks up only new and changed messages
- **Multi-account**: archive several Gmail and IMAP accounts in a single database
- **Resumable**: interrupted syncs resume from the last checkpoint
- **Content-addressed attachments**: deduplicated by SHA-256

## Installation

**macOS / Linux:**
```bash
curl -fsSL https://msgvault.io/install.sh | bash
```

**macOS via Homebrew**
```bash
brew install msgvault
```

**Windows (PowerShell):**
```powershell
powershell -ExecutionPolicy ByPass -c "irm https://msgvault.io/install.ps1 | iex"
```

The installer detects your OS and architecture, downloads the latest release from [GitHub Releases](https://github.com/kenn-io/msgvault/releases), verifies the SHA-256 checksum, and installs the binary. You can review the script ([bash](https://msgvault.io/install.sh), [PowerShell](https://msgvault.io/install.ps1)) before running, or download a release binary directly from GitHub.

To build from source instead (requires **Go 1.25+** and a C/C++ compiler for CGO and to statically link DuckDB):

```bash
git clone https://github.com/kenn-io/msgvault.git
cd msgvault
make install
```

**Conda-Forge:**

You can install msgvault [from conda-forge](https://prefix.dev/channels/conda-forge/packages/msgvault) using Pixi or Conda:

```bash
pixi global install msgvault
conda install -c conda-forge msgvault
```

## Quick Start

> **Prerequisites:** You need a Google Cloud OAuth credential before adding an account.
> Follow the **[OAuth Setup Guide](https://msgvault.io/guides/oauth-setup/)** to create one (~5 minutes).

```bash
msgvault init-db
msgvault add-account you@gmail.com          # opens browser for OAuth
msgvault sync-full you@gmail.com --limit 100
msgvault tui
```

## Commands

| Command | Description |
|---------|-------------|
| `init-db` | Create the database |
| `add-account EMAIL` | Authorize a Gmail account (use `--headless` for servers) or add an IMAP account |
| `sync-full EMAIL` | Full sync (`--limit N`, `--after`/`--before` for date ranges) |
| `sync EMAIL` | Sync only new/changed messages |
| `add-calendar EMAIL` | Authorize read-only Google Calendar access and register calendars |
| `sync-calendar NAME\|EMAIL` | Sync Google Calendar events (full first run, then incremental) |
| `add-teams EMAIL` | Authorize delegated Microsoft Graph access for Teams |
| `sync-teams EMAIL` | Sync Microsoft Teams chats and channels |
| `backup` | Initialize, create, list, verify, and restore backup snapshots |
| `pack-attachments` | Migrate all eligible loose attachment files into immutable packs |
| `repack-attachments` | Reclaim dead space from sparse attachment packs |
| `unpack-attachments` | Restore packed attachments to loose files for downgrade or recovery |
| `tui` | Launch the interactive TUI (`--account` to filter, `--local` to force the local daemon) |
| `search QUERY` | Search messages (`--account` and `--message-type` to filter, `--json` for machine output) |
| `show-message ID` | View full message details (`--json` for machine output) |
| `mcp` | Start the MCP server for AI assistant integration |
| `serve` | Run the API/scheduler or manage the background daemon (`start`, `status`, `stop`, `restart`) |
| `stats` | Show archive statistics |
| `list-accounts` | List synced email accounts |
| `verify EMAIL` | Verify archive integrity against Gmail |
| `export-eml` | Export a message as `.eml` |
| `import-mbox` | Import email from an MBOX export or `.zip` of MBOX files |
| `import-emlx` | Import email from an Apple Mail directory tree |
| `build-cache` | Rebuild the Parquet analytics cache |
| `update` | Update msgvault to the latest version |
| `setup` | Interactive first-run configuration wizard |
| `repair-encoding` | Fix UTF-8 encoding issues |
| `list-senders` / `list-domains` / `list-labels` | Explore metadata |

See the [CLI Reference](https://msgvault.io/cli-reference/) for full details.

## Vector Search

msgvault can search your archive semantically using vector embeddings in addition to the default FTS5 keyword search. Point it at a self-hosted OpenAI-compatible embedding endpoint (Ollama, llama.cpp, LM Studio) and three surfaces accept either pure semantic search or BM25+vector fused via Reciprocal Rank Fusion:

- **CLI:** `msgvault search "..." --mode vector` or `--mode hybrid`
- **HTTP:** `GET /api/v1/search?q=...&mode=vector` or `mode=hybrid`
- **MCP:** the `search_messages` tool with a `mode` argument set to `vector` or `hybrid`

A separate MCP tool, `find_similar_messages`, returns nearest neighbors for a seed message. See the [Vector Search guide](https://msgvault.io/usage/vector-search/) for setup, backfill, and troubleshooting.

> **Archive writes are daemon-owned.** CLI writer commands such as `msgvault sync-full`, `msgvault embeddings build`, `msgvault repair-encoding`, and `msgvault rebuild-fts` send their work to the configured remote server or local background daemon. The daemon serializes archive mutations and streams progress back to your terminal, so normal CLI ergonomics stay the same without opening a second SQLite writer process.

Large archives can scope an embedding generation with `[vector.embed.scope] message_types = ["sms", "mms"]`. Scoped vector and hybrid searches must include a matching `message_type` filter so a partial index is never used as if it covered the whole archive.

## Importing from MBOX or Apple Mail

Import email from providers that offer MBOX exports or from a local Apple Mail data directory:

```bash
msgvault init-db
msgvault import-mbox you@example.com /path/to/export.mbox
msgvault import-mbox you@example.com /path/to/export.zip   # zip of MBOX files
msgvault import-emlx                                        # auto-discover Apple Mail accounts
msgvault import-emlx you@example.com ~/Library/Mail/V10     # explicit path
```

### Import SMS Backup & Restore for Android (`synctech-sms`)

Msgvault can import XML backups produced by **[SMS Backup & Restore](https://play.google.com/store/apps/details?id=com.riteshsahu.SMSBackupRestore)** by SyncTech Pty Ltd. The Android app is listed in Google Play as `SMS Backup & Restore` and uses package `com.riteshsahu.SMSBackupRestore`; the Pro app uses `com.riteshsahu.SMSBackupRestorePro`.

Install the Android app on the phone that owns the messages, then configure a scheduled backup:

1. Open SMS Backup & Restore.
2. Choose **Set Up A Backup**.
3. Include **Messages**, **MMS media**, and **Call logs**.
4. Choose **Google Drive** as the backup location.
5. Use a dedicated Drive folder for Msgvault imports.
6. Choose **Incremental** backups for daily operation. Full and archive backups also import correctly, but incremental backups keep each daily upload smaller.
7. Schedule the Android backup for a quiet time such as `4:00 AM`.
8. Leave backup encryption off. Msgvault does not import encrypted Pro backups.

Configure Msgvault to read that Drive folder:

```bash
msgvault add-synctech-sms-drive pixel \
  --owner-phone +15550000001 \
  --folder-id 1exampleDriveFolderId \
  --google-account you@gmail.com \
  --schedule "30 4 * * *"
```

The folder ID is the final path segment in a Google Drive folder URL. For example, in `https://drive.google.com/drive/folders/1exampleDriveFolderId`, the folder ID is `1exampleDriveFolderId`.

Run the source immediately:

```bash
msgvault sync-synctech-sms pixel
```

You can also import local files, folders, or unencrypted ZIP backups:

```bash
msgvault import-synctech-sms --owner-phone +15550000001 ~/Downloads/sms-backup.xml
msgvault import-synctech-sms --owner-phone +15550000001 ~/Downloads/sms-backups/
msgvault import-synctech-sms --owner-phone +15550000001 ~/Downloads/sms-backup.zip
```

SMS and MMS messages appear in text-message search. Call logs are imported as searchable call records with `message_type = synctech_sms_call`, so missed and outgoing calls do not mix into normal text threads.

### Google Calendar

Archive your calendars alongside email. Events become searchable (full-text and, when vector search is enabled, semantic) and join the same contact graph as your email, so organizers and attendees dedupe with the people you email.

```bash
# Authorize read-only Calendar access and register your calendars.
# If the account already has Gmail access, the consent screen asks for
# Gmail + Calendar together — keep BOTH checked so Gmail access is kept.
msgvault add-calendar you@gmail.com

# First run does a full sync; later runs are incremental.
msgvault sync-calendar you@gmail.com
msgvault sync-calendar you@gmail.com --full          # force a full re-sync
msgvault sync-calendar you@gmail.com --all-calendars # include subscribed/holiday calendars

# Find events
msgvault search "standup" --message-type calendar_event
```

By default only calendars you own or can write to are synced (add `--all-calendars` for subscribed and holiday calendars). Calendar sync is read-only and never modifies your Google Calendar. Cancelled events are kept (marked cancelled), not deleted, so your archive preserves that a meeting once existed. The Calendar API must be enabled on your Google Cloud OAuth project.

Msgvault stores Google OAuth refresh tokens under the Msgvault home directory with file permissions restricted to the current user. Tokens and client secrets are not written into `config.toml`, logs, README examples, or exported fixtures.

### Microsoft Teams

Archive Microsoft Teams chats and channels through delegated Microsoft Graph
sync. Teams uses the `[microsoft]` OAuth app config but stores a separate
`teams_<email>.json` token from Outlook/IMAP OAuth.

```bash
msgvault add-teams user@example.com
msgvault sync-teams user@example.com
msgvault search "incident review" --message-type teams
```

See the [Microsoft Teams guide](https://msgvault.io/usage/teams/) for Graph
permissions, scheduling, channel sync behavior, and inline media backfill.

### Backup Snapshots

Create an append-only backup repository, take incremental snapshots, and verify
or restore them later:

```bash
msgvault backup init --repo ~/Backups/msgvault
msgvault backup create --repo ~/Backups/msgvault
msgvault backup verify --all --quick --repo ~/Backups/msgvault
msgvault backup restore --target ~/msgvault-restored --repo ~/Backups/msgvault
```

Set `repo` under `[backup]` in `config.toml` to omit `--repo` from every
command after `init`.

See the [Backup guide](https://msgvault.io/usage/backup/) for repository format,
secret-handling flags, restore proof, and operating recommendations.

### Packed Attachment Storage

Msgvault stores attachment bytes in sealed content-addressed packs to reduce
file-count overhead, especially on Windows and NAS filesystems. Existing loose
vaults remain readable and migrate gradually after successful ingest runs and
during daily maintenance; no startup migration is required. Run the following
once to migrate the complete eligible backlog immediately:

```bash
msgvault pack-attachments
```

Reads, exports, MCP, and backups work transparently with loose, packed, or mixed
storage. `repack-attachments` reclaims dead pack space and also runs
automatically as bounded maintenance. A restored backup intentionally contains
loose attachment files and can be packed again with the command above.

`unpack-attachments` is the downgrade escape hatch. It requires exclusive local
access because it deletes production pack files, so stop the local daemon first:

```bash
msgvault serve stop
msgvault unpack-attachments
```

## Configuration

All data lives in `~/.msgvault/` by default (override with `MSGVAULT_HOME`).

```toml
# ~/.msgvault/config.toml
[oauth]
client_secrets = "/path/to/client_secret.json"

[sync]
rate_limit_qps = 5
```

See the [Configuration Guide](https://msgvault.io/configuration/) for all options.

### Multiple OAuth Apps (Google Workspace)

Some Google Workspace organizations require OAuth apps within their org.
To use multiple OAuth apps, add named apps to `config.toml`:

```toml
[oauth]
client_secrets = "/path/to/default_secret.json"   # for personal Gmail

[oauth.apps.acme]
client_secrets = "/path/to/acme_workspace_secret.json"
```

Then specify the app when adding accounts:

```bash
msgvault add-account you@acme.com --oauth-app acme
msgvault add-account personal@gmail.com              # uses default
```

To switch an existing account to a different OAuth app:

```bash
msgvault add-account you@acme.com --oauth-app acme   # re-authorizes
```

### Google Service Accounts

Workspace admins can use a Google service account with domain-wide delegation instead of per-user OAuth tokens:

```toml
[oauth.apps.acme]
service_account_key = "/secure/path/service-account.json"
```

In Google Admin Console, authorize the service account client for `https://www.googleapis.com/auth/gmail.readonly` and `https://www.googleapis.com/auth/gmail.modify`. If you will archive Google Calendar, also authorize `https://www.googleapis.com/auth/calendar.readonly`. If you will run `delete-staged` with permanent deletion, also authorize `https://mail.google.com/`. Keep the key file owner-only, for example `chmod 600 /secure/path/service-account.json`.

```bash
msgvault add-account you@acme.com --oauth-app acme
msgvault sync-full you@acme.com
```

## MCP Server

msgvault includes an MCP server that lets AI assistants search, analyze, and read your archived messages. Connect it to Claude Desktop or any MCP-capable agent and query your full message history conversationally. See the [MCP documentation](https://msgvault.io/usage/chat/) for setup instructions.

## Daemon Mode (Local/Remote)

Run msgvault as a foreground server for scheduled syncs and remote access:

```bash
msgvault serve
```

For local CLI use, msgvault can also manage a background daemon:

```bash
msgvault serve start
msgvault serve status
msgvault serve stop
msgvault serve restart
```

Archive-access CLI commands use the HTTP API by default. If `[remote].url` is configured, the CLI talks to that remote server. Otherwise, it discovers or starts the local background daemon instead of opening the SQLite database itself. This keeps local and remote CLI behavior aligned and avoids repeated startup cost on large archives. Use `--local` to force the local daemon when a remote server is configured.

The server exposes its generated OpenAPI document at `/openapi.json` and interactive API docs at `/docs`.

Configure scheduled syncs in `config.toml`:

```toml
[[accounts]]
email = "you@gmail.com"
schedule = "0 2 * * *"   # 2am daily (cron)
enabled = true

[[gcal]]                  # scheduled Google Calendar sync
email = "you@gmail.com"
schedule = "0 */6 * * *" # every 6 hours
enabled = true

[server]
api_port = 8080
bind_addr = "0.0.0.0"
api_key = "your-secret-key"
daemon_idle_timeout = "20m" # background daemon idle timeout; "0s" disables
```

`daemon_idle_timeout` applies to lifecycle-managed background daemons started by `msgvault serve start` or auto-started by a CLI command. A foreground `msgvault serve` keeps running until you stop it. See the [Web Server reference](https://msgvault.io/api-server/) or `/openapi.json` on a running server for the HTTP API.

## Documentation

- [Setup Guide](https://msgvault.io/guides/oauth-setup/): OAuth, first sync, headless servers
- [Searching](https://msgvault.io/usage/searching/): query syntax and operators
- [Search ranking across backends](https://msgvault.io/architecture/search-ranking/): how result order differs between SQLite and PostgreSQL
- [PostgreSQL backend](https://msgvault.io/architecture/postgresql/): run msgvault on PostgreSQL with pgvector semantic/hybrid search
- [Interactive TUI](https://msgvault.io/usage/tui/): keybindings, views, deletion staging
- [CLI Reference](https://msgvault.io/cli-reference/): all commands and flags
- [Multi-Account](https://msgvault.io/usage/multi-account/): managing multiple Gmail accounts
- [Configuration](https://msgvault.io/configuration/): config file and environment variables
- [Architecture](https://msgvault.io/architecture/storage/): SQLite, Parquet, and attachment storage
- [MCP Server](https://msgvault.io/usage/chat/): AI assistant integration
- [Troubleshooting](https://msgvault.io/troubleshooting/): common issues and fixes
- [Development](https://msgvault.io/development/): contributing, testing, building

## Community

Join the [msgvault Discord](https://discord.gg/fDnmxB8Wkq) to ask questions, share feedback, report issues, and connect with other users.

## Development

```bash
git clone https://github.com/kenn-io/msgvault.git
cd msgvault
make install-hooks  # install pre-commit hook (requires prek)
make test           # run tests
make lint           # run linter (auto-fix)
make install        # build and install
```

Pre-commit hooks are managed by [prek](https://prek.j178.dev/) (`brew install prek`).

## License

MIT. See [LICENSE](LICENSE) for details.
