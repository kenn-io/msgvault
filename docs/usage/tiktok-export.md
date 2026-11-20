---
title: TikTok Export
description: Import direct messages, your own comments, and account identity from a TikTok data export.
last_edited: "2026-11-20"
---

Import a TikTok **Download your data** ZIP or extracted folder into msgvault to
search your direct messages and the comments you wrote. This importer is
unreleased. Its synthetic fixtures use the fields in TikTok's
[published export documentation](https://developers.tiktok.com/docs/en/data-portability-data-types),
and the exact JSON and TXT layouts below have not been checked against a real
export. You don't need live API access, and none was tested for this command.

## Import an archive

1. In TikTok, request your data and choose **JSON**. Include direct messages,
   comments, and profile information.
2. Download the completed export.
3. Put the ZIP or extracted folder on the machine running the msgvault daemon.
4. Pick an identifier for this account and run the import:

   ```bash
   msgvault import-tiktok example-account ./tiktok-export.zip
   msgvault import-tiktok example-account ./tiktok-export --json
   ```

You don't need a TikTok app, token, or provider configuration. Run the command
on the daemon host. Local files can't be imported through a configured remote
daemon, so pass `--local` there. Pass `--json` to write the summary to stdout.
Warnings go to stderr, and `unknown_sections` counts ignored sections or TXT
files. The importer skips unknown sections, while malformed recognized records
fail the import with an error and are never reported as a complete import.

To add new messages later, re-import with the same identifier. Each DM partner
gets one conversation, and each message's key includes its chat, UTC timestamp,
sender, content, and separate link field. Reordering records or importing
equivalent JSON and TXT records doesn't create duplicates. Because exports have
no native message ID, identical records in one chat at the same timestamp
collapse into one message, and changing a message's content or link in a later
export creates a new record. If an import is interrupted, already-written
messages stay in your archive. Re-run the command to continue safely.

## What appears in the archive

- **Direct messages.** DMs keep their sender, date, text, and shared-post or
  media URLs. Links stay as text, and the importer never downloads their
  targets.
- **Your comments.** Comments are archived as messages authored by you. When a
  video URL is present, it appears in the body and in `post_url` metadata.
  Comments on the same URL share a public-comments conversation, while comments
  without URLs share an unlinked one.
- **Account identity.** Your profile `Username` identifies your messages as
  **me**. Once an identifier is bound to a profile, you can't use that same
  source identifier to import a different profile. Use a separate identifier
  for another account.

Imported records use `tiktok_export` as both their source type and message
type. Usernames are compared without a leading `@` and without case
differences, and participant identities stay scoped to the source identifier. A
username is not a permanent TikTok user ID, and renaming a partner can create
another conversation. Matching display names across accounts also do not mean
two participants are the same person.

## Accepted layouts and fallback

The importer reads named keys in any order and ignores case, spaces,
underscores, and punctuation when matching them. It recognizes these JSON
wrappers:

| Section | Accepted names and records |
|---|---|
| Profile | `Profile`, `Profile and Settings`, `Profile Information`, `ProfileMap`; fields `Username`/`userName`, optional `Nickname`/`nickName` |
| DMs | `Direct Message`, `Direct Messages`, `ChatHistory`/`Chat History`; maps keyed by `Chat History with <username>:` to arrays of `Date`, `From`, `Content` records |
| Own comments | `Comment`, `Comments`, `CommentsList`, `CommentList`; arrays of `Date`, `Comment content`/`Comment`/`Content`, optional `Original post landing page link`/`Video Link`/`Link`/`URL` |
| Activity containers | `Your Activity`, `Activity`, containing the named sections above |

For example, a combined JSON file may look like this:

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

When TXT files are present, the fallback parser reads `Direct Messages.txt`,
`Direct Message.txt`, `Chat History.txt`, `Comments.txt`, `Comment.txt`,
`Profile Information.txt`, and `Profile.txt`. Profile files use `Username: …`
and an optional `Nickname: …`. Messages use `Date: …`, `From: …`, and
`Content: …` under a `Chat History with <username>:` heading. Comments use
`Date: …`, `Comment: …`, and an optional `Link: …` or `URL: …`. Content can span
multiple lines until the next recognized field or chat header. If a content
line starts with one of those headers, TXT parsing is ambiguous. Choose JSON to
preserve it.

JSON takes precedence for each chat and for any nonempty comment array, while
TXT fills in chats or comments missing from JSON along with missing profile
fields. If a JSON record has no separate link, it can pick up a link from TXT
when the category, chat, timestamp, sender, and text match exactly, as long as
there is one matching JSON record and one distinct TXT link. Conflicting or
ambiguous links stay unresolved. Enriched links record `link_source: "txt"` in
message metadata, and JSON text is never replaced. Conflicting profile
usernames fail the import. Every import requires a profile username, including
comments-only exports. Export filenames and enclosing directories don't affect
message keys.

Dates accept `YYYY-MM-DD HH:MM:SS`, that format with a numeric timezone such as
`+0100`, or RFC 3339. Dates without timezone information are treated as UTC,
which is an assumption rather than a verified TikTok timezone guarantee.

## Limits

This importer archives exported DMs and your own comments. It can't retrieve
comments other people left on your videos, missing history, or content omitted
by TikTok. Unsupported sections are counted and skipped, and formats with
different chat container layouts require a parser update.

The importer reads ZIP members without extracting them. It rejects symbolic
links, unsafe or duplicate ZIP paths, archives with more than 10,000 entries,
files larger than 64 MiB, and parsed content exceeding 256 MiB per import. Keep
each export separate from unrelated JSON files, because any malformed JSON
causes an import error.
