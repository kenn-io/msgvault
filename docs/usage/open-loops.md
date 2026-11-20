---
last_edited: 2026-11-08
title: Open loops
description: See which conversations still wait on your reply, and which replies you are waiting on.
---

Open loops show who is waiting on a reply from you, and who you are waiting on,
across email and direct chats. msgvault works them out from your archive each
time you ask, so a newly synced reply clears its loop on the next list. There is
nothing to turn on, no model is called, and nothing changes at your email
provider.

## See your open loops

List your open loops from the CLI:

```bash
msgvault loops list
msgvault loops list --person 42
msgvault loops list --json
```

The list splits into two sections, **Waiting on you** and **Waiting on them**,
oldest first. Each row shows how long the wait has lasted, the person, the
subject, and the ID of the message that started the wait. To read that message,
run `msgvault show-message <id>`.

In the Web UI, open a saved person in **People** and select **Details**. The
**Open loops** section lists that person's loops, and selecting one opens its
message.

If a list says more history remains, the daemon keeps filling the cache in the
background and later requests reuse completed work. If the daemon stops or the
background pass reaches its time limit, a later incomplete request starts
another pass.

## What counts as a loop

A loop starts with a message that asks something and has no answer yet:

- **Waiting on you** (`reply_owed`). Someone asked you a question or made a
  request, and 24 hours have passed without your reply. In email, you must be
  in To; mail that only copies you in Cc or Bcc doesn't count. In a direct
  chat, any message to you counts.
- **Waiting on them** (`awaiting_reply`). You asked one person something, and
  72 hours have passed without their reply. In email, the message must go to
  exactly one person in To. Email to several people doesn't start a loop.

A message counts as asking something when its own text includes a question mark
or one of these phrases: "can you", "could you", "would you", "will you", "let
me know", "let us know", "lmk", or "please" followed by send, confirm, review,
reply, advise, share, check, or call. Quoted earlier messages, signatures,
links, and forwarded text don't count. These rules read English only, and a
statement such as "See you Saturday!" starts nothing.

Each person has their own loop, and all addresses saved on one person count as
that person. Every address across your accounts counts as you, along with every
address linked to your own person. Mail between your own accounts never starts
a loop.

A loop keeps the age of its oldest unanswered question. For example, if Alex
asks "Can you send the contract?" on Aug 20 and sends "Thanks!" on Oct 18, the
loop still starts on Aug 20. Loops don't expire, no matter how old they are.

## What answers a loop

A real reply from the person you are waiting on answers the loop, including a
reply from a `support@` or `info@` address or one that only copies you. A reply
from someone else doesn't. Your own reply answers a request only when the person
who asked is among its recipients; forwarding the thread to a colleague leaves
the request waiting on you.

These messages neither start nor answer a loop:

- A bare acknowledgement with no file attached. It is 60 characters or fewer,
  has no question mark, and contains only words such as thanks, thank you, thx,
  ty, many thanks, got it, noted, received, ack, ok, okay, cheers, perfect, or
  great, or emoji such as 👍 or ✅. "Great, thanks!" leaves the loop open, while
  "Sounds good." answers "Does 3pm work?".
- Automatic replies, including your own out-of-office reply.
- Mailing-list and bulk mail, calendar invitations, and mail from no-reply
  addresses.
- A message with no text of its own and no file, such as a reply that only
  quotes yours.
- Drafts.

A message deleted at the provider stays in your archive and still counts.

## Mark a loop done or snooze it

Pass the message ID from `msgvault loops list` to clear, snooze, or reopen a
loop:

```bash
msgvault loops done 4521
msgvault loops snooze 4521 --until 48h
msgvault loops snooze 4521 --until 2026-12-01
msgvault loops reopen 4521
```

- `done` hides the loop's questions for good.
- `snooze` hides them until `--until` passes. Pass a duration such as `48h`, a
  date (midnight local time), or an RFC3339 time.
- `reopen` removes the done or snooze decision from one question.

Decisions belong to questions rather than to the conversation. A new question
from the same person brings the loop back after its own 24- or 72-hour wait,
and an older question imported later does the same.

Both `done` and `snooze` check the conversation again before saving. If the
person answered in the meantime, the command fails with
`<id> isn't an open loop.`

A loop can gain a question after you list it. To act only on the questions you
saw, pass the item's `question_message_ids` from `msgvault loops list --json`:

```bash
msgvault loops done 4521 --questions 4521,4530
```

Decisions follow their questions when duplicates or accounts are merged. If two
copies of a question carry different decisions, done wins over snooze, and the
later of two snoozes wins.

## Use loops from an assistant

An MCP assistant connected as the owner can call `list_open_loops`. When started
with `--allow-loop-writes`, it can also mark loops done, snooze them, or reopen
them, with your approval on each call. See
[Open loops in the MCP guide](chat.md#open-loops) for the tools and their
parameters.

## Use loops from the HTTP API

You can also list loops, mark loops done, snooze them, and reopen them through
the HTTP API. See the [HTTP API reference](../api-server.md#open-loops) for the
routes and the [CLI reference](../cli-reference.md#loops) for every command
flag.

Only the owner can use loops. Delegated agent tokens and remote clients can't.

## Limits

- **Large archives take more than one request.** Each request reads at most
  1,000 messages it hasn't seen before, oldest conversations first, and saves
  what it learned. If it stops early, the list notes that more history remains
  (`coverage_incomplete` in JSON). Run the command again to continue, or select
  **Check more** in the **Details** card under **People**. The same thing
  happens when a request runs out of time.
- **Unreadable messages are counted, not hidden.** If a stored email can't be
  parsed, the list reports how many it skipped, because a reply in one of them
  may have answered a loop.
- **A reply must land in the same conversation.** If Alex answers at another of
  your accounts, that reply sits in a different conversation, and the loop stays
  open until you mark it done.
- **Chats have no headers to check.** A bot or short-code sender that asks
  something starts a loop. Mark it done to clear it.
- **Email threads and direct chats only.** Group chats, channels, and meetings
  are skipped.
