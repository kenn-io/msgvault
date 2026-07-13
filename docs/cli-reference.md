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
| `--local` | Use the local daemon instead of a configured remote for archive-access commands |
| `--log-file <path>` | Override log file path (default: `<data_dir>/logs/msgvault-YYYY-MM-DD.log`) |
| `--log-level <level>` | Log level: `debug`, `info`, `warn`, `error` (default: `info`) |
| `--no-log-file` | Disable file logging for this run (stderr output stays on) |
| `--log-sql` | Log every SQL query at info level (verbose, for debugging) |
| `--log-sql-slow-ms <ms>` | Slow query threshold in ms (default: 100; 0 uses built-in default) |
| `--help` | Show help |

---

## HTTP-Backed CLI Behavior

Commands that access archive state keep their usual stdout/stderr output while using the same API path as remote access:

1. If `[remote].url` is configured and `--local` is not passed, the CLI talks to that remote server.
2. Otherwise, archive-access commands discover or start the local background daemon and talk to it over HTTP.
3. `--local` selects the local daemon even when `[remote].url` is configured; it is not a request to open SQLite in the CLI process.

This makes local and remote msgvault behavior the same from the CLI's point of view and avoids opening a large SQLite database from foreground CLI processes.

The local daemon publishes its binary version and API schema version. By default, a newer compatible CLI restarts an older local daemon before issuing the request; configure `[server].daemon_auto_restart` as `newer`, `never`, or `always` to control that lifecycle behavior. Remote servers are not restarted by clients, so compatibility is negotiated from the API schema version exposed in the OpenAPI document.

---

## init-db

Initialize the archive schema through the configured remote server or the local
daemon. When no remote is configured, the CLI starts the local daemon if needed
and the daemon owns the database initialization work.

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

