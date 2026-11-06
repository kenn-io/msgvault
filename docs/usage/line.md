---
last_edited: 2026-11-02
title: LINE Official Account
description: Archive LINE Official Account chats and send reviewed text replies from local drafts.
---

# Archive and reply to LINE chats

Connect a LINE Official Account to archive incoming direct chats, group chats,
and existing multi-person rooms. Text is archived before profile or
media downloads finish. Quotes link to messages in the same archived chat;
`join`, `leave`, `memberJoined`, and `memberLeft` events update participants.
Friendship events (`follow` and `unfollow`) are ignored. Incoming direct messages
create their conversation. With an explicit send opt-in, the owner
can reply from a reviewed local draft and archive the sent message.

!!! note "Available on main"
    This integration is available in builds from `main`, newer than the latest
    release documented in the [changelog](../changelog.md).

The archive starts when the webhook goes live. LINE has no message-history API,
so msgvault cannot backfill earlier chats. Staff replies from Official Account
Manager or the Official Account app, auto-replies, greetings, broadcasts, and
messages sent through other tools are invisible to this integration. msgvault
archives its own replies when the owner uses a
[reviewed reply draft](#reply-from-a-reviewed-draft).

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
    source, and saves configuration.
4. Restart the daemon or service with the secret variable present. This loads
   the saved channel and mounts its receiver. Enabled LINE channels keep a
   background daemon running through quiet periods; idle shutdown is disabled.
5. Make the [public webhook route](#expose-only-the-webhook-route) reachable.
   Set `https://archive.example.com/webhooks/line/2000000001` as the webhook URL
   in LINE Developers Console, using your public hostname and channel ID.
   The optional `--webhook-base-url` flag prints this URL during registration.
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
allow_send = false
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
| Images, video, audio, and files | LINE-provided bytes streamed into the archive, up to the configured cap (250 MiB by default) |
| External-provider media | URLs retained as metadata; msgvault never fetches those URLs |
| Stickers | Native sticker metadata and available text or keywords; no scraped sticker art |
| Quotes | Linked when the quoted message is archived in the same chat; missing targets can resolve later |
| Text edits in groups | Latest text replaces the archived text and is marked edited; earlier versions remain in sanitized inbox payloads |
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

## Reply from a reviewed draft

The owner can send a text reply to an archived LINE direct chat, group, or room.
Sending is off by default. In that channel's `[[line]]` block, set:

```toml
allow_send = true
```

The daemon reads the current saved channel policy before admitting a send and
before each provider retry. The channel must remain enabled, with its secret
in the daemon environment. No agent grant permits sending. Creating, reading,
editing, or deleting a local draft does not send it.

1. Create a draft for the archived conversation and an archived message in it:

    ```bash
    msgvault draft-compose --conversation 42 --reply-to 123 --body 'Thanks for the update.' --json
    ```

2. Review the returned draft ID, revision, destination, and body:

    ```bash
    msgvault draft-get chat-draft-example --json
    ```

3. Send the reviewed revision:

    ```bash
    msgvault send-line-reply chat-draft-example --revision 1 --json
    ```

The destination and parent must belong to the same archived source and chat.
Text must be nonblank UTF-8 without NUL, up to 5,000 UTF-16 code units. msgvault
quotes the parent when its archived metadata has a `quoteToken`. It sends one
text message through LINE's push API; outbound files, images, stickers, and
reply-token sends are not supported.

Push messages consume the account's monthly message quota. LINE also restricts
eligible recipients: a user generally must be a friend, or have messaged the
account within the previous seven days; the account must belong to a target
group or room. A successful response proves acceptance, not delivery. A blocked
account or other LINE restrictions can prevent delivery. See
[LINE's push API](https://developers.line.biz/en/reference/messaging-api/#send-push-message).

### Read a send receipt

The JSON result includes `draft_id`, `revision`, `source_id`, `conversation_id`,
`status`, and `accepted`. Archived receipts include `remote_message_id` and
`message_id`; pending receipts can include `retry_after`.

| Status | Next action |
|---|---|
| `sent` | The sent message is archived in the original chat. Its parent link is empty if archive cleanup removed the original message. The draft is removed; repeat calls return the same receipt. |
| `sent_archive_pending` | LINE accepted the message and its ID is saved. Repeat the same command to repair the archive locally. This recovery does not send again, even after policy revocation or retry expiry. |
| `admitted` | Nothing has reached LINE. Repeat the same command after `retry_after`, or delete the draft to cancel the unsent reply. Its receipt is kept. |
| `remote_unknown` | A send may have reached LINE, or LINE accepted it without a usable message ID. Keep any returned IDs, and repeat the same command after `retry_after` before the cutoff. Recovery needs sending enabled and credentials available. The draft can be deleted and its receipt is kept. |
| `provider_rejected` | The first submission was definitively rejected. The draft stays frozen for edits and can be deleted. This draft ID keeps its terminal receipt; a new send requires a new reviewed draft. |
| `expired` | Retries have stopped; LINE may have accepted the message. Don't send a replacement. The draft can be deleted; its receipt is kept. |
| `credentials_unavailable` error | LINE returned HTTP 401 or 403, or local credentials are unavailable. Fix credentials or channel permissions, then repeat the same reviewed send with its original retry key before the original cutoff. |
| HTTP 429 | A rate limit on the first submission returns the attempt to `admitted`; retry after `retry_after` with the same key, before the original cutoff. A rate limit during recovery keeps the same key and cutoff. |

Every admitted LINE draft can be deleted, even after sending is disabled.
Deletion cancels an unsent `admitted` attempt and keeps its receipt. A request
that may have reached LINE remains recoverable after deletion. Editing stays
refused once admitted. Repeat
`send-line-reply` with the deleted draft ID and reviewed revision to recover
the saved attempt or return its receipt. Provider retries still require
sending to be enabled and credentials to be available.

Deleting an unsent `admitted` draft, a rejected draft, or one whose retry window
expired without acceptance clears the attempt's text, quote token, and
conversation key. Attempts that can still send or recover keep their frozen content.

An admitted attempt keeps one persisted UUID. Its text, recipient, and quote
token stay frozen while it can still send or recover. Explicit retries reuse
that UUID. LINE remembers retry keys for
24 hours; msgvault stops provider retries 23 hours after admission and waits
at least 30 seconds between attempts, honoring longer rate-limit or service
retry delays. There are no background sends. See
[LINE's retry semantics](https://developers.line.biz/en/docs/messaging-api/retrying-api-request/).
Never create a replacement draft just to bypass an uncertain or expired send.
A receipt without a remote ID may remain unarchived after the recovery window.
The archived timestamp estimates send time from the first request.

The daemon HTTP API accepts the same command through
[`POST /api/v1/cli/run`](../api-server.md#line-reply).
The owner MCP tool `send_line_reply` requires confirmation of the exact draft ID
and revision. Clients unable to complete confirmation cannot send. HTTP MCP
also requires its existing write opt-in. Delegated MCP sessions omit the tool.
See [chat tools](chat.md) for connection options.

Messages sent from Official Account Manager or other tools still do not appear
in this archive. Only replies sent through this msgvault command have a sent
receipt that msgvault can archive.

## Check delivery

The Sources page shows whether this daemon mounted the channel's receiver,
when its last event arrived, and how many events are pending or failed. An
empty LINE webhook verification request does not count as a received event.
The invalid-signature count resets when the daemon restarts. Rejected
signatures produce a warning at most once per minute per channel.

**Process inbox** processes up to 500 due events per pass. The daemon continues
the remaining work on its 30-second tick. It cannot fetch earlier LINE history.
If the receiver is inactive, check the channel configuration and secret
environment variable, then restart the daemon.

## Delivery and retries

Each channel can disable downloads with `media = false` or set a per-file
cap with `max_media_mb`. The default cap is 250 MiB.
Changes apply to messages processed after the change; earlier skipped media isn't fetched later, and LINE content expires.

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
again, use **Process inbox** or:

```bash
msgvault sync-line
msgvault sync-line --channel-id 2000000001
```

Each command drains due work across as many 500-event passes as needed.
**Process inbox** runs one pass; the daemon continues the remaining work on its
30-second tick. Neither fetches history or makes a delayed retry due immediately.
Review pending work separately from failed work when checking source health.

## Privacy and retention

LINE [recommends removing unsent content](https://developers.line.biz/en/docs/messaging-api/receiving-messages/#webhook-unsend-message).
msgvault retains archived body and media with a source-deleted flag, as it does
for other sources. Choose this archive behavior deliberately and arrange user
consent and a reachable privacy policy covering retention under
[LINE's terms](https://developers.line.biz/en/terms-and-policies/).
This integration has no option to purge content automatically on unsend.

Reply and mark-as-read capability tokens are removed recursively before events
are persisted. Native metadata, including `quoteToken`, may remain in the
archive. Inbox rows and their sanitized payloads remain after processing for
duplicate detection and lifecycle reconciliation, including earlier text versions.
Archive garbage collection clears inbox payloads for messages it purges while
keeping event IDs for duplicate detection.

Official Account Manager's [Chat Pro history export](https://www.lycbiz.com/jp/service/line-official-account/chat-pro-option/)
is a separate paid feature. Check its availability for your account and region.
msgvault does not import its CSV files; this webhook source cannot recover old
chats from an export.
