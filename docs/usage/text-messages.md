---
last_edited: "2026-09-08"
title: Text Messages
description: Import chats and texts from common exports, and browse synchronized Teams and Discord conversations in msgvault.
---

Search old texts and chats alongside your email. Import a local export below,
or connect [Beeper](/docs/usage/beeper/), [Slack](/docs/usage/slack/),
[Microsoft Teams](/docs/usage/teams/), or [Discord](/docs/usage/discord/) for
ongoing sync.

The [Web UI](/docs/web-ui/) groups chats into conversations in Everything;
open a conversation to read its messages. In the [TUI](/docs/usage/tui/), press
`m` to switch to Texts mode.

| Local source | What to provide |
|---|---|
| [WhatsApp](#import-whatsapp) | Decrypted Android `msgstore.db` or Apple `ChatStorage.sqlite` |
| [iMessage](#import-imessage) | macOS `chat.db` |
| [Google Voice](#import-gvoice) | Google Takeout Voice directory |
| [Facebook Messenger](#import-messenger) | Download Your Information export |
| [SMS Backup & Restore](#import-synctech-sms) | XML backup |
| [Slackdump](/docs/usage/slack/#import-a-slackdump-export) | Export directory or ZIP |

## import-whatsapp

Import direct and group chats from a decrypted Android `msgstore.db` or an
Apple `ChatStorage.sqlite` database. msgvault detects the database format.

```bash
msgvault import-whatsapp <database> --phone <your-number>
```

The `--phone` flag is required and must be in E.164 format (for example, `+447700900000`).

### Flags

| Flag | Required | Description |
|---|---|---|
| `--phone` | Yes | Your phone number in E.164 format (must start with `+`) |
| `--contacts` | No | Path to contacts `.vcf` file for name resolution |
| `--media-dir` | No | Android Media folder for attachments; Apple import is text-only |
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

### Apple WhatsApp on macOS

Point the command at the native WhatsApp database or a copy of it:

```bash
msgvault import-whatsapp --phone +447700900000 \
  "$HOME/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite"
```

Reading the native store may require Full Disk Access for your terminal in
**System Settings → Privacy & Security**.

### Format limits

| Format | Imported today | Not included |
|---|---|---|
| Android `msgstore.db` | Chats, messages, participants, reactions, attachment metadata, and available media from `--media-dir` | Database decryption |
| Apple `ChatStorage.sqlite` | Text from direct and group chats, sender attribution, and available group participant names | Media downloads and reactions |

Supply your own phone number with `--phone` for either format. msgvault records
it as the source's confirmed “me” identity unless you pass
`--no-default-identity`. This confirmation also happens after a completed
run that reports recoverable message errors.

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

## Browsing Texts

Start `msgvault serve` and open the [Web UI](/docs/web-ui/) to search email, chats,
calendar events, and meeting notes together. Chat results stay grouped as
conversations so short message fragments do not overwhelm Everything. Open a
conversation to inspect its matching messages in context.

For terminal browsing, launch the TUI and press `m` to cycle from Email to
Texts mode. Text mode shows a conversations list; select a conversation to
drill down into its messages. Pressing `m` again reaches Meetings, then returns
to Email.

```bash
msgvault tui
```

Text mode is only available when text data has been imported. See the [TUI documentation](/docs/usage/tui/) for keyboard shortcuts and navigation.

## Deduplication

All importers on this page are safe to run multiple times. Running the same import again does not create duplicates.

## Resumable Imports

Imports use checkpoint-based resumption. If interrupted (Ctrl+C, power loss), run the same command again and it picks up where it left off.

## After Importing

Most chat import and sync commands rebuild the analytics cache automatically.
If a newly imported or synced source does not appear in aggregate views
immediately, run `msgvault build-cache`. Your imported texts, Teams messages,
and Discord conversations are then available in the Web UI and TUI.

```bash
# Launch the TUI and press 'm' for text mode
msgvault tui

# View updated archive stats
msgvault stats
```
