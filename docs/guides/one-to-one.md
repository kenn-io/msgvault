---
last_edited: 2026-11-06
title: One-to-one continuity
description: Compare your prior meeting's actions with later evidence before the next one-to-one.
---

Install the agent skills, then ask your terminal agent to prepare a recurring
one-to-one:

```bash
msgvault skills install
```

> Use msgvault-one-to-one to prepare my next one-to-one with Alex Example.
> Review as of 2026-11-06 12:00 UTC. Compare what we agreed at our previous
> meeting with what happened, and include unresolved feedback and personal
> follow-up questions.

The skill reads your saved person, archived meetings and messages, existing
briefs and live Kata agenda. It produces a cited action/outcome table,
unresolved feedback and blockers, fresh agenda items and personal questions.
It does not generate a brief, create tasks or reminders, or send messages.
You can also provide a specific prior meeting ID or confirmed conversation date.

## What counts as the previous meeting

The agent looks for primary notes or transcript that establish an actual
one-to-one with you and this person. Invitations, participant lists and a later
group meeting do not establish that meeting. The review names its date, time
basis, source and coverage limits. When the archive cannot establish a prior
meeting, the agent shows current context separately and asks for a boundary
when needed. It does not invent a comparison window.

Metadata discovery covers all linked participant IDs, including phone senders,
CC recipients and conversation members. Only bounded relevant source content
is read. The default history search is 90 days, with limits of 200 metadata rows,
10 meeting reads and 30 other messages. A separate older-history pass looks
for feedback and personal followups that were not repeated at the last meeting.
Truncation, unread sources, undated records and cache changes remain explicit gaps.

## How outcomes are supported

Each prior action retains its meeting/action citation or explicitly linked
Kata task. Later evidence distinguishes documented completion, reported
completion, cancellation, a changed agreement, a blocker, an open action and an
unknown outcome. Acknowledged feedback stays unresolved until evidence shows
the requested change. Personal followups become questions grounded in dated
direct evidence, with later changes taken into account.

Meeting actions are the currently archived record of the prior meeting.
Reimport can change their wording, ordering or source status; this is not a
frozen historical ledger. A source status has no status-change timestamp.
Disappearance from the open agenda, a due date or silence does not prove an
action was completed. A retrospective review excludes evidence after its cutoff
and marks current state without a historical timestamp as uncertain.

For known closed Kata tasks, the workflow can read the existing issue-scoped
native event API with Python 3. It requires the same verified service and
narrow credentials already configured for native Kata. It selects the native
effective closure and its evidence, checks for concurrent task changes and
refuses redirects or broader history reads. If that route or capability is
unavailable, the review reports the missing task evidence and continues with
archive sources. Evidence strings are read as data, never executed.

The workflow needs only merged msgvault readers. It can supplement a meeting
card when that workflow is available, without requiring the pending card,
observation, timeline or loop CLI features. See [meeting evidence](../usage/meetings.md),
[saved briefs](../usage/people-briefs.md) and [agent skills](agent-skills.md).
