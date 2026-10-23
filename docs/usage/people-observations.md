---
last_edited: "2026-10-22"
title: Person observations
description: Record statements about a person and curate them without changing their profile fields.
---

Record something you want to remember about a saved person, then confirm,
pin, end, or remove it later. Owner-authored observations use your archive
without a model call or a person tracking requirement. They work for a saved
person with no linked messages or participants.

Adding a statement confirms it, including when it adopts an existing automatic
statement with the same content key.

Observations are separate statements. They never update profile fields or
become person fact claims. See [People and profiles](people.md) for contact
details and [profile automation](people-automation.md) for automatic fields.

## Record and read a statement

Use a durable person ID. Write `[PERSON]` for the person and `[OWNER]` for
yourself. These placeholders keep the stored statement independent of later
name changes. Their spelling is case-insensitive on input.

```bash
msgvault person observations add 42 '[PERSON] prefers morning calls'
msgvault person observations list 42
msgvault person observations show observation:17 --json
```

The content must contain `[PERSON]` and have 1–500 characters. `--subject-ref`
adds a subject label of at most 64 characters. A statement can also have:

- `--kind`: `state`, `event`, `plan`, `preference`, `relationship`, `pattern`,
  or `other`. The default is `state`.
- `--stability`: `fixed`, `durable`, `volatile`, or `episodic`. Dates can
  make the stored stability more volatile.
- `--valid-from` and `--valid-until`: RFC3339 timestamps.
- `--occurs-on`: a calendar date, month, or year.
- `--category`: a sensitive category; repeat the flag to add several.

The program also detects sensitive categories from a closed English lexicon.
Lists and individual reads hide sensitive statements by default. Add
`--include-sensitive` to include them. Every subcommand supports `--json`.
List filters include `--level`, `--status`, `--kind`, `--as-of`, `--limit`
and `--offset`. `--as-of` filters the statement's valid interval. The default
limit is 100 and the maximum is 500.

An observation reference is `observation:<id>` or a bare positive ID.

## Curate a statement

```bash
msgvault person observations decide observation:17 confirm
msgvault person observations decide observation:17 pin
msgvault person observations decide observation:17 end --end-at 2026-10-22T12:00:00Z
msgvault person observations decide observation:17 reject --reason wrong
```

| Action | Result |
|---|---|
| `confirm` | Record your confirmation |
| `unsure` | Record uncertainty |
| `clear` | Clear confirmation or uncertainty |
| `pin`, `unpin` | Set or clear the pin |
| `end` | End the valid interval and supersede the statement |
| `reject` | Reject the statement and suppress the same content key |
| `forget` | Delete the statement and its children; keep a hash-only suppression |

The CLI reads the current update timestamp before making a decision. A
concurrent change returns a conflict. For a previously inspected revision,
supply `--expected-updated-at` with that statement's `updated_at` timestamp.
Read it again before retrying a stale decision.

A pin or confirmation protects a statement from automatic withdrawal. If its
support disappears, `support_flag` becomes `support_lapsed`. Dependent
statements stop treating it as supported. Ending, rejecting, or forgetting a
premise also rechecks its dependents.

## Manage suppression

```bash
msgvault person observations suppressions 42 --json
msgvault person observations suppressions clear 42 <content-key>
```

A suppression retains a normalized content hash, reason, actor and timestamps.
Forgetting removes the statement text, citations, categories, premise edges
and its event history from the observation store. Earlier merge snapshots
retain their existing recovery data. It does not delete the underlying archived message.

Re-adding the statement yourself clears its active suppression and records
`suppression-lifted` with `owner-readded`. Automatic writes remain suppressed
unless an `outdated` suppression receives enough new support. `wrong` and
`not_useful` do not lift automatically.

## Use the owner API

These routes require owner authentication. Delegated tokens receive HTTP 401
`unauthorized`.

| Method | Route |
|---|---|
| GET | `/api/v1/people/{id}/observations` |
| GET | `/api/v1/observations/{id}` |
| POST | `/api/v1/people/{id}/observations` |
| POST | `/api/v1/observations/{id}/decisions` |
| GET | `/api/v1/people/{id}/observation-suppressions` |
| DELETE | `/api/v1/people/{id}/observation-suppressions/{key}` |

Reads return `Cache-Control: no-store`. Pass `include_sensitive=true` to list
or retrieve sensitive statements. A hidden or missing individual statement
returns 404. Decision bodies require `action` and `expected_updated_at`; a
stale timestamp returns 409. Creation returns 201 and clearing a suppression
returns 204. API schema 3.25.0 describes request and response fields.
