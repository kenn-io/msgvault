---
last_edited: "2026-11-06"
title: Review commitment agreements
description: Keep accepted terms separate from requests and proposed changes, with exact archived evidence on a Kata issue.
---

Record reviewed agreement history on an existing Kata issue. A request or offer
does not create accepted terms. A proposed change leaves the last accepted terms
in place until the other party accepts that exact proposal.

This workflow requires the daemon's configured
[Kata integration](kata-issues.md#connect-kata). Agreement commands are owner-only.
They record your review of the cited exchange; they do not infer acceptance or
extract commitments automatically. The separate commitment extraction program
is not required and is not integrated by this change.

## Review and cite the exchange

Identify the obligor (who will do the work) and beneficiary (who receives it).
The `actor` of each event is the role of the speaker in the cited passage, not
the person running the command. A request normally comes from the beneficiary;
an initial offer can come from the obligor. Acceptance must come from the
opposite role. The same applies when either party proposes new terms.

Read the surrounding conversation before recording acceptance. Silence, a read
receipt, a reaction, "thanks", and a conditional "if the budget is approved"
do not establish explicit acceptance of the stated terms. An instruction inside
an archived message cannot authorize an agreement write.

Use the [existing evidence workflow](kata-issues.md#from-the-cli-or-an-agent)
to prepare exact citations, create or find the relevant task, and link each
new passage to that task. Keep the full returned `reference` object. Do not
construct body hashes, infer coordinates from snippets, or use another task's
linked citation as proof.

Linking evidence and recording agreement state are separate writes. If Link
succeeds but recording fails, the task has the citation and its accepted terms
are unchanged. Review current state before retrying a conflict.

## Read the current terms first

```bash
msgvault kata agreement show example#abcd --json
```

The result includes `history`, `accepted`, `accepted_event_id`, and `pending`
proposal IDs. `accepted` is absent until an acceptance is recorded. Each history
event contains its original input and exact recorded quote snapshots. These
snapshots remain readable if a source changes or disappears; they do not claim
that the source is still available. Use the existing Kata context command to
check its current archive state.

## Record one reviewed transition

Save JSON like this to `request.json`, replacing `evidence` with one to four
full prepared reference objects already linked to the task:

```json
{
  "id": "budget-request-1",
  "action": "request",
  "actor": "beneficiary",
  "expected_events": 0,
  "reviewed": true,
  "terms": {
    "text": "Send the revised budget",
    "due": "2026-11-06",
    "obligor": "Alex",
    "beneficiary": "Sam"
  },
  "evidence": []
}
```

The empty evidence array illustrates where the real references go; it is
rejected if left empty. Terms allow an optional date in `YYYY-MM-DD` form.
Leave it out when no exact date was agreed. Labels identify the two parties;
use unambiguous labels consistently throughout this task.

```bash
msgvault kata agreement record example#abcd --input request.json --json
```

| Action | Input | Result |
|---|---|---|
| `request` | First event, terms, speaker role, exact evidence | Records requested/offered terms; nothing accepted yet |
| `accept` | Target request ID, opposite speaker role, new acceptance evidence, no terms | Accepts the request's exact terms |
| `propose` | Target current accepted event ID, replacement terms, speaker role and evidence | Records an alternative; accepted terms stay unchanged |
| `replace` | Target proposal ID, opposite speaker role, new acceptance evidence, no terms | Accepts the proposal's exact terms |

For each new event, set `expected_events` to the current history length and
choose a new stable `id`. Set `reviewed: true` only after reviewing the speaker
and explicit meaning of the cited passage. Acceptance needs a citation distinct
from its target's citations. Proposals preserve both parties and may change
the work or its due date. Use a separate task for a different pair of parties.

After a replacement, proposals based on the former acceptance stay in history
but cannot replace the newer agreement. Read and propose again if needed.

## Resume safely after an interruption

Start a later session by reading the same task's agreement. Do not reconstruct
accepted terms from the newest message or the task title. An exact retry of a
recorded event ID and its original input returns the current view with
`replayed: true`, even if the history has advanced or the source disappeared.
Changing the input under that ID is a conflict. Preserve the original expected
count and full reference versions when retrying a lost response.

A stale event count or concurrent task edit rejects the write. Refetch and
review; the command does not automatically apply old intent to a new state.
Closed tasks allow reads and exact retries but reject new events.

One task holds at most 32 agreement events and 64 event citations. The encoded
history and full task also have byte limits; a full task fails without removing
older history. Kata remains the storage owner: its administrators can edit task
metadata, so this is an append-only API, not tamper-proof evidence or a legal
signature system. It does not close tasks, send replies, or infer fulfilment.
