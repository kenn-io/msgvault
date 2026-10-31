---
title: X posts and replies
description: Collect public X conversations under a spending limit and send reviewed owner replies.
last_edited: "2026-10-31"
---

Connect your own X account to keep its posts and returned replies and mentions
searchable in msgvault. Review author evidence through the people workflow and
send an explicit text reply from a managed draft.

This functionality is unreleased. Collection depends on X access, endpoint
reach and prepaid credits. It does not establish complete reply trees or
permanent history. Live wire spelling, loopback authorization, billing and reply
eligibility still require verification before release.

## Connect your account

1. Create your own application in the [X Developer Console](https://console.x.com).
   Choose a Native App unless you need a confidential client. Add prepaid credits
   and set a console spending limit.
2. Register the exact callback `http://127.0.0.1:8765/callback`. If choosing another
   port, register that exact callback too. Current acceptance of HTTP loopback
   requires live verification; never substitute a public unencrypted callback.
3. Run `msgvault add-x --client-id <client-id>`. Add `--allow-send` to request
   write scope. For a confidential app, add `--client-type confidential`; enter its secret
   through the masked prompt or `MSGVAULT_X_CLIENT_SECRET` in the local process.
4. Complete authorization. OAuth verifies the numeric owner with one user read. Registration performs
   one more on the selected daemon, so a complete setup uses two disclosed reads.
   The numeric ID selects policy and identifies the source. Registration caches
   the verified handle separately from the source's editable display label.

Authorization stages an owner-only file at
`tokens/.x-staged/x_<user-id>.json`. Registration installs the authoritative
`tokens/x_<user-id>.json` and consumes that exact stage. These files are separate
from the archive and contain rotating refresh tokens.
Registration, sync and sending belong to the selected daemon.

For a headless daemon, use `add-x --headless` for the authorization/copy steps.
Authorize on the browser machine, securely copy the staged credential to the
printed staging location on the daemon host, then run
`msgvault add-x --register --user-id <numeric-id>` there. A remote command does
not upload a workstation credential path. Once transferred, let only the
selected daemon refresh that credential; do not run copies in parallel.

## Choose spending and scheduling

Configure the numeric account with `[[x.accounts]]`; see the
[configuration reference](../configuration.md) for exact fields and units.
The default monthly allowance is $5 for reads and sends together. Zero blocks
billable work. Enable `owned_reads` only after verifying that the account owns
the developer app and qualifies for that endpoint's pricing. Expanded authors,
refreshes and retries also cost money. Prices and reserved amounts are estimates,
not an X invoice; the console cap remains the external spending stop.

Empty and failed responses retain their full reservations while their billing
is unverified. Repeated quiet polls can exhaust the local allowance or bounded
reservation capacity. Further calls then stop. Reauthorization and read-model
acknowledgement do not release these amounts. Resolving this billing uncertainty
is a draft release condition.

A response that exceeds the reserved resource model pauses further read
requests across restarts. Verify the adapter and price assumptions before clearing
that hold with the network-free owner action
`msgvault sync-x --source-id 42 --acknowledge-read-model`. Clearing it preserves
spend and coverage state; the next ordinary sync can resume.

Use the account's normal cron schedule to poll. Disabling that schedule pauses
collection and preserves its state. It does not remove the account or discharge
its outstanding content-refresh/removal obligations. Reauthorizing the same
numeric account keeps its history, checkpoints, spending and send receipts.
Keep different numeric X accounts as separate sources; X sources cannot be
merged. `remove-account` removes that source and its local credential.

## Collect and backfill

```bash
msgvault sync-x --source-id 42
msgvault sync-x --source-id 42 --archive --after 2020-01-01
msgvault sync-x --source-id 42 --archive --after 2020-01-01 --max-spend 25
```

The archive command without a cap is a network-free preview: it shows exact
queries, the frozen interval, the conservative page reservation and an unknown
total. It does not count or sample paid data. A new paid job freezes its upper
bound at execution; later previews and resumes of that job use its stored window.

Providing a cap creates a persistent job. Repeating the command resumes that
job and its remaining lifetime allowance, including after interruption or month
rollover. Raising its cap explicitly increases the same allowance. Use
`--job-id` to select a job and `--new-job` to request a new interval/job; repeating
a completed job does not silently grant a fresh cap. Without `--max-spend`,
selecting an existing job also previews it. To resume paid collection, include
its existing cap or a higher one. An explicit zero cap admits no resource calls.
The account's monthly limit still applies. The scheduler never starts archive
searches.

Paid archive collection stops with `refresh_required` when existing content is
due for refresh or an earlier sweep is incomplete. Run `msgvault sync-x
--source-id 42` without `--archive` first, then resume the archive job. This
preserves monthly allowance for retention checks before historical expansion.
Preview and an explicit zero cap still make no resource requests.

Own-post and mentions progress are independent. Interrupted newest-first walks
resume their frozen window; the complete watermark advances only after all
pages and required item results are persisted. Partial, budget, credential and
retention failures remain visible in source/run status.

Timelines reach at most 3,200 recent own posts and 800 mentions. Saturation or
outages can leave gaps; msgvault does not buy additional searches automatically.
Backfill uses the stored handle and explicit from/to queries across the entire
interval. It cannot promise old-handle history, every bare mention, quote,
nested reply or hidden reply. Unavailable content is not recoverable through
this API. No media downloads, DMs or Web UI reply composer are included.

## Review authors and reply

Authors have stable numeric participant IDs, provider identity evidence and
observed usernames. Renames update evidence for that numeric author. A recycled
handle does not transfer identity or merge people. Use the normal discovery and
review flow; this source adds no external enrichment or automatic cross-network
matching rule.

```bash
msgvault draft-reply 55102 --body 'Thanks for the details.' --json
msgvault draft-get chat-draft-example
msgvault send-reply --draft chat-draft-example --revision 1 --json
msgvault send-reply 55102 --body 'Thanks!' --json
```

Sending requires owner authentication, a matching enabled `[[x.sends]]` entry,
granted `tweet.write`, a current available archived target and sufficient
budget. Enable it on the selected daemon with the numeric ID printed by
`add-x`:

```toml
[[x.sends]]
user_id = "1000000001"
enabled = true
```

The default is disabled. This selects the stable numeric X owner, not a local
source ID or username. See the [configuration reference](../configuration.md#x-sends)
for the exact fields and obsolete `allow_send` handling. The `--allow-send`
setup flag requests write scope; it does not enable this policy.

Direct parent/body mode prints the prepared draft ID and revision on stderr
before it admits a POST. Keep that handle for recovery if stdout is lost. MCP
prepares one managed draft and confirms that same revision.

Review the exact draft revision. The native target version is pinned; edited
or unavailable targets require new review. Every send reserves at least $0.200 conservatively,
including texts containing URLs. X enforces weighted text limits and reply
restrictions. msgvault checks basic text safety and a 16 KiB transport bound;
it does not claim an exact local 280-character validator or guarantee that an
ordinary reply satisfies X's summons rule.

Agent grants may create/edit/delete drafts within existing source-scoped
permissions; they cannot send. Reading a draft body requires `draft.edit`.
`draft.delete` permits metadata-only reads, and `draft.create` alone does not
grant later read access. MCP requires destructive-action confirmation for a new
owner send, showing exact text, target, revision and reservation; clients without
confirmation support must use the CLI.

A definite provider rejection preserves the draft. A new explicit invocation
checks current policy and budget again. A timeout, malformed success or crash
can leave `remote_unknown`. Recover using the returned durable draft ID and
revision:

```bash
msgvault send-reply --draft <returned-draft-id> --revision <returned-revision> --json
```

This reports the existing attempt without another POST. A fresh positional
message/body invocation creates a new draft; use the returned draft form for
recovery. There is no retry override or automatic body-hash adoption.
If you identify the actual remote reply, add `--adopt-post <native-id>` to
that draft command for explicit owner association. A budgeted lookup
must verify its author and exact frozen parent. Adoption sends nothing.

`sent_archive_pending` means X accepted the reply and local archival needs
repair. Submit that same draft ID/revision to repair locally. Durable receipts
survive draft removal and response loss; status and repair remain available when sending is
disabled. No outcome recovery replays an uncertain POST.

## Remove content and recover backups

The owner handles written removal requests within 24 hours; polling is not a
promise to discover every deletion within that time. Read-only source status
shows the last complete refresh sweep, oldest unchecked time, outstanding work
and overdue coverage, even with a paused schedule or revoked credential. A
small budget may be insufficient for the archive's refresh target.

Apply a confirmed local removal without contacting X or waiting for read budget:

```bash
msgvault sync-x --source-id 42 --remove-post 123
```

Removal resolves all known versions and archived occurrences across X account
sources, suppresses future replay, purges content and coordinates
searchable/cache/vector copies. It preserves minimal
IDs/status and supported independent or curated facts. It does not delete the
post on X. Incomplete cleanup remains pending and blocks affected reads until
finished, including after daemon restart.

Existing owner exports and backup snapshots are separate copies. A database
mutation does not retroactively scrub them. Stop publishing affected copies,
remove or replace pre-cleanup exports/backups, and create a fresh snapshot after
local cleanup finishes.

Both old and fresh X snapshots require current authority before restore. Use
this procedure even when the backup predates the archive's first X connection:

1. Pause every X schedule and finish active collection, sending and maintenance
    work. Keep the authoritative archive quiescent through capture and restore.
2. Capture the complete archive-wide record from the authenticated selected
    daemon. The command refuses active source work, pending cleanup or unbound
    source identities. It makes no X request and does not need read allowance:

    ```bash
    msgvault sync-x --export-removals --json > current-x-removals.json
    ```

3. Retain the current head printed on stderr separately through a trusted
    channel. Use that freshly captured value for restoration. Do not calculate
    the expected head from the supplied file, reuse a snapshot's old head, or
    treat a file's own hash as proof of currency.
4. Restore into a new isolated target, without `--overwrite`:

    ```bash
    msgvault backup restore <snapshot-id> --target <new-isolated-directory> \
      --x-removals current-x-removals.json --x-removals-head <current-head>
    ```

    Before publishing the restored database, msgvault checks the complete record
    against that head, archive identity and every restored X source incarnation.
    It also requires the snapshot to retain current spending, pending reservations,
    archive-job allowances and durable send attempts. A snapshot that predates
    this authority is ineligible; create a new snapshot instead. Current
    reply-target verification must also match, so restoration cannot reopen a
    known unverified target for sending. The record contains fingerprints, not
    a replacement spending ledger or reply body.
    It replays local removals in private scratch space and seals the database only
    after cleanup finishes. Aliases learned by sources created after the snapshot
    remain suppressed without recreating those missing account sources. Missing, shortened, malformed or mismatched authority
    refuses publication. Reconnecting a legacy unbound source does not make an
    old unbound snapshot eligible.

5. Rebuild and verify derived data and suppression in isolation before exposing
    readers. Replace any other affected backup/export copies separately.

Offline restore cannot independently discover the latest removal set. Obtaining
its current authenticated head and keeping the source archive quiescent are the
owner's responsibilities. Without that authority, the snapshot is ineligible.
Keep the feature in draft until the bounded live compatibility and operational
removal/restore checks have also been completed.
