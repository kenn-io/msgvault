---
title: TikTok Business Messages
description: Capture signed TikTok business-message events, archive media promptly, and inspect or replay durable deliveries.
last_edited: 2026-11-02
---

Capture incoming TikTok business messages and outgoing echoes in searchable
conversations. msgvault preserves text, images, video, stickers, shared-post
links, and raw typed content. This integration is unreleased. Its HTTP and
archive paths use synthetic fixtures; an approved live app and live delivery
have not been verified.

**Only messages captured after connection can be complete.** The pull API
returns conversations from the last 90 days, at most 100 conversations, with
only the 20 newest messages in each. Continuous webhook delivery supplies
future history. Downtime, region restrictions, failed deliveries, and unavailable
media can still leave gaps. A successful sync is not proof of complete history.

## Check eligibility and app access

Bring your own approved **TikTok API for Business** company app. There is no
shared msgvault developer app or hosted relay. Start with the
[company registration and authorization steps](tiktok-comments.md#get-api-access).

Business Messaging adds these requirements:

- The account is a Business account with Advanced Access, or a Verified
  Business Account. General personal accounts are not eligible. The account
  owner must be at least 18 years old.
- Accounts registered in the EEA, Switzerland, or the UK cannot use this API.
  Messages from users in those regions arrive as `im_receive_msg_eu`; msgvault
  records an unsupported-delivery receipt without inventing message content.
- The developer app must pass TikTok's Data Security and Privacy Review.
  Managing US accounts also requires the US Data Security review and USDS
  Addendum.
- Request `message.list.read` and the account-profile permissions your app
  needs. TikTok documents `user.account.type` for checking Advanced Access.
  msgvault calls read endpoints and does not send messages. Authorization and
  webhook configuration must follow the approved app's granted permissions.

See TikTok's [Business Messaging access requirements](https://business-api.tiktok.com/portal/docs/access-to-business-messaging-api/v1.3),
[conversation list](https://business-api.tiktok.com/portal/docs/get-a-list-of-conversations/v1.3),
and [message list](https://business-api.tiktok.com/portal/docs/get-a-list-of-messages/v1.3).
TikTok enforces account eligibility; registration in msgvault verifies the
selected profile, not the app's full permission grant or review status.

## Connect an account

1. Authorize the account through your own app and put its access token in a
   named daemon environment variable. Put the app secret in a separate named
   variable. For automatic refresh, use the
   [token bundle with absolute expiries](tiktok-comments.md#keep-authorization-working).
2. Configure the account and receiver. This example disables comment polling
   for an app authorized only for messaging. Use both sources by leaving
   `comments_enabled = true`.

   ```toml
   [[tiktok]]
   identifier = "example-account"
   app_id = "100001"
   account_id = "example-open-id"
   token_env = "EXAMPLE_TIKTOK_TOKEN"
   app_secret_env = "EXAMPLE_TIKTOK_APP_SECRET"
   enabled = true
   comments_enabled = false
   messages_enabled = true
   schedule = "*/5 * * * *"
   max_media_mb = 50

   [tiktok_webhooks]
   listen_addr = "127.0.0.1:8788"

   [[tiktok_webhooks.apps]]
   app_id = "100001"
   app_secret_env = "EXAMPLE_TIKTOK_APP_SECRET"
   ```

3. Restart the daemon with those environment variables, then register and
   backfill the account:

   ```bash
   msgvault source tiktok-messages add example-account
   ```

   `add` performs a bounded pull backfill immediately. If backfill fails after
   registration, the source remains registered. Retry it with `sync`.
4. Expose the separate receiver through your own public HTTPS reverse proxy.
   Register `https://<your-public-domain>/webhooks/tiktok/100001` as the app's
   `DIRECT_MESSAGE` callback in TikTok's webhook configuration. Replace the
   final path component with your app ID. Do not register the daemon's ordinary
   archive API address as the callback.
5. Confirm the receiver and delivery counts:

   ```bash
   msgvault source tiktok-messages status example-account
   ```

Keep the daemon running as a persistent service for webhook capture. A daemon
started automatically for a CLI command can stop when idle, which also stops
the receiver. The HTTPS proxy does not start msgvault for incoming deliveries.

TikTok configures one callback per event category per app; that callback can
receive events for every account authorized to the app. msgvault routes on the
app and the event's `user_openid`. An account needs an enabled profile with
`messages_enabled = true` and a registered messaging source before normal
messages can be applied. Unrouted deliveries remain held for later replay.

Set the account's DMs to accept messages from everyone if appropriate. Message
requests that must first be accepted in TikTok may not produce normal delivery
until the account accepts them.

## Keep delivery working

The receiver verifies `Tiktok-Signature: t=...,s=...` using HMAC-SHA256 of
`<timestamp>.<raw body>` and the configured app secret. It rejects signatures
older than **five seconds**, future timestamps, and changed bytes. Keep the
daemon clock synchronized. The reverse proxy must preserve the raw request
body and signature header and forward requests promptly.

Verified raw bytes are flushed to private disk storage before HTTP 200 is
returned. A restart recovers pending deliveries. Duplicate raw deliveries are
deduplicated in the spool, and native conversation/message IDs deduplicate
archive writes. The double-encoded `content` JSON is parsed after acceptance.

`im_receive_msg` captures inbound messages. `im_send_msg` captures outgoing
messages, including native-app echoes. `im_receive_msg_eu` is counted as
unsupported. Unknown or invalid events remain held with their raw bytes.

The daemon retries archive and media failures with backoff. After 12 attempts,
a delivery stays failed until replay. Fix the missing credentials, routing,
registration, or media limit, restart the daemon if configuration changed, then
request replay:

```bash
msgvault source tiktok-messages replay example-account
msgvault source tiktok-messages status example-account --json
```

Status and replay operate **per app**, including other accounts authorized to
that app. Omit the identifier to include all apps. Status shows the listener's
last snapshot and counts of pending, held, failed, done, and unsupported EU
deliveries. It does not print tokens or message bodies. The running receiver
processes replay requests on its next drain; replay does not start a receiver.

The default body limit is 1 MiB and the default spool capacity is 128 MiB.
Successful raw deliveries are retained for 30 days. Pending, held, and failed
deliveries are not automatically pruned. Counts cover retained records. A full
spool returns HTTP 503, allowing the provider to retry within its own retry
window. Configure [receiver limits](../configuration.md#tiktok-webhooks) for
your expected traffic and monitor failures before that window expires.

## Understand media and history

Image and video messages carry media IDs. msgvault obtains a fresh download
URL and downloads the bytes immediately with the required `x-user` header.
TikTok's download URLs last 24 hours; sticker URLs last 30 days. Stickers and
emoji are also downloaded at ingestion. Shared posts remain links, which may
stop working when a post becomes private or disappears.

Media downloads use a 45-second request timeout and a per-file cap of 50 MiB
by default. Set `max_media_mb` to change the cap, up to 1,024 MiB. Redirects
and URLs outside trusted TikTok media hosts fail without forwarding credentials.
Messages and their native media references remain archived after a failed
download; the delivery stays retryable or failed rather than being marked done.
Raising a size cap takes effect after a daemon restart and replay.

Messages use source and message type `tiktok_messages`. Native payload types,
reply references, templates, reactions, and other returned fields remain in raw
JSON. Known archived parents receive reply links. Matching
`unique_identifier` identities in comments and DMs share the same participant
within the selected app/account. Display names and usernames never establish
identity; use the [people tools](people.md) for maintained person links.

Use periodic pull sync to reconcile the limited recent window:

```bash
msgvault source tiktok-messages sync example-account
```

It considers both accepted (`SINGLE`) conversations and requests (`STRANGER`),
archives at most the 100 most recently updated conversations returned across
them, and reads at most 20 messages from each. Repeated syncs update existing
rows. Failed runs retain accepted messages and can be repeated. This cannot
recover older gaps, and there is no full-history flag. msgvault does not send
messages, manage read receipts, or archive TikTok Shop messages through this API.
