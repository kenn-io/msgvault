---
last_edited: "2026-11-10"
title: Meta Webhooks
description: Archive Facebook Page comment changes as Meta reports them, between polling syncs.
---

Polling sync finds new comments only when it runs. With webhooks, Meta sends
each Page comment change to the msgvault daemon as it happens: new and edited
comments are archived, hidden comments are marked hidden, and removed comments
and posts are marked deleted. Keep [Facebook Page comment sync](facebook-comments.md)
for backfill; webhooks only report changes after you subscribe.

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

## How deliveries are applied

The daemon checks each delivery's `X-Hub-Signature-256` signature with the app
secret and refuses deliveries that fail. It saves each accepted delivery under
`meta-webhooks/pending` in the data directory before it answers Meta, so a
delivery survives a daemon restart. Deliveries are applied in the order they
arrived and deleted once applied.

A comment whose post or parent comment is not archived yet causes the daemon
to read them from Meta with the Page token first. When a delivery cannot be
applied, for example while a polling sync of the same Page is running, the
daemon retries it with increasing waits and holds later deliveries until it
succeeds. After an hour of failures it logs the delivery and drops it; the
next polling sync still archives the comment text.

## Limits

- Only comment changes from the Page `feed` field are applied. Reactions,
  other feed changes and other fields are logged and ignored.
- A hide or unhide for a comment that is not archived yet is ignored.
- Deliveries larger than 8 MiB are refused.
