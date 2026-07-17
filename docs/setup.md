---
title: Setup Guide
description: Install msgvault, configure OAuth, and sync your first emails.
---


## Install Release

**macOS / Linux:**
```bash
curl -fsSL https://msgvault.io/install.sh | bash
```

**Windows (PowerShell):**
```powershell
powershell -ExecutionPolicy ByPass -c "irm https://msgvault.io/install.ps1 | iex"
```

The installer detects your OS and architecture, downloads the latest release from [GitHub Releases](https://github.com/kenn-io/msgvault/releases), verifies the SHA-256 checksum, and installs the binary.

Windows releases include native AMD64 and ARM64 packages. On Windows ARM64,
the PowerShell installer selects the native package when the release provides
one and falls back to the AMD64 package under emulation for older releases.

!!! tip "Running on a headless server?"
    msgvault works on headless machines (SSH, VPS, NAS, Docker), but OAuth requires a browser for the initial authorization. You'll authorize on your local machine and copy the token file to the server. See [Headless Server Setup](/guides/oauth-setup/#headless-server-setup) for the copy-token workflow, or jump to the [Remote Deployment](/guides/remote-deployment/) guide for a full NAS/server setup with Docker Compose.

Verify the installation:

```bash
msgvault --help
```

## Conda-Forge

If you use [conda](https://docs.conda.io/) or [pixi](https://pixi.sh/):

```bash
# Using pixi (recommended)
pixi global install msgvault

# Using conda
conda install -c conda-forge msgvault
```

## Build From Source

Requires Go 1.26+ and a C/C++ compiler (GCC or Clang). CGO is required because msgvault uses `mattn/go-sqlite3` (SQLite with FTS5) and `duckdb-go/v2` (Parquet analytics), both of which compile native extensions.

```bash
git clone https://github.com/kenn-io/msgvault.git
cd msgvault
make install
```

On macOS and Linux this installs to `~/.local/bin` or `$GOPATH/bin`. For a
debug build use `make build`, or `make build-release` for an optimized binary
with stripped debug symbols.

On Windows, use the native PowerShell helper:

```powershell
.\scripts\build.ps1          # Debug build
.\scripts\build.ps1 -Release # Optimized, stripped build
```

It detects AMD64 or ARM64 automatically and writes `msgvault.exe` in the
repository root. See [Development and Roadmap](/development/#windows) for the
one-time MSYS2 compiler prerequisites.

Verify the installation:

```bash
msgvault --help
```

## Configure OAuth

Create a Google Cloud project, enable the Gmail API, and download your `client_secret.json`. If you plan to archive Google Calendar, enable the Google Calendar API too. See the full [OAuth Setup Guide](/guides/oauth-setup/).

### Where to put config.toml

msgvault stores all data (config, database, tokens, attachments) in a single directory. The default location depends on your platform:

| Platform | Data directory | Config file |
|---|---|---|
| **macOS / Linux** | `~/.msgvault/` | `~/.msgvault/config.toml` |
| **Windows** | `C:\Users\<you>\.msgvault\` | `C:\Users\<you>\.msgvault\config.toml` |

!!! tip
    The `.msgvault` directory is created automatically the first time you run any msgvault command. If you're unsure of the exact path, run `msgvault add-account you@gmail.com`; the error message may show you where to create the config file.

To store data on a different drive or location, use the `--home` flag or set the `MSGVAULT_HOME` environment variable. If `MSGVAULT_HOME` is set, paths in the table above are relative to that directory instead:

**Per-command (any platform):**
```bash
msgvault sync --home E:/msgvault
```

**Windows (PowerShell, persistent):**
```powershell
$env:MSGVAULT_HOME = "E:\msgvault"
# Or set it permanently:
[Environment]::SetEnvironmentVariable("MSGVAULT_HOME", "E:\msgvault", "User")
```

**macOS / Linux (persistent):**
```bash
export MSGVAULT_HOME=/mnt/data/msgvault
```

The `--home` flag takes priority over `MSGVAULT_HOME`. See [Configuration](/configuration/) for all options.

### Create the config file

**macOS / Linux:**
```toml
[oauth]
client_secrets = "/path/to/client_secret.json"
```

**Windows:** use forward slashes in the path:
```toml
[oauth]
client_secrets = "C:/Users/you/Downloads/client_secret.json"
```

## Add Your Account

```bash
msgvault add-account you@gmail.com
```

This opens your browser for OAuth consent. For headless servers, see the [copy-token workflow](/guides/oauth-setup/#headless-server-setup).

If you plan to deploy to a remote host (NAS, cloud VM, etc.), run `msgvault setup` after this step to generate a ready-to-run deployment bundle with Docker Compose and remote configuration. See the [Remote Deployment](/guides/remote-deployment/) guide.

## Add an IMAP Account

To sync mail from a non-Gmail provider (Fastmail, Outlook, Yahoo, self-hosted, etc.), use `add-imap`:

```bash
msgvault add-imap --host imap.fastmail.com --username you@fastmail.com
```

You will be prompted for your password (or set `MSGVAULT_IMAP_PASSWORD` / pipe via stdin for scripting). The command tests the connection before saving credentials. Use an app-specific password if your provider supports them.

Common IMAP servers:

| Provider | Host | Port | Notes |
|---|---|---|---|
| Fastmail | `imap.fastmail.com` | 993 | App password recommended |
| Outlook / Hotmail | `outlook.office365.com` | 993 | Use [`add-o365`](/guides/oauth-setup/#microsoft-365-outlook-hotmail) for OAuth (recommended); or app password with 2FA |
| Yahoo | `imap.mail.yahoo.com` | 993 | [App password](#yahoo-app-passwords) required |
| iCloud | `imap.mail.me.com` | 993 | App-specific password required |
| Gmail (IMAP) | `imap.gmail.com` | 993 | Use `add-account` for Gmail API instead |
| Self-hosted | your server hostname | 993 | |

For STARTTLS connections (port 143), add `--starttls`:

```bash
msgvault add-imap --host mail.example.com --username you@example.com --starttls
```

After adding the account, sync it the same way as a Gmail account:

```bash
msgvault sync-full
```

IMAP accounts are stored in the same database as Gmail accounts. All tools (TUI, search, MCP, web server) work with IMAP messages the same way.

!!! tip "Microsoft 365 / Outlook.com"
    For Outlook, Hotmail, Live.com, and Microsoft 365 accounts, `add-o365` provides OAuth-based access without app passwords. It auto-detects the correct IMAP host and configures XOAUTH2 authentication. See the [OAuth Setup guide](/guides/oauth-setup/#microsoft-365-outlook-hotmail) for details.

<span id="yahoo-app-passwords"></span>

!!! tip "Yahoo App Passwords"
    Yahoo requires an App Password for IMAP access. Your regular Yahoo password will not work. To generate one:

    1. Go to [Yahoo Account Security](https://login.yahoo.com/account/security)
    2. Under **Generate and manage app passwords**, click **Generate app password**
    3. Enter `msgvault` as the app name and copy the generated password
    4. Use this password when `add-imap` prompts for your credentials

!!! note
    IMAP sync always performs a full scan of the mailbox. The `sync` (incremental) command falls back to a full sync for IMAP accounts because IMAP does not provide a change-tracking API like Gmail's History API. Messages already in the database are skipped efficiently.

## Sync Email

```bash
# Test with a small batch first
msgvault sync-full you@gmail.com --limit 100

# Or sync a specific date range
msgvault sync-full you@gmail.com --after 2024-01-01 --before 2024-02-01

# Sync everything (no limit)
msgvault sync-full you@gmail.com
```

### What to Expect

The initial full sync downloads every message and attachment from Gmail, so it can take a while. In testing we have observed roughly 50 messages per second on fast internet, but the Gmail API has per-user quotas that may throttle throughput further. An account with hundreds of thousands of messages and large attachments may take several hours; an account with millions of messages could take significantly longer. We recommend starting with `--limit` or a date range to verify everything works before kicking off the full run.

The good news: syncs are resumable (see below), and once the initial sync is complete, incremental syncs only fetch new and changed messages, which is much faster.

### Disk Usage

msgvault stores raw MIME data compressed with zlib (typically 3-5x compression). As a rough guide:

| Gmail usage (Settings → Storage) | SQLite DB on disk | Parquet cache | Attachments |
|---|---|---|---|
| 5 GB | ~1-2 GB | < 10 MB | varies |
| 25 GB | ~5-10 GB | < 50 MB | varies |
| 100 GB | ~20-40 GB | < 100 MB | varies |

Gmail's "storage used" number includes attachments at full size. Your on-disk footprint depends on the ratio of message text to attachments:

- **Message metadata + bodies** go into the SQLite database, compressed ~3-5x.
- **Attachments** are extracted and stored as-is (PDFs, images, etc. are already compressed). Identical attachments across messages are deduplicated by content hash.
- **Parquet analytics cache** is a lightweight projection for the TUI — typically a few MB even for large archives.

Use `--limit` or a date range for your first sync to gauge the ratio for your mailbox before committing to a full sync. After syncing, `msgvault stats` shows the actual sizes. See [Data Storage](/architecture/storage/) for details on compression and storage layers.

### Full Sync Flags

| Flag | Description |
|---|---|
| `--limit N` | Download at most N messages |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--query` | Gmail search query filter |
| `--noresume` | Start fresh instead of resuming |
| `--verbose` | Show detailed progress |

### Incremental Sync

After the initial full sync, use incremental sync for efficient updates. It uses the Gmail History API to fetch only new and changed messages:

```bash
msgvault sync you@gmail.com

# Or sync all accounts at once
msgvault sync
```

### Resumable Checkpoints

If a sync is interrupted (network error, Ctrl+C), run the same command again. It resumes from the last checkpoint:

```bash
# This resumes automatically
msgvault sync-full you@gmail.com --after 2024-01-01 --before 2024-02-01
```

Checkpoint data is stored in the `sync_checkpoints` table. Use `--noresume` to discard checkpoints and start over.

### Rate Limiting

msgvault uses token bucket rate limiting to respect Gmail API quotas. The default is 5 requests per second, configurable in `config.toml`:

```toml
[sync]
rate_limit_qps = 5
```

Reduce this value if you encounter rate limit errors during large syncs.

### Safety

Sync operations are **read-only**. They use only `messages.list` and `messages.get` Gmail APIs. No write operations are performed. Your Gmail data remains untouched.

## Explore

```bash
# Search your archive
msgvault search from:alice@example.com

# Launch the interactive TUI
msgvault tui

# View stats
msgvault stats
```
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-senders.svg" alt="msgvault TUI showing the Senders view" loading="lazy">
</figure>
See [Searching](/usage/searching/) and [Interactive TUI](/usage/tui/) for more.

## Optional: Sync Google Calendar

To archive Calendar events alongside email, authorize Calendar access and run a
calendar sync:

```bash
msgvault add-calendar you@gmail.com
msgvault sync-calendar you@gmail.com
```

Calendar sync is read-only. Events become searchable with
`--message-type calendar_event`; see [Google Calendar](/usage/calendar/) for the
full workflow, scheduled sync, and headless-server setup.

## Optional: Sync Microsoft Teams

To archive Teams chats and channels, add Microsoft Graph permissions to your
Microsoft app registration, authorize Teams, then sync:

```bash
msgvault add-teams user@example.com
msgvault sync-teams user@example.com
```

Teams messages become searchable with `--message-type teams`. See
[Microsoft Teams](/usage/teams/) for required Graph permissions, scheduling,
and inline media backfill.

## Optional: Configure Backups

Create a backup repository before relying on the archive as your source of
truth:

```bash
msgvault backup init --repo ~/Backups/msgvault
msgvault backup create --repo ~/Backups/msgvault
msgvault backup verify --repo ~/Backups/msgvault
```

Record the repository in `config.toml` so future commands can omit `--repo`:

```toml
[backup]
repo = "~/Backups/msgvault"
```

See [Backup](/usage/backup/) for restore, verification, scheduling, and
secret-handling details.
