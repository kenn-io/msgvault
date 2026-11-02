---
title: TikTok Export
description: Import direct messages, your own comments, and account identity from a TikTok data export.
last_edited: 2026-11-02
---

Import a TikTok **Download your data** ZIP or extracted folder to search your
DMs and the comments you wrote. This importer is unreleased. Its synthetic
fixtures use the fields in TikTok's [published export documentation](https://developers.tiktok.com/docs/en/data-portability-data-types).
The precise JSON and TXT layouts below have not been checked against a real
export. No live API access was tested or is needed for this command.

## Import an archive

1. Request your data in TikTok and choose **JSON**. Include direct messages,
   comments, and profile information. Download the completed export.
2. Put the ZIP or extracted folder on the machine running the msgvault daemon.
3. Choose an identifier for this account and import it:

   ```bash
   msgvault import tiktok example-account ./tiktok-export.zip
   msgvault import tiktok example-account ./tiktok-export --json
   ```

No TikTok app, token, or provider configuration is needed. The archive path is
resolved on the daemon host; a configured remote daemon reads its own files.
`--json` writes the summary to stdout and command errors as JSON to stderr.
The daemon refreshes search views according to your analytics settings.
`--build-cache` requests a refresh; `--no-build-cache` defers it.
Warnings go to stderr and `unknown_sections` counts ignored sections or TXT
files. Unknown sections are skipped. Malformed recognized records fail the
import with an error; they are never reported as a complete import.

Re-import with the same identifier to add messages. Each DM partner has one
conversation. A message's key includes its chat, UTC timestamp, sender,
content, and separate link field. Reordering records or importing equivalent
JSON and TXT records does not duplicate them. Identical records in one chat
at the same timestamp collapse because exports provide no native message ID.
If a later export changes a message's content or link, it becomes a new record.
Interrupted writes remain archived; re-run the command to continue safely.

## What appears in the archive

- DMs retain their sender, date, text, and shared-post or media URLs. Links
  remain text; the importer never downloads their targets.
- Comments are messages authored by you. A video URL, when present, appears
  in the body and `post_url` metadata. Comments on the same URL share a
  public-comments conversation; comments without URLs share an unlinked one.
- Profile `Username` identifies your messages as **me**. The same source
  identifier cannot later import another profile. Use a different identifier
  for another account.

The source and message type are `tiktok_export`. Usernames are compared without
an initial `@` and without case differences. Participant identities are scoped
to the source identifier. A username is not a permanent TikTok user ID;
renaming a partner can create another conversation. Matching display names
across accounts do not establish that two participants are the same person.

## Accepted layouts and fallback

The importer reads named keys, independent of their order. Key matching ignores
case, spaces, underscores, and punctuation. Recognized JSON wrappers are:

| Section | Accepted names and records |
|---|---|
| Profile | `Profile`, `Profile and Settings`, `Profile Information`, `ProfileMap`; fields `Username`/`userName`, optional `Nickname`/`nickName` |
| DMs | `Direct Message`, `Direct Messages`, `ChatHistory`/`Chat History`; maps keyed by `Chat History with <username>:` to arrays of `Date`, `From`, `Content` records |
| Own comments | `Comment`, `Comments`, `CommentsList`, `CommentList`; arrays of `Date`, `Comment content`/`Comment`/`Content`, optional `Original post landing page link`/`Video Link`/`Link`/`URL` |
| Activity containers | `Your Activity`, `Activity`, containing the named sections above |

For example, a combined JSON file may contain:

```json
{
  "Profile": {"Profile Information": {"ProfileMap": {"userName": "example_owner"}}},
  "Direct Message": {"Direct Messages": {"ChatHistory": {
    "Chat History with example_friend:": [
      {"Date": "2026-01-02 03:04:05", "From": "example_owner", "Content": "Hello"}
    ]
  }}}
}
```

TXT fallback accepts files named `Direct Messages.txt`, `Direct Message.txt`,
`Chat History.txt`, `Comments.txt`, `Comment.txt`, `Profile Information.txt`, or
`Profile.txt`. Profile files use `Username: …` and optional `Nickname: …`.
Messages use `Date: …`, `From: …`, `Content: …` under a
`Chat History with <username>:` heading. Comments use `Date: …`, `Comment: …`
and optional `Link: …`. Content can continue across lines until the next
recognized field or chat header. A content line that starts with such a header
is ambiguous in TXT; choose JSON to preserve it.

JSON wins for each chat and for a nonempty comment array. TXT supplies chats
or comments absent from JSON, plus missing profile fields. It does not enrich
individual JSON messages with extra TXT fields. Conflicting profile usernames
fail the import. Every import requires a profile username, including comments-only
exports. Export filenames and enclosing directories do not affect message keys.

Dates accept `YYYY-MM-DD HH:MM:SS`, that format followed by a numeric timezone
such as `+0100`, or RFC 3339. Dates without timezone information are treated
as UTC; this is an assumption rather than a verified TikTok timezone guarantee.

## Limits

This path archives exported DMs and your own comments. It cannot retrieve
comments other people left on your videos, missing history, or content omitted
by TikTok. Unsupported sections are counted and skipped. Formats with different
chat container layouts require a parser update.

ZIP members are read without extraction. Symbolic links, unsafe or duplicate
ZIP paths, archives with more than 10,000 entries, files larger than 64 MiB,
and parsed content exceeding 256 MiB per import are rejected. Keep each export
separate from unrelated JSON files, because malformed JSON is an import error.
