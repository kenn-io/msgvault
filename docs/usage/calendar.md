---
last_edited: "2026-10-04"
title: Google Calendar
description: Archive Google Calendar events alongside your email, with full-text and semantic search over meetings, organizers, and attendees.
---

msgvault can archive your Google Calendar events into the same local database as
your email. Events become searchable by keyword (and semantically, when vector
search is enabled), and their organizers and attendees join the same contact
graph as the people you email — so a meeting with `alice@example.com` dedupes
against the messages you exchanged with her.

Calendar sync is **read-only**. The separate event-control commands can change
live events when you opt in to write consent and source permissions. Event
control described below is unreleased functionality.

## Prerequisites

- A Google OAuth client (see [OAuth Setup](/docs/guides/oauth-setup/)), or a
  [Workspace service account](#google-workspace-service-accounts). Browser
  authorization can reuse the `client_secret.json` already configured for Gmail.
- The **Google Calendar API** enabled on that OAuth project. In the
  [Google Cloud Console](https://console.cloud.google.com/), go to
  **APIs & Services > Library**, search for "Google Calendar API", and click
  **Enable**.

## Authorize and register calendars

```bash
msgvault add-calendar you@gmail.com
```

This grants read-only Calendar access (`calendar.readonly`) and registers your
calendars for sync.

!!! warning "Keep both Gmail and Calendar checked"
    If the account already has a Gmail token, re-consent **replaces** the granted
    scopes, so msgvault re-requests Gmail **and** Calendar together. On Google's
    consent screen, keep **both** checked — unchecking Gmail would drop Gmail
    access for that account.

By default only calendars you own or can write to are registered. Add
`--all-calendars` to also include subscribed and holiday calendars (those you can
only read).

| Flag | Description |
|---|---|
| `--all-calendars` | Include reader/freeBusyReader (subscribed, holiday) calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--calendars` | Comma-separated calendar IDs to register |
| `--oauth-app` | Named OAuth app to use |
| `--headless` | Print headless-server setup instructions instead of opening a browser |
| `--write` | Also request `calendar.events` for event control; source write permissions are still required |

## Sync events

```bash
# First run does a full sync and registers calendars; later runs are incremental.
msgvault sync-calendar you@gmail.com

# Force a full re-sync
msgvault sync-calendar you@gmail.com --full

# Include subscribed and holiday calendars
msgvault sync-calendar you@gmail.com --all-calendars

# Bound a full sync to a date range (full sync only)
msgvault sync-calendar you@gmail.com --full --after 2020-01-01 --before 2024-12-31
```

The first run (or `--full`) enumerates and registers calendars and downloads
events. Subsequent runs are incremental, using the Calendar `syncToken` to fetch
only what changed. Interrupted full syncs resume from a checkpoint; pass
`--noresume` to start over.

| Flag | Description |
|---|---|
| `--full` | Force a full sync (ignore stored sync tokens) |
| `--limit` | Max events per calendar (0 = unlimited) |
| `--after` / `--before` | Bound a full sync to a date range (`YYYY-MM-DD`); full sync only |
| `--calendar` | Restrict to specific calendar IDs |
| `--all-calendars` | Include reader/freeBusyReader calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--oauth-app` | Named OAuth app to use |
| `--noresume` | Do not resume an interrupted full sync |

The first argument can be an account email or the `name` of a `[[gcal]]` entry in
`config.toml` (see [Scheduled sync](#scheduled-sync-daemon) below).

## Control events (unreleased)

Authorize writes separately from sync:

```bash
msgvault add-calendar person@example.com --write
```

Re-consent preserves the existing Gmail grant, including a Gmail grant narrowed
to read-only, and other already granted Google scopes. Keep them checked on the
consent screen. Headless setup uses the same steps below with `--write` on both
machines. Workspace service accounts also need the `calendar.events` scope in
their domain-wide delegation grant.

Enable the source and select exact writable calendar IDs in `config.toml`:

```toml
[[gcal]]
email = "person@example.com"
enabled = true
write_calendars = ["team@example.com"]
invite_calendars = ["team@example.com"]
calendar_aliases = { team = "team@example.com" }
```

Restart the daemon after changing its configuration. `--account` chooses the
OAuth account; the positional calendar chooses the event's calendar and
organizer. They may differ. The daemon checks the target's current `accessRole`
from Google before every write. A non-primary calendar must be present with
`owner` or `writer` access; `reader` access cannot create events.

```bash
msgvault calendar create team --account person@example.com \
  --summary "Planning" --from 2026-10-02T09:00:00Z --to 2026-10-02T10:00:00Z \
  --attendees guest@example.com --dry-run --json

# After inspecting the plan, repeat without --dry-run to create the event.
msgvault calendar update team EVENT_ID --account person@example.com \
  --add-attendee another@example.com

msgvault calendar respond team EVENT_ID --account person@example.com --status accepted
msgvault calendar delete team EVENT_ID --account person@example.com
```

Guest notifications default to `sendUpdates=none`. Pass `--send-updates all` or
`externalOnly` to request them. `none` suppresses notifications; it does not
prevent guest or invitation state from changing. Guest changes, RSVP, and edits,
deletes, or moves of events with guests require `invite_calendars` as well as
`write_calendars`. Moving also checks both calendars' roles and permissions.
`respond` changes only the attendee marked as self by Google. Organizers cannot
respond to their own event.

`--dry-run` performs the live reads and permission checks, then returns a plan
without changing Google or the archive. `--read-only` rejects every event write,
including a dry run. Use `--all-day` with date-only bounds; `--to` is exclusive.
For timed events, use RFC3339 with an offset, or local `YYYY-MM-DDTHH:MM` with an
IANA `--tz`. Timed recurrence uses the target calendar's time zone when the
event has no explicit zone. See the [CLI contract](../cli-reference.md#calendar) for field and
reminder flags.

Recurring edits default to `--scope single`. A series ID requires
`--original-start`; an instance ID selects that occurrence directly.
`--scope all` selects the series master. `--scope future --original-start ...`
truncates the original series and creates a replacement for an update, or only
truncates it for a delete. Future scope supports one RRULE and rejects RDATE,
EXDATE, multiple rules, non-default event types, and detached future exceptions,
including cancellations. Future updates also reject private-copy propagation,
meeting details, attachments, custom metadata, labels, colors, visibility, and
guest permissions that a new series would lose. They also reject future splits when
guest RSVP responses would be lost; edit those series manually.
An `UNTIL`-limited series rejects a replacement start after its existing
cutoff unless the update supplies a new recurrence rule.
Changing between all-day and timed events also requires an explicit compatible
rule when the inherited rule uses `UNTIL`. A changed start must match its
recurrence rule; moving a Monday series to Tuesday requires a Tuesday rule.
Future edits reject an `original_start` that differs from the selected instance
and scheduling changes that overlap retained occurrences, including rescheduled
exceptions. Earlier moves that fit between retained events remain allowed.
It checks at most 10,000 recurrence instances and 100 event-list pages.
Recurring moves are rejected; move supports standalone events.

Successful writes enter the archive immediately, through the normal calendar
sync persistence path. Cancelling retains the archived event and marks it
cancelled; moving archives the destination and cancellation on the old calendar.
Sync cursors do not advance, so the next sync can safely re-deliver the change.
The response includes completed writes and archive message IDs. If a remote
change succeeds but archiving fails, run `sync-calendar` to reconcile it; do not
repeat the mutation. Future-series updates use two Google requests. If the replacement definitely
fails, the daemon attempts to restore the original
recurrence using the version returned by the shortening request. A concurrent
edit prevents that restoration. The response reports completed writes and whether
restoration succeeded; inspect it before taking further action.
If the second write's outcome is unknown, the daemon does not attempt restoration
and the result sets `outcome_unknown`;
the `outcome_code` is `calendar_outcome_unknown`. Reconcile the current calendar
state and completed receipts before taking further action. Do not replay the
uncertain write based only on its response. A known partial provider failure
uses `outcome_code: calendar_partial`.

Query availability without event-write consent:

```bash
msgvault calendar freebusy team --account person@example.com \
  --from 2026-10-02T00:00:00Z --to 2026-10-03T00:00:00Z --json
msgvault calendar conflicts team --account person@example.com \
  --calendars team,other@example.com \
  --from 2026-10-02T00:00:00Z --to 2026-10-03T00:00:00Z --json
```

Conflicts are overlapping busy periods between selected calendars. Provider
errors are reported instead of treating unavailable calendars as free time.

The [HTTP API](../api-server.md#calendar-control) and
[MCP tools](chat.md#calendar-control) use the same daemon checks and archive path.
Delegated grants apply to the exact calendar source identity
`gcal` plus `account-email/calendar-id`: `calendar.read` permits availability,
`calendar.event.read` permits provider-derived event details in delegated plans
and write receipts, `calendar.write` permits event changes, and `calendar.invite`
additionally permits guest changes. None of these permissions implies the others.
Write-only grants still receive live validation results, which can reveal timing
constraints even when event details are hidden.

## What gets archived

Each event is stored as a searchable record with `message_type = calendar_event`:

- The **organizer** becomes the `from` participant and **attendees** become `to`
  participants, so they dedupe with your email contacts.
- The **subject** is the event summary; the searchable body includes the title,
  time range, location, description, and attendee names.
- **Recurring events** are grouped into one conversation titled by the series;
  individually edited occurrences keep their own details.
- **Cancelled events are kept**, marked cancelled rather than deleted, so your
  archive preserves that a meeting once existed.
- The full original event record is retained for fidelity.

## Find events

Calendar events are searchable like any other message. Restrict a search to
events with `--message-type calendar_event`:

```bash
# Keyword search across event summaries, locations, descriptions, and attendees
msgvault search "standup" --message-type calendar_event

# Everything on a calendar within a date range
msgvault search "after:2024-01-01 before:2024-04-01" --message-type calendar_event
```

When [vector search](/docs/usage/vector-search/) is enabled, events become eligible
for embedding after sync and can be found semantically with `--mode vector` or
`--mode hybrid` once the embedding worker has processed them. For manual
`sync-calendar` runs, follow up with `msgvault embeddings build`. In the
daemon, scheduled `[[gcal]]` syncs do not trigger the
`[vector.embed.schedule].run_after_sync` hook (it applies to scheduled
account syncs such as Gmail, IMAP, and Teams); newly synced events are
picked up by the embed worker's `[vector.embed.schedule].cron` schedule.

## Scheduled sync (daemon)

Run calendar sync automatically with `msgvault serve` by adding a `[[gcal]]`
entry to `config.toml`:

```toml
[[gcal]]
email = "you@gmail.com"
schedule = "0 */6 * * *"   # every 6 hours (5-field cron)
enabled = true
```

The first scheduled run full-syncs and registers calendars; later runs are
incremental. See [Configuration](/docs/configuration/#google-calendar-sources) for
every field.

!!! note
    An `enabled` `[[gcal]]` entry with no `schedule` is never synced by the
    daemon — set a cron `schedule` so its freshness does not drift stale.

## Headless server setup

A headless server can't complete Google's browser consent, and the OAuth device
flow doesn't support Calendar scopes. Authorize on a machine with a browser,
then copy the token to the server. If the server already has a token for the
account, copy that token to the browser machine first so re-consent preserves
Drive or other previously granted Google scopes.

1. **If a token already exists on the server**, copy it to the browser machine:
    ```bash
    mkdir -p ~/.msgvault/tokens
    scp user@server:~/.msgvault/tokens/you@gmail.com.json ~/.msgvault/tokens/
    ```

2. **On a machine with a browser**, using the **same `client_secret.json`** as
    the server:
    ```bash
    msgvault add-calendar you@gmail.com
    ```
    Keep all existing permissions plus Calendar checked on the consent screen.

3. **Copy the token back to the server**, replacing the existing one. It now
    carries Calendar plus the existing Google permissions, so current sync jobs
    keep working:
    ```bash
    ssh user@server mkdir -p ~/.msgvault/tokens
    scp ~/.msgvault/tokens/you@gmail.com.json user@server:~/.msgvault/tokens/
    ```

4. **On the server**, register the calendars (no browser needed) and sync:
    ```bash
    msgvault add-calendar you@gmail.com
    msgvault sync-calendar you@gmail.com
    ```

Run `msgvault add-calendar you@gmail.com --headless` on the server to print these
steps at any time.

## Google Workspace service accounts

Workspace admins using domain-wide delegation do not need per-user browser
tokens for Calendar. Enable the Google Calendar API, authorize the service
account client ID for `https://www.googleapis.com/auth/calendar.readonly`, and
configure `[oauth].service_account_key` or
`[oauth.apps.<name>].service_account_key` as described in
[OAuth Setup](/docs/guides/oauth-setup/#google-workspace-service-accounts).

Register the calendars, then sync their events:

```bash
msgvault add-calendar user@example.com --oauth-app example
msgvault sync-calendar user@example.com --oauth-app example
```

Replace `example` with the configured OAuth app name. Omit `--oauth-app` when
using the default `[oauth]` configuration. `add-calendar` uses delegated access
directly: it does not request `client_secrets`, open a browser, or create a
per-user refresh token. The usual `--all-calendars`, `--min-access-role`, and
`--calendars` registration filters still apply.

You can also start with `sync-calendar`, which registers matching calendars on
its first run, or configure a scheduled `[[gcal]]` entry. If Google rejects the
delegated permission or reports that the Calendar API is disabled, msgvault
reports the error immediately. Correct the service account's Calendar permission
or enable the API before trying again.

## Privacy

Calendar sync is read-only and runs only when you invoke it (or on the schedule
you configure). OAuth tokens are stored under your msgvault home directory with
owner-only permissions and are never written into `config.toml`, logs, or
exported data.
