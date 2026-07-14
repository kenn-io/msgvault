---
title: Text Messages
description: Import chats and text messages from WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup & Restore, and browse Teams conversations, in msgvault.
---

msgvault can import chats and text messages from WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup & Restore. It can also sync Microsoft Teams chats and channels through [Microsoft Teams](/usage/teams/). These records are stored in the same database as email, and you can browse text/chat conversations in the [TUI](/usage/tui/) by pressing `m` to switch to text mode.

## import-whatsapp

Import messages from a decrypted WhatsApp `msgstore.db` SQLite database.

```bash
msgvault import-whatsapp <msgstore.db> --phone <your-number>
```

The `--phone` flag is required and must be in E.164 format (for example, `+447700900000`).

### Flags

| Flag | Required | Description |
|---|---|---|
| `--phone` | Yes | Your phone number in E.164 format (must start with `+`) |
| `--contacts` | No | Path to contacts `.vcf` file for name resolution |
| `--media-dir` | No | Path to decrypted Media folder for attachments |
| `--limit` | No | Limit number of messages (for testing) |
| `--display-name` | No | Display name for the phone owner |
| `--no-default-identity` | No | Do not auto-confirm the phone number as this source's "me" identity |

### Examples

```bash
# Basic import with required phone number
msgvault import-whatsapp ~/whatsapp/msgstore.db --phone +14155551234

# With contacts file for name resolution
msgvault import-whatsapp msgstore.db --phone +14155551234 \
  --contacts contacts.vcf

# With contacts and media
msgvault import-whatsapp msgstore.db --phone +14155551234 \
  --contacts contacts.vcf --media-dir ./Media
```

### Prerequisites

You need a decrypted copy of your WhatsApp `msgstore.db` file. This is the SQLite database where WhatsApp stores all messages on your device. How you obtain the decrypted database depends on your platform; msgvault does not handle decryption itself.

The importer brings in chats, messages, participants, reactions, and attachments (when `--media-dir` is provided).

## import-imessage

Import messages from the local iMessage database on macOS.

```bash
msgvault import-imessage
```

By default, the command reads from `~/Library/Messages/chat.db`. No positional arguments are needed.

!!! warning
    **Full Disk Access required.** macOS protects `~/Library/Messages/chat.db`. Before running this command, grant Full Disk Access to your terminal app in System Settings > Privacy & Security > Full Disk Access.

This is a read-only operation. msgvault does not modify your iMessage database.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--db-path` | `~/Library/Messages/chat.db` | Path to chat.db |
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--me` | — | Your phone/email for recipient tracking |
| `--contacts` | — | Path to a `.vcf` file used to backfill participant display names |

### Examples

```bash
# Import all iMessages (auto-discovers chat.db)
msgvault import-imessage

# Import only recent messages
msgvault import-imessage --after 2024-01-01

# Use a custom database path (e.g., from a backup)
msgvault import-imessage --db-path /Volumes/Backup/Messages/chat.db

# Set your identity for recipient tracking
msgvault import-imessage --me +14155551234

# Backfill display names from a Contacts.app vCard export
msgvault import-imessage --contacts ~/contacts.vcf
```

`--contacts` accepts a vCard file such as macOS Contacts.app's **File > Export > Export vCard** output. Display names are matched by phone number or email address, and only currently-empty participant names are updated.

## import-gvoice

Import texts, calls, and voicemails from a Google Voice Takeout export.

```bash
msgvault import-gvoice <takeout-voice-dir>
```

