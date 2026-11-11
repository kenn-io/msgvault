---
last_edited: 2026-11-08
title: Open loops
description: See which conversations still wait on your reply, and which replies you are waiting on.
---

Open loops show who is waiting on a reply from you, and who you are waiting
on, across email and direct chats. msgvault works them out from your archive
each time you ask, so a reply that just synced clears its loop on the next
list. Reply loops are available immediately. They use local wording rules and leave your email provider unchanged.

Open loops are on unreleased `main`. They need a daemon with HTTP API schema
3.44.0 or newer.

## See your open loops

```bash
msgvault loops list
msgvault loops list --person 42
msgvault loops list --json
```

The list has two sections, **Waiting on you** and **Waiting on them**, oldest
first. Each row shows how long the wait has lasted, the person, the subject,
and a message ID. That ID is the message that started the wait. Read it with
`msgvault show-message <id>`.

In the Web UI, open a saved person in **People** and choose **Details**. The
**Open loops** section lists that person's loops. Select one to open its
message.

## What counts as a loop

A loop starts with a message that asks something and has no answer yet:

- **Waiting on you** (`reply_owed`): someone asked you a question or made a
  request, and 24 hours have passed without your reply. In email, you must be
  in To. Mail that only copies you, in Cc or Bcc, doesn't count. In a direct
  chat, any message to you counts.
- **Waiting on them** (`awaiting_reply`): you asked one person something, and
  72 hours have passed without their reply. In email, the message must go to
  exactly one person in To. Email to several people doesn't start a loop.

A message asks something when its own text has a question mark, or one of
these phrases: "can you", "could you", "would you", "will you", "let me
know", "let us know", "lmk", or "please" followed by send, confirm, review,
reply, advise, share, check, or call. Quoted earlier messages, signatures,
links, and forwarded text don't count. These rules read English only. A
statement such as "See you Saturday!" starts nothing.

Each person has their own loop. All addresses saved on one person count as
that person. Every address of your accounts counts as you, and so does every
address linked to your own person. Mail between your own accounts never
starts a loop.

A loop keeps the age of its oldest unanswered question. If Alex asks "Can you
send the contract?" on Aug 20 and sends "Thanks!" on Oct 18, the loop still
starts on Aug 20. Loops don't expire, however old they are.

## What answers a loop

A real reply from the person you are waiting on answers the loop. That
includes a reply from a `support@` or `info@` address, and a reply that only
copies you. A reply from someone else doesn't. Your own reply answers a
request only when the person who asked is among its recipients. Forwarding
the thread to a colleague leaves the request waiting on you.

These messages neither start nor answer a loop:

- A bare acknowledgement with no file attached. It is 60 characters or fewer,
  has no question mark, and contains only words such as thanks, thank you,
  thx, ty, many thanks, got it, noted, received, ack, ok, okay, cheers,
  perfect, or great, or emoji such as 👍 or ✅. "Great, thanks!" leaves the
  loop open. "Sounds good." answers "Does 3pm work?".
- Automatic replies, including your own out-of-office reply.
- Mailing-list and bulk mail, calendar invitations, and mail from no-reply
  addresses.
- A message with no text of its own and no file, such as a reply that only
  quotes yours.
- Drafts.

A message deleted at the provider stays in your archive and still counts.

## Mark a loop done or snooze it

```bash
msgvault loops done 4521
msgvault loops snooze 4521 --until 48h
msgvault loops snooze 4521 --until 2026-12-01
msgvault loops reopen 4521
```

Use the message ID from `msgvault loops list`.

- `done` hides the loop's questions for good.
- `snooze` hides them until `--until` passes. It takes a duration such as
  `48h`, a date (midnight local time), or an RFC3339 time.
- `reopen` removes the done or snooze decision from one question.

Decisions belong to questions, not to the conversation. A new question from
the same person brings the loop back after its own 24- or 72-hour wait. So
does an older question that is imported later.

`done` and `snooze` check the conversation again before saving. If the person
answered in the meantime, the command fails with `<id> isn't an open loop.`

A loop can gain a question after you list it. To act only on the questions
you saw, pass the item's `question_message_ids` from
`msgvault loops list --json`:

```bash
msgvault loops done 4521 --questions 4521,4530
```

Decisions follow their questions when duplicates or accounts are merged. If
two copies of a question carry different decisions, done wins over snooze,
and the later of two snoozes wins.

## Use loops from an assistant

An MCP assistant connected as the owner can call `list_open_loops`. Started
with `--allow-loop-writes`, it can also mark loops done, snooze them, or
reopen them, and you approve each call. See
[Open loops in the MCP guide](chat.md#open-loops) for the tools and their
parameters.

## Use loops from the HTTP API

The list, done, snooze, and reopen routes are described in the
[HTTP API reference](../api-server.md#open-loops). See the
[CLI reference](../cli-reference.md#loops) for every command flag.

Only the owner can use loops. Delegated agent tokens and remote clients can't.

## Limits

- **Large archives take more than one request.** Each request reads at most
  1,000 messages it hasn't seen before, oldest conversations first, and saves
  what it learned. If it stops early, the list says more history remains
  (`coverage_incomplete` in JSON). Run it again to continue. In **People**,
  the Details card has a **Check more** button. The same happens when a
  request runs out of time.
- **Unreadable messages are counted, not hidden.** If a stored email can't be
  parsed, the list says how many it skipped, because a reply in one of them
  may have answered a loop.
- **A reply must land in the same conversation.** If Alex answers at another
  of your accounts, that reply is in a different conversation. The loop stays
  until you mark it done.
- **Chats have no headers to check.** A bot or short-code sender that asks
  something starts a loop. Mark it done.
- **Email threads and direct chats only.** Group chats, channels, and meetings
  are skipped.

## See cited commitments

Commitments are optional and require daemon API schema 5.2.0 or newer.
Enable `[people.sweep.commitments]` in [configuration](../configuration.md#commitment-extraction).
Use `msgvault person provider consent --commitments` to see the provider,
source dates, and what the request discloses. Repeat with `--yes` to consent
to sending conversation text from both sides of verified direct chats and
email. Each message contributes at most 2,000 runes of authored text.
Changing the provider source dates or commitment window and batch limits restarts
recent discovery while keeping saved promise decisions.
The profile must allow sensitive `conversation_text` sources. Check the grant
with `msgvault person provider status`. Use `msgvault person provider revoke`
to revoke both the ordinary inference and commitment grants for the profile.

Cited promises appear under **Promises you made** and **Promises to you**.
Each row links to the quoted message. The JSON response also gives the claim
ID and original-body rune range. See the [person facts CLI reference](../cli-reference.md#person-track-tiers-provider-sweep-and-facts) for claim kinds
and freshness. A due date is a calendar date in the
configured activity timezone. A promise expires after the due date and
configured grace period, or after the maximum duration if it has no due
date. Expired and unsupported claims leave the list.

Use the commitment ID for actions on a promise. This ID starts with `c`.
The same message can anchor multiple promises and a reply loop. Choose
**Show closed promises** in People Details or run
`msgvault loops list --include-closed` to find done, snoozed, or fulfilled promises
that you can reopen.

```bash
msgvault loops done c12
msgvault loops snooze c12 --until 48h
msgvault loops reopen c12
```

The list may show a proposed fulfilment with a quote from a newer message by
the person who made the promise. Review the quote and mark it done if the
promise was kept. With `auto_fulfil = true`, an explicit fulfilment supported
by newer evidence can close it automatically. Reopen keeps older evidence
from closing it again. If the supporting fulfilment is withdrawn, the
promise can appear again.
