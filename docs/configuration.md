---
last_edited: 2026-09-03
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

# Keep attachment content as individual files instead of creating packs.
# loose_attachments = true

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
# redirect_uri = "http://localhost:8089/callback/microsoft"  # default
# tenant_id = "your-tenant-id"   # optional, default "common"

# Optional source-scoped Fastmail alias inventory.
[[fastmail]]
source_id = 14
api_token = "replace-with-a-Fastmail-API-token"
auto_confirm_identities = false

[discord]
# Per-attachment download cap (default: 50 MiB)
max_media_bytes = 52428800
# Skip attachments from rooms with more than this many participants
# (default: 20; 0 = no cap). Shared by [beeper], [slack], and [teams].
media_max_participants = 20
# Trailing edit/delete/reaction repair window (default: seven days)
edit_rescan_window = "168h"

[discord.guilds."123456789012345678"]
# Channel, thread, and forum-post IDs; empty include means all accessible.
include = ["456789012345678901"]
exclude = ["567890123456789012"]

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
# API server settings (used by `msgvault serve` and `msgvault daemon`)
# api_port is optional; omit it (or set 0) to auto-select an open port that
# clients discover automatically. Set a fixed port for remote/NAS deployments.
api_port = 0
bind_addr = "127.0.0.1"
api_key = "your-secret-key"
daemon_idle_timeout = "20m" # background daemon idle timeout; "0s" disables
daemon_auto_restart = "newer" # newer, never, or always

[analytics]
# Daemon-side analytics engine for Web UI, TUI, and aggregate HTTP views:
# "auto" starts on live SQL and switches to DuckDB after cache maintenance.
# "sql" always uses live SQL. "duckdb" requires a usable Parquet cache.
engine = "auto"
# Build a stale/missing cache during daemon startup and after scheduled syncs.
auto_build_cache = true
# Minimum age of a usable cache before a scheduled sync may rebuild it again.
# min_rebuild_interval = "6h"

[backup]
# Default repository for `msgvault backup`.
repo = "~/Backups/msgvault"
zstd_level = 0

[deletion]
# Durable consent for remote deletion execution. Opt in deliberately;
# defaults to false.
remote_enabled = false

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
document_prefix = "search_document: "
query_prefix = "search_query: "
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
# Use stable account identifiers, not numeric source IDs. This keeps a scoped
# generation usable after a daemon restart.
# accounts = ["you@work.example"]

[attachments.documents]
# Hosted extraction is opt-in and requires a separately recorded consent.
enabled = false
provider = "mistral"
region = "eu"
api_key_env = "MISTRAL_API_KEY"
model = "mistral-ocr-4-0"
retention_posture = "zdr"
training_posture = "opted-out"
max_file_bytes = 52428800
max_pages_per_document = 500
max_response_bytes = 67108864
max_normalized_chars = 25000000
max_spool_bytes = 536870912
min_free_space_bytes = 1073741824
request_timeout = "5m"
max_retries = 3
max_pages_per_run = 10000
max_estimated_cost_usd_per_run = 50
# Set both pricing fields together to include a cost estimate in manual build preflight.
# estimated_cost_usd_per_1000_units = 0.001
# pricing_assumption_on = "2026-08-17"

[attachments.documents.scope]
# Empty includes every supported message type.
message_types = ["email"]

[attachments.documents.index]
lexical = true
store_chunk_text = true

[[synctech_sms.sources]]
name = "phone-backups"
enabled = true
backend = "drive"
folder_id = "google-drive-folder-id"
google_account = "you@gmail.com"
owner_phone = "+14155551234"
schedule = "30 4 * * *"
```

## Remote Deletion Consent

Starting in v0.20.0, remote deletion remains permanently opt-in. The invoking
CLI may enable it durably with `[deletion] remote_enabled = true` or for one
command with `MSGVAULT_ENABLE_REMOTE_DELETE=1`. Both mechanisms are permanent;
there is no planned automatic removal of the guardrail.

Consent belongs to the invoking CLI. When a command uses a remote daemon, the
CLI forwards its effective consent for that operation; the remote daemon's own
`[deletion]` section is not server policy for a command invoked elsewhere.
Staging, listing, inspecting, and dry-running deletion batches remain ungated.

## People sweep inference

People sweeps use one named protocol profile at a time. A profile records the
exact endpoint, model, wire protocol, negotiated output mode, privacy posture,
and source scope. It is configuration, not a provider preset. Msgvault never
changes the active profile or switches providers automatically.

```toml
[people.sweep]
enabled = true
provider = "glm"

