---
last_edited: "2026-11-07"
title: Setup guide
description: Install msgvault, choose a source, and explore your first archived messages.
---

Install msgvault, pull in a small first batch of messages, and open your archive
in the browser or terminal. The walkthrough below covers Gmail and Google OAuth.
For other mail providers, chats, meetings, contacts, or local exports, install
msgvault first and then use [Choose a source](guides/sources.md).

## Install release

**macOS / Linux:**
```bash
curl -fsSL https://msgvault.io/install.sh | bash
```

**Windows (PowerShell):**
```powershell
powershell -ExecutionPolicy ByPass -c "irm https://msgvault.io/install.ps1 | iex"
```

The installer detects your OS and architecture, downloads the latest release
from [GitHub Releases](https://github.com/kenn-io/msgvault/releases), checks the
SHA-256 checksum, and installs the binary. On Unix, it also adds msgvault to
your Bash, Zsh, or Fish profile; set `MSGVAULT_NO_MODIFY_PATH=1` to skip that
edit.

Windows releases include native AMD64 and ARM64 packages. On Windows ARM64, the
PowerShell installer picks the native package when the release provides one and
falls back to the AMD64 package under emulation for older releases.

!!! tip "Running on a headless server?"
    msgvault runs on headless machines such as SSH hosts, VPS instances, NAS
    boxes, and Docker containers, though OAuth needs a browser for the initial
    sign-in. You authorize on your local machine and copy the token file to the
    server. See [Headless Server Setup](/docs/guides/oauth-setup/#headless-server-setup)
    for the copy-token workflow, or go to
    [Remote Deployment](/docs/guides/remote-deployment/) for a full NAS or
    server setup with Docker Compose.

Verify the installation:

```bash
msgvault --help
```

### Runtime requirements

Direct release downloads bundle DuckDB, SQLite with FTS5, sqlite-vec, and the
Web UI inside the executable. You do not need DuckDB, Go, a C/C++ compiler,
Bun, or Node.js to run them; build tools are only required when
[building from source](#build-from-source).

Bundling DuckDB does not make the executable fully static. The v0.23.0 direct
archives have these native runtime requirements on both AMD64 and ARM64:

| Platform | Runtime requirements |
| --- | --- |
| macOS | The executables declare macOS 26.0 as their minimum deployment target and load Apple system libraries, including libc++. |
| Linux | glibc with `GLIBC_2.34`, plus system `libstdc++.so.6` with `GLIBCXX_3.4.30` and `CXXABI_1.3.13`, `libgcc_s.so.1`, and `libm.so.6`. Debian 12 (bookworm) supplies these. Alpine/musl does not supply the required glibc loader. |
| Windows | Windows system DLLs and the Universal C Runtime supplied by current Windows installations. The executables do not import separate DuckDB, MinGW, or MSVC C++ runtime DLLs. |

These are inspected loader requirements rather than a claim that every OS
version has been tested. We also ran v0.23.0 with a fresh home on macOS ARM64
and in stock Debian 12 ARM64 with networking disabled, importing one synthetic
email, exporting it to Parquet, and querying it through DuckDB. Windows and
native Intel macOS execution were not part of that check. Pick the release
archive that matches your host; the existence of an archive alone does not establish
compatibility with an older OS.

DuckDB may try to download its optional SQLite scanner extension. When that
download is unavailable, including offline, msgvault falls back to its built-in
SQLite/CSV path. The Parquet cache and DuckDB queries do not require that
download.

Homebrew and Conda builds manage their own dependencies. In particular,
`make homebrew` links Homebrew's DuckDB on purpose; the direct-download
runtime contract above applies only to ordinary release archives.

## Vendor a pinned binary

Applications can download a release archive directly from
[GitHub Releases](https://github.com/kenn-io/msgvault/releases). Pin the tag,
choose the archive for your platform from that release's asset list, and verify
it against the release's `SHA256SUMS` file before extracting it. Fail the
download if either the archive or its checksum entry is missing.

Archives follow the name `msgvault_<version>_<os>_<arch>`, without the leading
`v` on the version. Use `darwin` for macOS, `linux` for Linux, and `windows`
for Windows, with `amd64` or `arm64` for the architecture. macOS and Linux
archives end in `.tar.gz`, while Windows archives end in `.zip`, such as
`msgvault_0.23.0_windows_arm64.zip`.

### Migrate before replacing the executable

These commands are available in 0.22.0 and later. Run them from the candidate
binary on the archive host. If you have a remote server configured, pass
`--local` only when you intentionally want to target this host's archive.

1. Download, verify, and extract the candidate binary into a separate directory.
2. Stop the daemon and any other processes using the archive. For PostgreSQL,
   stop users on other hosts too.
3. Back up the archive before migrating.
4. Compare the candidate's expected archive version with the stored version:

   ```bash
   ./candidate/msgvault schema-version
   ./candidate/msgvault --home /path/to/archive-home schema-version --database
   ```

5. Run the candidate's migrations and require exit status zero before
   continuing:

   ```bash
   ./candidate/msgvault --home /path/to/archive-home migrate
   ./candidate/msgvault --home /path/to/archive-home schema-version --database
   ```

6. Confirm that the stored version equals the candidate's expected version,
   then replace the installed executable and restart the daemon.

A stored version of `0` means the archive predates this contract or has not
finished schema initialization. A higher stored version requires a newer
compatible binary; there is no schema downgrade command.

Migrations can commit progress before failing or being interrupted. Keeping the
old version number does not make partial changes safe for an older executable.
Retry with the candidate binary, or restore your backup before starting the old
binary again. Older releases do not include the future-version guard.

See [schema-version](cli-reference.md#schema-version) for the exact probe
contract and [migrate](cli-reference.md#migrate) for its scope and failure
behavior.

## Conda-Forge

If you use [conda](https://docs.conda.io/) or [pixi](https://pixi.sh/):

```bash
# Using pixi (recommended)
pixi global install msgvault

# Using conda
conda install -c conda-forge msgvault
```

## Build from source

On macOS and Linux, source builds require Go 1.27.4+, Bun 1.4.4+, Node.js
22.23.4+ on 22.x, 24.22.1+ on 24.x, or 26.12.1+, and a C/C++ compiler such as
GCC or Clang. Bun builds the browser application embedded in the binary, and
Node runs the embed validator that `make install` invokes. CGO is required
because msgvault uses `mattn/go-sqlite3` for SQLite with FTS5 and
`duckdb-go/v2` for Parquet analytics, and both compile native extensions.

On Debian or Ubuntu, also install `libsqlite3-dev`, which supplies the
`sqlite3.h` header needed to compile the default `sqlite_vec` extension:

```bash
sudo apt install -y libsqlite3-dev
```

```bash
git clone https://github.com/kenn-io/msgvault.git
cd msgvault
make install
```

On macOS and Linux, `make install` places the binary in `~/.local/bin` or
`$GOPATH/bin`. For a debug build, run `make build`; for an optimized binary with
stripped debug symbols, run `make build-release`.

On Windows, start by building the embedded Web UI from an MSYS2 shell with GNU
Make, Bun, and Node available:

```bash
make web-embed
```

Then run the native PowerShell helper to build the Go binary:

```powershell
.\scripts\build.ps1          # Debug build
.\scripts\build.ps1 -Release # Optimized, stripped build
```

The script detects AMD64 or ARM64 automatically and writes `msgvault.exe` in
the repository root. It does not build the Web UI itself. See
[Development](/docs/development/#windows) for the UI build step and one-time
MSYS2 compiler prerequisites.

Verify the installation:

```bash
msgvault --help
```

## Configure OAuth

Follow this section for Gmail and other Google-backed sources. If you are
importing local files or using a provider with its own authorization guide, skip
ahead.

Create a Google Cloud project, enable the Gmail API, and download your
`client_secret.json` file. If you also plan to archive Google Calendar, enable
the Google Calendar API at the same time. See the full
[OAuth Setup Guide](/docs/guides/oauth-setup/).

### Where to put config.toml

msgvault keeps your config, database, tokens, and attachments together in one
directory. The default location depends on your platform:

| Platform | Data directory | Config file |
|---|---|---|
| **macOS / Linux** | `~/.msgvault/` | `~/.msgvault/config.toml` |
| **Windows** | `C:\Users\<you>\.msgvault\` | `C:\Users\<you>\.msgvault\config.toml` |

!!! tip
    msgvault creates the `.msgvault` directory automatically the first time you
    run any command. If you are unsure of the exact path, run
    `msgvault add-account you@gmail.com`; the error message may tell you where
    to put the config file.

To keep your data on another drive or path, pass `--home` or set the
`MSGVAULT_HOME` environment variable. When `MSGVAULT_HOME` is set, the paths in
the table above are relative to that directory instead:

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

The `--home` flag takes priority over `MSGVAULT_HOME`. See
[Configuration](/docs/configuration/) for all options.

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

## Add your account

```bash
msgvault add-account you@gmail.com
```

This opens your browser for OAuth consent. On a headless server, use the
[copy-token workflow](/docs/guides/oauth-setup/#headless-server-setup) instead.

If you plan to run msgvault on a remote host such as a NAS or cloud VM, run
`msgvault setup` after this step to generate a ready-to-run deployment bundle with Docker
Compose and remote configuration. See the
[Remote Deployment](/docs/guides/remote-deployment/) guide.

During setup, pick the detected host IANA timezone for activity day boundaries,
or enter another IANA zone. Setup writes the chosen zone to
`[activity].timezone` in your local config and in any NAS bundle it generates.
If the daemon is already running, restart it to load the new zone; later
projection passes then re-date existing activity rows automatically. `Local` is
not accepted because its meaning depends on the host at runtime.

## Add an IMAP account

To sync mail from a non-Gmail provider such as Fastmail, Outlook, Yahoo, or a
self-hosted server, run `add-imap`:

```bash
msgvault add-imap --host imap.fastmail.com --username you@fastmail.com
```

The command prompts for your password, or you can set `MSGVAULT_IMAP_PASSWORD`
or pipe the password on stdin for scripting. It tests the connection before
saving your credentials. Use an app-specific password if your provider supports
them.

Common IMAP servers:

| Provider | Host | Port | Notes |
|---|---|---|---|
| Fastmail | `imap.fastmail.com` | 993 | App password recommended |
| Outlook / Hotmail | `outlook.office365.com` | 993 | Use [`add-o365`](/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail) for OAuth (recommended); or app password with 2FA |
| Yahoo | `imap.mail.yahoo.com` | 993 | [App password](#yahoo-app-passwords) required |
| iCloud | `imap.mail.me.com` | 993 | App-specific password required |
| Gmail (IMAP) | `imap.gmail.com` | 993 | Use `add-account` for Gmail API instead |
| Self-hosted | your server hostname | 993 | |

For STARTTLS connections on port 143, add `--starttls`:

```bash
msgvault add-imap --host mail.example.com --username you@example.com --starttls
```

After adding the account, list its folders and run a sync:

```bash
msgvault list-folders you@fastmail.com
msgvault sync-full you@fastmail.com
```

IMAP accounts share the same database as Gmail accounts, and the Web UI, TUI,
search, MCP, and REST API all work with IMAP messages the same way. If you
want to start with only part of a large mailbox, see
[IMAP Folder Sync](usage/imap.md) for `--folder` and `--skip-folder` examples.

!!! tip "Microsoft 365 / Outlook.com"
    For Outlook, Hotmail, Live.com, and Microsoft 365 accounts, `add-o365` sets
    up OAuth access without app passwords. It detects the right IMAP host and
    configures XOAUTH2 authentication automatically. See the
    [OAuth Setup guide](/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail)
    for details.

<span id="yahoo-app-passwords"></span>

!!! tip "Yahoo App Passwords"
    Yahoo requires an App Password for IMAP access; your regular Yahoo password
    does not work. To generate one:

    1. Go to [Yahoo Account Security](https://login.yahoo.com/account/security).
    2. Under **Generate and manage app passwords**, click **Generate app password**.
    3. Enter `msgvault` as the app name and copy the generated password.
    4. Enter that password when `add-imap` prompts for your credentials.

!!! note
    After the first scan, `msgvault sync` uses QRESYNC when the server supports
    it. Otherwise it falls back to CONDSTORE or a UID-based scan and skips
    unchanged messages. See
    [IMAP Sync and Repair](usage/imap.md#how-later-syncs-find-changes) for the
    fallback order and deletion limits.

## Sync email

```bash
# Test with a small batch first
msgvault sync-full you@gmail.com --limit 100

# Or sync a specific date range
msgvault sync-full you@gmail.com --after 2024-01-01 --before 2024-02-01

# Sync everything (no limit)
msgvault sync-full you@gmail.com
```

### What to expect

The initial full sync downloads every message and attachment from Gmail, so it
can take a while. On fast internet, testing shows roughly 50 messages per
second, though Gmail API per-user quotas can throttle throughput further. An
account with hundreds of thousands of messages and large attachments can take
several hours, and an account with millions of messages can take even longer. Start
with `--limit` or a date range to confirm everything works before launching a
full run.

Syncs are resumable. Once the initial sync finishes, incremental syncs only pull
new and changed messages and run much faster.

### Disk usage

msgvault compresses raw MIME data with zlib, typically at a 3-5x ratio. As a
rough guide:

| Gmail usage (Settings → Storage) | SQLite DB on disk | Parquet cache | Attachments |
|---|---|---|---|
| 5 GB | ~1-2 GB | < 10 MB | varies |
| 25 GB | ~5-10 GB | < 50 MB | varies |
| 100 GB | ~20-40 GB | < 100 MB | varies |

Gmail's storage figure includes attachments at full size. Your on-disk footprint
depends on the mix of message text and attachments:

- **Message metadata and bodies.** Stored in the SQLite database at roughly
  3-5x compression.
- **Attachments.** Extracted and saved as-is, because files such as PDFs and
  images are already compressed. Identical attachments across messages are
  deduplicated by content hash.
- **Parquet analytics cache.** A lightweight projection for the Web UI and TUI,
  typically a few MB even for large archives.

Use `--limit` or a date range on your first sync to gauge the ratio for your
mailbox before committing to a full sync. After syncing, run `msgvault stats` to
see the actual sizes. See [Data Storage](/docs/architecture/storage/) for
details on compression and storage layers.

### Full sync flags

| Flag | Description |
|---|---|
| `--limit N` | Download at most N messages |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--query` | Gmail search query filter |
| `--noresume` | Start fresh instead of resuming |
| `--verbose` | Show detailed progress |

### Incremental sync

After the initial full sync finishes, switch to incremental sync for regular
updates. It uses the Gmail History API to fetch only new and changed messages:

```bash
msgvault sync you@gmail.com

# Or sync all accounts at once
msgvault sync
```

### Resumable checkpoints

If a sync stops partway through from a network error or Ctrl+C, run the same
command again to resume from the last checkpoint:

```bash
# This resumes automatically
msgvault sync-full you@gmail.com --after 2024-01-01 --before 2024-02-01
```

msgvault stores checkpoints in the `sync_checkpoints` table. Pass `--noresume`
to discard saved checkpoints and start over.

### Rate limiting

msgvault uses token bucket rate limiting to stay within Gmail API quotas. The
default is 5 requests per second, which you can adjust in `config.toml`:

```toml
[sync]
rate_limit_qps = 5
```

Lower this value if you hit rate limit errors during large syncs.

### Safety

Sync operations are **read-only**. They read your Gmail profile, labels,
message lists, raw messages, and history as needed, without calling Gmail write
APIs or changing provider data. Remote deletion is a separate workflow that you
enable explicitly.

## Explore

```bash
# Search your archive
msgvault search from:alice@example.com

# Open the analytical Web UI
msgvault serve

# Launch the interactive TUI
msgvault tui

# View stats
msgvault stats
```
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-senders.svg" alt="msgvault TUI showing the Senders view" loading="lazy">
</figure>
See [Web UI](/docs/web-ui/), [Searching](/docs/usage/searching/), and [Interactive
TUI](/docs/usage/tui/) for more.

## Optional: Turn on search and people lanes

Semantic search, visual attachment search, and the people sweep are opt-in. Put
your API keys in the environment so setup can propose search features. Hosted
people inference also requires an explicit provider and model:

```bash
export VOYAGE_API_KEY="..."     # text, people, and visual search
export OPENAI_API_KEY="..."     # text search without a Voyage key
msgvault setup providers        # review provider disclosures and write opt-in config
msgvault setup status           # what is on, what is off, and why
```

Enabling message embeddings in your config authorizes them with no separate
stored consent record. Semantic people search, visual processing, and people
sweeps each have their own consent gates, and `setup status` lists any command
still needed. The people sweep also requires an explicit hosted provider choice,
for example:

```bash
msgvault setup providers --provider openai --model <model> \
  --credential-env OPENAI_API_KEY --retention-posture <assertion> \
  --training-posture <assertion> --allow-sensitive
```

`--allow-sensitive` permits sensitive archive excerpts and sensitive personal
inferences. Having an API key in the environment alone never selects a hosted
people provider. Codex enrollment remains unavailable until an approved build
ships.

See [Recommended Configuration](/docs/usage/recommended-configuration/) for the
values `setup providers` writes and the probe steps the hosted lanes still need.

Document text search uses Docbank processing and its consent settings. See
[Docbank configuration](https://msgvault.io/docs/configuration/#search-document-text-through-docbank)
for the profile and setup steps.

## Optional: Sync Google Calendar

To archive Calendar events alongside your email, authorize Calendar access and
run a calendar sync:

```bash
msgvault add-calendar you@gmail.com
msgvault sync-calendar you@gmail.com
```

Calendar sync is read-only. You can search events with
`--message-type calendar_event`; see [Google Calendar](/docs/usage/calendar/)
for the full workflow, scheduled sync, and headless-server setup.

## Open the Web UI

Restart the daemon to keep the Web UI running until you stop it:

```bash
msgvault daemon restart
```

Open the URL it prints in your browser. If the analytical cache is missing or
stale, the daemon builds it during startup. With the default loopback bind, the
daemon trusts the local browser. If you bind the daemon to another interface,
you must configure an API key; the browser then shows a login screen and stores
only an in-memory daemon session. The release binary includes the complete UI,
so you do not need a frontend runtime or separate web files.

To run in the foreground instead, stop the daemon with `msgvault daemon stop`
and run `msgvault serve`.

On a server or NAS, prefer HTTPS at a reverse proxy and list only that proxy in
`server.trusted_proxies`. Plain HTTP is an explicit private-network
tradeoff because its session cookie cannot be marked `Secure`. See
[Web UI](/docs/web-ui/) for URL discovery, search and index states, keyboard
controls, and deployment examples.

## Optional: Sync Microsoft Teams

To archive Teams chats and channels, add Microsoft Graph permissions to your
Microsoft app registration, authorize Teams, and run a sync:

```bash
msgvault add-teams user@example.com
msgvault sync-teams user@example.com
```

You can search Teams messages with `--message-type teams`. See
[Microsoft Teams](/docs/usage/teams/) for required Graph permissions,
scheduling, and inline media backfill.

## Optional: Sync Discord

To archive Discord guild channels and threads, create a dedicated bot with
**Message Content Intent**, **View Channels**, and **Read Message History**,
then register and sync a guild:

```bash
msgvault add-discord --guild 123456789012345678
msgvault sync-discord 123456789012345678
```

You can search Discord messages with `--message-type discord`. The bot API is
guild-only and does not expose personal direct-message history. See
[Discord](/docs/usage/discord/) for least-privilege setup, scheduling, filters,
repair behavior, and attachment limits.

## Optional: Configure backups

Create a backup repository before relying on the archive as your source of
truth:

```bash
msgvault backup init --repo ~/Backups/msgvault
msgvault backup create --repo ~/Backups/msgvault
msgvault backup verify --repo ~/Backups/msgvault
```

Add the repository path to `config.toml` so future commands can omit `--repo`:

```toml
[backup]
repo = "~/Backups/msgvault"
```

See [Backup](/docs/usage/backup/) for restore, verification, scheduling, and
secret-handling details.
