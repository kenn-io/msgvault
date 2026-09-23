---
last_edited: "2026-09-23"
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
| [iMazing Messages](#import-imazing-csv) | CSV export root or `csv/` directory |
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

| Format                     | Imported today                                                                                                            | Not included                  |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------- | ----------------------------- |
| Android `msgstore.db`      | Chats, messages, participants, reactions, attachment metadata, and available media from `--media-dir`                     | Database decryption           |
| Apple `ChatStorage.sqlite` | Text and URL messages from direct and group chats, sender attribution, and available contact, participant, and push names | Media downloads and reactions |

For `--contacts` name matching, vCard phone numbers must include a country code;
msgvault does not guess a country for local numbers. Apple imports can also use
the sender names stored by WhatsApp when no stronger name is available. Exports
without the optional group-participant table can still be imported.

Supply your own phone number with `--phone` for either format. msgvault records
it as the source's confirmed “me” identity unless you pass
`--no-default-identity`. This confirmation also happens after a completed run
that reports recoverable message errors.

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

## import-imazing-csv

Use this when the original Apple Messages database is gone but an iMazing CSV
export still exists.

```bash
msgvault import-imazing-csv ~/Downloads/messages-export \
  --me +14155550100 --timezone America/Los_Angeles
```

You can pass either of these layouts:

```text
messages-export/                 messages-export/
├── csv/                         ├── conversation-a.csv
│   └── conversation-a.csv       └── conversation-b.csv
└── attachments/
    └── photo.jpg
```

For the second layout, put `attachments/` beside the directory you pass. CSV
discovery is non-recursive and accepts comma, tab, or semicolon delimiters.
Files must contain named `Chat Session`, `Message Date`, `Service`, and `Type`
columns. The other standard iMazing columns are imported when present.

The importer uses observed senders to recognize groups. An `A & B & C` title
can supply additional member names only after messages identify at least two
other senders. A title alone does not create participants.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--me` | (required) | Your phone number or email address; determines outgoing identity |
| `--timezone` | local IANA zone (required on Windows) | Timezone used for dates without an explicit offset |
| `--contacts` | — | vCard file used to fill empty participant names by phone or email |

Pass `--timezone` when the export came from a different timezone or the local
zone cannot be resolved. When the flag is omitted, msgvault resolves the local
IANA zone on Unix hosts; Windows has no dependable local IANA zone, so the
flag is required there. The chosen IANA name is stored with the source. A
later import for the same `--me` value must use the same zone, which keeps
offset-free dates stable across machines and daylight-saving transitions.

Running the command again converges on the same messages and attachment
occurrences. New rows are added without moving existing message IDs. Referenced
files under `attachments/` are copied into msgvault's content-addressed store,
with a 100 MiB limit per file. Larger files are recorded as skipped. Missing
files and filenames that match several files are reported as missing and can
be resolved on a later rerun. These outcomes let the import finish, including
contact enrichment. If a previously stored source file disappears, its archived
bytes stay available. If its bytes change, the same attachment occurrence is
updated to the new content.

The raw `Replying to` value is always preserved. msgvault creates a reply link
only when that value contains exact body, sender, and rendered-date evidence
for one earlier message in the same conversation. Ambiguous replies stay
unlinked.

!!! note
    iMazing CSV rows have no IDs shared with Apple's `chat.db`. A rerun of the
    same CSV is deterministic, but importing overlapping history through both
    `import-imazing-csv` and `import-imessage` can create duplicates.

    `Chat Session` titles identify conversations within one imported source.
    Identical titles share a conversation. Renaming a title on a later export
    creates another conversation and can duplicate its messages.

## import-gvoice

Import texts, calls, and voicemails from a Google Voice Takeout export.

```bash
msgvault import-gvoice <takeout-voice-dir>
```

The directory must be the "Voice" folder from a [Google Takeout](https://takeout.google.com) export. It should contain a `Calls/` subdirectory and a `Phones.vcf` file.

!!! note
    Only text messages appear in TUI text mode. Call logs and voicemails are stored but not currently browsable in the TUI.

Keep the voicemail recordings beside their HTML files when extracting the
Takeout export. Msgvault archives each available recording as an attachment to
its voicemail, so you can retrieve it through the usual
[attachment export commands](exporting.md#export-all-attachments-from-a-message).

When a voicemail has no usable audio reference or its recording cannot be
stored, the voicemail still gets an attachment record marked `failed` with
reason `fetch_failure` and zero stored bytes. A named recording keeps its source
filename. Attachment queries can therefore distinguish missing audio from a
voicemail with an archived recording.

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

All importers on this page are safe to run multiple times. Running the same
source import again does not create duplicates within that source. Different
formats without shared source IDs, such as an iMazing CSV and `chat.db`, can
still overlap.

## Resumable Imports

Large importers use checkpoints where their source format supports it. Other
importers, including iMazing CSV, are deterministic: if interrupted, run the
same command again and existing rows are updated in place.

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
