---
last_edited: "2026-11-13"
title: Meta Webhooks
description: Archive Facebook Page and Instagram comment changes as Meta reports them, between polling syncs.
---

Polling sync finds new comments only when it runs. With webhooks, Meta sends
each Page comment change to the msgvault daemon as it happens: new and edited
comments are archived, hidden comments are marked hidden, and removed comments
and posts are marked deleted. New Instagram comments and live comments are
archived the same way. Keep [Facebook Page comment sync](facebook-comments.md)
and [Instagram comment sync](instagram-comments.md) for backfill; webhooks only
report changes after you subscribe.

## Set up the receiver

1. Give the daemon a listener that Meta can reach over public HTTPS. msgvault
   listens on plain HTTP, so put it behind a TLS proxy or tunnel that forwards
   to `listen_addr`. Use a reverse proxy that also limits slow or abusive
   clients: msgvault only caps how many delivery bodies it reads at once.
2. Store your Meta app secret and a verify token of your choice in two
   environment variables on the **daemon host**.
3. Add the receiver to the [configuration](../configuration.md#meta-webhooks):

   ```toml
   [meta_webhooks]
   listen_addr = "127.0.0.1:8787"

   [[meta_webhooks.apps]]
   app_id = "9"
   app_secret_env = "EXAMPLE_META_APP_SECRET"
   verify_token_env = "EXAMPLE_META_VERIFY_TOKEN"
   ```

4. Restart the daemon with `msgvault daemon restart`, or `msgvault daemon start`
   when none is running. That daemon keeps running until you stop it. If
   either variable is empty, the daemon logs a warning and does not start the
   receiver.

   A daemon that another command started automatically also stays up while
   the receiver is listening. Nothing starts it again after a reboot or
   `msgvault daemon stop`, and the receiver is down until a daemon runs.
5. In your Meta app's Webhooks settings, subscribe the Page object to the
   `feed` field, and to `mention` to archive posts and comments by others
   that mention the Page. Use `https://<your-host>/meta/webhook/<app_id>` as the callback
   URL and the same verify token.
6. Subscribe the Page to the app with its Page token, once per Page:

   ```bash
   curl -X POST -H "Authorization: Bearer $EXAMPLE_FACEBOOK_PAGE_TOKEN" \
     "https://graph.facebook.com/v26.0/<page_id>/subscribed_apps?subscribed_fields=feed,mention"
   ```

Each Page must have an enabled [`[[facebook_comments]]` profile](../configuration.md#facebook-comments)
for the same app and Page, registered with `msgvault source facebook-comments add`.

For Instagram, subscribe the app's Instagram object to the `comments` and
`live_comments` fields with the same callback URL, and to `mentions` for
[mentions of the account](instagram-comments.md#archive-media-that-tags-the-account)
on a profile that sets `mentions = true`. Each account must have an
enabled [`[[instagram_comments]]` profile](../configuration.md#instagram-comments)
for the same app and account, registered with
`msgvault source instagram-comments add`. Mentions also need
`msgvault source instagram-mentions add`, which registers the separate
mentions source.

## How deliveries are applied

The daemon checks each delivery's `X-Hub-Signature-256` signature with the app
secret and refuses deliveries that fail. It refuses a request without a
well-formed signature header before reading its body, and it reads at most 8
delivery bodies at once; above that it answers 503 and Meta retries.

The daemon saves each accepted delivery under `meta-webhooks/pending` in the
data directory before it answers Meta. Deliveries are applied in the order
they arrived and deleted once applied.

New and edited comments, and the hidden state of a comment, are read from Meta
rather than taken from the delivery, so a late or repeated delivery cannot
bring back an older state. A removed comment is marked deleted together with
its archived replies. A removed post is marked deleted; its comments stay
archived. An Instagram comment is read from Meta too, and media that is not
archived yet must be owned by the configured account. A post or comment that
mentions the Page is read from Meta and archived in the Page's comment source
as its own conversation, with the post it belongs to.

### When a change fails

- A change that fails is retried with increasing waits. Later changes and
  deliveries wait behind it.
- Each delivery has one hour. After that, the daemon skips the failing change
  and tries each remaining change of the delivery once.
- A change that can never be applied, such as one for a comment or post that
  is gone, that the token may not read, or for a Page with no enabled profile,
  is skipped at once.
- An expired token, throttling and Meta error codes msgvault does not know are
  retried.

Polling does not recover a skipped hide, unhide or removal. A later sync
archives a skipped new or edited comment when it reads that comment. No sync
can find Instagram caption or comment mentions or posts by others that mention
a Page, so a skipped mention is lost.

## Limits

- msgvault applies comment changes and post removals from the Page `feed`
  field, new mentions from the Page `mention` field, new comments from the
  Instagram `comments` and `live_comments` fields, and caption and comment
  mentions from the Instagram `mentions` field. A change for a field
  msgvault does not handle is logged and dropped. Other changes inside a
  handled field, such as reactions, are dropped without a log line.
- Instagram does not send edits, hides or removals of comments.
- A hide or unhide for a comment that is not archived yet is ignored. Polling
  sync does not read hidden state, so that comment is not marked hidden.
- Deliveries larger than 8 MiB are refused.
- If a resumed comment sync fails on a comment that was deleted and then
  purged by `msgvault gc`, run the sync once with `--full`.
