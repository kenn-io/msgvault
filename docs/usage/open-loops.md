---
last_edited: 2026-10-22
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

Each person gets a separate loop, starting at their oldest unanswered, unmarked
question or request. Addresses linked to one person count together.

- **Waiting on you** (`reply_owed`): an email addressed to you in To asks
  something, and 24 hours have passed. In a direct chat, a message to you that
  asks a question or makes a request counts. An email that only copies you in
  Cc or Bcc opens no obligation, and neither does one between two other people.
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

Quiet conversations stay eligible at any age. Acknowledgements and replies
that settle nothing still count toward conversation activity; list mail,
drafts, and calendar items don't. Snoozed questions return when their deadline
passes.

An old question keeps its original age. For example, Alex asks "Can you send
the contract?" on Aug 20 and sends "Thanks!" on Oct 18. On Oct 20 the loop
lists with Aug 20 as its start and 61 days as its age. If neither of you had
written since Aug 20, the loop still lists with the same start and age.
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

A real answer from the awaited person settles it, including a reply from a
`support@` or `info@` address. Their reply also counts when you appear only on
Cc or Bcc. A reply from someone else leaves that wait open. These messages
neither open nor settle a loop:

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
- Mail between other people and drafts.
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
msgvault loops reopen 4521
```

`done` hides this person's open questions. `snooze` hides them until the time
passes. They cover the questions the list shows when you act. A question still
inside its 24- or 72-hour wait stays undecided and lists when its wait ends. A
distinct question imported later can list again, whatever its message date. For
example, Alex asks for the contract on Monday and you mark it done. "And the
invoice?" on Wednesday starts a new visible wait dated Wednesday. An invoice
question from Sunday imported afterward also appears. A message that asks
nothing leaves the loop hidden.

`reopen` clears the done or snooze choice on one exact question. Use the
message ID from the earlier list or decision output. Other questions keep
their decisions, and the question lists again only if it still waits for an
answer and has passed its grace period.

If an action reaches the evidence limit, retry it to continue checking the
conversation before saving the decision. The HTTP API returns
`503 loop_scan_incomplete` with `Retry-After: 1` for this condition.

Each question keeps its own decision when dates change or accounts merge.
The list shows the oldest question that is neither done nor actively snoozed,
and uses that question's age and message ID. Distinct questions keep their
own snooze deadlines through a merge, and each reopens when its deadline
passes. A new question must wait its own 24 or 72 hours before it lists.

`done` and `snooze` take the current message ID from `loops list` and re-read
the conversation before acting. A covered anchor, an archived answer, or a
question still inside its grace period makes the action fail with the
existing not-open error. `reopen` clears the decision by exact message ID; it
returns an error when that question has no decision.

A confirmed duplicate inherits the original question's decision. Hiding a
duplicate preserves its own decision so undo restores it too. Done wins over
snooze when duplicate decisions meet, and two snoozes retain the later
deadline. A deliberate new snooze can replace an earlier deadline on an open
question. Deleting a question removes only that question's decision.

## Use an assistant

On unreleased `main`, an owner-connected MCP assistant can call
`list_open_loops`, `close_open_loop`, `snooze_open_loop`, and
`reopen_open_loop`. The daemon must advertise each exact route and its required
fields. API schema 3.25.0 alone is insufficient. Delegated sessions expose none
of these tools.

Start MCP with `--allow-loop-writes` to allow done, snooze, and reopen. Each
decision needs approval for that call, showing the action and exact anchor ID.
HTTP also requires `--http-allow-writes` and MCP protocol `2026-07-28` or newer.
Older HTTP clients retain the list.

1. Call `list_open_loops` with an optional positive JSON-safe `person_id` and a `limit` from 1 to 200, default 50. The list includes both `reply_owed` and `awaiting_reply`.
2. Pass a returned `anchor_message_id` as `message_id` to `close_open_loop` or `snooze_open_loop`. They recheck the conversation and cover its currently eligible questions. The done tool retains the name `close_open_loop`; its receipt uses state `done`.
3. For snooze, supply `until` as RFC3339, including fractional seconds, or `YYYY-MM-DD` at midnight in the MCP process's local timezone. Approval shows the resolved instant and offset. The deadline must still be future when execution starts. MCP rejects relative durations such as `24h`; the CLI accepts them.
4. To clear one question's saved decision, call `reopen_open_loop` with its exact `message_id` from an earlier list or decision receipt. A hidden question is absent from the current list.

The tools return the daemon's list and decision responses. In a list,
`coverage_incomplete` means call again to continue checking retained evidence.
`truncated` means narrow by `person_id`; there is no cursor.
`skipped_unparseable` counts unreadable originals, one of which may contain a
reply that settled a listed loop. These warnings describe evidence coverage,
rather than source freshness.

A write refused with `loop_scan_incomplete` saves no decision. Wait at least the
daemon's `Retry-After: 1` second, then start a new call and approve it again.
Approval is consumed by an attempted write, and MCP performs no automatic write
retry. Transport interruptions and unclassified write failures can report
`operation_may_have_completed: true`; inspect the archive before deciding what
to do next.

The adapter provides the current general list and decision receipts. Ranked
waiting, held, and closed views, detail/history, source freshness reports, mute
controls, decision notes, kind filters, commitment kinds, and cursor paging
belong to the replaced projection and are outside this workflow. Old loop-row
`id` values identify different objects; use the new anchor `message_id`.

## Limits

- A reply settles a loop when it is archived in the same conversation. If
  Alex answers an address of another account of yours, that reply sits in
  that account's conversation, and the loop stays until you run
  `msgvault loops done`.
- Each request checks up to 1,000 uncached messages, starting with the eligible conversation whose latest activity is oldest. It saves body classifications and MIME facts so another request continues farther through history. Message text stays in the archive's existing body storage.
- When that budget runs out, the response sets `coverage_incomplete` and includes only conversations checked in full. Returned loops sort oldest-owed first among those checked conversations. Run `msgvault loops list` or issue another API request to continue; the Directory card has a Check more button.
- A very large archive can still exceed the API's default 60-second request budget and return HTTP 500 (`internal_error`). Successful evidence writes remain available for the next request. A smaller `limit` may still scan the same history.
- Done decisions remain attached to the exact questions marked; snoozed questions return after expiry.
- A direct chat has no stored original to check, so the automatic-reply and
  bulk-mail rules don't apply. A bot or short-code sender that asks something
  opens a loop, and `msgvault loops done` clears it.
- Email threads and direct chats only. Group chats, channels, and meetings
  are skipped.

See the [CLI reference](../cli-reference.md#loops) for flags and the
[HTTP API](../api-server.md#open-loops) for endpoints. Loop routes are
owner-only: delegated agent tokens cannot reach them.
