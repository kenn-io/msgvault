---
last_edited: "2026-11-13"
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
data directory before it answers Meta. Deliveries are applied in the order
they arrived and deleted once applied.

New and edited comments, and the hidden state of a comment, are read from Meta
rather than taken from the delivery, so a late or repeated delivery cannot
bring back an older state. A removed comment is marked deleted together with
its archived replies. A removed post is marked deleted; its comments stay
archived.

### When a change fails

- A change that fails is retried with increasing waits, together with the
  rest of its delivery. Changes that already applied are applied again, which
  is harmless. Later deliveries wait behind it.
- Each delivery has one hour. After that, the daemon drops the delivery,
  including the changes after the failing one.
- A change msgvault cannot decode, and a change for a Page with no enabled
  profile, are skipped at once.
- Every other failure is retried. This includes a comment or post that is
  already gone on Meta and a token that may not read it.

Polling does not recover a skipped hide, unhide or removal. A later sync
archives a skipped new or edited comment when it reads that comment.

## Limits

- From the Page `feed` field, msgvault applies comment changes and post
  removals. A change for a field msgvault does not handle is logged and
  dropped. Other changes inside the `feed` field, such as reactions, are
  dropped without a log line.
- A hide or unhide for a comment that is not archived yet is ignored. Polling
  sync does not read hidden state, so that comment is not marked hidden.
- Deliveries larger than 8 MiB are refused.
- If a resumed comment sync fails on a comment or post that was deleted, for
  example one purged by `msgvault gc`, run the sync once with `--full`.
