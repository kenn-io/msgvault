---
last_edited: "2026-11-10"
title: Instagram Comments
description: Archive a linked professional Instagram account's media comments and replies.
---

Search returned Instagram media captions, comments, and replies beside your
other communications, and link commenters with existing person tools. This
unreleased integration reads comments; it does not publish replies or send
messages.

## Connect a professional account

Use Facebook Login and a Page token for an explicitly selected Page linked to
the professional Business or Creator Instagram account. Consumer accounts and
Instagram Login are unsupported by this integration. Keep the token in a named
environment variable on the **daemon host**; msgvault does not acquire tokens
or expand their permissions.

Add a [`[[instagram_comments]]` profile](../configuration.md#instagram-comments),
restart the daemon with that environment, then register and sync:

```toml
[[instagram_comments]]
identifier = "example-account"
app_id = "9"
page_id = "100"
instagram_account_id = "200"
token_env = "EXAMPLE_INSTAGRAM_PAGE_TOKEN"
enabled = true
schedule = "*/30 * * * *"
```

```bash
msgvault source instagram-comments add example-account
msgvault source instagram-comments sync example-account --max-requests 1000
```

Registration and each sync verify both the selected Page and its linked
`instagram_business_account` against the configured account ID. A changed or
missing link fails explicitly. App grants alone do not guarantee access to
all media or comments.

## Resume and search

The default budget is 1,000 API reads per account per run. The two verification
reads count toward it, so collection needs at least three reads. Media,
comments, and replies each paginate through bounded checkpoints. Partial
writes remain searchable; unfinished work resumes next time. New complete
cycles revisit old media to find late comments, without a creation-date cutoff.
Use `--full` to restart selected progress with fresh cursors under the same
budget, preserving and updating existing rows. Provider failures never reset
progress automatically or delete archived history.

Each media item forms one Conversation, titled from its caption when present.
The source and message type are `instagram_comments`. Identities include the
app, Page, linked account, and native identifier family. Equal usernames or
numeric IDs across families do not merge people. Use the existing
[participant linking tools](people.md) to link an observed commenter explicitly.
Missing authors stay unknown; later omissions preserve stronger stored evidence.
Comment self attribution is unproven when Meta returns only a non-comparable
commenter identity; equal digits do not establish account ownership.

Only media returned by the verified account's media edge is discovered here.
Missing media owners remain unknown; explicit foreign owners are refused.
Ad-only discovery, media downloads, and public replies are separate work.
To archive new comments as they arrive, set up [Meta webhooks](meta-webhooks.md).

## Archive media that tags the account

Media in which other accounts tag the linked account is not on its media
edge. To archive it, set `mentions = true` on the profile, restart the daemon,
then register and sync the separate mentions source:

```bash
msgvault source instagram-mentions add example-account
msgvault source instagram-mentions sync example-account
```

The source and message type are `instagram_mentions`, so this media stays apart
from the account's own comments. Each tagged media item forms one
Conversation. The author's `@username` is the subject, so you can search for
it; Meta returns no author ID for media the account does not own, so the
author is not linked to a person. Media the account owns is skipped here.

Each request reads up to 100 tagged media items, and `--max-requests` limits a
run. An unfinished run resumes from its saved position. A finished run starts
again from the newest tagged media on the next sync, and `--full` does the same
for an unfinished one. The profile's `schedule` also schedules this source.
