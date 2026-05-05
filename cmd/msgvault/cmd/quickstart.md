# msgvault Agent Quickstart

You have access to `msgvault`, an offline Gmail archive tool. The user's email
archive is stored locally in a SQLite database with Parquet-based analytics.
Use the commands below to help the user set up, sync, explore, search, and
manage their email archive.

All data is stored in `~/.msgvault/` by default (override with `MSGVAULT_HOME`).

## Setup and account management

### Initialize the database

```bash
msgvault init-db
```

Creates the SQLite database and schema at `~/.msgvault/msgvault.db`. Safe to
run multiple times — tables are only created if they don't exist.

### Add a Gmail account

```bash
# Browser-based OAuth (default)
msgvault add-account user@gmail.com

# Headless / device code flow (for SSH sessions, no browser)
msgvault add-account user@gmail.com --headless
```

Requires `oauth.client_secrets` to be set in `~/.msgvault/config.toml` pointing
to a Google Cloud OAuth client secrets JSON file.

### Configuration

The config file is at `~/.msgvault/config.toml`:

```toml
[oauth]
client_secrets = "/path/to/client_secret.json"

[sync]
rate_limit_qps = 5
```

## Syncing email

### Full sync

Downloads all messages from Gmail. Supports resumption — if interrupted, just
run again to continue from the last checkpoint.

```bash
# Sync everything
msgvault sync-full user@gmail.com

# Sync a date range
msgvault sync-full user@gmail.com --after 2024-01-01 --before 2024-12-31

# Sync with a Gmail search query
msgvault sync-full user@gmail.com --query "from:someone@example.com"

# Limit message count (useful for testing)
msgvault sync-full user@gmail.com --limit 100

# Force fresh sync (skip resume from checkpoint)
msgvault sync-full user@gmail.com --noresume
```

Sync is **read-only** — it never modifies anything in Gmail.

### Incremental sync

Fetches only changes since the last sync using the Gmail History API. Much
faster than a full sync. Requires a prior full sync.

```bash
msgvault sync user@gmail.com
```

If Gmail's history has expired (~7 days), it will suggest running a full sync.

### Build the analytics cache

The TUI and aggregate queries use Parquet files for fast analytics. The cache
is built automatically when launching the TUI, but you can also build it
manually:

```bash
# Incremental update (only new messages)
msgvault build-cache

# Full rebuild from scratch
msgvault build-cache --full-rebuild
```

Cache files are stored in `~/.msgvault/analytics/`.

## Check archive status

```bash
msgvault stats
```

Shows message count, thread count, attachment count, label count, source
(account) count, and database size.

## Search messages

Use Gmail-like query syntax. All commands support `--json` for structured output.

```bash
# Full-text search
msgvault search "project update" --json

# Filter by sender
msgvault search from:alice@example.com

# Combine filters
msgvault search from:alice@example.com has:attachment after:2024-01-01

# Filter by label
msgvault search label:INBOX newer_than:30d

# Size filters
msgvault search larger:5M

# Pagination
msgvault search "quarterly report" --limit 100 --offset 50
```

### Supported search operators

| Operator      | Description                          | Example                    |
|---------------|--------------------------------------|----------------------------|
| `from:`       | Sender email address                 | `from:bob@example.com`     |
| `to:`         | Recipient email address              | `to:team@company.com`      |
| `cc:`         | CC recipient                         | `cc:manager@company.com`   |
| `bcc:`        | BCC recipient                        | `bcc:hr@company.com`       |
| `subject:`    | Subject text                         | `subject:meeting`          |
| `label:`      | Gmail label (or `l:`)                | `label:IMPORTANT`          |
| `has:`        | `has:attachment`                     | `has:attachment`           |
| `before:`     | Messages before date                 | `before:2024-06-01`        |
| `after:`      | Messages after date                  | `after:2024-01-01`         |
| `older_than:` | Relative date                        | `older_than:1y`            |
| `newer_than:` | Relative date                        | `newer_than:7d`            |
| `larger:`     | Minimum size                         | `larger:10M`               |
| `smaller:`    | Maximum size                         | `smaller:100K`             |

Bare words and `"quoted phrases"` perform full-text search across subject and body.

## View a single message

```bash
# By internal ID (from search results)
msgvault show-message 12345

# By Gmail message ID
msgvault show-message 18f0abc123def

# Structured output
msgvault show-message 12345 --json
```

The JSON output includes: `id`, `source_message_id`, `subject`, `from`, `to`,
`cc`, `bcc`, `sent_at`, `labels`, `body_text`, `body_html`, `attachments`.

## Aggregate analytics

These commands show top senders, domains, and labels ranked by message count.
All support `--limit` (`-n`), `--after`, `--before`, and `--json`.

