---
last_edited: "2026-11-13"
title: Meta Webhooks
description: Archive Facebook Page and Instagram comments, mentions and inbox messages as Meta reports them, between polling syncs.
---

Polling sync picks up new comments only when it runs. With webhooks, Meta
pushes each Page comment change to the msgvault daemon as it happens: msgvault
archives new and edited comments, marks hidden comments as hidden, and marks
removed comments and posts as deleted. It archives new Instagram comments and
live comments the same way. Keep
[Facebook Page comment sync](facebook-comments.md) and
[Instagram comment sync](instagram-comments.md) for backfill. Webhooks only
report changes after you subscribe.

## Set up the receiver

1. Expose a listener that Meta can reach over public HTTPS. msgvault listens on
   plain HTTP, so put it behind a TLS proxy or tunnel that forwards to
   `listen_addr`. Use a reverse proxy that also limits slow or abusive clients,
   because msgvault only caps how many delivery bodies it reads at once.
2. Store your Meta app secret and a verify token of your choice in two
   environment variables on the **daemon host**.
3. Add the receiver to your [configuration](../configuration.md#meta-webhooks):

   ```toml
   [meta_webhooks]
   listen_addr = "127.0.0.1:8787"

   [[meta_webhooks.apps]]
   app_id = "9"
   app_secret_env = "EXAMPLE_META_APP_SECRET"
   verify_token_env = "EXAMPLE_META_VERIFY_TOKEN"
   ```

4. Restart the daemon with `msgvault daemon restart`, or start it with
   `msgvault daemon start` when none is running. That daemon stays running
   until you stop it. If either environment variable is empty, the daemon logs
   a warning and does not start the receiver.

   A daemon started automatically by another command also stays up while the
   receiver is listening. Nothing starts it again after a reboot or
   `msgvault daemon stop`, and the receiver stays down until a daemon runs.
5. In your Meta app's **Webhooks** settings, subscribe the Page object to the
   `feed` field, and to `mention` to archive posts and comments by others that
   mention the Page. Set the callback URL to
   `https://<your-host>/meta/webhook/<app_id>` and use the same verify token.
6. Subscribe each Page to the app with its Page token, once per Page:

   ```bash
   curl -X POST -H "Authorization: Bearer $EXAMPLE_FACEBOOK_PAGE_TOKEN" \
     "https://graph.facebook.com/v26.0/<page_id>/subscribed_apps?subscribed_fields=feed,mention"
   ```

Each Page needs an enabled
[`[[facebook_comments]]` profile](../configuration.md#facebook-comments) for
the same app and Page, registered with `msgvault source facebook-comments add`.

For Instagram, subscribe the app's Instagram object to the `comments` and
`live_comments` fields with the same callback URL. To archive
[mentions of the account](instagram-comments.md#archive-media-that-tags-the-account),
also subscribe to `mentions` on a profile that sets `mentions = true`. Each
account needs an enabled
[`[[instagram_comments]]` profile](../configuration.md#instagram-comments) for
the same app and account, registered with
`msgvault source instagram-comments add`. Mentions also need
`msgvault source instagram-mentions add` to register the separate mentions
source.

## Inbox messages

To archive new Messenger and Instagram direct messages as they arrive,
subscribe the Page object to the `messages` and `message_echoes` fields, and
subscribe the Instagram object to `messages`. Echoes are the messages sent by
the Page or account. Each inbox needs an enabled
[`[[facebook_messages]]` or `[[instagram_messages]]` profile](meta-messages.md),
registered with `source facebook-messages add` or
`source instagram-messages add`. Include the two Page fields in your Page
subscription as well, for example
`subscribed_fields=feed,mention,messages,message_echoes` in the `curl` call
above.

Each message lands in the same chat that inbox sync uses. If a chat first
appears through a webhook, it gets the customer's name on the next sync. A
webhook only adds messages that are not archived yet, so a late event never
replaces text that a sync updated. Messages without text are skipped. A message
is missed if the daemon skips its change after an hour of failures, or if the
receiver stays unreachable for longer than Meta keeps retrying. When that
happens, the next sync archives the missed message only if it is still among
the 20 most recent in its chat.

## How deliveries are applied

The daemon verifies each delivery's `X-Hub-Signature-256` signature with your
app secret and refuses deliveries that fail. If a request lacks a well-formed
signature header, the daemon refuses it before reading the body. It reads at
most 8 delivery bodies at once; above that limit, it answers 503 and Meta
retries.

Before answering Meta, the daemon saves each accepted delivery under
`meta-webhooks/pending` in your data directory. It applies deliveries in the
order they arrived and deletes each one once applied.

For new and edited comments, and for a comment's hidden state, msgvault reads
the current values from Meta rather than taking them from the delivery, so a
late or repeated delivery cannot bring back an older state. A removed comment
is marked deleted together with its archived replies. A removed post is marked
deleted while its comments stay archived. msgvault reads Instagram comments
from Meta too, and media that is not archived yet must be owned by the
configured account. A post or comment that mentions the Page is also read from
Meta and archived in the Page's comment source as its own conversation, with
the post it belongs to.

### When a change fails

- If a change fails, the daemon retries it with increasing waits alongside the
  rest of its delivery. Changes that already applied are applied again, which
  is harmless, and later deliveries wait behind it.
- Each delivery has one hour. After that, the daemon drops the delivery,
  including any changes after the failing one.
- The daemon immediately skips any change it cannot decode, as well as any
  change for a Page or Instagram account with no enabled profile.
- Every other failure is retried, including a comment or post that is already
  gone on Meta and a token that may not read it.

Polling does not recover a skipped hide, unhide, or removal. A later sync
archives a skipped new or edited comment when it reads that comment. Because no
sync can find Instagram caption or comment mentions or posts by others that
mention a Page, a skipped mention is lost.

## Limits

- msgvault handles comment changes and post removals from the Page `feed`
  field, new mentions from the Page `mention` field, new comments from the
  Instagram `comments` and `live_comments` fields, caption and comment mentions
  from the Instagram `mentions` field, and new inbox messages and echoes. A
  change for a field msgvault does not handle is logged and dropped. Other
  changes inside a handled field, such as reactions, message edits and
  deletions, and read receipts, are dropped without a log line.
- Instagram does not send comment edits, hides, or removals.
- If a hide or unhide arrives for a comment that is not archived yet, msgvault
  ignores it. Polling sync does not read hidden state, so that comment is not
  marked hidden.
- The receiver refuses deliveries larger than 8 MiB.
- If a resumed comment sync fails on a comment or post that was deleted, for
  example one purged by `msgvault gc`, run the sync once with `--full`.