The directory must be the "Voice" folder from a [Google Takeout](https://takeout.google.com) export. It should contain a `Calls/` subdirectory and a `Phones.vcf` file.

!!! note
    Only text messages appear in TUI text mode. Call logs and voicemails are stored but not currently browsable in the TUI.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--no-default-identity` | `false` | Do not auto-confirm the phone number as this source's "me" identity |

### Examples

```bash
# Import from Google Takeout Voice directory
msgvault import-gvoice ~/Downloads/Takeout/Voice

# Import only messages from a date range
msgvault import-gvoice ~/Downloads/Takeout/Voice \
  --after 2020-01-01 --before 2024-01-01
```

### Getting your Google Voice data

1. Go to [Google Takeout](https://takeout.google.com)
2. Deselect all products, then select only **Google Voice**
3. Export and download the archive
4. Extract the zip. The `Voice` folder inside the `Takeout` directory is what you pass to the command.

## import-messenger

Import Facebook Messenger conversations from a Download Your Information export.

```bash
msgvault import-messenger --me <you@facebook.messenger> <dyi-export-dir>
```

`--me` is required and must use msgvault's synthetic Messenger identifier format, for example `test.user@facebook.messenger`. It becomes the source identifier and determines which messages are marked as yours.

Messenger DYI exports may contain JSON, HTML, or both. The default `--format auto` imports JSON when JSON is present because it preserves millisecond timestamps and richer reaction data. Use `--format html`, `--format json`, or `--format both` when you need to force a specific path.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--me` | (required) | Your synthetic Messenger identifier, e.g. `test.user@facebook.messenger` |
| `--format` | `auto` | Export format to import: `auto`, `json`, `html`, or `both` |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--no-resume` | `false` | Start fresh instead of resuming an interrupted import |
| `--checkpoint-interval` | `200` | Save progress every N messages |

### Examples

```bash
# Import a Facebook DYI export
msgvault import-messenger --me test.user@facebook.messenger ~/Downloads/facebook-export

# Import both JSON and HTML copies when you deliberately want both
msgvault import-messenger --me test.user@facebook.messenger --format both ./dyi
```

!!! note
    Facebook DYI exports do not contain stable participant IDs. msgvault synthesizes participant identifiers from names as `<slug>@facebook.messenger`; identical slugs are treated as the same participant.

## import-synctech-sms

Import XML or ZIP backups produced by **SMS Backup & Restore** by SyncTech Pty Ltd.

```bash
msgvault import-synctech-sms <path> --owner-phone <your-number>
```

The `--owner-phone` flag is required and must be in E.164 format. The importer can bring in SMS, MMS, call logs, and MMS attachments from local XML or ZIP backup files.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--sms` | `true` | Import SMS records |
| `--mms` | `true` | Import MMS records |
| `--calls` | `true` | Import call logs |
| `--attachments` | `true` | Import MMS attachments |

### Examples

```bash
# Import one local backup file
msgvault import-synctech-sms sms-backup.xml --owner-phone +14155551234

# Import a ZIP backup but skip call logs
msgvault import-synctech-sms sms-backup.zip --owner-phone +14155551234 --calls=false
```

!!! warning
    Encrypted SMS Backup & Restore backups are not supported. Disable encryption in the Android app and export again before importing.

## SyncTech Google Drive Sources

If SMS Backup & Restore writes backups to Google Drive, configure a source once and let `msgvault serve` schedule it.

```bash
msgvault add-synctech-sms-drive phone-backups \
  --owner-phone +14155551234 \
  --folder-id <drive-folder-id> \
  --google-account you@gmail.com

msgvault sync-synctech-sms phone-backups
```

`add-synctech-sms-drive` appends a `[[synctech_sms.sources]]` entry to `config.toml`. Drive imports skip files that were already imported, wait for files to be stable before reading them, and stage downloads under the msgvault data directory while the import runs.

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--folder-id` | (required) | Google Drive folder ID containing backup files |
| `--google-account` | (required) | Google account used for Drive access |
| `--schedule` | `30 4 * * *` | Cron schedule used by `msgvault serve` |
| `--oauth-app` | — | Named Google OAuth app to use |

## Browsing Texts in the TUI

After importing, launch the TUI and press `m` to cycle from Email to Texts mode. Text mode shows a conversations list; select a conversation to drill down into its messages. Pressing `m` again reaches Meetings, then returns to Email.

```bash
msgvault tui
```

Text mode is only available when text data has been imported. See the [TUI documentation](/usage/tui/) for keyboard shortcuts and navigation.

## Deduplication

All importers on this page are safe to run multiple times. Running the same import again does not create duplicates.

## Resumable Imports

Imports use checkpoint-based resumption. If interrupted (Ctrl+C, power loss), run the same command again and it picks up where it left off.

## After Importing

Most chat import commands rebuild the analytics cache automatically after import. If a newly imported or synced source does not appear in aggregate views immediately, run `msgvault build-cache`. Your imported texts and Teams conversations are then available for TUI browsing.

```bash
# Launch the TUI and press 'm' for text mode
msgvault tui

# View updated archive stats
msgvault stats
```
