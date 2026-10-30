---
last_edited: "2026-10-30"
title: LINE Official Account
description: Archive incoming LINE Official Account chats through signed Messaging API webhooks.
---

# Archive LINE Official Account chats

Connect a LINE Official Account to archive incoming direct chats, group chats,
and existing multi-person rooms. Text becomes searchable before profile or
media downloads finish. Quotes link to messages in the same archived chat;
membership events update participants.

!!! note "Available on main"
    This integration is available in builds from `main`, newer than the latest
    release documented in the [changelog](../changelog.md).

The archive starts when the webhook goes live. LINE has no message-history API,
so msgvault cannot backfill earlier chats. Staff replies from Official Account
Manager or the Official Account app, auto-replies, greetings, broadcasts, and
messages sent through other tools are invisible to this integration. msgvault
currently sends no LINE messages.

## Connect your account

1. Enable **Messaging API** for your account in LINE Official Account Manager.
   Then open the resulting channel in LINE Developers Console and find its
   channel ID and channel secret. Follow LINE's
   [account and channel setup](https://developers.line.biz/en/docs/messaging-api/getting-started/).
2. Put the secret in the named environment variable on the daemon host, for
   example `MSGVAULT_LINE_SECRET_EXAMPLE`. For a service or container, configure
   its environment. Exporting a variable in another terminal does not change
   an already running service; restart it when its environment changes.
   Never put the secret in a command argument or `config.toml`.
3. Register the channel:

    ```bash
    msgvault add-line --channel-id 2000000001 --secret-env MSGVAULT_LINE_SECRET_EXAMPLE
    ```

    The command checks the credentials and bot identity, registers the archive
    source, and saves configuration. This first invocation leaves LINE's remote
    webhook settings alone.
4. Restart the daemon or service with the secret variable present. This loads
   the saved channel and mounts its receiver. The saved file alone does not
   prove that the running daemon can receive events.
5. Make the [public webhook route](#expose-only-the-webhook-route) reachable,
   then run the command again with your public HTTPS base URL:

    ```bash
    msgvault add-line --channel-id 2000000001 --secret-env MSGVAULT_LINE_SECRET_EXAMPLE \
      --webhook-base-url https://archive.example.com
    ```

    This invocation can set and test the webhook after the active receiver
    matches the registered channel. Without `--webhook-base-url`, use the
    printed route and Console instructions to configure it yourself.
6. In the channel's Messaging API settings, enable **Use webhook** and enable
   **Webhook redelivery**. Use **Verify**, then send a test message and check
   your archive. LINE's [redelivery guide](https://developers.line.biz/en/docs/messaging-api/receiving-messages/#enable-webhook-redelivery)
   explains that delivery remains best-effort.

The saved block is:

```toml
[[line]]
channel_id = "2000000001"
secret_env = "MSGVAULT_LINE_SECRET_EXAMPLE"
enabled = true
```

Use another block for each channel. See [LINE configuration](../configuration.md#line)
for validation and [command options](../cli-reference.md#add-line) for registration.
Each channel has one webhook URL. Replacing an existing bot's URL moves delivery
to msgvault; forwarding events to multiple receivers is outside this integration.

For groups and legacy multi-person rooms, enable **Allow bot to join group
chats** before inviting the account. LINE disables it by default and permits
only one Official Account in a group or room. See LINE's
[group chat setup](https://developers.line.biz/en/docs/messaging-api/group-chats/).

## Expose only the webhook route

The operator provides public HTTPS with a certificate from a public certificate
authority and TLS 1.2 or later. See LINE's
[TLS requirements](https://developers.line.biz/en/docs/messaging-api/ssl-tls-spec-of-the-webhook-source/).
msgvault does not provision tunnels, ports, DNS, or certificates.

For a reverse proxy on the same host, choose a stable private backend port.
The daemon's default `api_port = 0` selects an available port at startup;
this example explicitly uses 8080:

```toml
[server]
bind_addr = "127.0.0.1"
api_port = 8080
```

A Caddy site can forward only this channel's POST route:

```caddyfile
archive.example.com {
    @lineWebhook {
        method POST
        path /webhooks/line/2000000001
    }
    handle @lineWebhook {
        reverse_proxy 127.0.0.1:8080
    }
    handle {
        respond 404
    }
}
```

Replace the example hostname and channel ID. Add explicit channel paths for
additional accounts. Keep the original request body and `x-line-signature`
header intact; rewriting the JSON breaks signature verification. See
[Caddy request matching](https://caddyserver.com/docs/caddyfile/matchers) and
[reverse proxy behavior](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy).

Keep the Web UI and private API behind their existing access controls. This
proxy does not forward them. See [remote deployment](../guides/remote-deployment.md)
for daemon hosting and [the webhook API contract](../api-server.md#line-webhook)
for authentication and response codes. LINE does not publish webhook source IP
ranges; the [signature authenticates delivery](https://developers.line.biz/en/docs/messaging-api/verify-webhook-signature/).

## What gets archived

| Incoming content | Archive behavior |
|---|---|
| Text and locations | Searchable text with native metadata |
| Images, video, audio, and files | LINE-provided bytes streamed into the archive, up to a fixed 250 MiB per attachment |
| External-provider media | URLs retained as metadata; msgvault never fetches those URLs |
| Stickers | Native sticker metadata and available text or keywords; no scraped sticker art |
| Quotes | Linked when the quoted message is archived in the same chat; missing targets can resolve later |
| Text edits in groups | Latest text replaces the archived text and is marked edited; earlier edit versions are not kept |
| Unsends | Previously archived body and media retained and marked deleted from the source; see [retention](#privacy-and-retention) |

LINE user IDs belong to a provider's identity space. msgvault qualifies them by
channel so unrelated accounts cannot accidentally share participants. Display
names never establish identity. Group and room messages without a sender ID
use an anonymous participant scoped to that channel and chat. A direct-chat
event without a user ID stays as failed evidence rather than creating a shared
anonymous direct chat.

Template and Flex messages sent on a user's behalf by a LINE Front-end Framework
(LIFF) app do not produce webhooks and cannot be archived. See
[LINE's LIFF webhook limits](https://developers.line.biz/en/docs/messaging-api/receiving-messages/#webhook-when-sending-a-message-using-liff-sendmessages).

## Check delivery

The Sources page shows whether this daemon mounted the channel's receiver,
when its last event arrived, and how many events are pending or failed. An
empty LINE webhook verification request does not count as a received event.
The invalid-signature count resets when the daemon restarts. Rejected
signatures produce a warning at most once per minute per channel.

**Process inbox** drains due events. It cannot fetch earlier LINE history.
If the receiver is inactive, check the channel configuration and secret
environment variable, then restart the daemon.

## Delivery and retries

LINE may deliver duplicates or events out of order, and redelivery cannot
guarantee completeness. msgvault acknowledges a batch only after committing
its sanitized events. Duplicate event IDs are ignored within each source;
processing resumes after daemon restarts.

Media can expire at LINE before it is downloaded. A 202 content response retries
while LINE prepares the file; 404 or 410 marks it unavailable. Profile and
recoverable media failures retry after 30 seconds, doubling to at most one hour
between attempts. LINE rate limits can postpone an attempt further. Automatic
retries stop 24 hours after first receipt. Text already archived stays readable.
Edits and unsends whose original message has not arrived retain their evidence
and reconcile if that original arrives later, even after automatic retries end.

The daemon processes incoming work automatically. To process due inbox work
again, use **Sync now** or:

```bash
msgvault sync-line
msgvault sync-line --channel-id 2000000001
```

These commands drain due work; they never fetch history or make a delayed retry
due immediately. Review pending work separately from failed work when checking
source health.

## Privacy and retention

LINE [recommends removing unsent content](https://developers.line.biz/en/docs/messaging-api/receiving-messages/#webhook-unsend-message).
msgvault retains archived body and media with a source-deleted flag, as it does
for other sources. Choose this archive behavior deliberately and arrange user
consent and a reachable privacy policy covering retention under
[LINE's terms](https://developers.line.biz/en/terms-and-policies/).
This integration has no option to purge content automatically on unsend.

Reply and mark-as-read capability tokens are removed recursively before events
are persisted. Native metadata, including `quoteToken`, may remain in the
archive. Completed and ignored inbox payloads older than 30 days are cleared,
while compact event IDs stay for duplicate detection. This cleanup does not
remove archived message bodies or their raw evidence. Failed lifecycle evidence
is retained for later reconciliation.

Official Account Manager's [Chat Pro history export](https://www.lycbiz.com/jp/service/line-official-account/chat-pro-option/)
is a separate paid feature. Check its availability for your account and region.
msgvault does not import its CSV files; this webhook source cannot recover old
chats from an export.
