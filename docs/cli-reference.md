---
title: CLI Reference
description: Complete command reference for all msgvault commands.
---

## Global Flags

| Flag | Description |
|---|---|
| `--config` | Path to config file (default: `~/.msgvault/config.toml`) |
| `--home` | Home directory for all data (overrides `MSGVAULT_HOME`) |
| `-v`, `--verbose` | Verbose output (implies `--log-level=debug`) |
| `--local` | Force local database mode when `[remote]` is configured |
| `--log-file <path>` | Override log file path (default: `<data_dir>/logs/msgvault-YYYY-MM-DD.log`) |
| `--log-level <level>` | Log level: `debug`, `info`, `warn`, `error` (default: `info`) |
| `--no-log-file` | Disable file logging for this run (stderr output stays on) |
| `--log-sql` | Log every SQL query at info level (verbose, for debugging) |
| `--log-sql-slow-ms <ms>` | Slow query threshold in ms (default: 100; 0 uses built-in default) |
| `--help` | Show help |

---

## init-db

Initialize the SQLite database.

```bash
msgvault init-db
```

---

## add-account

Add a Gmail account and authorize via OAuth.

```bash
msgvault add-account <email>
msgvault add-account <email> --headless
msgvault add-account <email> --oauth-app <name>
```

| Flag | Description |
|---|---|
| `--headless` | Show instructions for headless server setup |
| `--oauth-app` | Use a named OAuth app from `[oauth.apps.<name>]` in config |
| `--force` | Delete existing token and re-authorize |
| `--display-name` | Set a display name for the account |
| `--no-default-identity` | Do not auto-confirm the email address as this account's "me" identity |

If `[oauth].service_account_key` or `[oauth.apps.<name>].service_account_key` is configured, `add-account` authorizes via Google service account domain-wide delegation instead of browser OAuth. Service-account accounts do not use `--headless` or `--force`.

---

## add-imap

Add an IMAP account for syncing mail from any standard IMAP server.

```bash
msgvault add-imap --host <hostname> --username <email>
```

The command prompts interactively for your password (never accepted as a flag to avoid shell history exposure). For scripting or Docker, set `MSGVAULT_IMAP_PASSWORD` or pipe via stdin:

```bash
MSGVAULT_IMAP_PASSWORD="..." msgvault add-imap --host imap.example.com --username user@example.com
# or
echo "$PASS" | msgvault add-imap --host imap.example.com --username user@example.com
```

It tests the connection before saving credentials.

| Flag | Default | Description |
|---|---|---|
| `--host` | (required) | IMAP server hostname |
| `--username` | (required) | IMAP username or email address |
| `--port` | `993` | IMAP server port (993 for TLS, 143 for STARTTLS/plain) |
| `--starttls` | `false` | Use STARTTLS instead of implicit TLS |
| `--no-tls` | `false` | Disable TLS entirely (plaintext, not recommended) |
| `--no-default-identity` | `false` | Do not auto-confirm the username as this account's "me" identity |

Credentials are stored in `tokens/imap_<hash>.json` with restricted file permissions (0600). Use app-specific passwords when your provider supports them.

