---
title: Configuration
description: Configuration file reference, environment variables, and file locations.
---

## Config File

Default location:

| Platform | Path |
|---|---|
| **macOS / Linux** | `~/.msgvault/config.toml` |
| **Windows** | `C:\Users\<you>\.msgvault\config.toml` |

Override the data directory with the `MSGVAULT_HOME` environment variable or the `--home` flag (see below).

```toml
[data]
# Base data directory (default: ~/.msgvault)
data_dir = "/path/to/msgvault/data"

# Database URL (default: {data_dir}/msgvault.db; PostgreSQL DSN supported)
database_url = "/path/to/msgvault.db"

[oauth]
# Path to Google OAuth client secrets JSON for browser OAuth
client_secrets = "/path/to/client_secret.json"

# Google service account key for Workspace domain-wide delegation (optional)
# service_account_key = "/path/to/service-account.json"

# Named OAuth apps for Google Workspace orgs (optional)
[oauth.apps.acme]
client_secrets = "/path/to/acme_workspace_secret.json"
# service_account_key = "/path/to/acme_service_account.json"

[microsoft]
# Azure AD app registration client ID (required for M365)
client_id = "your-azure-app-client-id"
# tenant_id = "your-tenant-id"   # optional, default "common"

[log]
# Persistent structured file logging (opt-in)
enabled = true
# dir = "/path/to/logs"        # default: <data_dir>/logs
# level = "info"                # debug, info, warn, error
# sql_trace = false             # log every SQL query (verbose)
# sql_slow_ms = 100             # slow query threshold in ms

[sync]
# Gmail API rate limit (requests per second)
rate_limit_qps = 5

[server]
# API server settings (used by `msgvault serve`)
# api_port is optional; omit it (or set 0) to auto-select an open port that
# clients discover automatically. Set a fixed port for remote/NAS deployments.
api_port = 8080
bind_addr = "127.0.0.1"
api_key = "your-secret-key"
daemon_idle_timeout = "20m" # background daemon idle timeout; "0s" disables
daemon_auto_restart = "newer" # newer, never, or always

[analytics]
# Daemon-side analytics engine for TUI and aggregate HTTP views:
# "auto" uses DuckDB/Parquet when usable, otherwise live SQL.
# "sql" always uses live SQL. "duckdb" requires a usable Parquet cache.
engine = "auto"
auto_build_cache = true

[backup]
# Default repository for `msgvault backup`.
repo = "~/Backups/msgvault"
zstd_level = 0

[remote]
# Remote msgvault endpoint for CLI remote mode
url = "http://nas-ip:8080"
api_key = "remote-api-key"
allow_insecure = true

# Scheduled sync accounts
[[accounts]]
email = "you@gmail.com"
schedule = "0 * * * *"
enabled = true

[vector]
# Semantic and hybrid search (opt-in)
enabled = true
backend = "sqlite-vec"
# backend = "pgvector"  # with a PostgreSQL database_url and pgvector build

[vector.embeddings]
endpoint = "http://localhost:11434/v1"
model = "nomic-embed-text"
dimension = 768
eta_window = 10

[vector.preprocess]
strip_quotes = true
strip_signatures = true
strip_html = true
strip_base64 = true
strip_url_tracking = true
collapse_whitespace = true

[vector.embed.scope]
# Empty means embed the full archive. Set this for partial generations.
message_types = ["sms", "mms"]

[[synctech_sms.sources]]
name = "phone-backups"
enabled = true
backend = "drive"
folder_id = "google-drive-folder-id"
google_account = "you@gmail.com"
owner_phone = "+14155551234"
schedule = "30 4 * * *"
```

### Windows Paths

TOML treats backslashes inside double-quoted strings as escape characters. On Windows, this means native paths like `"C:\Users\you\..."` will cause a parse error.

Use one of these formats instead:

```toml
# Forward slashes (recommended)
client_secrets = "C:/Users/you/Downloads/client_secret.json"

# Single-quoted string (backslashes are literal)
client_secrets = 'C:\Users\you\Downloads\client_secret.json'
```

## Sections

