---
last_edited: "2026-09-08"
title: Choose a Source
description: Find the right sync or import path for mail, chat, meetings, calendars, and contacts.
---

Bring a live account or a local export into the same archive. **Sync** contacts
a service to collect new and changed records. **Import** reads an export you
already have. Available history and attachments depend on the service's access
rules and what the export contains.

Install msgvault with the [setup guide](../setup.md), then choose a source below.
You do not need Google OAuth credentials to import local files or use a
non-Google provider.

## Email

| Your source | Start here | What you need |
|---|---|---|
| Gmail or Google Workspace | [Gmail setup](../setup.md#configure-oauth) | Google OAuth app and account authorization; read-only access is an option |
| An IMAP mailbox | [IMAP sync](../usage/imap.md) | Server address and credentials, often an app password |
| Microsoft 365 mail | [Microsoft 365 setup](../cli-reference.md#add-o365) | A Microsoft OAuth app and IMAP access |
| Maildir or Maildir++ archive | [Maildir import](../usage/importing.md#import-maildir) | A stable snapshot with `cur`, `new`, and `tmp` directories |
| MailMate-style `.mailbox` directories | [EML import](../usage/importing.md) | A `.mailbox` tree containing `.eml` files and an archive identifier |
| MBOX, Apple Mail, or Outlook PST | [Local email import](../usage/importing.md) | An exported mailbox or readable local mail directory |

Sync preserves mail in the archive without sending or replying to messages.
[Removing mail from a provider](../usage/deletion.md) is a separate, explicitly
authorized operation. [Remote image archiving](../usage/remote-images.md) is
also a separate opt-in because downloading an image can activate email tracking.

## Chat and text messages

| Your source | Start here | Scope |
|---|---|---|
| Slack | [Slack sync](../usage/slack.md) | Channels you have joined, group DMs, DMs, threads, and available files |
| Slackdump export | [Slackdump import](../usage/slack.md) | Local directory or ZIP; no Slack token needed |
| Microsoft Teams | [Teams sync](../usage/teams.md) | Chats, self-chat, channels, replies, and available media |
| Discord | [Discord sync](../usage/discord.md) | Bot-accessible guild channels, threads, and forums; personal DMs are outside this integration |
| Beeper Desktop | [Beeper sync](../usage/beeper.md) | History and media exposed by the running local Beeper API |
| WhatsApp, iMessage, Google Voice, Messenger | [Text message imports](../usage/text-messages.md) | Supported backups or exports, with your identity supplied where required |
| SMS Backup & Restore | [Android SMS and call logs](../usage/text-messages.md) | Local XML/ZIP or scheduled imports from a configured Drive folder |

Chat media has size and room-participant limits. A message can be archived
without all its media. Review [media policy](../configuration.md#media-policy)
before a large sync and use the provider's backfill command to retry eligible
missing downloads.

## Meetings, calendars, and contacts

| Your source | Start here | What it adds |
|---|---|---|
| Granola, Circleback, or Notion AI Meeting Notes | [Meeting notes and transcripts](../usage/meetings.md) | Searchable notes, transcripts where available, and participants |
| Another meeting capture tool | [Meeting import API](../api-server.md) | Provider-neutral ingestion keyed by source and external meeting ID |
| Google Calendar | [Calendar sync](../usage/calendar.md) | Events, organizers, attendees, recurrence, and cancellation state |
| CardDAV address book | [CardDAV contacts](../usage/people-carddav.md) | Imported contacts and explicit publication of curated profiles |

Meeting and calendar sync reads source data. CardDAV can also write to address
books when you select a publication role and explicitly publish a person;
review its conflict and consent workflow before enabling that direction.

## After the first import or sync

1. Open the [Web UI](../web-ui.md) or [TUI](../usage/tui.md) and inspect a small
   sample, including attachments and dates.
2. Confirm [which identifiers mean you](../usage/people.md) so sent/received
   classification and contact activity have the right starting point.
3. Set a provider schedule if you want ongoing sync. Local export imports remain
   separate commands; see each guide for repeat-import behavior.
4. Create a [backup](../usage/backup.md). Add more sources and
   [organize accounts into collections](../usage/multi-account.md) as needed.

For records already stored, keyword search and analytics use the archive.
Optional [semantic search and profile automation](../usage/recommended-configuration.md)
have their own provider configuration and consent steps.
