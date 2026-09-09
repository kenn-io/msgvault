---
last_edited: "2026-09-08"
title: Importing Local Email
description: Bring local email archives into msgvault, or backfill older Gmail and IMAP messages.
---

Bring old mail into the same searchable archive as your live accounts. Local
imports preserve message content and attachments; mailbox folders become
labels where the format provides them.

## Choose an import path

| What you have | Command or guide |
|---|---|
| Outlook `.pst` archive | [`import-pst`](#import-pst) |
| MBOX file or ZIP of MBOX files | [`import-mbox`](#import-mbox) |
| Maildir or Maildir++ archive | [`import-maildir`](#import-maildir) |
| MailMate-style tree of `.eml` files | [`import-eml`](#import-eml) |
| Apple Mail directory or backup | [`import-emlx`](#import-emlx) |
| Older messages still in Gmail or IMAP | [Historical import jobs](#historical-import-jobs) |
| Chat database or export | [Text messages](/docs/usage/text-messages/) or [Slackdump](/docs/usage/slack/#import-a-slackdump-export) |

Choose an identifier that names the owner of the export, usually an email
address. Imports and live syncs remain separate sources even when they use the
same address. Use [collections](/docs/usage/multi-account/#collections) to group
them and [deduplication](/docs/usage/deduplication/) to review overlapping mail.

For continuing sync from a non-Gmail provider, start with
[IMAP setup](/docs/setup/#add-an-imap-account).

## import-pst

Import a Microsoft Outlook PST archive.

```bash
msgvault import-pst <identifier> <pst-file>
```

The identifier is the email address associated with the archive. PST folder structure is preserved as labels, so `Inbox/Projects` becomes a searchable label path. Email messages are imported; calendar, contact, task, and note items are skipped automatically.

### Examples

```bash
# Import a PST archive
msgvault import-pst you@company.com /path/to/archive.pst

# Skip folders you do not want in the archive
msgvault import-pst you@outlook.com backup.pst --skip-folder "Deleted Items"

# Start from the beginning instead of resuming an interrupted run
msgvault import-pst you@outlook.com backup.pst --no-resume
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `pst` | Source type recorded in the database |
| `--skip-folder` | — | Folder name to skip, case-insensitive; repeat for multiple folders |
| `--no-resume` | `false` | Start fresh instead of resuming an interrupted import |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |

PST imports are resumable. msgvault records a content-based archive fingerprint so an interrupted import resumes only when the file still matches the checkpointed archive.

## import-mbox

Import a standard [MBOX](https://en.wikipedia.org/wiki/Mbox) file (any extension) or a `.zip` archive containing one or more MBOX files.

```bash
msgvault import-mbox <identifier> <export-file>
```

The identifier is the email address associated with the export (e.g., `you@example.com`). It does not need to be a Gmail address.

### Examples

```bash
# Import a single MBOX file
msgvault import-mbox you@example.com /path/to/export.mbox

# Import a zip containing multiple MBOX files
msgvault import-mbox you@example.com /path/to/export.zip

# HEY.com export (uses MBOX format internally)
msgvault import-mbox you@hey.com hey-export.zip --source-type hey --label hey
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `mbox` | Source type recorded in the database (e.g., `hey` for HEY.com exports) |
| `--label` | — | Label(s) to apply to imported messages (repeatable, or comma-separated) |
| `--no-resume` | `false` | Start fresh instead of resuming an interrupted import |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk (messages still record attachment metadata) |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

### Where to get MBOX files

Most email providers offer an MBOX export option:

- **Google Takeout**: Export your Gmail data as MBOX files at [takeout.google.com](https://takeout.google.com)
- **HEY.com**: Export from Settings, downloads as a `.zip` of MBOX files
- **Thunderbird**: Use the ImportExportTools NG add-on to export folders as MBOX
- **Fastmail, ProtonMail, Yahoo**: Check your provider's export/download settings

### Google Groups

Export groups you own with [Google Takeout](https://support.google.com/groups/answer/9975859?hl=en), then import the downloaded ZIP or an extracted group MBOX:

```bash
msgvault import-mbox test-group@googlegroups.com takeout.zip --source-type google-groups
msgvault import-mbox test-group@googlegroups.com topics.mbox --source-type google-groups
```

Select only Google Groups when creating the ZIP. A ZIP import reads every MBOX inside it, so a combined Gmail and Groups export would also import the Gmail files into this source. For a combined export, extract it first and import the individual group MBOX files. Localized MBOX filenames work too.

Messages retain their senders, recipients, dates, bodies, raw MIME, and attachments. Groups headers supply group labels and topic-state labels, including localized labels. Exported thread IDs keep replies together even when reply headers are absent; different groups remain separate. Without an exported thread ID, normal email reply threading applies.

The identifier names the group or archive. It is used as the group label when Groups headers are missing, and is not automatically added as your personal identity. For a ZIP containing several groups, use a stable archive name as the identifier; message headers provide each group label. Reuse the same identifier and source type when resuming or reimporting.

Membership CSV files, group settings, favorites, and other non-message export data are skipped. Google only includes archived group messages when the exporting account has access to download them; an export containing only personal Groups activity is not a message archive.

### Supported formats

The importer accepts:

- Plain mbox files with any extension (standard mboxo/mboxrd format)
- `.zip` archives containing one or more `.mbox` or `.mbx` files

ZIP archives are extracted to a cache directory and reused on subsequent runs, so re-importing the same zip does not re-extract.

## import-maildir

Import a stable snapshot of a Maildir or Maildir++ mailbox:

```bash
msgvault import-maildir ~/Maildir --identifier you@example.com
```

Each mailbox needs `cur`, `new`, and `tmp` directories. msgvault reads delivered
messages from `cur` and `new`, skips `tmp` and symlinks, and leaves the source
files unchanged. Nested folders and filename flags become archive labels.

Rerun the same command to add messages and labels or resume an interrupted
import. Filename changes do not create another copy. Reruns do not remove
previously archived messages or labels, so this is an accumulating archive
rather than a mirror of current mailbox state.

See the [command reference](../cli-reference.md#import-maildir) for flags,
label mappings, message size limits, and attachment failure behavior.

## import-eml

Import a MailMate-style mailbox tree containing standard `.eml` message files:

```bash
msgvault import-eml ~/MailExport --identifier you@example.com
```

The directory must contain `.mailbox` folders with `.eml` files directly inside
them. You can also point at one `.mailbox` folder. For example:

```text
MailExport/
├── Inbox.mailbox/
│   └── message-1.eml
└── Projects.mailbox/
    └── Planning.mailbox/
        └── message-2.eml
```

This creates the labels `Inbox` and `Projects/Planning`. The importer reads
nested `.mailbox` folders, preserves raw MIME and attachments, and adds every
mailbox label when the same raw message appears in several folders. A loose
`.eml` file or a directory without `.mailbox` folders is not an accepted input.

| Flag | Default | Description |
|---|---|---|
| `--identifier` | required | Account identifier for the imported mail |
| `--source-type` | `eml` | Source type recorded in the archive |
| `--no-resume` | `false` | Start a new import instead of resuming a checkpoint |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachment files |
| `--no-default-identity` | `false` | Do not confirm the identifier as this source's “me” identity |

## import-emlx

Import Apple Mail `.emlx` files from a Mail directory tree. The importer can auto-discover accounts by reading macOS `Accounts4.sqlite`, or accept explicit arguments.

```bash
# Auto-discover all accounts
msgvault import-emlx

# Specify the mail directory
msgvault import-emlx ~/Library/Mail/

# Legacy form: explicit identifier and directory
msgvault import-emlx me@gmail.com ~/Downloads/mail-2009/Mail/
```

When run without arguments, the importer reads `~/Library/Accounts/Accounts4.sqlite` to map Apple Mail directory GUIDs to email addresses, then imports all discovered accounts. Use `--account` to filter to specific accounts during auto-discovery.

The mail directory should be an Apple Mail mailbox tree containing `.mbox` or `.imapmbox` directories, each with a `Messages/` subdirectory of `.emlx` files. You can also point directly at a single `.mbox` directory.

### Examples

```bash
# Auto-discover and import all accounts
msgvault import-emlx

# Auto-discover but only import one account
msgvault import-emlx --account me@gmail.com

# Import from a specific directory
msgvault import-emlx ~/Library/Mail/

# Import a single mailbox
msgvault import-emlx me@gmail.com ~/Mail/INBOX.mbox/
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `apple-mail` | Source type recorded in the database |
| `--account` | — | Filter to specific account(s) during auto-discover (repeatable) |
| `--accounts-db` | — | Custom path to macOS `Accounts4.sqlite` |
| `--identifier` | — | Manual identifier when auto-discover is not suitable |
| `--no-resume` | `false` | Start fresh instead of resuming an interrupted import |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk (messages still record attachment metadata) |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

### How Apple Mail organizes files

Apple Mail stores each message as an individual `.emlx` file. The on-disk layout depends on your macOS version.

**Modern layout (macOS 13+, V10):**

Messages live under versioned directories (`V10/`, `V9/`, etc.) with GUID subdirectories for each account and mailbox. Large mailboxes use numeric partition directories to spread `.emlx` files across subdirectories:

```
~/Library/Mail/V10/
└── 13C9A646-.../                    (account GUID)
    ├── INBOX.mbox/
    │   └── 9F0F15DD-.../           (mailbox GUID)
    │       └── Data/
    │           ├── Messages/
    │           │   └── 1.emlx
    │           ├── 0/
    │           │   └── 3/
    │           │       └── Messages/
    │           │           └── 123.emlx
    │           └── 9/
    │               └── Messages/
    │                   └── 456.emlx
    └── Sent Messages.mbox/
        └── .../
```

The importer discovers `.emlx` files in all partition subdirectories at arbitrary nesting depths. When `~/Library/Mail/` contains multiple versioned directories, the newest one is used to avoid importing stale data from previous macOS upgrades.

**Legacy layout (older macOS):**

```
~/Library/Mail/
├── INBOX.mbox/
│   └── Messages/
│       ├── 1.emlx
│       └── 2.emlx
├── IMAP-user@gmail.com/
│   ├── INBOX.imapmbox/
│   │   └── Messages/
│   └── [Gmail]/All Mail.imapmbox/
│       └── Messages/
└── Mailboxes/
    └── Projects/
        └── Work.mbox/
            └── Messages/
```

Both layouts are supported. The importer discovers all `.mbox` and `.imapmbox` directories automatically. Labels are derived from directory names: `Mailboxes/Projects/Work.mbox` becomes the label `Projects/Work`, and `INBOX.imapmbox` becomes `INBOX`.

### Where to find Apple Mail files

Apple Mail stores its data at `~/Library/Mail/` on macOS. The auto-discover mode reads `~/Library/Accounts/Accounts4.sqlite` (the macOS accounts database) to map V10 directory GUIDs to email addresses. You can also use a Time Machine backup or a copy of the Mail directory from another machine.

!!! note
    Apple Mail stores IMAP and Gmail messages whose attachments have not been downloaded as `.partial.emlx` files. The message body in these files is complete, so they are imported normally — only the uncached attachment parts are absent. When both `N.emlx` and `N.partial.emlx` exist for the same message, the fully-downloaded copy is used. The import summary reports how many partial files were imported.

## Deduplication

MBOX, EML, and EMLX imports deduplicate messages by SHA-256 hash of the raw MIME content. Running the same import twice produces no duplicates. If the same message appears in multiple mailboxes within that source, it is stored once and given labels from each location.

PST imports namespace source message IDs by a stable archive fingerprint, so importing multiple PST files into the same source does not collide on Outlook EntryIDs that are only unique inside one archive. Re-running the same PST import is idempotent and resumes from checkpoints by default.

## Resumable Imports

Imports are resumable by default. If an import is interrupted (Ctrl+C, power loss, error), run the same command again and it picks up from the last checkpoint. Use `--no-resume` to discard progress and start fresh.

During import, a progress summary is printed on completion:

```
Import complete.
  Imported:           /path/to/export.mbox
  Processed:      1234 messages
  Added:          1200 messages
  Updated:          30 messages
  Skipped (dup):     4 messages
  Errors:            0
  Bytes:          45.67 MB
```

## Error Handling

Review the final error count as well as the number of messages added. The
importers recover usable headers and content from malformed MIME where
possible. File-read failures and incomplete attachment downloads can still
leave gaps; keeping raw MIME for a stored message does not prove the whole
export was imported. Correct the reported errors and rerun the import.

## After Importing

Import commands refresh the analytical cache after writing. Imported mail is
then available through the Web UI, TUI, CLI, and MCP. If a cache refresh was
interrupted or failed, rebuild it:

```bash
msgvault build-cache
```

Then explore your imported messages:

```bash
# Search imported messages
msgvault search from:alice@example.com

# Open the browser interface
msgvault serve

# Launch the TUI
msgvault tui

# View updated stats
msgvault stats
```

## Historical import jobs

Backfill an older date range from an existing Gmail or IMAP account while the
daemon works in the background. This reads the provider; it does not upload a
local archive file.

1. Use the account identifier or an unambiguous display name from
   `msgvault list-accounts`.
2. Start a bounded import through the API:

   ```bash
   curl http://localhost:8080/api/v1/imports \
     -H "Authorization: Bearer $MSGVAULT_API_KEY" \
     -H "Content-Type: application/json" \
     --data '{"account":"you@example.com","after":"2020-01-01","before":"2021-01-01"}'
   ```

3. Save the returned `job_id` and poll its progress:

   ```bash
   curl -H "Authorization: Bearer $MSGVAULT_API_KEY" \
     http://localhost:8080/api/v1/imports/JOB_ID
   ```

The start request returns `202 Accepted`. Status moves from `pending` to
`running`, then `done` or `failed`. Closing the requesting client does not
cancel the daemon's job. Progress includes processed, added, and skipped counts;
a completed job also includes a summary with updates and errors. After a daemon
restart, unfinished jobs are marked `failed`. Submit another job with the same
account and bounds to continue from available import checkpoints.

Jobs accept optional `limit` and `noresume` fields. Gmail jobs also accept a
Gmail `query`; IMAP jobs reject that field. An account with an active sync
rejects another import with `409 sync_already_active`. See the
[API reference](/docs/api-server/#historical-import-jobs) for the request,
response, and restart behavior.

For a foreground CLI workflow, use `sync-full` with the same date bounds:

```bash
msgvault sync-full you@example.com --after 2020-01-01 --before 2021-01-01
```

## Images hosted outside the message

Email imports store attachments embedded in the original message. Images
loaded from a sender's website need a separate download and can trigger
tracking. [Remote image archiving](/docs/usage/remote-images/) explains the
explicit opt-in for new imports and the command for existing mail.