Requires a `[microsoft]` section with `client_id` in `config.toml`. See the [OAuth Setup guide](/guides/oauth-setup/#microsoft-365-outlook-hotmail) for Azure AD app registration.

| Flag | Default | Description |
|---|---|---|
| `--tenant` | `common` | Azure AD tenant ID (restricts which accounts can authorize) |
| `--no-default-identity` | `false` | Do not auto-confirm the email address as this account's "me" identity |

After adding the account, sync it with `msgvault sync-full`.

---

## add-teams

Authorize a Microsoft Teams account through delegated Microsoft Graph OAuth and
register a `teams` source.

```bash
msgvault add-teams <email>
msgvault add-teams <email> --tenant <tenant-id>
```

This stores a Teams Graph token under `tokens/teams_<email>.json`, separate from
the Microsoft IMAP token used by `add-o365`. Requires `[microsoft].client_id` in
`config.toml` and the Graph permissions documented in
[Microsoft Teams](/usage/teams/).

| Flag | Default | Description |
|---|---|---|
| `--tenant` | `common` | Azure AD tenant ID to use for authorization |
| `--no-default-identity` | `false` | Do not auto-confirm the email address as this source's "me" identity |

After adding the account, sync it with `msgvault sync-teams`.

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

The CLI sends the sync request to the configured remote server or local daemon
and streams the daemon's stdout/stderr back to the terminal. This keeps local
and remote full sync behavior aligned and avoids running a separate direct
SQLite writer beside `msgvault serve`.

---

## sync

Sync new and changed messages. Gmail accounts use the Gmail History API; IMAP accounts perform a mailbox scan and skip messages already in the database. When called without an email argument, syncs all accounts that have completed an initial full sync.

```bash
msgvault sync [email]
```

The CLI sends the incremental sync request to the configured remote server or
local daemon and streams the daemon's stdout/stderr back to the terminal. The
daemon serializes this work with other archive mutations.

---

## sync-teams

Sync Microsoft Teams chats and channels for an authorized account.

```bash
msgvault sync-teams <email>
msgvault sync-teams <email> --no-channels
msgvault sync-teams <email> --limit 100
msgvault sync-teams <email> --full
```

Full versus incremental sync is detected from stored cursors and checkpoints.
`--full` ignores those cursors and re-fetches messages, upserting rows in place
so importer upgrades can repair existing data without creating duplicates.

| Flag | Default | Description |
|---|---|---|
| `--no-channels` | `false` | Sync chats only and skip team channels |
| `--limit` | `0` | Maximum messages per conversation (`0` = unlimited) |
| `--full` | `false` | Ignore stored cursor and re-fetch every message |

See [Microsoft Teams](/usage/teams/) for setup, scheduling, search, and inline
media backfill.

---

## add-granola

Register a configured Granola account and validate its API key with a live
API call.

```bash
msgvault add-granola [identifier]
```

Reads the key from the matching `[[granola]]` entry in `config.toml`. With a
single configured entry the identifier may be omitted. Granola API keys are
created in the desktop app's settings and require a Business plan. The command
also confirms the source's effective `account_email`, even when other aliases
already exist. Meeting-source identity confirmation is mandatory.

After adding the account, sync it with `msgvault sync-granola`. If you later
change `account_email` or add/remove an alias with `msgvault identity`, run
`msgvault sync-granola <identifier> --full` to repair existing `is_from_me`
attribution.

---

## sync-granola

Sync Granola meeting notes and transcripts.

```bash
msgvault sync-granola [identifier]
msgvault sync-granola --limit 5
msgvault sync-granola --full --after 2024-01-01
```

Incremental by default: only notes updated since the last successful run are
fetched. With no identifier, every configured `[[granola]]` source is synced.
Re-fetched notes are upserted in place, so `--full` repairs existing rows
without creating duplicates. A partial run with one or more failed notes is
recorded and returned as an error without advancing the successful cursor. If
other notes were added or updated first, the cache is refreshed before the
error is returned. Scheduled sync refuses a configured source that has been
removed from the archive and directs you to run `add-granola` again.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum notes per run (`0` = unlimited) |
| `--after` | — | Full-sync only notes created after this date (YYYY-MM-DD; implies `--full`) |
| `--full` | `false` | Ignore stored cursor and re-fetch every note |

See [Meeting Transcripts](/usage/meetings/) for setup and what gets stored.

---

## add-circleback

Authorize a configured Circleback account using browser OAuth (their MCP
server uses OAuth with dynamic client registration).

```bash
msgvault add-circleback [identifier]
```

The token is stored under `tokens/circleback_<identifier>.json`. With a
single configured `[[circleback]]` entry the identifier may be omitted. The
command always confirms the source's effective `account_email`; there is no
identity opt-out flag.

Circleback redirects OAuth to `localhost:8090`. A CLI configured for a remote
daemon refuses to proxy this command: run it on the daemon host, where the
token is stored. Over SSH, use `ssh -L 8090:localhost:8090 user@daemon-host`,
run `msgvault --local add-circleback <identifier>` in that remote shell, and
open the printed authorization URL in your local browser if necessary. On the
daemon host, `--local` selects that host's archive; on a workstation it would
target a separate local archive.

After adding the account, sync it with `msgvault sync-circleback`. Run a
`--full` sync after changing the primary email or confirmed aliases.

---

## sync-circleback

Sync Circleback meetings, notes, action items, and transcripts.

```bash
msgvault sync-circleback [identifier]
msgvault sync-circleback --limit 5
msgvault sync-circleback --full --after 2024-01-01
msgvault sync-circleback --probe
```

Incremental by default: each run enumerates meeting IDs without a date bound
so newly created backfills are discovered even when their scheduled date is
old. Unknown meetings and known meetings created within the 48-hour refresh
overlap are fetched in detail; unchanged snapshots are skipped without
invalidating the search cache. With no identifier, every configured
`[[circleback]]` source is synced. Missing or recognized-empty transcripts
enter a bounded `pending` state and retry every six hours, normally until seven
days after the scheduled meeting time (48 hours when no usable time exists).
Expired retries become `unavailable`; a later `--full` run can check them again.

Due transcript retries are maintenance work and run outside the new-meeting
limit. Provider, contract, missing-result, ingest, archive-recovery, and
cancellation failures fail the sync and preserve the prior successful cursor.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum newly searched meetings; due maintenance items are additional (`0` = unlimited) |
| `--after` | — | Full-sync only meetings after this date (YYYY-MM-DD; implies `--full`) |
| `--full` | `false` | Ignore stored cursor and re-fetch every meeting |
| `--probe` | `false` | Print the MCP tool inventory and a sample result instead of syncing |

See [Meeting Transcripts](/usage/meetings/) for setup and what gets stored.

---

## backfill-teams-media

Re-fetch Microsoft Teams inline hosted-content media for already imported
messages.

```bash
msgvault backfill-teams-media <email>
msgvault backfill-teams-media <email> --only-incomplete
```

The command scans stored Teams HTML bodies for Graph `hostedContents` URLs and
downloads those images into the attachment store. It is idempotent because
attachments are content-addressed.

| Flag | Default | Description |
|---|---|---|
| `--only-incomplete` | `false` | Retry only messages whose inline media is still missing |

---

## add-beeper

Register the chat accounts bridged through a locally running
[Beeper Desktop](/usage/beeper/) as `beeper` sources, one per network.

```bash
msgvault add-beeper
msgvault add-beeper --token-file ~/beeper-token.txt
MSGVAULT_BEEPER_TOKEN="..." msgvault add-beeper
```

Requires Beeper Desktop running on the same machine and an access token
minted in Beeper Desktop (Settings → Developer). The token is stored at
`tokens/beeper.json`. Accounts filtered out by `[beeper].accounts` /
`exclude_accounts` in `config.toml` are skipped.

| Flag | Default | Description |
|---|---|---|
| `--token-file` | | Read the access token from a file instead of prompting |
| `--no-default-identity` | `false` | Do not auto-confirm each account's own identity as that source's "me" identity |

After adding, sync with `msgvault sync-beeper`.

---

## sync-beeper

Sync chats from Beeper Desktop for every registered Beeper account (all
bridged networks). The first run backfills full locally-available history and
is resumable; later runs are incremental. Per-account failures do not stop the
run: remaining accounts still sync, the analytics cache is rebuilt for the
successful ones, and the command exits non-zero listing the failures. Without
`--account`, the `[beeper]` config `accounts`/`exclude_accounts` filters
select which registered sources sync. See [Beeper](/usage/beeper/).

```bash
msgvault sync-beeper
msgvault sync-beeper --account signal --account telegram
msgvault sync-beeper --full
```

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Beeper accountID to sync (repeatable) |
| `--limit` | `0` | Max messages per chat this run (0 = no limit; limited backfills resume next run) |
| `--full` | `false` | Ignore stored cursors and re-fetch every message (repairs rows in place) |
| `--no-media` | `false` | Skip attachment downloads for this run |

---

## backfill-beeper-media

Retry pending Beeper attachment downloads (media that failed or exceeded the
size cap during `sync-beeper`). Idempotent: attachments are content-addressed.
If the source message is gone (deleted in Beeper), its pending markers are
cleared permanently — already-downloaded attachments are kept. Transient fetch
errors keep the marker and count as still pending, so re-running retries them.

```bash
msgvault backfill-beeper-media
msgvault backfill-beeper-media --account signal
```

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Beeper accountID to backfill (repeatable) |

---

## add-calendar

Authorize read-only Google Calendar access for an account and register its calendars for sync. If the account already has a Gmail token, re-consent bundles Gmail + Calendar so Gmail access is not dropped — keep both checked on the consent screen. The Calendar API must be enabled on the OAuth project. By default only owned/writable calendars are registered.

```bash
msgvault add-calendar <email> [flags]
```

| Flag | Description |
|---|---|
| `--oauth-app` | Named OAuth app to use |
| `--headless` | Print token-copy instructions for a headless host instead of opening a browser |
| `--all-calendars` | Include reader/freeBusyReader (subscribed, holiday) calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--calendars` | Comma-separated calendar IDs to register |

---

## sync-calendar

Sync Google Calendar events for an account. The account is resolved from a `[[gcal]]` config entry (by name or email) or used directly as an email. The first run (or `--full`) does a full sync that registers calendars; later runs are incremental via the Calendar `syncToken`. Events are stored as searchable records (`message_type = calendar_event`) and become eligible for semantic search when the embedding worker runs. Cancelled events are retained and marked cancelled, never deleted. Sync is read-only.

```bash
msgvault sync-calendar <name|email> [flags]
```

| Flag | Description |
|---|---|
| `--full` | Force a full sync (ignore stored sync tokens) |
| `--limit` | Max events per calendar (0 = unlimited) |
| `--after` / `--before` | Bound a full sync to a date range (`YYYY-MM-DD`); full sync only |
| `--calendar` | Restrict to specific calendar IDs |
| `--all-calendars` | Include reader/freeBusyReader calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--oauth-app` | Named OAuth app to use |
| `--noresume` | Do not resume an interrupted full sync |

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

## backup

Create, list, verify, and restore incremental archive snapshots in a backup
repository.

```bash
msgvault backup init --repo ~/Backups/msgvault
msgvault backup create --repo ~/Backups/msgvault
msgvault backup list --repo ~/Backups/msgvault
msgvault backup verify --repo ~/Backups/msgvault
msgvault backup restore --target ~/msgvault-restored --repo ~/Backups/msgvault
```

Every backup subcommand requires a repository: pass `--repo`, or set
`[backup].repo` in `config.toml` to omit it. `backup init` initializes the
repository directory but does not modify `config.toml`.
`backup create` is routed through the selected daemon so the daemon can freeze a
consistent SQLite snapshot while it scans pages and attachments. `backup verify`
and `backup restore` run locally against the repository because they do not
write the live archive.

### backup init

```bash
msgvault backup init --repo <dir>
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |

### backup create

```bash
msgvault backup create [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--include-config` | Include `config.toml` verbatim; may contain API keys |
| `--include-tokens` | Include OAuth token files |
| `--allow-plaintext-secrets` | Allow config/tokens in an unencrypted repository |
| `--tag <text>` | Optional label recorded on the snapshot manifest |
| `--force-unlock` | Break a stale exclusive repository lock before creating |
| `--jobs N` | Concurrent attachment capture workers; `0` uses one per CPU |

### backup list

```bash
msgvault backup list [--repo <dir>]
```

Prints snapshot ID, creation time, message count, bytes added, and tag.

### backup verify

```bash
msgvault backup verify [snapshot] [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--all` | Verify every snapshot instead of only the latest |
| `--quick` | Skip reading and hash-verifying content blobs |
| `--force-unlock` | Break a stale exclusive repository lock before verifying |
| `--jobs N` | Concurrent pack readers; `0` uses one per CPU |

### backup restore

```bash
msgvault backup restore [snapshot] --target <dir> [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--target <dir>` | Directory to restore into (required) |
| `--overwrite` | Allow restoring into a non-empty target directory |
| `--integrity-check` | Run SQLite's full integrity check after restoring (slow for large databases) |
| `--loose-attachments` | Restore attachments as individual loose files instead of installing compatible packs |
| `--force-unlock` | Break a stale exclusive repository lock before restoring |
| `--jobs N` | Concurrent pack readers; `0` uses one per CPU |

By default, restore installs compatible repository packs directly into the
restored attachment store. Every selected attachment is still read and
SHA-256 verified; entries that exceed the target store's maintenance limits,
or packs that use an incompatible representation, are restored loose instead.
The summary reports the resulting packed/loose split and any fallback reasons.
Every restore verifies database pages and content blobs by hash and compares
the restored database statistics with the snapshot manifest. Pass
`--integrity-check` to additionally run SQLite's full `PRAGMA integrity_check`
scan; it is optional because it can dominate restore time for large databases.

Use `--loose-attachments` for downgrade or recovery. Restore into a fresh
target with that flag to guarantee a fully loose result. An overwritten target
can retain uncataloged old pack files, and `unpack-attachments` processes only
cataloged packs, so overwrite cannot currently make the same guarantee.
Restoring into the live archive home of a running daemon is refused. See
[Backup](/usage/backup/) for repository format, scheduling, verification, and
privacy details.

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
| `--message-type` | Limit results to one or more message types, e.g. `email`, `teams`, `calendar_event`, `sms` |
| `--mode` | Search mode: `fts` (default), `vector`, or `hybrid`. `vector` and `hybrid` require vector search to be configured. |
| `--explain` | Include per-signal scores (RRF, BM25, vector) in the output. Only applies to `--mode vector` and `--mode hybrid`. |

Without an explicit message-type filter, search intentionally returns all
matching cached message types, including meeting transcripts and chats.
Ordinary aggregate views and statistics still default to email-only; use
`--message-type` (or `message_type:` in the query) when you need an explicit
search scope.

`--mode vector` and `--mode hybrid` require at least one free-text term in the query (filter-only queries use `--mode fts`). They do not support pagination (`--offset` is rejected), so bump `--limit` to retrieve a larger candidate pool instead. See [Searching](/usage/searching/) for the operator reference and [Vector Search](/usage/vector-search/) for semantic setup.

---

## tui

Launch the interactive terminal interface.

```bash
msgvault tui [flags]
```

| Flag | Description |
|---|---|
| `--local` | Use the local daemon instead of the configured remote server |

Analytics engine and cache behavior are daemon-managed. Configure `[analytics].engine` and `[analytics].auto_build_cache` in `config.toml` to force live SQL, require DuckDB, or disable automatic cache builds. See [Configuration: analytics](/configuration/#analytics).

Deprecated in 0.17.0: the older TUI-only `--force-sql`, `--no-cache-build`, and `--no-sqlite-scanner` flags are hidden and no longer control the foreground CLI. Use `[analytics].engine = "sql"` for live SQL, `[analytics].auto_build_cache = false` to skip daemon cache builds, or `msgvault build-cache` to prebuild cache files on the daemon host.

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

Verify archive integrity against Gmail through the configured remote server or
local daemon. The command streams the daemon's stdout/stderr back to the
terminal.

```bash
msgvault verify <email> [flags]
```

| Flag | Description |
|---|---|
| `--sample N` | Messages to sample (default: 100) |
| `--skip-db-check` | Skip SQLite integrity check |
| `--json` | Emit machine-readable JSON summary |

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

The identity subcommands use the configured remote server or local daemon by default. `--local` uses the local daemon even when a remote is configured.

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

The collection subcommands use the configured remote server or local daemon by
default. `--local` uses the local daemon even when a remote is configured.

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

Permanently delete dedup-hidden messages from the selected msgvault archive.

```bash
msgvault delete-deduped --batch <batch-id>
msgvault delete-deduped --all-hidden
```

The CLI sends the request to the configured remote daemon, or to the
auto-started local daemon when no remote is configured. It no longer opens the
SQLite database directly. The delete cannot be undone with `deduplicate --undo`;
when backups are enabled, the daemon writes the backup next to the database it
owns before deleting.

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

Build or update the Parquet analytics cache through the configured remote server
or the local daemon.

```bash
msgvault build-cache [flags]
```

| Flag | Description |
|---|---|
| `--full-rebuild` | Discard existing cache and rebuild |

The CLI sends the request over HTTP and streams the daemon's stdout/stderr back
to the terminal. A local daemon runs the DuckDB export in an isolated child
process so DuckDB's bundled SQLite library never opens the archive inside the
long-lived daemon process. With `[remote].url` configured, the remote daemon
builds its own cache; use `--local` only to target this machine's local daemon.

For automatic cache rebuilds after daemon-owned syncs, configure
`[analytics].auto_build_cache` in `config.toml`.

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
| `resume` | Continue scan-and-fill embedding for the building or active generation. Always incremental. |
| `list` | List index generations with their state, model, dimension, and pending count. |
| `activate <generation-id>` | Activate a completed building generation, retiring the current active one. |
| `retire <generation-id>` | Retire a generation. |

### embeddings build

```bash
msgvault embeddings build [flags]
```

| Flag | Description |
|---|---|
| `--full-rebuild` | Create a new index generation and rebuild from scratch. The new generation is activated atomically once coverage reaches zero. Same-model rebuilds keep serving the previous active generation in the meantime, but active-generation top-ups are frozen until activation; model or dimension changes return `index_stale` for vector/hybrid search until the new generation activates. |
| `--yes` | Skip the confirmation prompt that `--full-rebuild` otherwise requires. |

Without `--full-rebuild`, the command is incremental: it resumes any in-flight rebuild that matches the configured model, otherwise scans for live messages still missing coverage in the active generation, then exits. Safe to schedule via cron (or let `msgvault serve` do it via `[vector.embed.schedule]`).

### embeddings resume

```bash
msgvault embeddings resume
```

Continue embedding work and finish the current generation. If a generation matching the configured model is building, this embeds its remaining rows and activates it once coverage reaches zero; otherwise it tops up the active generation. Equivalent to `msgvault embeddings build` with no flags, but never starts a full rebuild.

### embeddings list

```bash
msgvault embeddings list
```

Print one row per index generation: ID, state (`building`, `active`, or `retired`), model, dimension, embedded message count, pending count, fingerprint, and the start, completion, and activation timestamps.

### embeddings activate

```bash
msgvault embeddings activate <generation-id> [flags]
```

Activate a completed building generation and retire the currently active one. By default this refuses to activate a generation that still has messages missing coverage or whose fingerprint does not match the current config.

| Flag | Description |
|---|---|
| `--yes` | Skip the confirmation prompt. |
| `--force` | Activate even with missing coverage or a fingerprint mismatch. |

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

The command queries the configured msgvault server over HTTP. With local configuration,
the CLI auto-starts or reuses the local daemon, and the daemon reads the analytics
cache files. With remote configuration, the remote server reports its own cache state.

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
| `--force-sql` | `false` | Deprecated in 0.17.0; use `[analytics].engine = "sql"` in `config.toml` instead. See [Configuration: analytics](/configuration/#analytics). |
| `--no-sqlite-scanner` | `false` | Deprecated in 0.17.0; cache engine selection is daemon-managed. Use `[analytics].engine = "sql"` for live SQL. |
| `--http` | — | Serve MCP over StreamableHTTP on this address instead of stdio. Bare ports bind to loopback, e.g. `8080` becomes `127.0.0.1:8080`. |
| `--http-allow-insecure` | `false` | Allow non-loopback HTTP binding. The MCP server has no built-in auth; put it behind a trusted network or authenticated reverse proxy. |

See [MCP Server](/usage/chat/) for configuration and tool reference.

---

## openapi

Print the checked-in msgvault OpenAPI contract without starting the daemon or opening the archive database.

```bash
msgvault openapi [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--version` | `3.1` | OpenAPI version to emit: `3.1` or `3.0` |
| `--format` | `yaml` | Output format: `yaml` or `json` |

The OpenAPI `info.version` is the API schema version, not the msgvault binary version. Use it to reason about forward/backward compatibility when a local or remote CLI talks to a server. The running daemon also serves the same contract at `/openapi.json`; generated client artifacts are built from the OpenAPI 3.0 form for tool compatibility.

---

## serve

Start the web server with optional background sync scheduling, or manage the local background daemon used by HTTP-backed CLI commands.

```bash
msgvault serve
msgvault serve start
msgvault serve status
msgvault serve stop
msgvault serve restart
```

`msgvault serve` runs in the foreground and stays up until interrupted. `msgvault serve start` starts the daemon in the background, `status` reports the recorded daemon URL/PID/version/API schema/uptime, `stop` shuts it down, and `restart` performs a stop followed by a start. Starting a newer compatible binary replaces an older recorded daemon when `[server].daemon_auto_restart = "newer"`; incompatible running daemons are reported with a prompt to stop them first.

The `serve` command and lifecycle subcommands have no CLI flags. All configuration (port, bind address, API key, CORS, account schedules, SyncTech SMS sources, background idle timeout, daemon restart policy, and vector embedding schedule) is read from your `config.toml`. See [Web Server](/api-server/) for endpoint documentation, run `msgvault openapi`, or fetch `/openapi.json` from a running server for the generated OpenAPI contract. See [Configuration](/configuration/#server) for config options. When vector search is enabled, the daemon can also run the embed worker on a cron and/or after every successful sync, see [Configuration: vector.embed.schedule](/configuration/#vectorembedschedule).

Background daemons started by `serve start` or auto-started by a CLI command shut down after `[server].daemon_idle_timeout` with no requests. The default is `20m`; set it to `"0s"` to disable idle shutdown. `MSGVAULT_DAEMON_IDLE_TIMEOUT` can override the value for a lifecycle-managed background daemon. Foreground `msgvault serve` is not idle-stopped.

`[server].daemon_auto_restart` controls local daemon replacement when the CLI and recorded daemon versions differ. The default `newer` restarts only older compatible daemons, `never` leaves lifecycle to the operator or supervisor, and `always` restarts on any version mismatch that is safe for the current API schema.

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

Update account settings through the configured remote server or local daemon.
Use `--local` to force the local daemon when a remote is configured.

```bash
msgvault update-account <email> [flags]
```

| Flag | Description |
|---|---|
| `--display-name` | Set a display name for the account |

---

## remove-account

Remove an account and all its archived data from the selected msgvault archive. Deletes messages, labels, sync state, OAuth or IMAP credentials, and attachment files unique to this account. This is irreversible but does not touch the remote mail provider.

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

Fix UTF-8 encoding issues in existing messages through the configured remote
server or local daemon. The command streams the daemon's stdout/stderr back to
the terminal, and the daemon serializes the repair with other archive mutations.

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

View and tail structured log files from the selected daemon. With `[remote].url` configured, this shows remote daemon logs; otherwise it starts or contacts the local daemon. File logging must be enabled first (see [Configuration: Log](/configuration/#log)).

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
| `--path` | `false` | Print the selected daemon's log directory path and exit |

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
