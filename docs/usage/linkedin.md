---
title: LinkedIn Export Import
description: Import LinkedIn messages, your own comments and posts, and connection identities from a member data export.
last_edited: 2026-11-02
---

Import a LinkedIn member data export to search your DMs, posts, and comments
in msgvault. This importer is **unreleased**. It reads local files without an
API app, token, or network request.

## Get and import an export

1. In LinkedIn, open **Settings & Privacy → Data privacy → Get a copy of
   your data**. Request your messages, comments, shares, and optionally
   connections, or request the full archive.
2. Download the ZIP. You can import it directly or extract it first.
3. Run the command on the daemon host. Give the archive a stable identifier
   and supply your own member profile URL with `--me`:

   ```bash
   msgvault import linkedin example-account ./linkedin-export.zip \
     --me https://www.linkedin.com/in/alex-example/ \
     --display-name 'Alex Example'
   ```

`--display-name` is optional. The identifier names this archive source; it can
be a nickname. Use the same identifier and `--me` URL for later exports from
the same account. Relative paths resolve from the invoking directory. Local
files cannot be imported through a configured remote daemon; run the command
on that host with `--local` instead.

## What is imported

| Export file | Result |
|---|---|
| `messages.csv`, including nested `messages/` folders | One message per row, grouped by conversation ID and titled with the conversation title. Subjects, text, UTC dates, sender and recipients are preserved. The folder becomes a label. |
| `Comments.csv` | Your own comments, attributed to the owner supplied with `--me`, grouped by linked post URL. Backslash-escaped quotes and line breaks are decoded. |
| `Shares.csv` | Your posts, grouped by share URL. An imported comment on the same URL points to the archived post. |
| `Connections.csv` | Profile names and URLs, plus available email observations. Connections never create messages. |

Filenames and headers are matched without case sensitivity. Column order,
UTF-8 BOMs, extra columns, and missing optional columns are supported.
Connections preambles are skipped. Every message needs a valid date. A DM
without a conversation ID stays in its own conversation. Malformed CSV or an
invalid date stops the import with the file type and record number; completed
rows remain available. Each CSV is limited to 64 MiB, including when compressed.
ZIP entries are read in place. Directory imports skip symbolic links. Recipient
URL lists retain empty positions. When names and URLs cannot be aligned, a
record error stops the import instead of dropping recipients.

Message attachment URLs, post links, shared URLs, and media URLs remain link
attachments. msgvault does not download their content, and the links may
expire. Unknown export columns are retained in the archived raw row.

## Repeat imports and drafts

Run the same command with a newer ZIP or folder. DMs match on conversation ID,
sender identity, UTC date, and a hash of the text and attachment links. Without
a conversation ID, the key also includes the recipients in a stable order.
Reordered rows and repeated exports update existing messages, and new rows are
added. A change to the text or attachment links creates a separate message
because the export has no stable message ID. Posts match by URL when present; comments match by post URL, date, and content.
Absent rows never delete archived messages.

An interrupted or failed import can be repeated from the start. Existing
message identities prevent duplicate rows. Other source identifiers, including
Beeper's LinkedIn bridge, remain separate: the importer does not merge their
messages. Importing overlapping history from both sources can show duplicates.

Drafts are skipped by default. To archive them too:

```bash
msgvault import linkedin example-account ./linkedin-export \
  --me https://www.linkedin.com/in/alex-example/ --include-drafts
```

`--json` emits the source ID and added, updated, skipped-draft, and connection
counts on stdout. Command errors are JSON on stderr when that flag is set;
text errors include a `hint:` line.

## People and limits

The `--me` URL is confirmed as this source's owner identity, so relationship
activity can exclude your own participant. Profile URLs anchor LinkedIn
participants and their people profiles. Names
supply display labels; identical names do not merge different URLs. When a URL
is missing, a name identifies a participant only within this archive source.
Use the [people tools](people.md) to link identities to an existing person.
Connection email addresses are observations, not automatic cross-source merges.

The export contains comments **you wrote**, not a complete archive of comments
other people left on your posts. This importer does not read a Company Page's
inbox, poll LinkedIn, or use its live API. CSV behavior has been checked with
synthetic fixtures; no live LinkedIn API or personal export was used to verify
this integration.
