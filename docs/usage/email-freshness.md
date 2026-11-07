---
last_edited: "2026-11-06"
title: Keep Email Current
description: Enable optional rapid email polling and understand credentials, coverage, retries, and acquisition status.
---

Check an existing email account more often while retaining its cron recovery
schedule. Rapid polling reuses Gmail API, Microsoft Graph mail, or IMAP sync
with the account's existing credentials and saved recovery progress.

**Unreleased branch behavior:** this guide describes work under implementation.
Runtime verification and live-provider acceptance are pending. Source inspection
and synthetic transport tests do not establish compatibility with a live
provider or a delivery-time guarantee.

## Enable polling for one account

1. Register the account using the existing [setup](../setup.md) or
   [OAuth setup](../guides/oauth-setup.md) workflow. Polling does not register
   accounts or obtain new credentials.
2. Run `msgvault list-accounts`. Find the intended active row with type
   `gmail`, `imap`, or `msmail`. Copy its exact **ACCOUNT** value, rather than
   its ID, alias, or display name. IMAP uses the stored connection identifier,
   such as `imaps://sample@example.test@imap.example.test:993`.
3. Edit that account's existing `[[accounts]]` entry in the daemon's
   `config.toml`. For a synthetic Gmail or Graph account:

   ```toml
   [[accounts]]
   email = "sample@example.test"
   schedule = "*/15 * * * *"
   enabled = true
   poll_interval = "30s"
   ```

   For IMAP, replace `email` with the exact stored IMAP **ACCOUNT** value.
   Keep one enabled entry for the key. The cron schedule remains an
   independent backstop. The [configuration reference](../configuration.md#rapid-email-polling-unreleased)
   owns interval limits, validation, and account-selection rules.
4. Restart the daemon through its supervisor, or use
   `msgvault daemon restart` for a daemon managed by msgvault. Polling changes
   take effect on restart; changing a cron schedule through the API does not
   set the polling interval.

Startup queues catchup. Later timer ticks share the existing scheduled-work
queue with cron and manual scheduler requests. A busy account coalesces
requests instead of running overlapping passes. Removing `poll_interval` or
setting it to `"0s"`, then restarting, returns the account to cron-only sync.

## Choose the existing provider connection

These are transport paths, subject to the provider accepting the account's
credentials and policy. They are not a live-provider certification list.

| Account family | Polling path | Credentials and limits |
|---|---|---|
| Gmail / Google Workspace | Gmail API | Existing API OAuth, including a read-only grant, or a configured delegated Workspace service account. The authenticated Gmail profile must match the selected source before ingestion writes. No Pub/Sub setup is required. |
| Outlook.com / Microsoft 365 | Graph mail (`msmail`) or Microsoft IMAP (`imap`) | Existing Graph mail OAuth or the separate Microsoft IMAP OAuth flow. App registration, tenant consent, and mailbox access policy still apply. Graph checks `/me` through the ingestion client before archive mutations. |
| iCloud | Explicitly configured compatible IMAP | Existing working password/app-password credentials; Apple account security requirements still apply. |
| Yahoo | Explicitly configured compatible IMAP | Existing working password/app-password credentials where allowed. There is no native Yahoo IMAP OAuth manager. |
| Fastmail, Zoho, PurelyMail, Strato, other IMAP | Explicitly configured compatible IMAP | Supply the correct host, transport, username, and working provider-approved credentials. Plan restrictions, enabled IMAP access, quotas, and server behavior still apply. |

Use the existing [Google](../guides/oauth-setup.md#google-gmail-and-calendar),
[Microsoft](../guides/oauth-setup.md#microsoft-365-outlook-hotmail), and
[IMAP](../setup.md#add-an-imap-account) setup guides for credential setup.
Polling does not widen a read-only Gmail grant. Google API scopes do not grant
IMAP access, and this path adds no Gmail IMAP OAuth manager. Graph credentials
are separate from Microsoft IMAP credentials; neither token substitutes for
the other. Generic IMAP support does not supply provider-specific OAuth or
automatic host discovery. There is no special Strato IMAP4rev2 switch in this
path.

## Understand coverage and cost

Gmail uses saved History and existing recovery to reconcile mailbox changes,
including direct-to-archive arrivals, Sent, labels, read/star changes, and
deletions. Graph uses saved folder deltas and pending-fetch recovery. Its
provider unread and follow-up observations are separate from the archive
reader's local `is_read` state. Provider state is also separate from editable
Outlook categories.

Rapid IMAP enumerates all selectable folders, including newly discovered and
non-INBOX folders. Each rapid pass forces full metadata enumeration and
deliberately bypasses QRESYNC shortcuts, so flag-only changes can be observed
even when message counts and UIDNEXT do not change. Known archived messages
use metadata reads for flag refresh; new content and uncertain message
identities can still require content fetches. This can consume substantial
metadata traffic and time on large accounts, including servers that support
QRESYNC. The ordinary [IMAP sync guide](imap.md) describes the usual incremental
behavior and server command limits.

Rapid IMAP requires unfiltered coverage. Saved folder selections and effective
folder/date filters, including configured skipped folders, reject rapid mode
before ingestion. Remove the filters only if you want all selectable folders;
otherwise disable polling and retain ordinary filtered sync. The existing
[filtering guide](imap.md#what-filtering-changes) owns ordinary filtered-sync
behavior.

A `30s` interval is a request cadence, not a promise to finish within 30 seconds.
Provider changes, queueing behind other work, full metadata scans, recovery,
and backoff can all delay completion. Failed or incomplete acquisition remains
retryable using the existing saved cursors and recovery debt. Embedding and
search-index work runs separately after acquisition.

## Read status and handle delays

Use the existing authenticated
[`GET /api/v1/scheduler/status`](../api-server.md#get-apiv1schedulerstatus)
route. In this branch, entries under `accounts` expose the polling fields
below alongside the existing `last_error`:

| Field | Meaning |
|---|---|
| `poll_interval` | Configured interval for an opted-in account |
| `poll_state` | Current scheduler state: `waiting`, `queued`, `polling`, `deferred`, `degraded`, or `yielded` |
| `last_attempted` | Most recent acquisition start after admission to the work gate |
| `last_completed` | Most recent completed acquisition, excluding incomplete, failed, or interrupted work |
| `next_eligible_poll` | Earliest eligible polling retry; queueing can delay actual execution |
| `last_error` | Acquisition failure or remediation message, when present |

`next_run` remains the cron schedule's next run. Acquisition timestamps and
retry eligibility are held in memory and reset on restart; saved provider
cursors and recovery debt persist. Before a qualifying run, timestamps can be
absent. `last_completed` records acquisition completion, not embedding or
search readiness, and does not prove there are no later provider changes.

`queued` means waiting for the shared work gate. `polling` means an acquisition
is running and can also describe a cron or manual scheduler run for an opted-in
account. `yielded` means work was interrupted. `deferred` and `degraded` indicate
that acquisition must wait or retry. These are polling states, not proof of a
connected notification listener.

Failures increase the polling delay with exponential backoff and bounded
jitter, up to 15 minutes. A provider's retry floor can be longer and applies
to every trigger origin. Manual scheduler and cron requests can bypass the
adaptive polling delay, but cannot bypass a known provider retry floor.

If acquisition stays degraded, inspect `last_error` and the selected account:

- For a canonical-target error, compare the configured key with the exact
  **ACCOUNT** value and type in `msgvault list-accounts`. Missing, merged, or
  ambiguous targets have no alias/display-name fallback or cross-provider
  fanout.
- For a credential error, repair or reauthorize the selected account through
  its existing setup workflow. Check Microsoft tenant/IMAP policy and the
  selected Google OAuth app or delegated service-account setup as applicable.
  Polling does not open a browser to repair credentials.
- For filtered IMAP coverage, disable polling or remove the effective filters,
  then restart. Ordinary filtered sync remains available.
- For incomplete acquisition or provider throttling, allow recovery and retry
  to proceed. A shorter interval cannot remove provider limits or queueing.

## Polling and notification listeners

Polling periodically asks the provider for changes. It does not establish an
IMAP IDLE session or Gmail Pub/Sub subscription. The separate, unmerged
[IMAP IDLE](https://github.com/kenn-io/msgvault/pull/1934) and
[Gmail Pub/Sub](https://github.com/kenn-io/msgvault/pull/1935) work is outside
this path; its configuration is not a prerequisite or a current-release
capability described by this guide.