[people.sweep.providers.glm]
protocol = "openai_chat"
endpoint = "https://api.z.ai/api/paas/v4"
model = "glm-5.3"
auth = "bearer"
credential = "env"
credential_env = "ZAI_API_KEY"
output_mode = "prompt_json"
token_limit_parameter = "max_tokens"
reasoning_effort = "max"
request_timeout = "1m"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text", "meeting_text"]
source_since = "2026-01-01"
allow_sensitive = true
```

`allow_sensitive = true` is required for real sweeps: any packet that
carries seed or context evidence is marked sensitive, so a profile set to
`false` fails on every real sweep. `true` permits sending that verbatim
archive text to the selected provider; `false` leaves only the synthetic
capability check, which sends no archive text.

Usable protocols are `openai_chat`, `openai_responses`,
`anthropic_messages`, and `google_generate_content`. A fifth protocol,
`codex_app_server`, is defined but release-gated and cannot run yet; see the
Codex app-server profiles section below. Onboarding negotiates and saves
`native_json_schema`, `json_object`, or `prompt_json`. OpenAI Chat profiles
also save either `max_completion_tokens` or `max_tokens`; the other protocols
use their defined token-limit field.

These are examples of protocol profiles, not built-in presets:

| Example profile | Protocol | Typical profile choice |
|---|---|---|
| GLM 5.3 | `openai_chat` | Z.AI API base, `glm-5.3`, often `max_tokens` |
| Kimi K3 | `openai_chat` or `anthropic_messages` | Choose the exact API surface the account exposes |
| OpenRouter | `openai_chat` | OpenRouter API base and one explicit routed model ID |
| Venice | `openai_chat` | Venice API base and one explicit model ID |
| open-agent-api | `openai_chat` | The gateway's loopback API base and exposed model ID |
| Gemini | `google_generate_content` | Google API base and one Gemini model ID |
| Anthropic | `anthropic_messages` | Anthropic API base and one Claude model ID |
| OpenAI Responses | `openai_responses` | OpenAI API base and one Responses model ID |

Confirm current endpoints, model identifiers, privacy terms, and subscription
rules with the selected operator before saving a profile. OpenRouter and Venice
may route a request to another upstream operator, so the profile's retention
and training declarations must cover that full path. Logged-in or
subscription-backed endpoints, including local gateways, must be used within
their provider terms.

Credentials are not stored in this TOML. `credential = "stored"` keeps a
profile-specific secret under the private tokens directory and is supported
on Linux and macOS only; `credential = "env"` stores only the selected
environment-variable name and works everywhere.
`credential = "none"` is restricted to credentialless local or Codex paths.
Changing a credential value does not change the profile fingerprint, but
changing its source or reference does.

Only `msgvault person provider add` may contact models.dev, and only when a
transport field (`--protocol`, `--endpoint`, `--model`, `--auth`) is missing
or `--accept-catalog-prices` is set; it sends no archive data or provider
credential. `--custom` skips that catalog entirely; the required synthetic
check still contacts the endpoint selected in the profile. The catalog is never used
by scheduled or manual sweeps. A catalog suggestion also never chooses where a
credential is sent: onboarding pairs a credential only with an endpoint you
passed explicitly via `--endpoint` or with the first-party API hosts compiled
into msgvault, so a compromised catalog cannot redirect your key. A successful check does not grant consent:
`msgvault person provider consent <name> --yes` is a separate explicit step.
Live credential checks are optional developer or operator verification and are
never CI requirements.

### Codex app-server profiles

The `codex_app_server` protocol is not usable in this release. Its transport
stays unavailable until the executable isolation gate releases a verified
build, and until then every Codex operation fails closed with
`codex app-server isolation is not released`. The profile shape is documented
here so the configuration is ready when the gate ships.

`codex_app_server` profiles are also the one protocol `person provider add`
cannot create: generic onboarding negotiates HTTP capabilities through an
endpoint, while codex_app_server has no endpoint to negotiate against and
runs through an attested local Codex executable instead. Configure the
profile manually, then authorize it with the device-code login:

```toml
[people.sweep.providers.codex]
protocol = "codex_app_server"
model = "gpt-5.3-codex"
auth = "none"
credential = "none"
reasoning_effort = "medium"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2026-01-01"
allow_sensitive = true
```

`endpoint` is not allowed, and `auth` and `credential` must both be set to
`"none"`: the transport is the local Codex app server, authenticated by its
own ChatGPT login. Sensitivity policy is protocol-agnostic: real Codex sweeps
send the same seed and context packets, so `allow_sensitive = true` is
required here as well. After saving
the profile, run `msgvault person provider login` to complete the device-code
authorization, review `msgvault person provider models` for the exact model
identifiers and reasoning efforts your subscription exposes, then run
`msgvault person provider check codex` and
`msgvault person provider consent codex --yes` as with any other profile.

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
| `loose_attachments` | `false` | Keep attachments as loose files and reject pack/repack commands instead of creating immutable packs |

Attachments and OAuth tokens are stored in subdirectories of `data_dir` (`attachments/` and `tokens/` respectively). These paths are not independently configurable.

Setting `loose_attachments = true` prevents new pack files but does not
convert existing packs. Stop the daemon and run `msgvault unpack-attachments`
once to materialize their contents as loose files. Backup restore also restores
attachments loose while this setting is enabled.

### `[attachments.documents]`

Hosted extraction and local full-text indexing for standalone document
attachments. It is disabled by default. Enabling it does not grant consent or
send data: an operator must generate an authenticated capability manifest and
record consent for the exact effective policy before a build can upload a
document.

| Key | Default | Description |
|---|---:|---|
| `enabled` | `false` | Allow explicit document extraction commands |
| `provider` | `mistral` | Pinned extraction provider |
| `region` | `eu` | Pinned provider region and EU endpoint |
| `api_key_env` | `MISTRAL_API_KEY` | Environment variable containing the provider key |
| `model` | `mistral-ocr-4-0` | Pinned OCR model |
| `retention_posture` | `unknown` | Confirmed provider posture: `standard` or `zdr` |
| `training_posture` | `unknown` | Confirmed provider posture: `default-opt-out` or `opted-out` |
| `max_file_bytes` | `52428800` | Maximum original document size (50 MiB) |
| `max_pages_per_document` | `500` | Maximum provider units for one document |
| `max_response_bytes` | `67108864` | Maximum provider response size (64 MiB) |
| `max_normalized_chars` | `25000000` | Maximum locally retained normalized characters |
| `max_spool_bytes` | `536870912` | Maximum private staging-directory usage (512 MiB) |
| `min_free_space_bytes` | `1073741824` | Free space preserved before staging (1 GiB) |
| `request_timeout` | `5m` | Timeout for each provider request attempt |
| `max_retries` | `3` | Maximum transient retries |
| `max_pages_per_run` | `10000` | Conservative provider-unit budget for one run |
| `max_estimated_cost_usd_per_run` | `50` | Cost-planning ceiling for one run |
| `estimated_cost_usd_per_1000_units` | `0` | Operator-supplied current price assumption; zero disables cost calculation |
| `pricing_assumption_on` | — | Date for the price assumption, in `YYYY-MM-DD` form |
Provider uploads are manual-only: `msgvault serve` does not schedule document
extraction. Each `documents build` or `documents resume` receives its capability
manifest explicitly and displays its upload and cost preflight before requiring
`--yes`. When document indexing is enabled, the daemon's weekly reconciliation
and local derivative cleanup remain automatic and make no provider requests.

`[attachments.documents.scope]` accepts `message_types`; an empty list includes
all supported standalone attachment sources. The first release requires
`[attachments.documents.index].lexical = true` and `store_chunk_text = true`.
Hosted document embeddings are not enabled by this configuration.

See [Document Attachment Indexing](/docs/usage/document-indexing/) for the complete
probe, consent, build, and recovery flow.

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

See [OAuth Setup: Google Workspace Accounts](/docs/guides/oauth-setup/#google-workspace-accounts) for when and why you need named apps.

Discord's `--oauth-app` value is only a protected bot-token binding label. It
is not resolved from this section and does not require an `[oauth.apps]` entry.
`export-messages` uses the same daemon and database configuration as other
archive commands. It does not load provider credentials or make provider API
calls. The older `export-discord` compatibility command has the same read-only
provider behavior.

When `service_account_key` is configured, `msgvault add-account <email>` validates the delegated Gmail profile and registers the account without storing a per-user refresh token. The service account key file must be owner-only on Unix-like systems, for example `chmod 600 /path/to/service-account.json`.

### `[microsoft]`

Configuration for Microsoft 365 / Outlook.com OAuth and Microsoft Teams Graph
sync. Required only if you use `add-o365`, `add-teams`, or `sync-teams`.

| Key | Default | Description |
|---|---|---|
| `client_id` | — | Azure AD Application (client) ID (required) |
| `redirect_uri` | `http://localhost:8089/callback/microsoft` | OAuth redirect URI registered in the Azure AD app |
| `tenant_id` | `common` | Azure AD tenant ID; `common` allows both personal and org accounts |

