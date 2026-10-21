---
last_edited: 2026-10-20
title: Open loops
description: See which conversations still wait on your reply, and which replies you are waiting on.
---

Open loops list the email and direct-chat conversations that still wait on an
answer. They answer two questions: who is waiting on a reply from you, and who
you are waiting on. Each list is worked out from the archive when you ask, so a
reply that just synced to the same conversation clears its loop on the next
list.
There is nothing to turn on, and no indexing step. It calls no model and
changes nothing at the email provider.

```bash
msgvault loops list
msgvault loops list --limit 10
```

Each row shows its age, the person, the subject, and the message ID that
started the wait. Read that message with `msgvault show-message <id>`. The
Directory person page shows the same loops for one person in a read-only Open
loops card.

## What counts as a loop

A loop starts at the oldest message in the latest run of one side's messages
that asks a question or makes a request.

- **Waiting on you** (`reply_owed`): an email addressed to you in To asks
  something, and 24 hours have passed. In a direct chat, a message to you that
  asks a question or makes a request counts. An email that only copies you in
  Cc or Bcc never counts, and neither does one between two other people.
- **Waiting on them** (`awaiting_reply`): your message asks something of the
  one person in To, or of the other person in a direct chat, and 72 hours have
  passed. An email to several people is group handling, which loops don't cover
  yet, so it opens nothing. Two addresses of one person count as one person.

Every address of every account you added is you, whichever account received
the message. Mail between two of your own accounts opens nothing. Addresses
linked to your own person count as you too, even when they are not one of your
account addresses. A question sent only to an old address you linked in the
Directory waits on you, mail from your other mailbox opens nothing, and a
question sent only to it waits on nobody.

A loop stays open until someone answers, however old it gets and however many
follow-ups pile on. It keeps the age of the message that started it. For
example, Alex asks "Can you send the contract?" on Aug 20 and nudges on Oct 18.
On Oct 20 the loop lists with Aug 20 as its start and 61 days as its age.
Your message answers only the people it went to. If Alex asks you, you forward
the thread to Sam, and Sam replies, Alex still waits on you.

A message asks something when its own text has a question mark, or says "can
you", "could you", "would you", "will you", "let me know", "let us know",
"lmk", or "please" followed by send, confirm, review, reply, advise, share,
check, or call. Quoted lines, signatures, links, "Original Message" blocks,
Outlook "From:" and "Sent:" headers, and forwarded text do not count. These
wording rules read English only.

A statement such as "See you Saturday!" opens nothing. All addresses saved on
one person count together.

## What settles a loop

A real answer from the other side settles it, including a reply from a
`support@` or `info@` address. These messages neither open nor settle a loop:

- A bare acknowledgement, unless it has a file attached. Its own text is at
  most 60 characters with no question mark, and is only the words thanks,
  thank you, thx, ty, many thanks, got it, noted, received, ack, ok, okay,
  cheers, perfect, or great. "Great, thanks!" therefore leaves the loop open.
  "Sounds good." counts as an answer, so it settles "Does 3pm work?".
- Automatic replies, bulk or list mail, calendar items, and no-reply senders.
  Your own out-of-office reply doesn't settle a request either.
- A message with no text of its own and no file, such as a reply that only
  quotes yours.
- An email whose stored original fails to parse. The list says how many it
  passed over, so a reply in one of them that would have settled a loop is
  not a silent gap. A body that merely starts with the words "MIME parsing
  failed" is read like any other.
  An email with no stored original is judged by its sender, list ID, and text.
- An email where you are only on Cc or Bcc, mail between other people, and
  drafts.
- Your message that goes to someone other than the person who asked, such as
  a forward to a colleague. It leaves their request waiting on you. Your
  reply settles it when that person is on To or Cc.

A message you or the other side deleted at the provider stays in the archive
and still counts, as a question or as the answer.

Automatic-reply and bulk-mail headers, and calendar parts, are read from each
stored original with the parser that sync uses, wherever the part sits in the
message.

## Clear a loop yourself

```bash
msgvault loops done 4521
msgvault loops snooze 4521 --until 48h
msgvault loops snooze 4521 --until 2026-10-25
```

`done` hides the loop, and `snooze` hides it until the time passes. A mark
covers the questions asked so far. When a newer question arrives in the same
stretch, the loop lists again with its original age and message ID. A message
that asks nothing leaves it hidden. For example, Alex asks "Can you send the
contract?" on Monday and you mark it done. "And the invoice?" on Wednesday
lists the loop again, dated Monday. "Just following up." does not. Marking it
again covers Wednesday's question. A new stretch that starts with a different
message shows again too. The mark belongs to the conversation, so an older
question imported later stays hidden even though the loop now starts there. A
different question sent at the same instant as the last one you marked lists
the loop again.

Your latest mark on a loop replaces the earlier one. The one exception is
`snooze` on a loop marked done that no newer question has reopened: the loop
stays done, and the command says so. Both commands take the message ID from
`loops list`, and both survive duplicate removal and account merges. When two
copies of a message carry different marks, done wins over a snooze, two dones
keep the later question, and two snoozes keep the later deadline. Both re-read the conversation first and accept only what `loops list`
would show now, so they report that the message is not an open loop when a
reply is already archived, when the conversation is older than 30 days, or
when the loop is still inside its 24 or 72 hours.

## Limits

- A reply settles a loop when it is archived in the same conversation. If
  Alex answers an address of another account of yours, that reply sits in
  that account's conversation, and the loop stays until you run
  `msgvault loops done`.
- The list reads conversations that are not mailing-list mail and have a
  message in the last 30 days, oldest first. It stops once it holds more loops
  than you asked for, so a list with few or no loops reads every one of those
  conversations. It opens a stored original only for a message that could
  change the answer, such as the one that starts a wait. Automated mail among
  a conversation's newest messages means more originals, up to one per
  message, and the list takes longer. The figures below come from 3,000
  conversations of one or two human messages each, on one machine. A list that
  stopped at 50 loops took 0.25 seconds, and a list with no loops took 1 second
  for plain-text messages and 12 seconds when each message carried a 96 KiB
  attachment. Each person-card view runs the same list for that person's conversations.
- The list covers conversations with a message in the last 30 days. A question
  that nobody has written about for longer leaves the default list, and
  returns, with its original age, when anyone writes again.
- A direct chat has no stored original to check, so the automatic-reply and
  bulk-mail rules don't apply. A bot or short-code sender that asks something
  opens a loop, and `msgvault loops done` clears it.
- Email threads and direct chats only. Group chats, channels, and meetings
  are skipped.

See the [CLI reference](../cli-reference.md#loops) for flags and the
[HTTP API](../api-server.md#open-loops) for endpoints. Loop routes are
owner-only: delegated agent tokens cannot reach them.
