---
last_edited: "2026-11-19"
title: Importing Local Email
description: Bring local email archives into msgvault, or backfill older Gmail and IMAP messages.
---

You can bring old mail into the same searchable archive as your live accounts.
Local imports keep your message content and attachments, and turn mailbox
folders into labels wherever the format provides them.

## Choose an import path

| What you have | Command or guide |
|---|---|
| Outlook `.pst` archive | [`import-pst`](#import-pst) |
| MBOX file or ZIP of MBOX files | [`import-mbox`](#import-mbox) |
| Maildir or Maildir++ archive | [`import-maildir`](#import-maildir) |
| MailMate-style tree of `.eml` files | [`import-eml`](#import-eml) |
| Apple Mail directory or backup | [`import-emlx`](#import-emlx) |
| Calendar `.ics` file | [`import-ics`](../cli-reference.md#import-ics) |
| Older messages still in Gmail or IMAP | [Historical import jobs](#historical-import-jobs) |
| Chat database or export | [Text messages](/docs/usage/text-messages/) or [Slackdump](/docs/usage/slack/#import-a-slackdump-export) |
| LinkedIn member data export | [LinkedIn](/docs/usage/linkedin/) |
| TikTok **Download your data** export | [TikTok](/docs/usage/tiktok-export/) |
| RSS, Atom, or JSON feeds | [Feeds](/docs/usage/feeds/) |
| Messages from a source without a connector, such as tool notifications | [Messages from any source](/docs/usage/message-import/) |

Pick an identifier that names the owner of the export, usually an email
address. Even when an import and a live sync share the same address, msgvault
keeps them as separate sources. You can group them with
[collections](/docs/usage/multi-account/#collections) and review overlapping
mail with [deduplication](/docs/usage/deduplication/).

If you want ongoing sync from a non-Gmail provider, start with
[IMAP setup](/docs/setup/#add-an-imap-account).

## import-pst

Use `import-pst` to import a Microsoft Outlook PST archive:

```bash
msgvault import-pst <identifier> <pst-file>
```

Pass the email address associated with the archive as the identifier. The
importer preserves the PST folder structure as labels, so `Inbox/Projects`
becomes a searchable label path. It imports email messages and skips calendar,
contact, task, and note items automatically.

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

You can resume an interrupted PST import. msgvault records a
content-based archive fingerprint so a rerun only resumes when the file still
matches the checkpointed archive.

The importer keeps existing transport threading headers and fills in missing
`Message-ID`, `In-Reply-To`, and `References` fields from the PST's stored
email metadata (MAPI properties). Because it groups replies by archived email
identifiers after importing all folders, replies can appear before their
parents in the file. Messages without usable threading identifiers keep the
existing fallback grouping.

To repair missing identifiers from an earlier import, run the command again
with the same PST file, the same identifier, and `--no-resume`:

```bash
msgvault import-pst owner@example.com /path/to/archive.pst --no-resume
```

That rerun fills missing header metadata and reconciles conversations while
keeping your existing raw messages, bodies, attachments, and any conflicting
stored identifiers. Existing messages count as skipped and still receive their
folder labels as usual.

## import-mbox

Use `import-mbox` to import a standard
[MBOX](https://en.wikipedia.org/wiki/Mbox) file (with any extension) or a
`.zip` archive containing one or more MBOX files:

```bash
msgvault import-mbox <identifier> <export-file>
```

The identifier is the email address associated with the export (for example,
`you@example.com`), and it does not need to be a Gmail address.

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

Most email providers let you export your mail as MBOX files:

- **Google Takeout.** Export your Gmail data as MBOX files at [takeout.google.com](https://takeout.google.com).
- **HEY.com.** Export from Settings to download a `.zip` archive of MBOX files.
- **Thunderbird.** Use the ImportExportTools NG add-on to export folders as MBOX.
- **Fastmail, ProtonMail, Yahoo.** Check your provider's export or download settings.

### Google Groups

Export groups you own with
[Google Takeout](https://support.google.com/groups/answer/9975859?hl=en), then
run `import-mbox` on either the downloaded ZIP or an extracted group MBOX file:

```bash
msgvault import-mbox test-group@googlegroups.com takeout.zip --source-type google-groups
msgvault import-mbox test-group@googlegroups.com topics.mbox --source-type google-groups
```

Select only Google Groups when you create the ZIP. Because a ZIP import reads
every MBOX inside the archive, a combined Gmail and Groups export would also
pull the Gmail files into this source. If you have a combined export, extract
it first and import the individual group MBOX files. Localized MBOX filenames
work as well.

Imported messages keep their senders, recipients, dates, bodies, raw MIME, and
attachments. Groups headers supply group labels and topic-state labels,
including localized ones. Exported thread IDs keep replies together even when
reply headers are absent, and different groups stay separate. When an exported
thread ID is missing, normal email reply threading applies.

The identifier names the group or archive. msgvault uses it as the group label
when Groups headers are missing, and does not automatically add it as your
personal identity. For a ZIP that holds several groups, pick a stable archive
name as the identifier and let message headers supply each group label. Reuse
the same identifier and source type when you resume or re-import.

The importer skips membership CSV files, group settings, favorites, and other
non-message export data. Google only includes archived group messages when the
exporting account has access to download them. An export containing only
personal Groups activity is not a message archive.

### Supported formats

The importer accepts:

- Plain mbox files with any extension (standard mboxo/mboxrd format)
- `.zip` archives containing one or more `.mbox` or `.mbx` files

msgvault extracts ZIP archives to a cache directory and reuses them on later
runs, so re-importing the same ZIP does not extract it again.

## import-maildir

Import a stable snapshot of a Maildir or Maildir++ mailbox:

```bash
msgvault import-maildir ~/Maildir --identifier you@example.com
```

Each mailbox needs `cur`, `new`, and `tmp` directories. msgvault reads
delivered messages from `cur` and `new`, skips `tmp` and symlinks, and leaves
your source files untouched. Nested folders and filename flags become archive
labels.

To add new messages and labels or resume an interrupted run, run the same
command again. Filename changes do not create duplicate copies. Reruns do not
remove messages or labels you already archived, so this builds an accumulating
archive rather than mirroring the current mailbox state.

See the [command reference](../cli-reference.md#import-maildir) for flags,
label mappings, message size limits, and attachment failure behavior.

## import-eml

Import a MailMate-style mailbox tree containing standard `.eml` message files:

```bash
msgvault import-eml ~/MailExport --identifier you@example.com
```

The directory needs `.mailbox` folders with `.eml` files directly inside them,
or you can point at a single `.mailbox` folder. For example:

```text
MailExport/
├── Inbox.mailbox/
│   └── message-1.eml
└── Projects.mailbox/
    └── Planning.mailbox/
        └── message-2.eml
```

This layout creates the labels `Inbox` and `Projects/Planning`. The importer
reads nested `.mailbox` folders, keeps raw MIME and attachments, and applies
every mailbox label when the same raw message appears in multiple folders. It
does not accept a loose `.eml` file or a directory without `.mailbox` folders.

| Flag | Default | Description |
|---|---|---|
| `--identifier` | required | Account identifier for the imported mail |
| `--source-type` | `eml` | Source type recorded in the archive |
| `--no-resume` | `false` | Start a new import instead of resuming a checkpoint |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachment files |
| `--no-default-identity` | `false` | Do not confirm the identifier as this source's “me” identity |

## import-emlx

Import Apple Mail `.emlx` files from a Mail directory tree. The importer can
discover your accounts automatically by reading macOS `Accounts4.sqlite`, or
you can pass explicit arguments:

```bash
# Auto-discover all accounts
msgvault import-emlx

# Specify the mail directory
msgvault import-emlx ~/Library/Mail/

# Legacy form: explicit identifier and directory
msgvault import-emlx me@gmail.com ~/Downloads/mail-2009/Mail/
```

When you run `import-emlx` with no arguments, it reads
`~/Library/Accounts/Accounts4.sqlite` to map Apple Mail directory GUIDs to
email addresses and imports every account it finds. To limit auto-discovery to
specific accounts, pass `--account`.

Your mail directory needs to be an Apple Mail mailbox tree with `.mbox` or
`.imapmbox` directories, each containing a `Messages/` subdirectory of `.emlx`
files. You can also point directly at a single `.mbox` directory.

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
| `--accounts-db` | `~/Library/Accounts/Accounts4.sqlite` | Custom path to macOS `Accounts4.sqlite` |
| `--identifier` | — | Manual identifier when auto-discover is not suitable |
| `--no-resume` | `false` | Report totals for this run only; completed file receipts still apply |
| `--full-reconcile` | `false` | Reconcile every file after invalidating the selected root's receipts |
| `--max-message-bytes` | 128 MiB | Maximum EMLX file and merged MIME bytes, including encoding |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk (messages still record attachment metadata) |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

If restoring attachments pushes a message over the byte limit, msgvault leaves
previously archived attachments intact, counts an error in the summary, and
retries the file on the next import. Pick a larger limit that fits your
available memory and rerun the same import. For example, this raises the limit
to 256 MiB per message in automatic account mode:

```bash
msgvault import-emlx --max-message-bytes 268435456 ~/Library/Mail/
```

You can also use `--max-message-bytes` with `--identifier` in manual mode. If
you omit it, msgvault uses the 128 MiB default. See the
[CLI reference](/docs/cli-reference/#import-emlx) for the accepted byte range.
Changing the limit causes the next import to read each file once more.

### Message identifiers and replies

The importer saves each email's `Message-ID` in `rfc822_message_id` without
surrounding angle brackets, keeping its original case. It links `In-Reply-To`
to a message in the same source when exactly one visible parent matches.
Missing or ambiguous parents stay unresolved, and a later import can link a
parent that arrives afterward.

To fill missing identifiers in an archive created by an older version of
msgvault, re-import the same directory with the same source identifier and then
rebuild the analytics cache. Messages archived before file receipts existed
are finished again automatically on the first import. If you want to redo every
file, for example after editing archived messages directly, pass
`--full-reconcile`:

```bash
msgvault import-emlx --full-reconcile me@gmail.com ~/Mail/INBOX.mbox/
msgvault build-cache --full-rebuild
msgvault show-message 123 --json
msgvault query "SELECT id, rfc822_message_id FROM messages LIMIT 10"
```

`show-message --json` returns `rfc822_message_id` as a string, or an empty
string when it is absent, while analytics queries keep SQL `NULL` for absent
identifiers. Re-importing fills missing identifiers without overwriting
existing message content or nonempty identifiers.

To build a Mail.app link, wrap the stored ID in angle brackets and URL-encode
it. For example, `Case-ID@example.test` becomes
`message://%3CCase-ID%40example.test%3E`.

### How Apple Mail organizes files

Apple Mail stores each message in its own `.emlx` file, with an on-disk layout
that depends on your macOS version.

On macOS 13 and later (the V10 layout), messages live under versioned
directories (`V10/`, `V9/`, and so on) with GUID subdirectories for each
account and mailbox. Large mailboxes spread `.emlx` files across numeric
partition subdirectories:

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

The importer finds `.emlx` files across all partition subdirectories at any
nesting depth. When `~/Library/Mail/` holds multiple versioned directories,
msgvault reads the newest one so it does not import stale data from earlier
macOS upgrades.

On older macOS versions (the legacy layout), Apple Mail uses a flatter
directory tree:

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

msgvault supports both layouts and discovers all `.mbox` and `.imapmbox`
directories automatically. It derives labels from the directory names. For
example, `Mailboxes/Projects/Work.mbox` becomes `Projects/Work`, and
`INBOX.imapmbox` becomes `INBOX`.

### Where to find Apple Mail files

On macOS, Apple Mail stores its data at `~/Library/Mail/`. Auto-discover mode
reads `~/Library/Accounts/Accounts4.sqlite` (the macOS accounts database) to
map V10 directory GUIDs to email addresses. You can also point the importer at
a Time Machine backup or a copy of the Mail directory from another machine.

!!! note
    Apple Mail stores IMAP and Gmail messages with larger attachments as `.partial.emlx` files, where the message body is complete and the attachments live in a sibling `Attachments/` directory. The importer restores cached top-level attachments, though nested attachments are not restored. Re-import to add newly downloaded attachments to existing messages; messages that already have every cached attachment are skipped rather than rewritten. See [import-emlx](../cli-reference.md#import-emlx) for limits and warnings. When both `N.emlx` and `N.partial.emlx` exist for the same message, msgvault uses the full copy. The import summary reports how many partial files it read and how many attachments the run added to the archive.

## Deduplication

MBOX, EML, and EMLX imports deduplicate messages using the SHA-256 hash of
their raw MIME content, and running the same import twice produces no
duplicates. When the same message appears in multiple mailboxes within a
source, msgvault stores it once and attaches labels from each location.

PST imports namespace source message IDs with a stable archive fingerprint, so
importing multiple PST files into the same source does not collide on Outlook
EntryIDs that are only unique inside a single archive. Rerunning the same PST
file is idempotent, and it resumes from checkpoints by default.

### Repeat Apple Mail imports

A repeat Apple Mail import only reads new and changed `.emlx` files. It still
walks every mailbox and checks each file's metadata, including any attachments
Apple Mail cached for it, so a newly downloaded attachment or an older message
added late still gets imported. If a file's attachment or search-index work
failed earlier, msgvault retries it before trusting its receipt. Removing files
from Apple Mail's cache leaves their archived messages and labels untouched.

Problems with individual files count as errors without failing the whole run,
and the next import retries those files. See
[import-emlx](../cli-reference.md#import-emlx) for what counts as an error.

To redo every file under the selected directory even when its content already
matches the archive, run with `--full-reconcile` and the same source
identifier. If that run is interrupted, the next ordinary import finishes the
remaining files. Passing `--no-resume` only resets the run's reported totals
and still skips unchanged files.

## Resumable imports

Imports resume by default. If a run is interrupted by Ctrl+C, a power loss, or
an error, run the same command again to pick up from the last checkpoint. Apple
Mail (EMLX) imports are an exception. They always revisit every file and rely
on file receipts rather than a checkpoint position to skip completed work. To
discard saved progress and start fresh, pass `--no-resume`.

When an import finishes, msgvault prints a completion summary:

```
Import complete.
  Imported:           /path/to/export.mbox
  Processed:      1234 messages
  Added:          1200 messages
  Updated:        30 messages
  Skipped:        4 messages
  Labels updated: 0 messages
  Errors:         0
  Bytes:          45.67 MB
```

## Error handling

Check the final error count along with the number of messages added. The
importers recover usable headers and content from malformed MIME where they
can, though file-read failures and incomplete attachment downloads can still
leave gaps. Having raw MIME stored for a message does not guarantee the entire
export imported cleanly. Fix the reported errors and rerun the import.

## After you import

Import commands refresh the analytical cache after writing, and your imported
mail is then available in the Web UI, TUI, CLI, and MCP. If a cache refresh
was interrupted or failed, rebuild it:

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

You can backfill an older date range from an existing Gmail or IMAP account
while the daemon works in the background. This reads directly from your mail
provider rather than uploading a local archive file.

1. Find the account identifier or an unambiguous display name by running
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

The start request returns `202 Accepted`, and the job status moves from
`pending` to `running` and then to `done` or `failed`. Closing the requesting
client does not cancel the job on the daemon. Progress responses report
processed, added, and skipped counts, and a completed job adds a summary with
updates and errors. If the daemon restarts, any unfinished jobs are marked
`failed`. Submit another job with the same account and date
bounds to continue from available import checkpoints.

Jobs accept optional `limit` and `noresume` fields. Gmail jobs also accept a
Gmail `query`, which IMAP jobs reject. If an account already has an active
sync, the API rejects another import with `409 sync_already_active`. See the
[API reference](/docs/api-server/#historical-import-jobs) for request,
response, and restart details. To start and follow a job through an assistant,
see [MCP source status and sync](/docs/usage/chat/#source-status-and-sync).

If you prefer a foreground CLI workflow, run `sync-full` with the same date
bounds:

```bash
msgvault sync-full you@example.com --after 2020-01-01 --before 2021-01-01
```

## Images hosted outside the message

Email imports store attachments embedded in the original message, while images
loaded from a sender's website require a separate download and can trigger
tracking. See [Remote image archiving](/docs/usage/remote-images/) for the
opt-in setting for new imports and the backfill command for existing mail.
