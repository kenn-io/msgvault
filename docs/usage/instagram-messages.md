---
last_edited: 2026-11-03
title: Instagram Inbox
description: Archive recent messages and live events for a linked professional Instagram inbox.
---

Archive a linked professional Instagram account's DMs, story replies, and story
mentions as searchable Conversations. This integration is unreleased. Backfill
covers **at most the 20 most recent messages per thread**; new messages between
backfills require the Meta webhook receiver.

## Register the linked account

Use Facebook Login with a Page token from a person with the Page's `MESSAGING`
task. The selected Page must link to the configured professional Instagram
account. Consumer accounts and Instagram Login are unsupported here.

Access requires `instagram_basic`, `instagram_manage_messages`, and
`pages_manage_metadata`, an app owned by a verified business, and Advanced
Access for conversations between the business and other people. Grants on the
app alone do not prove that a particular token can read the inbox.

Add a [`[[instagram_messages]]` profile](../configuration.md#instagram-messages)
and restart the daemon with the named token environment variable:

```toml
[[instagram_messages]]
identifier = "example-instagram"
app_id = "9"
page_id = "100"
instagram_account_id = "1784"
token_env = "EXAMPLE_PAGE_TOKEN"
enabled = true
schedule = "17 */6 * * *"
actions = []
```

```bash
msgvault source instagram-messages add example-instagram
msgvault source instagram-messages sync example-instagram --max-requests 1000
```

Registration and each sync verify the Page and its linked
`instagram_business_account`. A missing or changed link fails explicitly.
Registration does not subscribe an app or change routing. `enabled = false`
leaves manual commands available and disables scheduled capture and webhook
routing for the profile.

The default budget is 1,000 reads, including both verification reads. Budget
pauses retain unfinished work for the next run. `--full` restarts with fresh
cursors without deleting history. Partial writes remain searchable. See the
[CLI reference](../cli-reference.md#source-instagram-messages-sync).

## Link people and retain events

The source type is `instagram_messages`; customer threads are `direct_chat`
Conversations keyed by Instagram-scoped ID (IGSID). Participant identities
include the app and professional account. A username is display evidence,
not an Instagram handle or proof that two participants are one person. Use the
[person linking tools](people.md) to link them explicitly. DMs are not
deduplicated against Download Your Information exports.

Configure the [receiver's](meta-webhooks.md) Instagram `messages` and `message_reactions` fields
in the App Dashboard. Echoes arrive under `messages`. The handler retains text,
attachment kinds, share counts, story reply references, story mentions, and
unsupported-message markers. Attachments are metadata only; ephemeral media
has no downloadable URL. Structured metadata and search text omit temporary
media URLs, although raw provider records may retain them. Backfill retains story
IDs; only webhooks distinguish a story reply from a mention.

An Instagram deletion marks a known message as deleted at the source and keeps
its archived body. A later backfill preserves that mark. A deletion of an
unknown message is ignored. Instagram message edits are unsupported and held
as `unhandled`. Reactions with missing targets retry within the receiver's
limits. After registering and enabling a profile, replay earlier unrouted
items with the receiver's `meta-webhooks replay --state held` command.

Threads in the Requests folder inactive for 30 days are absent from Meta's
conversation results. Old messages outside the 20-message cap remain a gap.
Webhook delivery cannot fill gaps from before capture started.

The [provider verification table](facebook-messages.md#provider-behavior-awaiting-live-verification)
records the remaining U1/U2/U6–U10 questions, including webhook/backfill ID
equality, matching caps across platforms, list ordering, and identity scope.
Live deduplication and event coverage remain unverified until those checks run.
