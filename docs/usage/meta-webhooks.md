---
last_edited: "2026-11-10"
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
   to `listen_addr`.
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

4. Restart the daemon. If either variable is empty, the daemon logs a warning
   and does not start the receiver.
5. In your Meta app's Webhooks settings, subscribe the Page object to the
   `feed` field. Use `https://<your-host>/meta/webhook/<app_id>` as the callback
   URL and the same verify token.
6. Subscribe the Page to the app with its Page token, once per Page:

   ```bash
   curl -X POST -H "Authorization: Bearer $EXAMPLE_FACEBOOK_PAGE_TOKEN" \
     "https://graph.facebook.com/v26.0/<page_id>/subscribed_apps?subscribed_fields=feed"
   ```

Each Page must have an enabled [`[[facebook_comments]]` profile](../configuration.md#facebook-comments)
for the same app and Page, registered with `msgvault source facebook-comments add`.

For Instagram, subscribe the app's Instagram object to the `comments` and
`live_comments` fields with the same callback URL. Each account must have an
enabled [`[[instagram_comments]]` profile](../configuration.md#instagram-comments)
for the same app and account, registered with
`msgvault source instagram-comments add`.

## How deliveries are applied

The daemon checks each delivery's `X-Hub-Signature-256` signature with the app
secret and refuses deliveries that fail. It saves each accepted delivery under
`meta-webhooks/pending` in the data directory before it answers Meta, so a
delivery survives a daemon restart. Deliveries are applied in the order they
arrived and deleted once applied.

A new or edited comment is read from Meta with the Page token rather than
taken from the delivery, so a late or repeated delivery cannot bring back older
text. A post or parent comment that is not archived yet is read too. An
Instagram comment is also read from Meta, and media that is not archived yet
must be owned by the configured account; otherwise the change is logged and
skipped. When a delivery cannot be
applied, for example while a polling sync of the same Page is running, the
daemon retries it with increasing waits and holds later deliveries until it
succeeds. After an hour of failures it logs the delivery and drops it; the
next polling sync still archives the comment text. A change that can never be
applied, such as a comment deleted before it was read, is logged and skipped
without holding back the other changes in its delivery.

## Limits

- Only comment changes from the Page `feed` field and the Instagram
  `comments` and `live_comments` fields are applied. Reactions, other feed
  changes and other fields are logged and ignored.
- Instagram does not send edits, hides or removals of comments.
- A hide or unhide for a comment that is not archived yet is ignored.
- Deliveries larger than 8 MiB are refused.
