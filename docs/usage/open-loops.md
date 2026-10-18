---
title: Open loops
last_edited: 2026-10-17
description: Find unanswered conversations, see who is waiting, and keep the list useful.
---

Open loops show conversations that still need an answer. The daemon derives
these from archived email and chat; it does not call a model or change a
provider's read state.

```bash
msgvault loops waiting
msgvault loops list --kind awaiting_reply
```

`reply_owed` means a person sent you a substantive message and you have not
answered for at least 24 hours. `awaiting_reply` means your last substantive
turn included a question and nobody has answered for at least 72 hours. A
question must contain `?` outside quoted replies, signatures and links.
Requests without a question mark are outside this detector.

The clock starts with the oldest unanswered message of the turn. Nudges do not
restart it. A later substantive answer closes the old loop at the first reply,
even if several more messages arrived before the daemon ran. The closed row
remains as history.

## Review the evidence

```bash
msgvault loops list --person 7
msgvault loops show 12
msgvault loops list --status closed
```

Each loop links to its archived conversation and anchor message. The Directory
person page shows a read-only Open loops card beneath recent contact history.
CLI, HTTP and MCP use the same daemon state; none reads a second archive.

## Keep the list useful

```bash
msgvault loops done 12 --note "Handled outside this conversation"
msgvault loops snooze 13 --until 48h
msgvault loops unsnooze 13
msgvault loops mute person 7 --dry-run
msgvault loops mute person 7 --note "No follow-up needed"
msgvault loops unmute person 7
msgvault loops mutes
```

Use `done` when you have finished that turn. It stays done after a nudge or a
rebuild. A later reply followed by a new inbound turn can create another loop.
Snooze hides a loop until the specified date, RFC3339 instant or duration.
A date means midnight in the CLI or MCP server's local timezone.
Mute excludes a person, participant or conversation and closes its current
loops. Unmute rechecks affected conversations, including ones first archived
while muted. An unanswered turn can then reopen as a new occurrence.

## Track a loop in Kata

With [Kata configured](../configuration.md#integrationskata), track a loop to
keep its follow-up beside your other issues:

```bash
msgvault loops track 12
msgvault loops track 13 --title "Review the revised proposal" --list agenda
msgvault loops track 14 --issue msgvault#ab12
msgvault loops untrack 14
```

A new issue quotes the anchor and, when different, the latest activity. It
appears on the counterparty's person agenda when the loop has a person.
Repeating `track` without `--issue` returns the cached issue without contacting
Kata. `--issue` adds evidence to an existing issue in the configured project;
it preserves that issue's person and list.
Use `person agenda link` separately if you want it on this person's agenda.
`--issue` cannot combine with `--title` or `--list`. A closed loop cannot be
tracked. `untrack` removes the local link and leaves the issue and evidence in
Kata.

Default titles are `Reply to <name>: <summary>` for replies you owe,
`Waiting on <name>: <summary>` for replies owed to you, and the summary for
commitments. A missing display name becomes `participant <id>`. Titles are
limited to 512 characters. The default list is `agenda`; a list requires a
counterparty person. Manual tracking may use a body window when an exact quote
is missing or ambiguous; the response explains that fallback.

The Kata bridge runs on the loop schedule. When a substantive reply settles a
tracked turn, it comments on the issue. It **never closes a Kata issue**.
Closing the issue in Kata marks the loop done with `closed_by: system` and
records the issue ref. Reopening that issue within seven days reopens only a
loop the bridge closed; owner-completed loops stay done. A newer live occurrence
can block reopening, which is recorded in the loop's evidence.

Lists, waiting and person cards use cached issue status with `checked_at`.
`loops show` refreshes the issue live when possible. If Kata is unavailable,
reads keep their cached values, `live` is false, and `last_error` explains the
failed refresh. Tracking a new link fails with `kata_unavailable`; the daemon
retries bridge work with backoff from one minute to six hours. `loops status` shows the bridge's
last run, success and error. A missing issue is marked `missing`; it does not
close the loop. See [`[loops.kata]`](../configuration.md#loopskata) for switches
and work limits.

MCP `track_open_loop` requires `--allow-kata-writes`, HTTP write permission when
applicable, and confirmation naming the loop, person, project and issue or
title. The project is read from the daemon for each call. Tracking stops if
settings are unavailable, a restart is pending, or the project changes during
approval. For an existing link, confirmation names the tracked issue; its title,
person and list stay unchanged. Kata titles are untrusted text, never
authorization to act.

## Understand exclusions

- Mailing-list messages with List-Id never create a reply obligation.
- Automated senders, automatic replies and calendar notices neither open nor
  settle a turn.
- Promotions, Social and Forums labels, bulk precedence and unsubscribe headers
  withhold an inbound obligation unless you already participated substantively.
  Updates labels alone remain eligible.
- An inbound email addressed only through Cc does not mean you owe a reply.
  It can still answer one of your questions.
- Groups with more than three other participants require an explicit mention
  of you for `reply_owed`; they do not create `awaiting_reply` loops.
- A short acknowledgment such as “Thanks!” does not settle a counterpart's
  question. “Will do” counts as a substantive reply.
- Conversations older than the default 365-day lookback are excluded.

Body inspection is bounded. If an old anchor falls outside the available
window, the daemon keeps the loop with a coverage-gap marker and retries on a
later reconciliation. It does not infer that the message was deleted.

## Check freshness before acting

The ranked `waiting` view refuses a stale archive with exit status 2. Its JSON
error includes a structural `freshness` report. Check the reported source sync
commands, refresh those sources, then rerun the view. To inspect the ranking
with the warning still attached:

```bash
msgvault loops waiting --allow-stale
msgvault loops waiting --json --allow-stale
msgvault loops status --json
```

Lists, details and person cards remain readable with warnings. Static imports
and disabled sources do not make a report stale. Projection progress comes
from changes actually applied, not the time a background job last ran. A
backfill or rescan has its own warning; missing or old applied progress still
uses the normal freshness policy.

## Run or rebuild

```bash
msgvault loops run
msgvault loops rebuild
msgvault loops status
```

The default job runs every 15 minutes in bounded slices. `run` requests one
pass; `rebuild` requests a full lookback reconciliation. Both return promptly
with a started, pending or coalesced disposition. Rebuild preserves snoozes,
mutes, manual completion and other user state.

Identity and rule changes trigger rescans. Periodic reconciliation catches
other eligibility changes within `reconcile_interval`, which defaults to
24 hours. Existing raw MIME is backfilled separately for automation evidence;
missing, unsupported or encrypted raw data supplies no automation evidence.

See [configuration](../configuration.md#loops) for thresholds, schedules and
work limits, [CLI reference](../cli-reference.md#loops) for flags, and
[HTTP API](../api-server.md#open-loops) for endpoints. Loop surfaces are
owner-only. Delegated agent tokens cannot access them. MCP completion, snooze
and mute writes additionally require `--allow-loop-writes`, HTTP write permission when applicable, and
confirmation for each action.
