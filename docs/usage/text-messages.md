---
last_edited: "2026-11-11"
title: Text Messages
description: Import chats and texts from common exports, and browse synchronized Teams and Discord conversations in msgvault.
---

You can search old texts and chats alongside your email. Import a local export
using one of the commands below, or connect [Beeper](/docs/usage/beeper/),
[Slack](/docs/usage/slack/), [Microsoft Teams](/docs/usage/teams/), or
[Discord](/docs/usage/discord/) for ongoing sync.

In the [Web UI](/docs/web-ui/), chats appear grouped into conversations under
**Messages**; open a conversation to read its messages. In the
[TUI](/docs/usage/tui/), press `m` to switch to Texts mode.

| Local source | What to provide |
|---|---|
| [WhatsApp](#import-whatsapp) | Decrypted Android `msgstore.db` or Apple `ChatStorage.sqlite` |
| [iMessage](#import-imessage) | macOS `chat.db` |
| [iMazing Messages](#import-imazing-csv) | CSV export root or `csv/` directory |
| [Google Voice](#import-gvoice) | Google Takeout Voice directory |
| [Telegram](#import-telegram) | Telegram Desktop Machine-readable JSON export |
| [X DMs](#import-x) | Extracted X account archive |
| [Facebook Messenger](#import-messenger) | Download Your Information export |
| [SMS Backup & Restore](#import-synctech-sms) | XML backup |
| [Slackdump](/docs/usage/slack/#import-a-slackdump-export) | Export directory or ZIP |

## import-whatsapp

Use `import-whatsapp` to import direct and group chats from a decrypted Android
`msgstore.db` or an Apple `ChatStorage.sqlite` database. msgvault detects the
database format automatically:

```bash
msgvault import-whatsapp <database> --phone <your-number>
```

You must pass `--phone` in E.164 format (for example, `+447700900000`).

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

Point `import-whatsapp` at the native WhatsApp database or a copy of it:

```bash
msgvault import-whatsapp --phone +447700900000 \
  "$HOME/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite"
```

Reading the native store may require Full Disk Access for your terminal in
**System Settings → Privacy & Security**.

To keep your archive current with the daemon, add a scheduled
[`[[whatsapp_apple]]` entry](/docs/configuration/#apple-whatsapp-sources) and
run `msgvault serve`. Make sure to grant Full Disk Access to the process
running the daemon as well. The first run reads the whole store, and later runs
pick up changed messages.

To pick up new messages manually, run the same command again. Each run writes
only messages that are new or changed, including edited text, a sender or chat
that `LID.sqlite` now resolves to a phone number, or a message that moved to
another chat. Because `--limit` counts only those writes, unchanged messages do
not use it up. If a run is interrupted, run the command again to finish.
Messages you delete in WhatsApp stay in your archive.

A rerun reads only the chats that changed since the last successful import.
WhatsApp's database tracks a revision counter on every message and group member
row, and msgvault compares each chat's name, message count, row IDs, and
revision totals against what it recorded last time. msgvault reads every chat
again when `LID.sqlite`, the profile push names, the phone number, or the
database itself (its Core Data store ID) changes, when any other import of the
same source ran in between (even one that failed), or when the database lacks
those counters or that ID. To compare every message regardless, for example
after restoring an older WhatsApp backup or editing archived messages by other
means, pass `--full`.

Apple imports link text and URL replies to quoted messages in the same source.
If the quoted message is in another chat and only becomes available in a later
run, use `--full` to revisit the reply's chat and fill in the link.

The first import of a large database can take several minutes. To pull in
recent messages first, pass `--after` (`YYYY-MM-DD` in local time; `--before`
also works), then run the command again without date flags:

```bash
msgvault import-whatsapp --phone +447700900000 --after 2026-01-01 \
  "$HOME/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite"
```

`--after` includes that day and `--before` excludes it, and `--after` must be
earlier than `--before`. A dated run imports only messages inside that window
and never removes messages already in your archive, so the later full run adds
the rest. It still creates every chat, so chats with no messages in the window
appear empty until you run the full import. Android `msgstore.db` imports
reject `--after` and `--before`.

### Format limits

| Format                     | Imported today                                                                                                                                            | Not included                  |
| -------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------- |
| Android `msgstore.db`      | Chats, messages, participants, reactions, attachment metadata, and available media from `--media-dir`                                                     | Database decryption           |
| Apple `ChatStorage.sqlite` | Text and URL messages from direct and group chats, reply links to archived quotes, sender attribution, and available contact, participant, and push names | Media downloads and reactions |

When you use `--contacts` for name matching, vCard phone numbers must include a
country code. msgvault does not guess a country for local numbers. Apple
imports can also fall back to the sender names stored by WhatsApp when no
stronger name is available. Exports without the optional group-participant
table can still be imported.

Pass your own phone number with `--phone` for either format. msgvault records
it as the source's confirmed "me" identity unless you pass
`--no-default-identity`. This confirmation also happens after a completed run
that reports recoverable message errors.

## import-imessage

Use `import-imessage` to import messages from the local iMessage database on
macOS:

```bash
msgvault import-imessage
```

By default, the command reads from `~/Library/Messages/chat.db`. You do not
need any positional arguments.

To keep Messages current with the daemon, enable a schedule under
[`[imessage]`](/docs/configuration/#imessage), grant Full Disk Access to the
process running the daemon, and run `msgvault serve`. Every run re-reads all of
`chat.db` and writes only what changed, including late arrivals and edits. Pick
an interval that leaves enough time to read your database. **Sources** shows
the run history and offers **Sync now**.

!!! warning
    **Full Disk Access required.** macOS protects `~/Library/Messages/chat.db`. Before running this command, grant Full Disk Access to your terminal app in **System Settings > Privacy & Security > Full Disk Access**.

This import is read-only and does not modify your iMessage database.

msgvault copies eligible attachments referenced by `chat.db` into the archive
from the `Attachments` and `StickerCache` folders under `~/Library/Messages`.
Every run reads those files again, so changed files are archived again, and
bytes already archived stay in place if a file can no longer be read. If you
are importing a copied `chat.db`, pass `--messages-root` to point at the copied
Messages folder and rebase paths from a Mac home folder onto it. msgvault keeps
attachment metadata when a file is unavailable or exceeds the default 250 MiB
file limit or 20-participant conversation limit. The import summary counts
unavailable and skipped attachments (including paths outside those folders), so
an import without Full Disk Access on the `Attachments` folder does not look
clean. iMessage voice notes are `.caf` files. They stay in the archive but are
not transcribed, because Docbank only accepts the audio formats listed in
[Send audio to Docbank](/docs/usage/beeper/#send-audio-to-docbank).

To record attachment metadata without copying files, pass `--no-attachments`.
When you pass `--no-attachments`, msgvault skips the size and participant
limits and records metadata only. You can configure the participant and size
limits under `[imessage]` in `config.toml`, or override the file size for one
run with `--max-media-mb`; see [Configuration](/docs/configuration/#imessage).
Skipped attachments keep their metadata and the reason they were skipped. If
you rerun with a stricter limit, msgvault keeps bytes already archived for
files that have not changed.

msgvault stores Apple timestamp sentinels and dates outside years 1 through
9999 as missing dates, while keeping those messages and their raw timestamp
evidence (including empty sync placeholders). Undated messages are excluded
from dated analytics.

To fix dates imported by an older version of msgvault, rerun `import-imessage`
against the same database without `--after`, `--before`, or `--limit`, keeping
your original `--db-path` and `--me` options if you used them. The import
updates existing messages without creating duplicates. When it clears a
previously stored date, msgvault rebuilds analytics so the old date is removed
there too.

Each run reads all of `chat.db` again and updates archived messages to match.
Edited text replaces the archived text. When a message no longer has text in
`chat.db` (such as when someone unsends a message on iOS 16, macOS 13, or
later), the next import clears its archived text and snippet, and keyword
search no longer matches that text. The message itself stays in the archive
with its date and participants. Messages deleted from `chat.db` also stay in
the archive.

If msgvault cannot decode a message's text in `chat.db`, it keeps the archived
text and counts the message under `Unreadable text` in the import summary.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--db-path` | `~/Library/Messages/chat.db` | Path to chat.db |
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--me` | — | Your phone/email for recipient tracking |
| `--contacts` | — | Path to a `.vcf` file used to backfill participant display names |
| `--messages-root` | `~/Library/Messages` | Messages folder to read attachments from; rebase copied Mac paths here |
| `--no-attachments` | `false` | Do not copy attachment files (attachment metadata is still recorded) |
| `--max-media-mb` | `0` | Skip files larger than this many MiB; `0` uses `max_media_mb` from `[imessage]` (default 250) |

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

`--contacts` accepts a vCard file such as macOS Contacts.app's
**File > Export > Export vCard** output. msgvault matches display names by
phone number or email address and only fills in participant names that are
currently empty.

## import-imazing-csv

Use `import-imazing-csv` when you no longer have the original Apple Messages
database, but you still have an iMazing CSV export:

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

If you use the second layout, place `attachments/` next to the directory you
pass. CSV discovery does not recurse into subdirectories, and it accepts
comma, tab, or semicolon delimiters. Each CSV file must include named
`Chat Session`, `Message Date`, `Service`, and `Type` columns; msgvault also
imports the other standard iMazing columns when they are present.

The importer identifies group chats from observed senders. A title like
`A & B & C` can supply extra member names only after messages identify at least
two other senders, and a title alone does not create participants.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--me` | (required) | Your phone number or email address; determines outgoing identity |
| `--timezone` | local IANA zone (required on Windows) | Timezone used for dates without an explicit offset |
| `--contacts` | — | vCard file used to fill empty participant names by phone or email |

Pass `--timezone` if the export came from a different timezone or your local
zone cannot be resolved. When you omit `--timezone`, msgvault resolves the
local IANA zone on Unix hosts; Windows has no dependable local IANA zone, so
the flag is required there. msgvault stores the chosen IANA name with the
source, and any later import for the same `--me` value must use the same zone
to keep offset-free dates stable across machines and daylight-saving
transitions.

Running the command again converges on the same messages and attachment
occurrences and adds new rows without moving existing message IDs. msgvault
copies referenced files under `attachments/` into its content-addressed store
up to 100 MiB per file, and records larger files as skipped. Missing files and
filenames that match several files are reported as missing and can be resolved
on a later rerun. None of those outcomes stop the import or contact enrichment
from finishing. If a previously stored source file disappears, its archived
bytes remain available; if its bytes change, msgvault updates the same
attachment occurrence with the new content.

msgvault always keeps the raw `Replying to` value. It creates a reply link only
when that value contains exact body, sender, and rendered-date evidence for one
earlier message in the same conversation. Ambiguous replies stay unlinked.

!!! note
    iMazing CSV rows do not share IDs with Apple's `chat.db`. Rerunning the same CSV is deterministic, but importing overlapping history through both `import-imazing-csv` and `import-imessage` can create duplicates.

    `Chat Session` titles identify conversations within one imported source. Identical titles share a conversation. Renaming a title on a later export creates another conversation and can duplicate its messages.

## import-gvoice

Use `import-gvoice` to import texts, calls, and voicemails from a Google Voice
Takeout export:

```bash
msgvault import-gvoice <takeout-voice-dir>
```

Pass the `Voice` folder from a [Google Takeout](https://takeout.google.com)
export. It should contain a `Calls/` subdirectory and a `Phones.vcf` file.

!!! note
    Only text messages appear in TUI text mode. Call logs and voicemails are stored, though they are not currently browsable in the TUI.

When you extract the Takeout export, keep the voicemail recordings beside their
HTML files. msgvault archives each available recording as an attachment to its
voicemail so you can export it with the standard
[attachment export commands](exporting.md#export-all-attachments-from-a-message).

If a voicemail has no usable audio reference or its recording cannot be stored,
msgvault still adds an attachment record marked `failed` with reason
`fetch_failure` and zero stored bytes. A named recording keeps its source
filename. Because of this, attachment queries can distinguish missing audio
from a voicemail with an archived recording.

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

### Get your Google Voice data

1. Go to [Google Takeout](https://takeout.google.com).
2. Deselect all products, then select only **Google Voice**.
3. Export and download the archive.
4. Extract the ZIP file, then pass the `Voice` folder inside `Takeout` to `msgvault import-gvoice`.

## import-messenger

Use `import-messenger` to import Facebook Messenger conversations from a
Download Your Information export:

```bash
msgvault import-messenger --me <you@facebook.messenger> <dyi-export-dir>
```

You must pass `--me` using msgvault's synthetic Messenger identifier format,
for example `test.user@facebook.messenger`. msgvault uses it as the source
identifier and to mark which messages are yours.

Messenger DYI exports can contain JSON, HTML, or both. By default,
`--format auto` imports JSON when it is present because JSON preserves
millisecond timestamps and richer reaction data. To force a specific format,
pass `--format html`, `--format json`, or `--format both`.

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
    Facebook DYI exports do not contain stable participant IDs. msgvault synthesizes participant identifiers from names as `<slug>@facebook.messenger`, and treats identical slugs as the same participant.

## import-synctech-sms

Use `import-synctech-sms` to import XML or ZIP backups created by
**SMS Backup & Restore** by SyncTech Pty Ltd:

```bash
msgvault import-synctech-sms <path> --owner-phone <your-number>
```

You must pass `--owner-phone` in E.164 format. The importer can bring in SMS,
MMS, call logs, and MMS attachments from local XML or ZIP backup files.

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
    Encrypted SMS Backup & Restore backups are not supported. Turn off encryption in the Android app and export again before importing.

## SyncTech Google Drive sources

If **SMS Backup & Restore** uploads backups to Google Drive, configure a source
once and let `msgvault serve` import them on a schedule:

```bash
msgvault add-synctech-sms-drive phone-backups \
  --owner-phone +14155551234 \
  --folder-id <drive-folder-id> \
  --google-account you@gmail.com

msgvault sync-synctech-sms phone-backups
```

`add-synctech-sms-drive` adds a `[[synctech_sms.sources]]` entry to
`config.toml`. Drive imports skip files they already imported, wait for files
to settle before reading them, and stage downloads under the msgvault data
directory while the import runs.

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--folder-id` | (required) | Google Drive folder ID containing backup files |
| `--google-account` | (required) | Google account used for Drive access |
| `--schedule` | `30 4 * * *` | Cron schedule used by `msgvault serve` |
| `--oauth-app` | — | Named Google OAuth app to use |

## Browse texts

To search email, chats, calendar events, and meeting notes together, start
`msgvault serve` and open the [Web UI](/docs/web-ui/). Chat results stay
grouped as conversations so short message fragments do not crowd **Messages**.
Open any conversation to read its matching messages in context.

To browse from the terminal, launch the TUI and press `m` to cycle from Email
to Texts mode:

```bash
msgvault tui
```

Texts mode shows your conversations list, and you can select a conversation to
read its messages. Pressing `m` again moves to Meetings, then People when it is
available, and then back to Email.

Texts mode is only available once you have imported text data. See the
[TUI documentation](/docs/usage/tui/) for keyboard shortcuts and navigation.

## Deduplication

You can safely run any importer on this page multiple times. Rerunning the same
source import does not create duplicates within that source. Different formats
that do not share source IDs, such as an iMazing CSV and `chat.db`, can still
overlap.

## Resumable imports

Large importers save checkpoints where the source format supports them. Other
importers, including iMazing CSV, are deterministic. If a run is interrupted,
run the same command again to update existing rows in place.

## After you import

Most chat import and sync commands rebuild the analytics cache automatically.
If a newly imported or synced source does not show up in aggregate views right
away, run `msgvault build-cache`. Your imported texts, Teams messages, and
Discord conversations are then available in the Web UI and TUI.

```bash
# Launch the TUI and press 'm' for text mode
msgvault tui

# View updated archive stats
msgvault stats
```

## import-telegram

In Telegram Desktop, export your data as **Machine-readable JSON**, choose the
media you want to keep, and import the directory containing `result.json`:

```bash
msgvault import-telegram /path/to/TelegramExport
```

Full exports identify your account automatically. For a single-chat export,
pass your numeric Telegram user ID with `--me`:

```bash
msgvault import-telegram --me 482111 /path/to/chat-export
```

Imported messages include senders, replies, edit markers, and available media.
The importer skips service messages, unsupported entries, and polls, locations,
or contacts that have no text or media, and counts missing media in the import
summary.

A later or overlapping export adds new messages and fills media slots that
still lack bytes, while archived messages keep their original text, sender,
timestamp, and edit flag. Media you already stored does not change. Replies
link once their parent arrives in a later export. Telegram export users link to
Beeper users who share the same numeric Telegram user ID; for Beeper users
already in your archive, the link appears when a sync revisits their chat
(`sync-beeper --full` covers quiet chats).

If you also sync Telegram through Beeper,
[choose which copy of each chat to search](searching.md#search-one-copy-of-overlapping-chats).

Every exported chat becomes a conversation, even if it has no messages. On
each import, msgvault updates the conversation title with the export's chat
name when that name is non-empty.

If an import is interrupted, rerunning it resumes after the last completed chat
as long as `result.json` in the same directory has the same size and
modification time. Pass `--no-resume` to scan every chat again, or `--limit N`
to cap how many messages one run adds.

## import-x

Use `import-x` to import direct and group DMs from an extracted
[X account archive](https://help.x.com/en/managing-your-account/how-to-download-your-x-archive):

```bash
msgvault import-x /path/to/x-archive
msgvault import-x --me 100001 /path/to/x-archive/data
```

Point the command at the archive root or its `data` directory. `account.js`
supplies your numeric owner ID; if it is missing, you must pass
`--me <numeric-id>`, and msgvault rejects an override that conflicts with
`account.js`. Numeric X IDs stay separate from public usernames. Missing names,
timestamps, and recipients remain unknown, and malformed timestamps also stay
unknown while their original strings remain in message evidence.

The importer reads `direct-messages.js`, `direct-messages-group.js`, and their
`-partN.js` files with matching `window.YTD.direct_messages.partN` or
`window.YTD.direct_messages_group.partN` assignments. It reads each
`dmConversation`'s `conversationId` followed by its `messages` array and
imports `messageCreate` records. Redacted real records cover direct and group
messages, a group `joinConversation.participantsSnapshot`, and a direct-media
URL, while numbered parts, account metadata, and media bytes have synthetic
test coverage. HTML-only exports, ZIP files, and newer encrypted-chat formats
are not supported. What gets imported depends on the archive's contents, and
deleted or lifetime history is not guaranteed.

Imported messages appear in Texts and in search. Each message keeps its
original JSON, timestamp string, and source filename as evidence, though this
preserves imported message evidence rather than a complete backup of the
original export. Messages without usable dates stay in the archive and live
full-text search, while date-based cache views may omit them. Group membership
snapshots add observed participants without removing earlier observations or
inferring per-message recipients. One encountered snapshot and its provenance
stay in conversation metadata, though it does not establish the final roster.
The importer skips service and membership events as messages and counts them in
the summary, with invalid records and unknown events counted separately.

Local media comes from `direct_messages_media` or
`direct_messages_group_media`. The archive viewer names each file
`<messageCreate.id>-<URL filename>` in the corresponding directory, including
video URLs. Video naming has synthetic test coverage against the viewer code,
and no real video record is included in the fixtures. msgvault never fetches
URLs. Nonregular, missing, unsafe, or oversized files remain missing
attachments, and the existing 250 MiB chat attachment limit applies. Each JSON
event is capped at 1 MiB, while whole conversations stream with no
conversation-size limit.

Rerunning the import skips archived messages, retries attachment slots that
still lack bytes, and keeps first-imported text, raw evidence, and stored
media. You can replay an interrupted import safely, and absent data never
deletes archived data. The summary reports direct and group
conversation entries (which can repeat across numbered parts), added and
duplicate messages, skipped events, invalid records, and media outcomes. To
skip recording the source account as a default identity, pass
`--no-default-identity`.