See [OAuth Setup: Microsoft 365](/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail) for app registration steps. Teams uses the same `client_id` but requests Microsoft Graph scopes and stores tokens under `tokens/teams_<email>.json`; Outlook/Hotmail IMAP OAuth uses `tokens/microsoft_<email>.json`.

### `[[fastmail]]`

Optional source-scoped Fastmail JMAP identity inventory. This does not replace
IMAP ingestion credentials: add and sync the mailbox normally, then use the API
token only to discover masked and send-as addresses that belong to that source.

| Key | Default | Description |
|---|---|---|
| `source_id` | — | Positive numeric archive source ID; mutually exclusive with `account` |
| `account` | — | Unambiguous source identifier or display name; mutually exclusive with `source_id` |
| `api_token` | (required) | Fastmail API token used for the JMAP identity inventory |
| `auto_confirm_identities` | `false` | Refresh and apply strong provider identity evidence after successful mailbox syncs |

Exactly one source selector is required. Prefer `source_id` when two sources
share an identifier or display name. With automatic confirmation disabled,
`msgvault identity discover --source-id <id> --provider` fetches the inventory
for an explicit preview; add `--apply` only after reviewing it. See [People,
Profiles, and Source Identities](/docs/usage/people/#fastmail-alias-inventory).

### `[discord]`

Provider-wide Discord import settings and optional message-container filters.
Register guilds and store their bot credential first with `msgvault
add-discord`; tokens and binding labels do not belong in `config.toml`.

| Key | Default | Description |
|---|---|---|
| `max_media_bytes` | `52428800` (50 MiB) | Maximum size of one Discord attachment downloaded during sync or backfill |
| `max_media_mb` | — | Same cap in MiB; when set it takes precedence over `max_media_bytes` |
| `media` | `true` | Download attachment bytes at all |
| `media_scope` | `all` | Which conversations collect media: `all`, `direct` (direct and group chats only), or `none` |
| `media_max_participants` | `20` | Skip media from conversations with more participants than this; `0` disables the cap. See [Media policy](#media-policy) |
| `edit_rescan_window` | `168h` (seven days) | Trailing per-channel/thread window refreshed for edits, deletions, and reaction summaries |

Use an exact guild ID for a per-guild filter block. The same block also takes
the per-account media overrides (`media`, `max_media_mb`) that the other chat
providers put under `accounts_config`:

```toml
[discord.guilds."123456789012345678"]
include = ["456789012345678901"]
exclude = ["567890123456789012"]
# media = false
# max_media_mb = 25
```

An empty `include` means every accessible text or announcement channel, thread,
and forum post. Top-level channels match directly. A child inherits its
parent's state unless its own ID appears explicitly. An explicit child include
can override an excluded parent; an explicit child exclude can override an
included parent. `exclude` wins when the same ID is in both lists. See
[Discord](/docs/usage/discord/#configure-media-repairs-and-channel-filters).

### Media policy

`[beeper]`, `[slack]`, `[discord]`, and `[teams]` share one attachment policy
vocabulary. It decides which chat media is downloaded during sync and backfill;
message text is always archived.

| Key | Default | Description |
|---|---|---|
| `media` | `true` | Download attachment bytes. `false` archives messages without their media and records a `policy_scope` skip marker |
| `media_scope` | `all` | `all` collects from every conversation; `direct` collects only from direct and group chats (not channels, rooms, or guild channels); `none` collects nothing |
| `media_max_participants` | `20` | Skip media from conversations with more participants than this. Omitting the key applies the default; an explicit `0` removes the cap |
| `max_media_mb` | `250` (Discord `50`) | Per-attachment size cap in MiB. Sized for long voice notes, screen recordings, and phone video from direct chats now that the participant cap keeps large-room volume out |
| `accounts_config` | — | Per-account overrides of `media` and `max_media_mb`, keyed by Beeper accountID, Slack team ID, or Teams account email. Discord uses `[discord.guilds."<id>"]` instead |

The participant cap exists because most attachment bytes in a real chat
archive come from large rooms whose forwarded videos nobody wants kept. Direct
chats and small groups keep their photos, voice notes, and files. A skipped
occurrence is recorded with a typed marker (`participant_threshold`,
`policy_scope`, `account_policy`, or `size_cap`) that distinguishes a
deliberate skip from a failed download, so the `backfill-*-media` commands do
not retry it unless the policy changes.

```toml
[beeper]
media_scope = "all"
media_max_participants = 20
max_media_mb = 250

# Keep everything from one account regardless of room size or size cap.
[beeper.accounts_config.signal]
media = true
max_media_mb = 500

# Never download from another account.
[beeper.accounts_config.telegram]
media = false
```

Policy changes apply to future downloads. Media already stored under an
earlier policy stays until you run `msgvault purge-excluded-media`, which
removes attachment bytes the current policy would no longer collect.

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

Use `msgvault logs` to view and tail log files from the selected local or remote daemon. See [CLI Reference: logs](/docs/cli-reference/#logs).

### `[sync]`

| Key | Default | Description |
|---|---|---|
| `rate_limit_qps` | `5` | Gmail API requests per second |

### `[server]`

Settings for the Web UI and API server started by `msgvault serve`. The same HTTP server is used by remote CLI access and by the local background daemon for archive-access CLI commands. The `api_key` setting is also reused for inbound bearer authentication when `msgvault mcp --http` starts a separate Streamable HTTP listener; that listener's address comes from the `--http` flag. See [Web UI & API Server](/docs/api-server/) for API endpoint documentation and [MCP Server](/docs/usage/chat/#streamablehttp-transport) for MCP client setup, or fetch `/openapi.json` from a running server for the generated OpenAPI contract.

| Key | Default | Description |
|---|---|---|
| `api_port` | `0` (auto-select) | Port the server listens on; `0` picks an open port at startup and clients discover it automatically. Set a fixed port for remote/NAS deployments. |
| `bind_addr` | `127.0.0.1` | Bind address |
| `api_key` | — | API key for daemon/API authentication and bearer authentication on `msgvault mcp --http` |
| `allow_insecure` | `false` | Allow non-loopback binding without `api_key` |
| `cors_origins` | `[]` | Allowed CORS origins |
| `cors_credentials` | `false` | Allow credentials in CORS requests |
| `cors_max_age` | `0` | CORS preflight cache duration in seconds |
| `trusted_proxies` | `[]` | IP addresses or CIDRs allowed to supply forwarded HTTPS/host headers |
| `daemon_idle_timeout` | `20m` | Idle timeout for lifecycle-managed background daemons; set to `"0s"` to disable |
| `daemon_auto_restart` | `newer` | Local daemon restart policy when the CLI finds a different daemon binary version: `newer`, `never`, or `always` |

`daemon_idle_timeout` applies only to background daemons started by `msgvault daemon start` or auto-started by a CLI command. Foreground `msgvault serve` keeps running until stopped. `MSGVAULT_DAEMON_IDLE_TIMEOUT` overrides the configured value for lifecycle-managed background daemons.

`daemon_auto_restart = "newer"` replaces an older compatible local daemon with the current CLI binary. Use `"never"` when another supervisor owns the daemon lifecycle, or `"always"` to restart whenever the recorded daemon version differs. Remote servers are never auto-restarted by a CLI client.

Browser sessions are additive to API-key authentication. Existing CLI and
programmatic clients continue to send the configured key. For remote browser
access, terminate TLS at a reverse proxy and list that proxy—not arbitrary
clients—in `trusted_proxies`. See [Web UI](/docs/web-ui/) for the complete security
model and the plain-HTTP warning.

For MCP Streamable HTTP, send `[server].api_key` as `Authorization: Bearer
<key>` on every `/mcp` request. This inbound credential is independent of
`[remote].api_key`, which authenticates `msgvault mcp` when it connects to a
remote daemon.

### `[web]`

Defaults for the daemon-served browser application. These values can also be
changed from Settings; `config.toml` remains authoritative.

| Key | Default | Description |
|---|---|---|
| `default_search_mode` | `full_text` | Initial mode: `full_text`, `semantic`, or `hybrid` |
| `theme` | `system` | Color theme: `system`, `light`, or `dark` |
| `density` | `compact` | Table density: `compact` or `comfortable` |

Browser-managed settings are validated and written with optimistic concurrency.
They are restart-required unless the UI explicitly says otherwise; a pending
restart banner means the file is saved but the running daemon still has its old
value. Changing `server.api_key` requires a confirmation and takes effect only
after restart, which also invalidates browser sessions.

### `[integrations.tasks]`

Optional provider-neutral task integration:

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Enable discovery and capability checks |
| `endpoint` | — | Explicit loopback HTTP, Unix socket, or HTTPS endpoint; empty requests secure local discovery |
| `api_key` | — | Server-side credential; never returned to the browser |
| `default_project` | `msgvault` | Fixed project used for create/link/search operations |

Remote plaintext HTTP is rejected. An endpoint is usable only when it supports
the required idempotency and compare-and-swap capabilities; the UI distinguishes
disabled, authentication required, incompatible, partial, stale, unavailable,
and ready states.

### `[analytics]`

Settings for daemon-side aggregate query behavior. The Web UI, TUI, MCP server, and aggregate list commands use these settings through the local daemon or a configured remote server.

| Key | Default | Description |
|---|---|---|
| `engine` | `auto` | Aggregate engine: `auto` starts with live SQL and switches to DuckDB after cache maintenance succeeds; `sql` always uses live SQL; `duckdb` requires a usable Parquet cache |
| `auto_build_cache` | `true` | Build a stale or missing Parquet cache during daemon startup and after scheduled syncs; `false` skips both automatic paths |
| `min_rebuild_interval` | `0s` | Minimum age of a usable cache before a scheduled sync may rebuild it; zero preserves rebuilding after each sync |

The daemon starts HTTP health and API routing before analytics cache
maintenance. With `engine = "duckdb"`, analytics remain unavailable until a
usable cache is ready; if the cache cannot be built or opened, `msgvault serve`
fails instead of silently falling back. With `auto_build_cache = false`, use
`msgvault build-cache` for explicit cache maintenance. Deprecated in 0.17.0:
per-command analytics flags such as `msgvault tui --force-sql`,
`msgvault mcp --force-sql`, `msgvault tui --no-cache-build`, and
`--no-sqlite-scanner` were replaced by this daemon-level section. Use
`engine = "sql"` to force live SQL.

`min_rebuild_interval` limits only automatic post-sync rebuilds. A busy archive
can therefore serve Parquet analytics that lag SQLite by approximately the
configured interval plus cache build time. Explicit `msgvault build-cache`
requests, startup maintenance, query-required builds, and recovery of an
absent, interrupted, incompatible, or otherwise unusable cache are not delayed.
Cache build memory and temporary disk usage scale with archive size, so a
minimum interval can prevent repeated archive-scale work when sources sync
frequently. Changes under `[analytics]` take effect after the daemon restarts.

This setting governs the aggregate views (Senders/Domains/Labels/Time) and is ignored entirely when `[data].database_url` points at PostgreSQL — a PostgreSQL backend always uses live SQL for those views, and `build-cache` refuses to run against it. It does not affect the Web UI's Explore, Files, or People/domains workspaces, which require the SQLite + DuckDB/Parquet cache regardless of this setting and are unavailable on PostgreSQL; see [PostgreSQL Backend](/docs/architecture/postgresql/) for the current scope.

### `[backup]`

Default settings for `msgvault backup`. See [Backup](/docs/usage/backup/) for the
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

Scheduled sync sources for the web server. Each `[[accounts]]` entry defines a
cron schedule for automatic background syncing. Gmail, IMAP, Microsoft Teams,
and Discord sources are supported. For IMAP or Teams, use the account display
name/email when available rather than a raw provider identifier. Discord
schedules must use the exact guild ID because guild display names are mutable
and may be duplicated.

| Key | Default | Description |
|---|---|---|
| `email` | (required) | Account identifier/display name, or exact Discord guild ID, to sync |
| `schedule` | — | Cron expression for sync schedule (e.g., `0 * * * *`) |
| `enabled` | `true` | Whether scheduled sync is active for this account |

For example, schedule one previously registered Discord guild independently:

```toml
[[accounts]]
email = "123456789012345678"
schedule = "*/30 * * * *"
enabled = true
```

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

Archive chats from a locally running [Beeper Desktop](/docs/usage/beeper/). A single
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
media_scope = "all"               # all, direct, or none
media_max_participants = 20       # skip media from larger rooms; 0 = no cap
max_media_mb = 250                # per-attachment download cap (MiB)

# [beeper.accounts_config.signal]  # per-account override, keyed by accountID
# media = true
# max_media_mb = 500
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
| `media_scope` | `all` | `all`, `direct`, or `none`; see [Media policy](#media-policy) |
| `media_max_participants` | `20` | Skip media from conversations above this many participants; `0` = no cap |
| `max_media_mb` | `250` | Per-attachment download cap in MiB (over-cap media is recorded as a `size_cap` skip and retried only after the cap changes) |
| `accounts_config` | — | Per-accountID `media` and `max_media_mb` overrides |

### `[slack]`

Archive [Slack workspaces](/docs/usage/slack/). A single block covers every
registered workspace (tokens are per-workspace files). Authorize each
workspace first with `msgvault add-slack`.

```toml
[slack]
enabled = true                    # gate for the daemon schedule
schedule = "*/30 * * * *"         # 5-field cron; empty = manual sync only
channels = []                     # channel-name include filter (empty = all memberships)
exclude_channels = []             # channel names to skip, e.g. ["noise"]
media = true                      # download shared-file bytes
media_scope = "all"               # all, direct, or none
media_max_participants = 20       # skip files from larger channels; 0 = no cap
max_media_mb = 250                # per-file download cap (MiB)

# [slack.accounts_config.T0123456]  # per-workspace override, keyed by team ID
# media = false
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Whether the daemon schedules Slack sync |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `channels` | all | Channel names to sync (include filter; DMs are never filtered) |
| `exclude_channels` | — | Channel names to skip (wins over `channels`) |
| `media` | `true` | Download shared-file bytes (failed downloads retry via `backfill-slack-media`) |
| `media_scope` | `all` | `all`, `direct` (DMs and group DMs only), or `none`; see [Media policy](#media-policy) |
| `media_max_participants` | `20` | Skip files from conversations above this many members; `0` = no cap |
| `max_media_mb` | `250` | Per-file download cap in MiB (over-cap files are recorded as a `size_cap` skip and retried only after the cap changes) |
| `accounts_config` | — | Per-team-ID `media` and `max_media_mb` overrides |

### `[teams]`

Media policy for [Microsoft Teams](/docs/usage/teams/) chats and channels. Teams
sync itself is scheduled through `[[accounts]]`; this table only decides which
attachments are downloaded.

```toml
[teams]
media = true
media_scope = "all"
media_max_participants = 20
max_media_mb = 250

[teams.accounts_config."user@example.com"]
media = true
max_media_mb = 500
```

| Key | Default | Description |
|---|---|---|
| `media` | `true` | Download attachment and inline hosted-content bytes (failed downloads retry via `backfill-teams-media`) |
| `media_scope` | `all` | `all`, `direct` (chats only, not channels), or `none`; see [Media policy](#media-policy) |
| `media_max_participants` | `20` | Skip media from chats and channels above this many members; `0` = no cap |
| `max_media_mb` | `250` | Per-attachment download cap in MiB |
| `accounts_config` | — | Per-account overrides of `media` and `max_media_mb`, keyed by the Teams account email |

### Granola Sources

Granola meeting-notes sync is configured with top-level `[[granola]]` entries.
Each entry is one Granola account. `identifier` is a stable source label;
`account_email` is the primary identity used for organizer attribution.
`msgvault serve` runs it on the given cron schedule. Register the account
first with `msgvault add-granola`. See
[Meeting Transcripts](/docs/usage/meetings/).

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
[Meeting Transcripts](/docs/usage/meetings/).

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

### Notion AI Meeting Notes Sources

Notion meeting sync uses one top-level `[[notion_meetings]]` entry per Notion
identity. The token must belong to a read-only integration with AI Meeting
Notes access and Read Content access. User Information access is optional; it
is required only to resolve attendee IDs to verified email addresses.

```toml
[[notion_meetings]]
identifier = "notion-personal"      # stable source label; defaults to "default" for one entry
account_email = "you@example.com"   # required primary account identity
token = "ntn_..."                   # Notion integration token; keep this file private
schedule = "15 */6 * * *"           # optional 5-field cron, no seconds
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Source name used by commands and scheduler logs |
| `account_email` | (required) | Normalized primary identity for relationships; it is not assumed to be the meeting organizer |
| `token` | (required) | Read-only Notion integration token |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `enabled` | `false` | Whether the source is daemon-scheduled |

Run `msgvault add-notion-meetings <identifier>` to validate access and register
the source before enabling a schedule. Removing the source prevents the
scheduler from recreating it. See [Meeting Transcripts](/docs/usage/meetings/) for
the 50-result discovery limit, attendee visibility, transcript retries, and
stored data.

### `[vector]`

Top-level toggle and backend marker for semantic/hybrid search. SQLite vector search requires a build with `sqlite_vec` support (default via `make build`). PostgreSQL vector search requires a build with the `pgvector` tag and a PostgreSQL `[data].database_url`. See [Vector Search](/docs/usage/vector-search/) for prerequisites, initial embedding, and the full workflow.

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
| `document_prefix` | `""` | Model-specific instruction prepended to every document chunk after chunking (for example, `"search_document: "` for `nomic-embed-text`). The prefix does not reduce `max_input_chars`; maximum 4096 UTF-8 bytes. |
| `query_prefix` | `""` | Model-specific instruction prepended to every vector-search query (for example, `"search_query: "` for `nomic-embed-text`); maximum 4096 UTF-8 bytes. |
| `api_key_env` | — | Name of an environment variable containing the API key. Omit for anonymous endpoints. |
| `batch_size` | `32` | Embedding inputs per HTTP call. Long messages can contribute multiple chunk inputs. |
| `timeout` | `30s` | Per-request timeout. |
| `max_retries` | `3` | Retries per batch on transient failures. |
| `max_input_chars` | `32768` | Character cap per embedding chunk. Set below your model's context window (e.g., `2000` for Ollama's default `nomic-embed-text`). |
| `eta_window` | `10` | Number of recent progress samples used for ETA smoothing. |

##### Stored provider credentials

Instead of naming an environment variable in `api_key_env`, you can store a
provider API key through Settings in the Web UI or the TUI. Stored keys live in
`tokens/provider-credentials.json` under the data directory with owner-only
file permissions. They are never written to `config.toml` and are never shown
again after saving; Settings only reports whether a key is configured and
whether it comes from the store or from the environment.

A stored key takes precedence over the environment variable named by
`api_key_env`. Each stored key is bound to the endpoint origin (scheme, host,
and port) it was saved for. If you later change the endpoint to another origin,
the stored key is removed automatically and must be entered again, so a key
is never sent to a host it was not entered for.

Changing a stored key for vector or multimodal (visual) embeddings requires a daemon
restart, like the other `[vector]` settings. Person enrichment and sweep keys
apply on the next run.

The index generation fingerprint includes the model, dimension, document and query prefixes, preprocessing settings, `max_input_chars`, embedding policy, and scope. Changing those settings triggers a stale-index error on the next vector/hybrid query. For an existing account-scoped generation built with CLI flags, set matching `[vector.embed.scope].accounts` and restart the daemon; otherwise run `msgvault embeddings build --full-rebuild`.

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

| Key | Default | Description |
|---|---|---|
| `message_types` | `[]` (all types) | Embed only messages of these types. |
| `accounts` | `[]` (all accounts) | Embed only these accounts' messages, by canonical account identifier (display names are rejected here — they are not stable identities for a privacy boundary). Resolved to source IDs at startup; an unknown identifier fails vector initialization (or the CLI command). The daemon's scheduled embeds honor this scope, so it also acts as a privacy boundary: unlisted accounts' text is never sent to the embedding endpoint. |

`accounts` and `message_types` compose (both filters apply). The CLI flags
`--account`/`--collection` on `msgvault embeddings build`/`resume` override
`accounts` for a single run. Either scope dimension is part of the generation
fingerprint: changing it requires `msgvault embeddings build --full-rebuild`,
and because the fingerprint records archive-local source IDs, re-adding an
account under a new source ID also requires a rebuild. Account-scoped indexes
do not gate search the way message-type scopes do: out-of-scope accounts
simply have no vector matches and rank on BM25 alone in hybrid mode.

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
| `logs/` | Structured log files (when [file logging](/docs/configuration/#log) is enabled) |
| `analytics/` | Parquet cache files for Web UI and TUI analytical views |