### `[data]`

| Key | Default | Description |
|---|---|---|
| `data_dir` | `~/.msgvault` | Base directory for all data |
| `database_url` | `{data_dir}/msgvault.db` | SQLite database path or PostgreSQL DSN |

Attachments and OAuth tokens are stored in subdirectories of `data_dir` (`attachments/` and `tokens/` respectively). These paths are not independently configurable.

### `[oauth]`

| Key | Default | Description |
|---|---|---|
| `client_secrets` | — | Path to Google OAuth `client_secret.json` for browser OAuth flows |
| `service_account_key` | — | Path to a Google service account key JSON for Workspace domain-wide delegation |

#### `[oauth.apps.<name>]`

Named OAuth apps for Google Workspace organizations that require their own OAuth credentials. Each entry can define a separate browser OAuth `client_secret.json`, service account key, or both. Use `--oauth-app <name>` with `add-account` to bind an account to a named app.

| Key | Default | Description |
|---|---|---|
| `client_secrets` | — | Path to the org's `client_secret.json` |
| `service_account_key` | — | Path to the org's Google service account key JSON |

See [OAuth Setup: Google Workspace Accounts](/guides/oauth-setup/#google-workspace-accounts) for when and why you need named apps.

When `service_account_key` is configured, `msgvault add-account <email>` validates the delegated Gmail profile and registers the account without storing a per-user refresh token. The service account key file must be owner-only on Unix-like systems, for example `chmod 600 /path/to/service-account.json`.

### `[microsoft]`

Configuration for Microsoft 365 / Outlook.com OAuth and Microsoft Teams Graph
sync. Required only if you use `add-o365`, `add-teams`, or `sync-teams`.

| Key | Default | Description |
|---|---|---|
| `client_id` | — | Azure AD Application (client) ID (required) |
| `tenant_id` | `common` | Azure AD tenant ID; `common` allows both personal and org accounts |

See [OAuth Setup: Microsoft 365](/guides/oauth-setup/#microsoft-365-outlook-hotmail) for app registration steps. Teams uses the same `client_id` but requests Microsoft Graph scopes and stores tokens under `tokens/teams_<email>.json`; Outlook/Hotmail IMAP OAuth uses `tokens/microsoft_<email>.json`.

### `[log]`

Structured file logging. Disabled by default. Enable it to get persistent, machine-readable logs for troubleshooting. Every CLI invocation writes a unique `run_id` on every log line so you can trace a single run across shared daily log files.

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Turn on persistent file logging. Setting `dir` also enables it implicitly. |
| `dir` | `<data_dir>/logs` | Directory for log files |
| `level` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `sql_trace` | `false` | Log every SQL query at info level (verbose, for debugging) |
| `sql_slow_ms` | `100` | Threshold in ms above which SQL queries are logged at warn level. `0` uses the built-in default (100 ms). |

Log files are named `msgvault-YYYY-MM-DD.log` (UTC date), written as newline-delimited JSON. When a daily log exceeds 50 MiB it rotates to `.log.1`, `.log.2`, etc. (up to 5 rotated files).

When SQL logging is enabled, slow/error entries include query arguments and streaming query durations, which makes it easier to diagnose expensive reads without enabling full trace output.

Use `msgvault logs` to view and tail log files from the selected local or remote daemon. See [CLI Reference: logs](/cli-reference/#logs).

### `[sync]`

| Key | Default | Description |
|---|---|---|
| `rate_limit_qps` | `5` | Gmail API requests per second |

### `[server]`

Settings for the web server started by `msgvault serve`. The same HTTP server is used by remote CLI access and by the local background daemon for archive-access CLI commands. See [Web Server](/api-server/) for endpoint documentation, or fetch `/openapi.json` from a running server for the generated OpenAPI contract.

| Key | Default | Description |
|---|---|---|
| `api_port` | `0` (auto-select) | Port the server listens on; `0` picks an open port at startup and clients discover it automatically. Set a fixed port for remote/NAS deployments. |
| `bind_addr` | `127.0.0.1` | Bind address |
| `api_key` | — | API key for authentication |
| `allow_insecure` | `false` | Allow non-loopback binding without `api_key` |
| `cors_origins` | `[]` | Allowed CORS origins |
| `cors_credentials` | `false` | Allow credentials in CORS requests |
| `cors_max_age` | `0` | CORS preflight cache duration in seconds |
| `daemon_idle_timeout` | `20m` | Idle timeout for lifecycle-managed background daemons; set to `"0s"` to disable |
| `daemon_auto_restart` | `newer` | Local daemon restart policy when the CLI finds a different daemon binary version: `newer`, `never`, or `always` |

`daemon_idle_timeout` applies only to background daemons started by `msgvault serve start` or auto-started by a CLI command. Foreground `msgvault serve` keeps running until stopped. `MSGVAULT_DAEMON_IDLE_TIMEOUT` overrides the configured value for lifecycle-managed background daemons.

`daemon_auto_restart = "newer"` replaces an older compatible local daemon with the current CLI binary. Use `"never"` when another supervisor owns the daemon lifecycle, or `"always"` to restart whenever the recorded daemon version differs. Remote servers are never auto-restarted by a CLI client.

### `[analytics]`

Settings for daemon-side aggregate query behavior. The TUI, MCP server, and aggregate list commands use these settings through the local daemon or a configured remote server.

| Key | Default | Description |
|---|---|---|
| `engine` | `auto` | Aggregate engine: `auto` uses DuckDB over Parquet when the cache is usable and falls back to live SQL; `sql` always uses live SQL; `duckdb` requires a usable Parquet cache |
| `auto_build_cache` | `true` | Build a stale or missing Parquet cache before the daemon opens DuckDB for aggregate views |

Deprecated in 0.17.0: per-command analytics flags such as `msgvault tui --force-sql`, `msgvault mcp --force-sql`, `msgvault tui --no-cache-build`, and `--no-sqlite-scanner` were replaced by this daemon-level section. Use `engine = "sql"` for live SQL, `auto_build_cache = false` to skip automatic daemon cache builds, or `msgvault build-cache` to prebuild cache files on the daemon host. If `engine = "duckdb"` and the cache cannot be built or opened, `msgvault serve` fails instead of silently falling back.

### `[backup]`

Default settings for `msgvault backup`. See [Backup](/usage/backup/) for the
capture, verify, and restore workflow.

| Key | Default | Description |
|---|---|---|
| `repo` | — | Default backup repository directory used when a backup subcommand omits `--repo` |
| `zstd_level` | `0` | Compression level for backup pack files. `0` uses msgvault's built-in default; otherwise use `1` through `19` |

### `[remote]`

When set, archive-access CLI commands use the remote server by default. Without `[remote].url`, they use the local background daemon instead. Pass `--local` to use the local daemon instead of the configured remote.

| Key | Default | Description |
|---|---|---|
| `url` | — | Remote API base URL (e.g. `http://nas-ip:8080`) |
| `api_key` | — | API key used by remote commands |
| `allow_insecure` | `false` | Allow HTTP remote connections |

Affected CLI commands include `search` (FTS mode), `query`, `show-message`, `stats`, `list-accounts`, `list-senders`, `list-domains`, `list-labels`, `identity` subcommands, `collection` subcommands, `export-eml`, `export-attachment`, `export-attachments`, and `tui`.

### `[[accounts]]`

Scheduled sync accounts for the web server. Each `[[accounts]]` entry defines a cron schedule for automatic background syncing. Gmail and IMAP sources are supported; for IMAP, use the account display name/email when available rather than the raw `imaps://...` source identifier.

| Key | Default | Description |
|---|---|---|
| `email` | (required) | Account identifier or display name to sync |
| `schedule` | — | Cron expression for sync schedule (e.g., `0 * * * *`) |
| `enabled` | `true` | Whether scheduled sync is active for this account |

### SyncTech SMS Sources

Scheduled SMS Backup & Restore sources are configured with `[[synctech_sms.sources]]` entries. These are created automatically by `msgvault add-synctech-sms-drive`, but can also be edited directly.

| Key | Default | Description |
|---|---|---|
| `name` | (required) | Source name used by `sync-synctech-sms <name>` and scheduler logs |
| `enabled` | `true` | Whether the source is active |
| `backend` | `local` | `local` for a path on disk, or `drive` for Google Drive |
| `path` | — | Local XML/ZIP file or directory when `backend = "local"` |
| `folder_id` | — | Google Drive folder ID when `backend = "drive"` |
| `google_account` | — | Google account used for Drive access |
| `owner_phone` | (required) | Owner phone number in E.164 format |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `include_sms` | `true` | Import SMS records |
| `include_mms` | `true` | Import MMS records |
| `include_calls` | `true` | Import call logs |
| `include_attachments` | `true` | Import MMS attachments |
| `stable_after` | `10m` | How long Drive files must remain unchanged before import |
| `oauth_app` | — | Named Google OAuth app to use |

### Google Calendar Sources

Scheduled Google Calendar sync is configured with top-level `[[gcal]]` entries. Each entry is one OAuth account; `msgvault serve` runs it on the given cron schedule (first run full-syncs and registers calendars, later runs are incremental). Authorize the account first with `msgvault add-calendar`.

```toml
[[gcal]]
name = "primary"                 # optional; defaults to email
email = "you@gmail.com"          # OAuth account = token key
oauth_app = ""                   # optional named OAuth app
calendars = []                   # optional calendarId filter; empty = owner+writer
schedule = "0 */6 * * *"         # 5-field cron, no seconds
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `name` | email | Source name used by `sync-calendar <name>` and scheduler logs |
| `email` | (required) | Google account that owns the token (the token key) |
| `oauth_app` | — | Named Google OAuth app to use |
| `calendars` | — | Specific calendar IDs to sync; empty syncs owned/writable calendars |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `enabled` | `false` | Whether the source is daemon-scheduled |

### `[beeper]`

Archive chats from a locally running [Beeper Desktop](/usage/beeper/). A single
block (not a list): the Beeper Desktop API is loopback-only, so there is one
instance per machine and the daemon must run beside it. Authorize first with
`msgvault add-beeper`.

```toml
[beeper]
# url = "http://localhost:23373"  # Beeper Desktop API (default)
enabled = true                    # gate for the daemon schedule
schedule = "*/30 * * * *"         # 5-field cron; empty = manual sync only
accounts = []                     # accountID include filter (empty = all)
exclude_accounts = []             # skip networks archived natively, e.g. ["whatsapp"]
rate_limit_qps = 20               # request rate against the local API
media = true                      # download attachment bytes
max_media_mb = 100                # per-attachment download cap (MiB)
```

| Key | Default | Description |
|---|---|---|
| `url` | `http://localhost:23373` | Beeper Desktop API base URL |
| `enabled` | `false` | Whether the daemon schedules Beeper sync |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `accounts` | all | Beeper accountIDs to sync (include filter) |
| `exclude_accounts` | — | Beeper accountIDs to skip (wins over `accounts`) |
| `rate_limit_qps` | `20` | Request rate limit against the local API |
| `media` | `true` | Download attachment bytes (failed downloads retry via `backfill-beeper-media`) |
| `max_media_mb` | `100` | Per-attachment download cap in MiB (over-cap media leaves a retry marker) |

### Granola Sources

Granola meeting-notes sync is configured with top-level `[[granola]]` entries.
Each entry is one Granola account. `identifier` is a stable source label;
`account_email` is the primary identity used for organizer attribution.
`msgvault serve` runs it on the given cron schedule. Register the account
first with `msgvault add-granola`. See
[Meeting Transcripts](/usage/meetings/).

```toml
[[granola]]
identifier = "work"              # stable label; defaults to "default" for a single entry
account_email = "you@example.com" # required primary account identity
api_key = "grn_..."              # from the desktop app's settings (Business plan)
schedule = "0 */6 * * *"         # 5-field cron, no seconds
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Source name used by `sync-granola <identifier>` and scheduler logs |
| `account_email` | (required) | Normalized primary account identity used for `is_from_me` |
| `api_key` | (required) | Granola API key (`grn_…`) |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `enabled` | `false` | Whether the source is daemon-scheduled |

Config loading preserves `identifier` and rejects labels without an effective
email, instructing you to add `account_email`. `msgvault add-granola` confirms
the primary identity even if aliases already exist. Manage aliases with
`msgvault identity add <identifier> <email>`, then run
`msgvault sync-granola <identifier> --full` after identity changes to repair
existing meeting attribution. A scheduled source must still be registered in
the archive; removing it prevents the scheduler from silently recreating it.

### Circleback Sources

Circleback meeting sync is configured with top-level `[[circleback]]`
entries. Authentication is browser OAuth (`msgvault add-circleback`); no
secret lives in the config file. See
[Meeting Transcripts](/usage/meetings/).

```toml
[[circleback]]
identifier = "work"              # stable label/token key; defaults to "default" for one entry
account_email = "you@example.com" # required primary account identity
schedule = "30 */6 * * *"        # 5-field cron, no seconds
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Source name used by `sync-circleback <identifier>`, the token filename, and scheduler logs |
| `account_email` | (required) | Normalized primary account identity used for `is_from_me` |
| `endpoint` | production | MCP endpoint override (testing only) |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `enabled` | `false` | Whether the source is daemon-scheduled |

Config loading and alias repair follow the same rules as Granola: preserve the
label, add `account_email`, manage aliases with `msgvault identity`, and run
`msgvault sync-circleback <identifier> --full` after identity changes.
Circleback OAuth always confirms the primary identity; there is no identity
opt-out flag.

### `[vector]`

Top-level toggle and backend marker for semantic/hybrid search. SQLite vector search requires a build with `sqlite_vec` support (default via `make build`). PostgreSQL vector search requires a build with the `pgvector` tag and a PostgreSQL `[data].database_url`. See [Vector Search](/usage/vector-search/) for prerequisites, initial embedding, and the full workflow.

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Turn on vector and hybrid search. When `false`, `mode=vector` and `mode=hybrid` return `vector_not_enabled`. |
| `backend` | `sqlite-vec` | Backend marker. Supported values are `sqlite-vec` and `pgvector`; the concrete backend is selected from `[data].database_url`. |
| `db_path` | `<data_dir>/vectors.db` | SQLite vector database path. Ignored by the PostgreSQL pgvector backend. |
| `skip_extension_create` | `false` | PostgreSQL only. Skip `CREATE EXTENSION IF NOT EXISTS vector` when pgvector is already installed by an administrator. |

#### `[vector.embeddings]`

External OpenAI-compatible embedding endpoint used to convert message text into vectors. msgvault does not host a model; it calls the endpoint you configure. Use a local or self-hosted endpoint (Ollama, llama.cpp `server`, LM Studio, etc.) when message text must stay on your machine or network. Hosted endpoints also work but receive the text being embedded.

| Key | Default | Description |
|---|---|---|
| `endpoint` | (required) | HTTP(S) base URL for an OpenAI-compatible embeddings API. msgvault appends `/embeddings` (for example, set `http://localhost:11434/v1`, not `.../embeddings`). |
| `model` | (required) | Model name to pass in each request (e.g., `nomic-embed-text`). |
| `dimension` | (required) | Vector dimension. Must match the model's output dimension. |
| `api_key_env` | — | Name of an environment variable containing the API key. Omit for anonymous endpoints. |
| `batch_size` | `32` | Embedding inputs per HTTP call. Long messages can contribute multiple chunk inputs. |
| `timeout` | `30s` | Per-request timeout. |
| `max_retries` | `3` | Retries per batch on transient failures. |
| `max_input_chars` | `32768` | Character cap per embedding chunk. Set below your model's context window (e.g., `2000` for Ollama's default `nomic-embed-text`). |
| `eta_window` | `10` | Number of recent progress samples used for ETA smoothing. |

The index generation fingerprint includes the model, dimension, preprocessing settings, `max_input_chars`, and embedding policy. Changing those settings triggers a stale-index error on the next vector/hybrid query until you run `msgvault embeddings build --full-rebuild`.

#### `[vector.preprocess]`

Controls text normalization before embedding.

| Key | Default | Description |
|---|---|---|
| `strip_quotes` | `true` | Drop quoted reply blocks (`> ...` lines, reply preambles) before embedding. |
| `strip_signatures` | `true` | Drop trailing signature blocks (content after `-- `). |
| `strip_html` | `true` | Convert HTML-only bodies to text and remove HTML markup before embedding. |
| `strip_base64` | `true` | Remove base64/data blobs before HTML stripping so encoded data does not crowd out prose. |
| `strip_url_tracking` | `true` | Remove common tracking parameters such as `utm_*`, `fbclid`, and `gclid` from URLs. |
| `collapse_whitespace` | `true` | Normalize repeated horizontal whitespace and blank lines. |

#### `[vector.search]`

Hybrid ranking parameters applied at query time.

| Key | Default | Description |
|---|---|---|
| `rrf_k` | `60` | Reciprocal Rank Fusion constant. Higher values flatten score differences between signals. |
| `k_per_signal` | `100` | Candidate pool size drawn from each signal (BM25 or vector) before fusion. |
| `subject_boost` | `2.0` | Multiplier applied when a query term matches a message's subject line. |
| `max_page_size_hybrid` | `50` | Hard cap on `page_size` for vector/hybrid responses. Set to `0` to disable clamping. |

#### `[vector.embed.scope]`

Optional scope for newly built embedding generations. The zero value embeds the
full archive. A scoped generation embeds only matching `messages.message_type`
values:

```toml
[vector.embed.scope]
message_types = ["teams"]
```

Scoped generations are intentionally partial. Vector and hybrid queries against
a scoped index must include a compatible `message_type` filter, such as
`msgvault search "release planning" --mode hybrid --message-type teams`; an
unscoped vector/hybrid query returns `index_scope_mismatch` instead of using the
partial index as if it covered the full archive.

#### `[vector.embed.schedule]`

Optional background scheduling for the embed worker inside `msgvault serve`. Empty config disables scheduled embedding; you can still run `msgvault embeddings build` by hand.

| Key | Default | Description |
|---|---|---|
| `cron` | — | 5-field cron expression. Empty string disables the standalone cron. |
| `run_after_sync` | `false` | When `true`, an embed pass runs after every successful scheduled sync. |

## Overriding the Home Directory

By default, msgvault stores everything under `~/.msgvault` (macOS/Linux) or `C:\Users\<you>\.msgvault` (Windows). To use a different location, you have two options:

**`--home` flag** (per-command):
```bash
msgvault sync --home /mnt/data/msgvault
```

**`MSGVAULT_HOME` environment variable** (persistent):
```bash
export MSGVAULT_HOME=/mnt/data/msgvault
```

Both options are equivalent: `config.toml` is loaded from the specified directory, and all data (database, tokens, attachments) is stored there. The `--home` flag takes priority over `MSGVAULT_HOME`.

The home or `[data].data_dir` directory may be a symlink to an existing
directory. Local daemon bookkeeping resolves the symlink and applies its
ownership and permission checks to the target directory.

## Environment Variables

| Variable | Description |
|---|---|
| `MSGVAULT_HOME` | Base directory for all data (default: `~/.msgvault`) |
| `MSGVAULT_REMOTE_URL` | Remote URL for `export-token` (flag > env > config) |
| `MSGVAULT_REMOTE_API_KEY` | Remote API key for `export-token` (flag > env > config) |

## File Locations

All data lives under the msgvault home directory (`~/.msgvault` on macOS/Linux, `C:\Users\<you>\.msgvault` on Windows). The directory is created automatically on first use.

| File | Description |
|---|---|
| `config.toml` | Configuration file |
| `msgvault.db` | SQLite database (system of record when PostgreSQL is not configured) |
| `attachments/` | Content-addressed attachment files |
| `tokens/` | OAuth tokens per account |
| `logs/` | Structured log files (when [file logging](/configuration/#log) is enabled) |
| `analytics/` | Parquet cache files for TUI |
