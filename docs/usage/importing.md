---
title: Importing Local Email
description: Import PST archives, MBOX archives, and Apple Mail exports into msgvault.
---

msgvault can import email from local files, not just Gmail. This lets you archive Microsoft Outlook PST files, messages from any provider that supports MBOX export, or Apple Mail's on-disk storage. For live syncing from non-Gmail providers, see [IMAP account setup](/setup/#add-an-imap-account).

Imported messages are stored in the same database as Gmail messages. You can search, browse, export, and analyze them with all the same tools (TUI, CLI, MCP server, web server). Labels, threading, attachments, and full-text search all work the same way.

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

### Supported formats

The importer accepts:

- Plain mbox files with any extension (standard mboxo/mboxrd format)
- `.zip` archives containing one or more `.mbox` or `.mbx` files

ZIP archives are extracted to a cache directory and reused on subsequent runs, so re-importing the same zip does not re-extract.

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

MBOX and EMLX imports deduplicate messages by SHA-256 hash of the raw MIME content. Running the same import twice produces no duplicates. If the same message appears in multiple MBOX files or Apple Mail mailboxes, it is stored once and given labels from each location.

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

Individual messages that fail to parse are logged and skipped; the import continues. Raw MIME bytes are preserved even for messages that cannot be fully parsed, so no data is lost. Attachment storage errors are also non-fatal.

## After Importing

Imported messages are immediately available for search, TUI browsing, and MCP queries. To update the Parquet analytics cache (used by the TUI for fast aggregation), run:

```bash
msgvault build-cache
```

Then explore your imported messages:

```bash
# Search imported messages
msgvault search from:alice@example.com

# Launch the TUI
msgvault tui

# View updated stats
msgvault stats
```
