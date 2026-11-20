---
last_edited: "2026-11-07"
title: Messages from Any Source
description: Archive messages from a source msgvault has no connector for, such as feed items or tool notifications, one HTTP request per message.
---

Archive messages from sources that msgvault does not have a built-in connector
for, such as feed items, notifications from internal tools, or messages from
another app, alongside your mail and chats. Write a small script to send each
message to the daemon, and msgvault keeps it searchable after the source
deletes it. For RSS, Atom, and JSON feeds, you do not need a script. msgvault
can [follow them](feeds.md) directly.

## Import a message

To import messages over HTTP:

1. Configure an API key.
2. Start `msgvault serve`.
3. Send one message per request to `POST /api/v1/import/message`:

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

The first import returns `201` with status `created`. If you send the same
`source.identifier` and `message.external_id` again, the API returns `200` with
status `updated`. Retrying an unchanged message writes nothing, while sending a
changed message replaces the archived copy instead of adding a second one. Keep
both identifiers stable in your source. For the full request and response
contract, see the [API reference](../api-server.md#post-apiv1importmessage).

Set `source.account_email` to identify yourself. msgvault records it as a
confirmed identity for the source, so messages whose sender has that address
count as sent by you. These sources update on demand. Run your script again to
add or update messages instead of using **Sync now** or a schedule.

## Group messages into conversations

Use `message.conversation.external_id` to group related messages, such as items
from a single feed. Messages in the same source with the same conversation ID
share a conversation, using `title` as its name in msgvault. If a later message
omits `title`, the conversation keeps its current name. If a replacement
message names a different conversation, msgvault moves the message there.

## Choose a message type

You can choose your own value for `message.message_type`. Start with a
lowercase letter and use only lowercase letters, digits, and underscores, for
example `feed_item`. You can filter on custom types the same way you filter on
built-in types:

```bash
msgvault search "release" --message-type feed_item
msgvault search "message_type:feed_item release"
msgvault export-messages --start 2026-11-01T00:00:00Z --end 2026-12-01T00:00:00Z --message-type feed_item
```

## How senders become people

Include at least one field in `message.sender`. msgvault checks the fields in
the following order and uses the first one present to determine which
participant sent the message:

| Field | Participant |
|---|---|
| `email` | The participant for that address, shared with mail and every other source |
| `phone` | The participant for that number; it must be international (`+` or `00`) and is stored in E.164 form |
| `id` | One participant per `id` within this import source |
| `url` | One participant per site, shared by every import source |
| `name` | One participant per name within this import source |

If the participant does not have a display name yet, msgvault uses `name`.
Because `id` and `name` are meaningful only inside their own source, sending
the same `id` from two sources creates two different senders.

## What gets stored

- Include a `title` or a body, and send at most one of `body_markdown`,
  `body_html`, or `body_text`.
- msgvault stores Markdown as plain text. For HTML, it keeps the original HTML
  and also stores a plain-text form for search and previews.
- msgvault appends the message `url` to the end of the stored body, so readers
  can open the original and search can find it.
- Format `sent_at` using RFC 3339 with an explicit offset.
- Put any custom fields from your source under `message.metadata`. The API
  rejects unknown fields elsewhere, and each request is limited to 16 MiB.
- All text, including metadata, must be valid UTF-8 with no NUL characters,
  which PostgreSQL cannot store.
- Keyword search finds a message as soon as the import returns.

## Limitations

- Each request carries one message.
- Attachments are not imported. Link to them from the body or `url` instead.
