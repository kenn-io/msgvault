---
last_edited: "2026-11-12"
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
secret and refuses deliveries that fail. It refuses a request without a
well-formed signature header before reading its body, and it reads at most 8
delivery bodies at once; above that it answers 503 and Meta retries.

The daemon saves each accepted delivery under `meta-webhooks/pending` in the
data directory before it answers Meta, so a delivery survives a daemon
restart. Deliveries are applied in the order they arrived and deleted once
applied. At most 10,000 deliveries or 1 GiB wait in that directory. Past
either limit the daemon answers 503 until it catches up, and Meta retries.
A delivery file the daemon cannot read or delete is renamed to
`.<name>.set-aside` in the same directory and left for you to inspect.

A new or edited comment is read from Meta with the Page token rather than
taken from the delivery, so a late or repeated delivery cannot bring back older
text. A post or parent comment that is not archived yet is read too. For a hide
or unhide, the daemon reads whether Meta hides the comment now and records
that, so a repeated hide cannot hide a visible comment. A removed comment is
marked deleted together with its archived replies. A removed post is marked
deleted; its comments stay archived.

### When a change fails

The daemon applies a delivery one change at a time. When a change fails, later
changes and deliveries, for every app and Page, wait until it succeeds or is
skipped. Changes that already succeeded are not applied again, unless the
daemon restarts during the delivery. It then applies the whole delivery again,
which is safe to repeat.

What the daemon does depends on why the change failed:

| Cause | What the daemon does |
|---|---|
| A polling sync of the same Page is running | Waits for the sync, checking again at least once a minute. This wait has no limit and does not count against the hour below. |
| Meta is unavailable or throttling, the Page token is expired or invalid, Meta returns an error code msgvault does not know, or the archive cannot be written | Retries with waits that grow to 5 minutes. Each delivery has one hour of these waits in total. |
| The change can never be applied: the comment or post is gone or cannot be read with this token, Meta rejects the request, the Page has no enabled profile or registered source, the profile's token variable is not set, or the comment is already archived under another post | Logs the change and skips it at once. |

When a delivery has used its hour, the daemon logs the failing change and
skips it, then tries each remaining change of the delivery once.

A skipped change is not always recovered. The next polling sync archives a new
or edited comment that was skipped. It does not recover a skipped hide, unhide
or removal, because polling does not read those.

## Limits

- Only comment changes from the Page `feed` field are applied. A change for a
  field msgvault does not handle is logged and dropped. Other changes inside
  the `feed` field, such as reactions, are dropped without a log line.
- A hide or unhide for a comment that is not archived yet is ignored. Polling
  sync does not read hidden state, so that comment is not marked hidden.
- Deliveries larger than 8 MiB are refused.