```bash
# Top senders by email address
msgvault list-senders --limit 20 --json

# Top sender domains
msgvault list-domains --after 2024-01-01 --json

# Labels with message counts
msgvault list-labels --json
```

Each row in JSON output contains: `key`, `count`, `total_size`, `attachment_size`.

## Export a message as .eml

```bash
msgvault export-eml 12345
msgvault export-eml 12345 --output message.eml
```

Exports the raw MIME data as a standard `.eml` file compatible with most email
clients.

## Deletion management

Messages are staged for deletion in the TUI (select messages, press `d`).
Staged deletions are stored as manifests and must be explicitly executed.

```bash
# List all deletion batches (pending, in-progress, completed, failed)
msgvault list-deletions

# Show details of a specific batch
msgvault show-deletion <batch-id>

# Cancel a pending batch
msgvault cancel-deletion <batch-id>

# List staged batches without executing (always allowed)
msgvault delete-staged --list

# Execute pending deletions (gated; default = trash, recoverable for 30 days)
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --yes

# Execute a specific batch
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged <batch-id>

# Permanently delete via batch API (fast, no recovery — opt-in)
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --permanent

# Dry run — show what would be deleted without doing it (always allowed)
msgvault delete-staged --dry-run

# Specify which account to delete from
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --account user@gmail.com
```

**Note:** Remote deletion is gated for the v1 release. Staging, listing,
inspecting, and dry-running deletion batches works without the gate;
executing against Gmail requires `MSGVAULT_ENABLE_REMOTE_DELETE=1` in the
environment.

**Warning:** `delete-staged --permanent` permanently deletes messages from
Gmail with no Gmail-side recovery. The default mode moves messages to Gmail
trash, which is recoverable for 30 days. Always verify with `--dry-run`
first regardless of mode.

## Verify archive integrity

```bash
msgvault verify user@gmail.com
msgvault verify user@gmail.com --sample 500
```

Compares local message count with Gmail, checks raw MIME data integrity, and
samples random messages to verify they can be decompressed.

## Maintenance

```bash
# Repair invalid UTF-8 encoding in message text fields
msgvault repair-encoding

# Self-update to latest release
msgvault update
msgvault update --check   # Check only, don't install
msgvault update --yes     # Skip confirmation

# Show version info
msgvault version
```

## Interactive TUI

```bash
# Launch the TUI (auto-builds analytics cache if needed)
msgvault tui

# Filter by account
msgvault tui --account user@gmail.com

# Force local database (override remote config)
msgvault tui --local
```

### Remote mode

When `[remote].url` is configured in `config.toml`, the TUI connects to a remote
msgvault server instead of the local database. This is useful for accessing an
archive on a NAS or server from another machine.

```toml
[remote]
url = "http://nas.local:8080"
api_key = "your-api-key"
```

In remote mode, deletion staging and attachment export are disabled for safety.

### TUI keybindings

| Key              | Action                                         |
|------------------|-------------------------------------------------|
| `j`/`k`, `↑`/`↓`| Navigate rows                                  |
| `Enter`          | Drill down into selection                       |
| `Esc`/`Backspace`| Go back                                        |
| `Tab`            | Cycle view (Senders → Sender Names → To → To Names → Domains → Labels → Time) |
| `t`              | Jump to Time view (cycle granularity when already in Time) |
| `s`              | Cycle sort (Name → Count → Size)                |
| `r`              | Reverse sort direction                          |
| `t`              | Cycle time granularity (Year/Month/Day)         |
| `a`              | Filter by account                               |
| `f`              | Filter by attachments                           |
| `/`              | Search                                          |
| `Space`          | Toggle selection                                |
| `A`              | Select all visible                              |
| `x`              | Clear selection                                 |
| `d`              | Stage selected for deletion                     |
| `D`              | Stage all matching current filter for deletion  |
| `?`              | Help                                            |
| `q`              | Quit                                            |

## Typical agent workflow

1. **Check status**: `msgvault stats` — see what's in the archive.
2. **Search**: `msgvault search <query> --json` — find relevant messages.
3. **Read details**: `msgvault show-message <id> --json` — get full message content.
4. **Analyze**: `list-senders`, `list-domains`, `list-labels` with `--json` for patterns.
5. **Sync new mail**: `msgvault sync user@gmail.com` if archive is stale.

## Tips

- **Always use `--json`** for programmatic access. Available on: `search`,
  `show-message`, `list-senders`, `list-domains`, `list-labels`.
- Search results return an `id` field — use it with `show-message` for full content.
- Date filters use `YYYY-MM-DD` format.
- Relative date units: `d` (days), `w` (weeks), `m` (months), `y` (years).
- All query/search commands are read-only and never modify data.
- Deletion requires explicit staging (TUI) + execution (`delete-staged`).
- Use `--verbose` (`-v`) on any command for debug logging.
