---
last_edited: "2026-11-07"
title: Messages from Any Source
description: Archive messages from a source msgvault has no connector for, such as feed items or tool notifications, one HTTP request per message.
---

Archive messages that msgvault has no connector for, next to your mail and
chats: the items of the feeds you follow, notifications from internal tools,
or another app's messages. A small script sends each message to the daemon,
and msgvault keeps it searchable after the source deletes it. For RSS, Atom,
and JSON feeds you need no script: msgvault can [follow them](feeds.md)
itself.

## Import a message

Configure an API key, start `msgvault serve`, then send one message per request
to `POST /api/v1/import/message`:

```bash
curl http://localhost:8080/api/v1/import/message \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  --data '{
    "source": {
      "identifier": "feeds",
      "display_name": "Feeds",
      "account_email": "you@example.com"
    },
    "message": {
      "external_id": "entry-12345",
      "message_type": "feed_item",
      "conversation": {"external_id": "https://example.com/releases.atom", "title": "Example Project releases"},
      "sender": {"name": "Example Project", "url": "https://example.com"},
      "title": "Example Project 1.2.0",
      "url": "https://example.com/releases/1.2.0",
      "sent_at": "2026-11-07T10:00:00-05:00",
      "body_markdown": "## Changes\n\n- Faster sync."
    }
  }'
```

The first import returns `201` with status `created`. Sending the same
`source.identifier` and `message.external_id` again returns `200` with status
`updated`: an unchanged retry writes nothing, and a changed message replaces
the archived copy instead of adding a second one. Keep both values stable in
your source; the [API reference](../api-server.md#post-apiv1importmessage) has
the full contract.

`source.account_email` identifies you. It becomes a confirmed identity for the
source, so messages whose sender has that address count as sent by you. These
sources are on demand: run your script again to add or update messages, rather
than using **Sync now** or a schedule.

## Group messages into conversations

`message.conversation.external_id` names the conversation a message belongs
to, such as one feed. Messages with the same value in the same source share a
conversation, and its `title` names it in msgvault. A later message without a
title keeps the current one. A replacement that names another conversation
moves the message there.

## Choose a message type

`message.message_type` is yours to choose. Use lowercase letters, digits, and
underscores, starting with a letter, for example `feed_item`. Filter on it like
any built-in type:

```bash
msgvault search "release" --message-type feed_item
msgvault search "message_type:feed_item release"
msgvault export-messages --start 2026-11-01T00:00:00Z --end 2026-12-01T00:00:00Z --message-type feed_item
```

## How senders become people

`message.sender` needs at least one field. The first field present, in this
order, decides which participant sent the message:

| Field | Participant |
|---|---|
| `email` | The participant for that address, shared with mail and every other source |
| `phone` | The participant for that number; it must be international (`+` or `00`) and is stored in E.164 form |
| `id` | One participant per `id` within this import source |
| `url` | One participant per site, shared by every import source |
| `name` | One participant per name within this import source |

The `name` also becomes the participant's display name when it has none. An
`id` or `name` means something only inside its source, so the same `id` sent
by two sources names two different senders.

## What gets stored

- A message needs a `title` or a body. Send at most one of `body_markdown`,
  `body_html`, and `body_text`.
- Markdown is stored as plain text. HTML is kept, and msgvault also stores a
  plain-text form for search and previews.
- The message `url` ends the stored body, so readers can open the original and
  search can find it.
- `sent_at` uses RFC 3339 with an explicit offset.
- Fields your source adds belong under `message.metadata`. Unknown fields
  elsewhere are rejected, and the request is limited to 16 MiB.
- Text, metadata included, must be valid UTF-8 without NUL characters, which
  PostgreSQL can't store.
- Keyword search finds a message as soon as the import returns.

## Limitations

- Each request carries one message.
- Attachments are not imported. Link to them from the body or `url`.
