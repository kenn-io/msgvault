---
last_edited: 2026-11-03
title: Instagram Comments
description: Archive a linked professional Instagram account's media comments and replies.
---

Search returned Instagram media captions, comments, and replies beside your
other communications, and link commenters with existing person tools. This
unreleased integration archives comments and supports opt-in replies and
moderation of archived comments.

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
Ad-only discovery and media downloads remain separate work.


## Reply and moderate

Use the owner CLI, HTTP API, or MCP tools with daemon API schema 3.39.0 or newer.
Delegated grants and remote-client keys cannot act, read the queue, or read
receipts. Add only the intended writes to the selected profile and restart:

```toml
enabled = true
actions = ["public_reply", "private_reply", "hide", "delete"]
```

An omitted `actions` list permits no provider writes. `hide` also permits
unhide. Actions apply only to the verified account's media discovered through
its media edge; ad-derived or other provenance is refused.

Before a new provider action, the daemon verifies the configured credential
against the selected Page and its linked Instagram account. Previews and receipt
resolution stay local.

Preview using the archived message ID, then copy its confirmation token:

```bash
msgvault comment reply 42 --visibility public --body "Example reply" --dry-run --json
msgvault comment reply 42 --visibility public --body "Example reply" --confirm CONFIRM_TOKEN --json
msgvault comment moderate 43 --hide --json
```

Reply targets must be top-level comments. A nested target returns
`ig_target_not_top_level` with the top-level message ID instead of silently
attaching to another comment. A comment recorded as hidden cannot receive a
new reply until it is unhidden. Repeating the same public reply still returns
its existing receipt after a later hide, without sending it again. Hiding the
account's own comment returns
`not_hideable`. Delete keeps archived history and requires confirmation.

Private replies use `--visibility private` and send as the selected Instagram
account. The one-message and seven-day limits, prior-author warnings,
confirmation, and [uncertain-outcome recovery](facebook-comments.md#receipts-and-recovery)
follow the same comment-action rules. Follow-up DMs use the messaging workflow.
Visibility never changes as a fallback after failure.

Permission failures list `instagram_basic`, `instagram_manage_comments`,
`pages_show_list`, and `pages_read_engagement`; private replies also list
`instagram_manage_messages`. Business Manager roles may additionally require
ad permissions. Live permission and write-token checks remain pending; the
client uses only this profile's named credential and never substitutes another.

Instagram sends return an ID without an extra read. A later sync must observe
the public reply before its receipt reports `archive_complete` and
`reply_message_id`. Source merges preserve these archive links and keep sent
replies out of incoming work. `comment queue` includes both Facebook and Instagram
comments; filter the exact source identifier to select this account. See the
[CLI reference](../cli-reference.md#comment-reply) for flags and receipt reads.
