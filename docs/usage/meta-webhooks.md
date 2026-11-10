---
last_edited: 2026-11-10
title: Meta Webhooks
description: Receive signed Facebook Page events, inspect delivery health, and replay retained deliveries.
---

Receive Facebook Page comment changes and new Page mentions without waiting
for the next polling scan. This feature is unreleased. The daemon verifies each
delivery and saves its original bytes before acknowledging it. A worker then
archives Page `feed` comments and new `mention` posts/comments through the
same persistence path as polling. Mentions become searchable in the existing
Facebook archive.

Comments keep their conversation, parent, and participant links. For `feed`
events, edits update the body; hide and unhide update metadata; removal leaves
a tombstone and preserves the archived row. Older webhook events cannot undo
newer webhook body or state changes. Equal timestamps apply in arrival order. Automatic
retries keep their original order; explicit replay gets a new order. Older
adds can fill missing author and creation time without replacing a newer body.
Polling remains the backfill and reconciliation path, with its own checkpoints.

## Connect the receiver

1. [Configure and register a Facebook Page](facebook-comments.md#connect-a-page).
   Enable its profile so the worker can route events to it. The Page must
   already be registered in the archive.
2. Add the [receiver configuration](../configuration.md#meta-webhooks). These
   IDs are synthetic placeholders:

   ```toml
   [meta_webhooks]
   listen_addr = "127.0.0.1:8791"

   [[meta_webhooks.apps]]
   app_id = "9"
   app_secret_env = "EXAMPLE_META_APP_SECRET"
   verify_token_env = "EXAMPLE_META_VERIFY_TOKEN"
   ```

3. Put the app secret and a chosen verify token in the named environment
   variables on the daemon host, then restart `msgvault serve`. Credentials
   are read only from these names. Missing variables disable the receiver;
   status names the missing variable without showing its value.
4. Configure your HTTPS proxy to forward
   `https://<public-host>/meta/webhook/<app_id>` to the listener at the same
   path. Preserve the raw request body and `X-Hub-Signature-256` header.
   Bind loopback when the proxy runs on the same host. Exposing this URL and
   obtaining a trusted TLS certificate are operator tasks.
5. In the Meta App Dashboard, enter that callback URL and the chosen verify
   token, and select the app-level webhook fields. Install the Page fields:

   ```bash
   msgvault meta-webhooks subscribe example-page --fields feed,mention
   msgvault meta-webhooks subscribe example-page --fields feed,mention --check
   msgvault meta-webhooks status
   ```

Subscription needs a Page token issued to the configured app, with
`pages_manage_metadata` and `pages_show_list` from a person with a MODERATE
task on the Page. The command verifies the token's app first and refuses any
mismatch or unverifiable result. It reads existing fields, installs their
union with your request, and reads them again to confirm. It never removes
an existing field. If a partial app listing omits the selected app, the command
refuses to change subscriptions. `--check` changes nothing and fails if the app
is absent or any requested field is missing.

Select a profile by its identifier. If the same identifier belongs to more than
one source kind, qualify it, for example `facebook_comments:example-page`.
Without an identifier, the command checks or subscribes all configured Page
profiles, including profiles whose polling is disabled.

App mode, access levels, Page tasks, and grants still determine live delivery
and ancestor reads. Configuration alone does not grant access. Instagram
fields are selected in the dashboard; this command manages Page fields only.

## Capture new Page mentions

The `mention` subscription captures newly delivered post and comment mentions
of the selected Page in threads its token can read, including other Pages'
posts. It does not discover historical mentions. Supported post items (`post`, `status`, `photo`,
`video`, `share`, and `link`) and comments are archived on `add` only. Other
mention lifecycle verbs are ignored: removing a mention does not establish
that its host post or comment was deleted.

When text is absent, null, or empty, the worker reads the target once for text.
Newer mentions update archived text, including text read from the node; older
deliveries cannot replace it.

Mentions in another Page's thread require that thread to be readable by the Page
token. An unreadable required target or ancestor remains retryable; retained
deliveries can be replayed after access is fixed.

## Hold other event types

Page `feed` comments and supported new `mention` posts/comments are applied.
Messenger messages and Instagram comments, messages, and mentions are **held
for a future release**.
The whole delivery is retained when it includes an unhandled item, even if its
comments were applied. Replay can therefore apply those comments again without
duplicating archive rows. An equal-time replay can temporarily replace a body
refreshed by polling; the next poll reconciles it. The ordering guard compares
webhook times, so it does not preserve the latest polled body in every replay.

Messenger fields require explicit `--fields` selection and `pages_messaging`.
Before subscribing, confirm which app is the primary receiver and how
conversation routing affects secondary apps. msgvault does not change that
routing or take control of conversations. Existing fields remain subscribed
even when a later command requests only `feed`.

## Inspect and replay deliveries

After upgrading a receiver that held Page mentions as unhandled, preview and
replay those retained deliveries with the commands below.

```bash
msgvault meta-webhooks status --json
msgvault meta-webhooks replay --state held --app 9 --dry-run
msgvault meta-webhooks replay --state held --app 9
```

Status reads files on disk and makes no Graph call. It reports listener health,
delivery counts and oldest pending age, held item categories, bytes against
the spool cap, counters since startup, and each Page's last completed polling
run. A missing snapshot or one older than two minutes is reported as
`daemon not running or no snapshot`. Diagnostic conditions keep exit status 0.

Replay defaults to held and failed deliveries. Filter by app, delivery ID, or
receipt time with `--since <RFC3339>`; use `--state done` to replay completed
deliveries. The CLI files a request and waits up to 45 seconds for the daemon
worker's count. Only the worker moves deliveries, resets attempts, and applies
events. A timeout reports that the daemon or worker may be unavailable; the
request remains on disk and may execute after recovery.

Pending work retries from 30 seconds up to one hour between attempts. After
12 counted failures it becomes failed. A busy source lock does not count as a
failed attempt. When a request budget interrupts a delivery, the next pass
starts at the first deferred item. Failures count after a full pass through
the items, so earlier failures cannot starve later events. Fix missing
credentials or permissions before replaying failed deliveries. If Meta disables a persistently failing callback, fix the receiver
and run the subscription command again; local replay cannot recover deliveries
Meta never sent or stopped retrying.

These are owner administration commands run through the daemon. Delegated
grants cannot subscribe, inspect receiver status, or replay deliveries.

## Protect the spool

Deliveries live under `<data_dir>/meta-webhooks/<app_id>/` in `pending`, `held`,
`failed`, or `done`. Directories are private (0700) and files are private (0600).
The spool contains raw message content when Messenger fields are subscribed,
and may contain broadcast subscription tokens when those fields are enabled.
Treat it with the same care as the archive.

The spool sits **outside `msgvault backup`**. Include it in your own protected
filesystem backups if you need to restore accepted deliveries. Completed
deliveries are pruned after 720 hours by default. Held and failed deliveries
are never pruned automatically and count toward the default 1 GiB cap. At
capacity the receiver returns 503 so Meta can retry. Oversized bodies return
413; the default body cap is 8 MiB. Monitor status and resolve backlogs.

Newer webhook metadata includes ordering and hidden-state fields. Polling leaves
visibility unknown; a hide or unhide event records an explicit `hidden` value.
An older binary may discard those fields when rewriting a row. Keep a backup before
downgrading and reconcile the provider state after upgrading again.

On Windows, delivery files are flushed before publication, but directory flushes
are unavailable. The receiver cannot promise the same power-loss durability as
on systems that support directory `fsync`.
