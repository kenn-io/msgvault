---
last_edited: "2026-10-23"
title: Standing Instructions
description: Keep a person's stated communication preferences with their profile and review changes as the owner.
---

Keep a person's stated communication preferences with their profile. For example,
record “call me Vee” or “no calls before 9am.” An instruction describes what the
person asked for. An owner's rule about managing their own work belongs elsewhere.

Instructions narrow how you communicate. They never authorize sending, adding
recipients, forwarding, or using another address. A delivery policy must authorize
any send separately. The validator rejects delivery-expanding language and unrelated
addresses or phone numbers.

## Record an instruction

```bash
msgvault person instructions add 42 --kind address_as --text 'Call me Vee' --value Vee
msgvault person instructions add 42 --kind timing --text 'No calls before 9am'
msgvault person instructions add 42 --kind avoid_address --text 'Do not use this address' --contact-point 17
msgvault person instructions list 42
```

The person and contact point must already exist. A scoped contact point must be
current and belong to that person. Instructions also work for people with no
linked archive participants.

Choose one of `address_as`, `prefer_channel`, `avoid_channel`, `avoid_address`,
`timing`, `format`, `topic`, or `other`. `address_as` requires a value. Channel
instructions require `email`, `phone`, `sms`, `chat`, or `in_person` as their
value. `avoid_address` requires `--contact-point`.

Keep text to one statement of at most 280 characters. Values may contain up to
64 characters. The ledger normalizes text for identity: capitalization,
compatibility characters, extra whitespace, and trailing punctuation do not
create a distinct instruction.

Adding an `address_as` or `prefer_channel` instruction supersedes the previous
active instruction of that kind within the same contact scope. Adding the same
live instruction twice returns a conflict. After revocation or rejection, the
same text may be added as a new instruction with its own history.

## Review or change an instruction

```bash
msgvault person instructions review --limit 50
msgvault person instructions confirm 123 --text 'Call me Vale' --value Vale
msgvault person instructions reject 123 --reason 'Incorrect statement'
msgvault person instructions unsure 123 --reason 'Needs checking'
msgvault person instructions revoke 124 --reason 'No longer applies'
msgvault person instructions adopt 125 --reason 'Keep this preference'
```

Only the owner can use these commands or their HTTP endpoints. Delegated tokens
receive `401 unauthorized` for all instruction operations, including reads.
The review table shows the person ID and scope: `person` means the whole person;
a contact scope shows its address kind and address.
The CLI reads the instruction's current revision before submitting a decision.
A concurrent change returns `409 instruction_conflict`; inspect the instruction
again before retrying.

Confirmation can edit kind, text, and value. Confirmation and adoption can return
suggestions such as saving a nickname or selecting a primary channel. The CLI
prints those suggestions and never applies them. A suspended instruction becomes
active only through owner confirmation or adoption. Confirmation accepts
proposed or suspended rows, and active rows flagged for review. It cannot
reactivate revoked, rejected, superseded, or expired history.

Owner-entered instructions become active immediately. Extraction proposals,
their evidence links, and background expiry handling arrive with the observation
program; the instruction ledger does not produce them yet.

## Expiry and history

Use `--until YYYY-MM-DD` for a temporary instruction. The date is interpreted at
midnight UTC, must be in the future, and may be no more than 183 days away.
Expired instructions disappear from the default list when read, even without a
background worker. The stored lifecycle status can still be `active`. Adding the
same instruction again, or confirming an edited proposal into its key, records an expiry event
for the ended active row before using that key. The old row remains in history.

Use `list 42 --all` to include every lifecycle status and expired rows. Add
`--json` to any instruction command for structured output. Owner decisions and
merge or split changes leave events in the ledger.

## Merges and splits

A merge carries owner-entered and owner-confirmed instructions to the surviving
person. Matching instructions combine under one identity, with owner-entered
lifecycle data taking precedence. Their events remain linked. Conflicting active
single-valued instructions stay active and enter the review queue.

Unconfirmed proposals do not carry over, but rejection history does. Contact
scopes follow an exact matching surviving contact point; a missing scope is
reported for review.

An exact split restores the pre-merge rows and their event links while preserving
later owner decisions. Instructions on the recreated person enter the review
queue with `split`. A partial split leaves instructions on the source person
and reports absorbed-origin rows as ambiguous.

## HTTP API

| Operation | Endpoint |
|---|---|
| List a person's instructions | `GET /api/v1/people/{id}/instructions?status=&include_expired=` |
| Add an owner instruction | `POST /api/v1/people/{id}/instructions` |
| Read an instruction | `GET /api/v1/person-instructions/{id}` |
| Review queue | `GET /api/v1/person-instructions/review?limit=50` |
| Record an owner decision | `POST /api/v1/person-instructions/{id}/decisions` |

An add request accepts `kind`, `text`, `value`, `contact_point_id`, and
`valid_until` (an RFC 3339 timestamp). A decision accepts `action`,
`expected_revision`, `reason`, and an optional `edit` containing `kind`, `text`,
and `value`. Decisions return `instruction` and `follow_ons`.

Invalid instruction content returns `400 instruction_invalid`. Missing people
or instructions return 404. Duplicate live keys, stale revisions, and invalid
lifecycle transitions return `409 instruction_conflict`. An unavailable ledger
returns 503. Responses use `Cache-Control: no-store`.
