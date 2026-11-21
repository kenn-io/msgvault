---
title: LinkedIn Export Import
description: Import LinkedIn messages, your own comments and posts, and connection identities from a member data export.
last_edited: "2026-11-20"
---

Import a LinkedIn member data export to search your direct messages, posts, and
comments in msgvault. This importer is **unreleased**. It reads local files and
needs no API app, token, or network request.

## Get and import an export

1. In LinkedIn, open **Settings & Privacy → Data privacy → Get a copy of
   your data**.
2. Request your messages, comments, shares, and optionally connections, or
   request the full archive.
3. Download the ZIP. You can import it directly or extract it first.
4. On the daemon host, run the command with a stable identifier for this
   archive and your own member profile URL in `--me`:

   ```bash
   msgvault import-linkedin example-account ./linkedin-export.zip \
     --me https://www.linkedin.com/in/alex-example/ \
     --display-name 'Alex Example'
   ```

The identifier names this archive source and can be a nickname, while
`--display-name` is optional. Use the same identifier and `--me` URL
when you import later exports from the same account. Relative paths resolve from the
directory where you run the command. You can't import local files through a
configured remote daemon. Run the command on that host with `--local` instead.

## What is imported

| Export file | Result |
|---|---|
| `messages.csv`, including nested `messages/` folders | One message per row, grouped by conversation ID and titled with the conversation title. Subjects, text, UTC dates, sender and recipients are preserved. The folder becomes a label. |
| `Comments.csv` | Your own comments, attributed to the owner supplied with `--me`, grouped by linked post URL. Backslash-escaped quotes and line breaks are decoded. |
| `Shares.csv` | Your posts, grouped by share URL. An imported comment on the same URL points to the archived post. |
| `Connections.csv` | Profile names and URLs, plus available email observations. Connections never create messages. |

The importer matches filenames and headers case-insensitively and handles any
column order, UTF-8 BOMs, extra columns, and missing optional columns. It skips
Connections preambles, reads ZIP entries in place, and skips symbolic links
during directory imports. Each CSV is limited to 64 MiB of uncompressed data,
including CSVs read from inside a ZIP.

Every message needs a valid date, and a DM without a conversation ID stays in
its own conversation. If the importer hits malformed CSV or an invalid date, it
stops and reports the file type and record number, while keeping any rows it
already imported. Recipient URL lists keep empty positions; when recipient
names can't be paired with recipient profile URLs, the importer keeps the
recipient URLs, drops the names, and continues the import.

For comments, the importer uses standard CSV quoting whenever that parses
cleanly. It falls back to the legacy backslash form only when the entire
transformed CSV validates. If neither form parses, it keeps the original input
and reports the malformed record. Comments without a valid post link stay
standalone, with no reply parent.

Message attachment URLs, post links, shared URLs, and media URLs stay as link
attachments. msgvault doesn't download their targets, and those links may
expire. Unknown export columns are preserved in the archived raw row.

## Repeat imports and drafts

To update your archive with a newer ZIP or folder, run the same command again.
DMs match on conversation ID, sender identity, UTC date, and a hash of the
subject, text, and attachment links. Without a conversation ID, the key also
includes the recipients in a stable order. Reordered rows and repeated exports
update existing messages and add new rows, while absent rows never delete
archived messages. Because the export has no stable message ID, a change to the
subject, text, or attachment links creates a separate message. Posts match by
URL when present, and comments match by post URL, date, and content.

If an import is interrupted or fails, run it again from the start. Existing
message identities prevent duplicate rows. Other source identifiers, including
Beeper's LinkedIn bridge, stay separate and are not merged with this importer.
If you import overlapping history from both sources, you can see duplicates.

Drafts are skipped by default. To include them:

```bash
msgvault import-linkedin example-account ./linkedin-export \
  --me https://www.linkedin.com/in/alex-example/ --include-drafts
```

Pass `--json` to print the source ID along with the added, updated,
skipped-draft, and connection counts on stdout.

## People and limits

The `--me` URL is confirmed as this source's owner identity, so relationship
activity can exclude your own participant. Each profile URL identifies one
LinkedIn participant, and names supply display labels without merging different
URLs that share a name. When a URL is missing, a name identifies a participant
only within this archive source. Importing doesn't create saved people; use the
[people tools](people.md) to save a participant or link one to an existing
person. Email addresses from Connections are stored as observations rather than
automatic cross-source merges.

The export contains comments **you wrote**, not a complete archive of comments
other people left on your posts. This importer doesn't read a Company Page's
inbox, poll LinkedIn, or call its live API. CSV behavior has been checked with
synthetic fixtures, and no live LinkedIn API or personal export was used to
verify this integration.