After adding an account, sync it with `msgvault sync-full`. IMAP accounts use the same `sync` and `sync-full` commands as Gmail. See [Setup Guide](/setup/#add-an-imap-account) for a walkthrough.

---

## add-o365

Add a Microsoft 365 or Outlook.com account via OAuth2 with XOAUTH2 IMAP authentication.

```bash
msgvault add-o365 <email>
```

The command opens your browser for Microsoft OAuth consent, then configures IMAP with XOAUTH2 automatically. The correct IMAP host is auto-detected: `outlook.office.com` for personal accounts (hotmail.com, outlook.com, live.com, msn.com) and `outlook.office365.com` for organizational accounts.

Requires a `[microsoft]` section with `client_id` in `config.toml`. See the [OAuth Setup guide](/guides/oauth-setup/#microsoft-365-outlook--hotmail) for Azure AD app registration.

| Flag | Default | Description |
|---|---|---|
| `--tenant` | `common` | Azure AD tenant ID (restricts which accounts can authorize) |
| `--no-default-identity` | `false` | Do not auto-confirm the email address as this account's "me" identity |

After adding the account, sync it with `msgvault sync-full`.

---

## sync-full

Download all messages from a Gmail or IMAP account. When called without an email argument, syncs all configured syncable accounts.

```bash
msgvault sync-full [email] [flags]
```

| Flag | Description |
|---|---|
| `--limit N` | Maximum messages to download |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--query` | Gmail search query filter |
| `--noresume` | Ignore checkpoints, start fresh |
| `--verbose` | Detailed progress output |

---

## sync

Sync new and changed messages. Gmail accounts use the Gmail History API; IMAP accounts perform a mailbox scan and skip messages already in the database. When called without an email argument, syncs all accounts that have completed an initial full sync.

```bash
msgvault sync [email]
```

---

## import-mbox

Import a local MBOX archive into msgvault.

```bash
msgvault import-mbox <identifier> <export-file>
```

The export file may be a plain mbox file (any extension) or a `.zip` containing one or more `.mbox`/`.mbx` files.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `mbox` | Source type recorded in database (e.g., `hey` for HEY.com) |
| `--label` | — | Label(s) to apply to imported messages (repeatable, or comma-separated) |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

See [Importing Local Email](/usage/importing/) for usage examples.

---

## import-emlx

Import Apple Mail `.emlx` files into msgvault. Can auto-discover accounts from macOS `Accounts4.sqlite` or accept explicit arguments.

```bash
# Auto-discover accounts (reads ~/Library/Accounts/Accounts4.sqlite)
msgvault import-emlx

# Specify mail directory
msgvault import-emlx <mail-dir>

# Legacy form: explicit identifier and directory
msgvault import-emlx <identifier> <mail-dir>
```

The mail directory should be an Apple Mail mailbox tree containing `.mbox` or `.imapmbox` directories, each with a `Messages/` subdirectory of `.emlx` files. You can also point directly at a single `.mbox` directory. Labels are derived from directory names.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `apple-mail` | Source type recorded in database |
| `--account` | — | Filter to specific account(s) during auto-discover (repeatable) |
| `--accounts-db` | — | Custom path to macOS `Accounts4.sqlite` |
| `--identifier` | — | Manual identifier when auto-discover is not suitable |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

See [Importing Local Email](/usage/importing/) for usage examples.

---

## import-pst

Import a Microsoft Outlook PST archive into msgvault.

```bash
msgvault import-pst <identifier> <pst-file>
```

The importer preserves PST folder structure as labels, imports email messages, and skips non-email PST items such as calendar entries, contacts, tasks, and notes.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `pst` | Source type recorded in database |
| `--skip-folder` | — | Folder name to skip, case-insensitive; repeat for multiple folders |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |

See [Importing Local Email](/usage/importing/) for usage examples.

---

## import-whatsapp

Import messages from a decrypted WhatsApp `msgstore.db` SQLite database.

```bash
msgvault import-whatsapp <msgstore.db> --phone <your-number>
```

The `--phone` flag is required and must be in E.164 format (e.g., `+447700900000`).

| Flag | Required | Description |
|---|---|---|
| `--phone` | Yes | Your phone number in E.164 format (must start with `+`) |
| `--contacts` | No | Path to contacts `.vcf` file for name resolution |
| `--media-dir` | No | Path to decrypted Media folder for attachments |
| `--limit` | No | Limit number of messages (for testing) |
| `--display-name` | No | Display name for the phone owner |
| `--no-default-identity` | No | Do not auto-confirm the phone number as this source's "me" identity |

See [Text Messages](/usage/text-messages/) for usage examples.

---

## import-imessage

Import messages from the local iMessage database on macOS. Requires Full Disk Access in System Settings.

```bash
msgvault import-imessage
```

Reads from `~/Library/Messages/chat.db` by default. This is a read-only operation.

| Flag | Default | Description |
|---|---|---|
| `--db-path` | `~/Library/Messages/chat.db` | Path to chat.db |
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--me` | — | Your phone/email for recipient tracking |
| `--contacts` | — | Path to contacts `.vcf` file for display-name backfill |

See [Text Messages](/usage/text-messages/) for usage examples.

---

## import-gvoice

Import texts, calls, and voicemails from a Google Voice Takeout export.

```bash
msgvault import-gvoice <takeout-voice-dir>
```

The directory must be the "Voice" folder from a Google Takeout export, containing `Calls/` and `Phones.vcf`.

| Flag | Default | Description |
|---|---|---|
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--no-default-identity` | `false` | Do not auto-confirm the phone number as this source's "me" identity |

See [Text Messages](/usage/text-messages/) for usage examples.

---

## import-messenger

Import Facebook Messenger conversations from a Download Your Information export.

```bash
msgvault import-messenger --me <you@facebook.messenger> <dyi-export-dir>
```

| Flag | Default | Description |
|---|---|---|
| `--me` | (required) | Your synthetic Messenger identifier, e.g. `test.user@facebook.messenger` |
| `--format` | `auto` | Export format: `auto`, `json`, `html`, or `both` |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |

See [Text Messages](/usage/text-messages/) for usage examples.

---

## import-synctech-sms

Import SMS Backup & Restore XML or ZIP backups.

```bash
msgvault import-synctech-sms <path> --owner-phone <your-number>
```

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--sms` | `true` | Import SMS records |
| `--mms` | `true` | Import MMS records |
| `--calls` | `true` | Import call logs |
| `--attachments` | `true` | Import MMS attachments |

See [Text Messages](/usage/text-messages/) for usage examples.

---

## add-synctech-sms-drive

Configure a Google Drive source for SMS Backup & Restore backups.

```bash
msgvault add-synctech-sms-drive <name> --owner-phone <number> --folder-id <id> --google-account <email>
```

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--folder-id` | (required) | Google Drive folder ID containing backups |
| `--google-account` | (required) | Google account used for Drive access |
| `--schedule` | `30 4 * * *` | Cron schedule used by `msgvault serve` |
| `--oauth-app` | — | Named Google OAuth app to use |

---

## sync-synctech-sms

Run one configured SMS Backup & Restore source immediately.

```bash
msgvault sync-synctech-sms <name>
```

---

## search

Search the archive with Gmail-like query syntax. Supports keyword (FTS5), semantic, and hybrid modes.

```bash
msgvault search <query> [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Maximum number of results (default: 50) |
| `--offset N` | Skip first N results (only valid for `--mode fts`) |
| `--json` | Output results as JSON |
| `--account` | Limit results to a specific account |
| `--collection` | Limit results to all member accounts of a collection |
| `--mode` | Search mode: `fts` (default), `vector`, or `hybrid`. `vector` and `hybrid` require vector search to be configured. |
| `--explain` | Include per-signal scores (RRF, BM25, vector) in the output. Only applies to `--mode vector` and `--mode hybrid`. |

`--mode vector` and `--mode hybrid` require at least one free-text term in the query (filter-only queries use `--mode fts`). They do not support pagination (`--offset` is rejected), so bump `--limit` to retrieve a larger candidate pool instead. See [Searching](/usage/searching/) for the operator reference and [Vector Search](/usage/vector-search/) for semantic setup.

---

## tui

Launch the interactive terminal interface.

```bash
msgvault tui [flags]
```

| Flag | Description |
|---|---|
| `--force-sql` | Force SQLite queries instead of Parquet |
| `--no-cache-build` | Skip automatic cache build/update |

---

## export-eml

Export a message as a `.eml` file. Accepts either a numeric database ID or a Gmail message ID.

```bash
msgvault export-eml <id> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <path>` | Output file (default: `<gmail_id>.eml`, use `-` for stdout) |

---

## export-attachment

Export an attachment by its SHA-256 content hash.

```bash
msgvault export-attachment <content-hash> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <path>` | Output file path (use `-` for stdout) |
| `--base64` | Output raw base64 to stdout |
| `--json` | Output as JSON with base64-encoded data |

The `--json`, `--base64`, and `--output` flags are mutually exclusive.

See [Exporting Data](/usage/exporting/) for usage examples.

---

## export-attachments

Export all attachments from a message as individual files.

```bash
msgvault export-attachments <message-id> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <dir>` | Output directory (default: current directory) |

Accepts internal numeric IDs or Gmail message IDs. See [Exporting Data](/usage/exporting/) for usage examples.

---

## export-token

Export a browser-created OAuth refresh token to a remote msgvault instance.

Use this for headless deployments (NAS, cloud VM, any remote server) that cannot run a browser flow.

```bash
msgvault export-token <email> [flags]
```

| Flag | Description |
|---|---|
| `--to <url>` | Remote msgvault URL (or `MSGVAULT_REMOTE_URL`) |
| `--api-key <key>` | API key (or `MSGVAULT_REMOTE_API_KEY`) |
| `--allow-insecure` | Allow HTTP for trusted networks (for example Tailscale) |

`export-token` uploads `~/.msgvault/tokens/<email>.json` to `/api/v1/auth/token/<email>`, saves it in the remote token store, and posts account metadata to `/api/v1/accounts`.

---

## verify

Verify local archive integrity against Gmail.

```bash
msgvault verify <email> [flags]
```

| Flag | Description |
|---|---|
| `--sample N` | Messages to sample (default: 100) |

---

## stats

Show archive statistics.

```bash
msgvault stats [flags]
```

| Flag | Description |
|---|---|
| `--account` | Show stats for a specific account |
| `--collection` | Show stats for all member accounts of a collection |

---

## identity

Manage the confirmed "me" identifiers for each account.

```bash
msgvault identity list [flags]
msgvault identity show <account> [flags]
msgvault identity add <account> <identifier> [flags]
msgvault identity remove <account> <identifier>
```

| Command | Description |
|---|---|
| `identity list` | List confirmed identifiers across accounts |
| `identity show <account>` | Show one account's identity in detail |
| `identity add <account> <identifier>` | Add a confirmed identifier |
| `identity remove <account> <identifier>` | Remove a confirmed identifier |

| Flag | Applies to | Description |
|---|---|---|
| `--account` | `list` | Restrict to a single account |
| `--collection` | `list` | Restrict to all member accounts of a collection |
| `--json` | `list`, `show` | Output as JSON |
| `--signal` | `add` | Evidence signal name (default `manual`) |

---

## collection

Manage named groups of accounts.

```bash
msgvault collection create <name> --accounts <account1,account2,...>
msgvault collection list
msgvault collection show <name>
msgvault collection add <name> --accounts <account1,account2,...>
msgvault collection remove <name> --accounts <account1,account2,...>
msgvault collection delete <name>
```

Deleting a collection does not delete sources or messages.

---

## deduplicate

Find and merge duplicate messages within an account or collection.

```bash
msgvault deduplicate [flags]
```

By default, each source is deduplicated independently. `--collection` is the explicit opt-in for cross-source deduplication.

| Flag | Description |
|---|---|
| `--dry-run` | Scan and report only; do not hide duplicates |
| `--account` | Scope dedup to one account |
| `--collection` | Dedup across every member account of a collection |
| `--content-hash` | Also detect duplicates by normalized raw MIME content |
| `--prefer` | Comma-separated source type preference order |
| `--undo <batch-id>` | Restore rows hidden by a previous dedup run; repeatable |
| `--delete-dups-from-source-server` | Stage same-source pruned duplicates for remote deletion |
| `--no-backup` | Skip the database backup before merging |
| `-y`, `--yes` | Skip confirmation prompt |

---

## delete-deduped

Permanently delete dedup-hidden messages from the local archive.

```bash
msgvault delete-deduped --batch <batch-id>
msgvault delete-deduped --all-hidden
```

This is local-only and cannot be undone with `deduplicate --undo`.

| Flag | Description |
|---|---|
| `--batch` | Delete rows hidden by this dedup batch ID; repeatable |
| `--all-hidden` | Delete every dedup-hidden row regardless of batch |
| `--no-backup` | Skip database backup before deleting |
| `-y`, `--yes` | Skip confirmation prompt (`--all-hidden` still prompts) |

---

## list-senders

List top senders by message count.

```bash
msgvault list-senders [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

---

## list-domains

List top sender domains by message count.

```bash
msgvault list-domains [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

---

## list-labels

List all labels with message counts.

```bash
msgvault list-labels [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

---

## build-cache

Build or update the Parquet analytics cache.

```bash
msgvault build-cache [flags]
```

| Flag | Description |
|---|---|
| `--full-rebuild` | Discard existing cache and rebuild |

---

## rebuild-fts

Rebuild the SQLite FTS5 search index.

```bash
msgvault rebuild-fts
```

Use this if `verify` reports FTS5 shadow-table corruption such as a malformed inverted index. The command rebuilds the search index from the canonical `messages` table.

---

## embeddings

Manage the vector embedding index used by `--mode vector` and `--mode hybrid` search. Requires a build with a vector backend (`sqlite_vec` for SQLite archives, `pgvector` for PostgreSQL archives) and a configured `[vector.embeddings]` endpoint. See [Vector Search](/usage/vector-search/) for prerequisites, model rotation, and troubleshooting.

```bash
msgvault embeddings <subcommand> [flags]
```

| Subcommand | Description |
|---|---|
| `build` | Build or update the index. Incremental by default; `--full-rebuild` starts a new generation. |
| `resume` | Drain the pending queue for the building or active generation. Always incremental. |
| `list` | List index generations with their state, model, dimension, and pending count. |
| `activate <generation-id>` | Activate a completed building generation, retiring the current active one. |
| `retire <generation-id>` | Retire a generation. |

### embeddings build

```bash
msgvault embeddings build [flags]
```

| Flag | Description |
|---|---|
| `--full-rebuild` | Create a new index generation and rebuild from scratch. The new generation is activated atomically once pending work drains. Same-model rebuilds keep serving the previous active generation in the meantime; model or dimension changes return `index_stale` for vector/hybrid search until the new generation activates. |
| `--yes` | Skip the confirmation prompt that `--full-rebuild` otherwise requires. |

Without `--full-rebuild`, the command is incremental: it resumes any in-flight rebuild that matches the configured model, otherwise drains the pending queue for the active generation, then exits. Safe to schedule via cron (or let `msgvault serve` do it via `[vector.embed.schedule]`).

### embeddings resume

```bash
msgvault embeddings resume
```

Drain the pending queue and finish the current generation. If a generation matching the configured model is building, this embeds its remaining rows and activates it once the queue reaches zero; otherwise it tops up the active generation. Equivalent to `msgvault embeddings build` with no flags, but never starts a full rebuild.

### embeddings list

```bash
msgvault embeddings list
```

Print one row per index generation: ID, state (`building`, `active`, or `retired`), model, dimension, embedded message count, pending count, fingerprint, and the start, completion, and activation timestamps.

### embeddings activate

```bash
msgvault embeddings activate <generation-id> [flags]
```

Activate a completed building generation and retire the currently active one. By default this refuses to activate a generation that still has pending rows, has not finished seeding, or whose fingerprint does not match the current config.

| Flag | Description |
|---|---|
| `--yes` | Skip the confirmation prompt. |
| `--force` | Activate even with pending rows or a fingerprint mismatch. |

### embeddings retire

```bash
msgvault embeddings retire <generation-id> [flags]
```

Mark a generation as retired. Retiring the active generation requires `--force-active`, since it leaves no generation serving vector/hybrid search.

| Flag | Description |
|---|---|
| `--yes` | Skip the confirmation prompt. |
| `--force-active` | Allow retiring the generation that is currently active. |

`msgvault build-embeddings` remains as a deprecated alias for `msgvault embeddings build` (same `--full-rebuild` and `--yes` flags).

---

## cache-stats

Show statistics about the analytics cache.

```bash
msgvault cache-stats
```

---

## query

Run arbitrary SQL against the Parquet analytics cache using an in-memory DuckDB engine.

```bash
msgvault query <sql> [flags]
```

If the analytics cache is stale, it is automatically rebuilt before the query runs.

| Flag | Default | Description |
|---|---|---|
| `--format` | `json` | Output format: `json`, `csv`, or `table` |

See [SQL Queries](/usage/querying/) for available views and example queries.

---

## mcp

Start the Model Context Protocol server for AI assistant integration.

```bash
msgvault mcp [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--force-sql` | `false` | Always use SQL retrieval instead of FTS5 |
| `--http` | — | Serve MCP over StreamableHTTP on this address instead of stdio. Bare ports bind to loopback, e.g. `8080` becomes `127.0.0.1:8080`. |
| `--http-allow-insecure` | `false` | Allow non-loopback HTTP binding. The MCP server has no built-in auth; put it behind a trusted network or authenticated reverse proxy. |

See [MCP Server](/usage/chat/) for configuration and tool reference.

---

## serve

Start the web server with optional background sync scheduling.

```bash
msgvault serve
```

The `serve` command has no CLI flags. All configuration (port, bind address, API key, CORS, account schedules, SyncTech SMS sources, and vector embedding schedule) is read from your `config.toml`. See [Web Server](/api-server/) for endpoint documentation and [Configuration](/configuration/#server) for config options. When vector search is enabled, the daemon can also run the embed worker on a cron and/or after every successful sync, see [Configuration: vector.embed.schedule](/configuration/#vectorembedschedule).

---

## setup

Run the first-run setup wizard for OAuth and optional remote deployment.

```bash
msgvault setup
```

If configured for a remote server, this command generates `<MSGVAULT_HOME>/nas-bundle` with:

- `config.toml` ready for container deployment
- `client_secret.json`
- `docker-compose.yml`

The wizard also stores remote URL/API key in `remote` config block so `export-token` can use it without extra flags.

---

## show-message

Show full message details.

```bash
msgvault show-message <id> [flags]
```

| Flag | Description |
|---|---|
| `--json` | Output as JSON |

---

## list-accounts

List synced email accounts.

```bash
msgvault list-accounts [flags]
```

| Flag | Description |
|---|---|
| `--json` | Output as JSON |

---

## update-account

Update account settings.

```bash
msgvault update-account <email> [flags]
```

| Flag | Description |
|---|---|
| `--display-name` | Set a display name for the account |

---

## remove-account

Remove an account and all its local data. Deletes messages, labels, sync state, OAuth or IMAP credentials, and attachment files unique to this account. This is irreversible and operates only on the local archive; it does not touch the remote provider.

```bash
msgvault remove-account <email> [flags]
```

| Flag | Description |
|---|---|
| `-y`, `--yes` | Skip the confirmation prompt (and allow removal when an active sync is in progress) |
| `--type` | Source type to remove when the same identifier exists across source types (`gmail`, `imap`, `mbox`, etc.) |

Attachment files are only deleted when no other account references the same content hash. The shared Parquet analytics cache is also cleared; run `msgvault build-cache` afterward to rebuild it.

---

## list-deletions

List pending and recent deletion batches.

```bash
msgvault list-deletions
```

---

## show-deletion

Show details of a deletion batch.

```bash
msgvault show-deletion <batch-id>
```

---

## cancel-deletion

Cancel pending or in-progress deletion batches. When called without a batch ID, lists available batches.

```bash
msgvault cancel-deletion [batch-id]
msgvault cancel-deletion --all
```

| Flag | Description |
|---|---|
| `--all` | Cancel all pending and in-progress batches |

---

## delete-staged

Execute staged remote deletions. By default, Gmail messages are moved to trash; pass `--permanent` for permanent Gmail batch deletion. IMAP deletion removes messages from the provider using IMAP delete/expunge behavior.

```bash
msgvault delete-staged [batch-id] [flags]
```

| Flag | Description |
|---|---|
| `-y`, `--yes` | Skip confirmation prompt |
| `--permanent` | Permanently delete through the Gmail batch API instead of moving to trash |
| `--dry-run` | Show what would be deleted without deleting |
| `-l`, `--list` | List staged deletion batches |
| `--account` | Filter to a specific account |

Execution requires `MSGVAULT_ENABLE_REMOTE_DELETE=1`. `--list` and `--dry-run` work without the gate. `--permanent` and `--yes` are mutually exclusive because permanent deletion always requires the destructive confirmation prompt.

---

## repair-encoding

Fix UTF-8 encoding issues in existing messages.

```bash
msgvault repair-encoding
```

---

## update

Update msgvault to the latest version.

```bash
msgvault update [flags]
```

| Flag | Description |
|---|---|
| `--check` | Check for updates without installing |
| `-y`, `--yes` | Skip confirmation prompt |
| `-f`, `--force` | Force update even if already on the latest version |

---

## version

Print version, commit, build date, and platform information.

```bash
msgvault version
```

---

## completion

Generate a shell completion script.

```bash
msgvault completion [bash|zsh|fish|powershell]
```

To load completions:

**Bash:**
```bash
source <(msgvault completion bash)

# Permanent (Linux):
msgvault completion bash > /etc/bash_completion.d/msgvault

# Permanent (macOS with Homebrew):
msgvault completion bash > $(brew --prefix)/etc/bash_completion.d/msgvault
```

**Zsh:**
```bash
msgvault completion zsh > "${fpath[1]}/_msgvault"
```

If shell completion is not already enabled, add `autoload -U compinit; compinit` to your `~/.zshrc` first.

**Fish:**
```bash
msgvault completion fish > ~/.config/fish/completions/msgvault.fish
```

**PowerShell:**
```powershell
msgvault completion powershell | Out-String | Invoke-Expression
```

---

## logs

View and tail structured log files. File logging must be enabled first (see [Configuration: Log](/configuration/#log)).

```bash
msgvault logs [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-f`, `--follow` | `false` | Follow today's log file as new lines are written |
| `-n`, `--lines` | `50` | Number of trailing lines to show before following |
| `--run-id <id>` | — | Filter to a single run (matches on prefix) |
| `--level <level>` | — | Filter by log level: `debug`, `info`, `warn`, `error` |
| `--grep <string>` | — | Substring filter applied to the raw JSON record |
| `--all` | `false` | Read every log file in the logs directory, not just today's |
| `--path` | `false` | Print the log directory path and exit |

Examples:

```bash
# Last 50 lines of today's log
msgvault logs

# Follow live
msgvault logs -n 200 -f

# Filter to a single run by its correlation ID
msgvault logs --run-id a1b2c3

# Only errors
msgvault logs --level error

# Substring search across all log files
msgvault logs --all --grep deduplicate
```

---

## quickstart

Print a quickstart guide for AI agents.

```bash
msgvault quickstart
```
