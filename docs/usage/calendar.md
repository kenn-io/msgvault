---
last_edited: "2026-11-20"
title: Google Calendar
description: Archive Google Calendar events alongside your email, with full-text and semantic search over meetings, organizers, and attendees.
---

Archive your Google Calendar events into the same local database as your email.
You can search events by keyword, and semantically when vector search is
enabled. Organizers and attendees join the same contact graph as the people you
email, so a meeting with `alice@example.com` dedupes against the messages you
exchanged with her.

Calendar sync is **read-only**. Separate event-control commands can change live
events if you opt in to write consent and source permissions.

## List archived events

You can list archived events across Google Calendar syncs, imported iCalendar
files, and invitations extracted from archived email. To import an iCalendar
file, run `msgvault import-ics calendar.ics --identifier personal-calendar`;
see the [import reference](../cli-reference.md#import-ics) for details.

```bash
msgvault calendar events --from 2027-03-01 --to 2027-04-01
msgvault calendar events --from 2027-03-01 --to 2027-04-01 --json
```

Repeating events expand into individual occurrences. Edited occurrences appear
at their new times, cancelled occurrences show `cancelled`, and a deleted
series lists only its archived edited or cancelled occurrences. Existing
archives rebuild event and attendee tables during setup, so you don't need to
re-sync.

Date-only bounds use midnight in your machine's time zone, and you can also
pass RFC3339 timestamps with offsets. The end bound is exclusive and must
follow the start. Timed events print in your machine's time zone, while all-day
events show their dates. The `END (EXCL)` column shows the exclusive end. Each
row's ID identifies the effective archive message, including any edited or
cancelled copy, and JSON output carries the series master ID separately in
`series_message_id`. To open an occurrence and its linked notes, copy the
table's `REF` into `msgvault meetings links`, for example
`msgvault meetings links evt:23@20270321T100000Z`.

JSON output includes status, attendees, roles, RSVPs, and any events that could
not be expanded. Table output prints a warning for unexpandable events and
shows `deleted` for events deleted at the source. If the command asks you to
narrow the window, use a shorter range.

Each query allows up to 2,000 recurring series and 500,000 candidate steps
across the request, with 50,000 steps per series. When recurrence expansion hits
the request work limit, the response keeps completed series, standalone events,
and archived exceptions, and lists the interrupted series and every remaining
series under `unexpandable`. Because long-running series count steps from their
start, a narrower window may not reduce the work. A narrower window also
doesn't reduce the number of open-ended series.

This archive read requires daemon API schema 3.35.0 or newer and owner access.
For `--account`, `--dry-run`, `--read-only`, and `--send-updates`, use the live
commands below. See the
[CLI reference](https://msgvault.io/docs/cli-reference/#calendar) for the
command contract.

## Prerequisites

Before you authorize Calendar, make sure you have:

- A Google OAuth client, as described in [OAuth Setup](/docs/guides/oauth-setup/),
  or a [Workspace service account](#google-workspace-service-accounts). Browser
  authorization can reuse the `client_secret.json` already configured for Gmail.
- The **Google Calendar API** enabled on that OAuth project. In the
  [Google Cloud Console](https://console.cloud.google.com/), go to
  **APIs & Services > Library**, search for "Google Calendar API", and click
  **Enable**.

## Authorize and register calendars

Run `add-calendar` to grant read-only Calendar access (`calendar.readonly`) and
register your calendars for sync:

```bash
msgvault add-calendar you@gmail.com
```

!!! warning "Keep both Gmail and Calendar checked"
    If the account already has a Gmail token, re-consent **replaces** the granted
    scopes, so msgvault re-requests Gmail **and** Calendar together. On Google's
    consent screen, keep **both** checked. Unchecking Gmail drops Gmail access
    for that account.

By default, msgvault registers only calendars you own or can write to. Pass
`--all-calendars` to also include subscribed and holiday calendars that you can only
read.

| Flag | Description |
|---|---|
| `--all-calendars` | Include reader/freeBusyReader (subscribed, holiday) calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--calendars` | Comma-separated calendar IDs to register |
| `--oauth-app` | Named OAuth app to use |
| `--headless` | Print headless-server setup instructions instead of opening a browser |
| `--write` | Also request `calendar.events` for event control; source write permissions are still required |

## Sync events

Sync your registered calendars manually with `sync-calendar`:

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

The first run, or any run with `--full`, enumerates and registers your
calendars and downloads events. Later runs are incremental and use the Calendar
`syncToken` to fetch only changes. If a full sync is interrupted, the next run
resumes from a checkpoint; pass `--noresume` to start over.

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

You can pass either an account email or the `name` of a `[[gcal]]` entry in
`config.toml` as the first argument. See
[Scheduled sync](#scheduled-sync-daemon) below for that configuration.

## Control events

Authorize writes separately from read-only sync:

1. Request write consent for the account:

   ```bash
   msgvault add-calendar person@example.com --write
   ```

   Re-consent preserves your existing Gmail grant, including a Gmail grant
   narrowed to read-only, along with other already granted Google scopes. Keep
   them checked on the consent screen. For headless setup, follow the headless
   steps below with `--write` on both machines. Workspace service accounts also
   need the `calendar.events` scope in their domain-wide delegation grant.

2. Enable the source and list the exact writable calendar IDs in `config.toml`:

   ```toml
   [[gcal]]
   email = "person@example.com"
   enabled = true
   write_calendars = ["team@example.com"]
   invite_calendars = ["team@example.com"]
   calendar_aliases = { team = "team@example.com" }
   ```

3. Restart the daemon after changing its configuration.

In event commands, `--account` selects the OAuth account and the positional
calendar selects the event's calendar and organizer. Those two can differ.
Before every write, the daemon checks the target calendar's current
`accessRole` from Google. A non-primary calendar must be present with `owner`
or `writer` access, and `reader` access can't create events.

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

Guest notifications default to `sendUpdates=none`, which suppresses
notifications without preventing guest or invitation state from changing. Pass
`--send-updates all` or `externalOnly` to send notifications. Guest changes,
RSVPs, and edits, deletes, or moves of events with guests require
`invite_calendars` as well as `write_calendars`. Moving an event also checks
roles and permissions on both calendars. The `respond` command changes only the
attendee marked as self by Google, and organizers can't respond to their own
event.

Pass `--dry-run` to run the live reads and permission checks and get a plan
without changing Google or your archive. Pass `--read-only` to reject every
event write, including a dry run. For all-day events, use `--all-day` with
date-only bounds; `--to` is exclusive. For timed events, pass RFC3339
timestamps with an offset, or local `YYYY-MM-DDTHH:MM` with an IANA `--tz`.
Timed recurrences use the target calendar's time zone when the event has no
explicit zone. See the [CLI contract](../cli-reference.md#calendar) for field
and reminder flags.

Recurring edits default to `--scope single`. Pass `--original-start` when
targeting a series ID, or pass an instance ID to select that occurrence
directly. Use `--scope all` to target the series master. Passing
`--scope future --original-start ...` truncates the original series and creates
a replacement when updating, or only truncates it when deleting.

With future scope:

- It supports one RRULE and rejects RDATE, EXDATE, multiple rules, non-default
  event types, and detached future exceptions, including cancellations.
- Future updates reject private-copy propagation, meeting details, attachments,
  custom metadata, labels, colors, visibility, and guest permissions that a new
  series would lose. They also reject future splits when guest RSVP responses
  would be lost; edit those series manually.
- On an `UNTIL`-limited series, a replacement start after the existing cutoff
  is rejected unless you supply a new recurrence rule.
- Switching between all-day and timed events also requires an explicit
  compatible rule when the inherited rule uses `UNTIL`.
- A changed start must match its recurrence rule. For example, moving a Monday
  series to Tuesday requires a Tuesday rule.
- Future edits reject an `original_start` that differs from the selected
  instance and scheduling changes that overlap retained occurrences, including
  rescheduled exceptions. Earlier moves that fit between retained events remain
  allowed.
- Recurring edits check at most 10,000 recurrence instances and 100 event-list
  pages.
- Recurring moves are rejected; move supports standalone events.

Successful writes enter your archive right away through the normal calendar
sync persistence path. Cancelling keeps the archived event and marks it
cancelled, while moving archives both the destination event and the
cancellation on the old calendar. Sync cursors don't advance, so the next sync
can safely re-deliver the change. Each response reports completed writes and
archive message IDs. If a remote change succeeds but archiving fails, run
`sync-calendar` to reconcile it rather than repeating the mutation.

Future-series updates require two Google requests. If the replacement request
definitely fails, the daemon attempts to restore the original recurrence using
the version returned by the shortening request, though a concurrent edit
prevents that restoration. Check the response for completed writes and whether
restoration succeeded before taking further action. If the second write's
outcome is unknown, the daemon doesn't attempt restoration and returns
`outcome_unknown` with `outcome_code` set to `calendar_outcome_unknown`.
Reconcile the current calendar state and completed receipts before taking
further action, and don't replay the uncertain write based only on its
response. A known partial provider failure uses
`outcome_code: calendar_partial`.

You can query availability without event-write consent:

```bash
msgvault calendar freebusy team --account person@example.com \
  --from 2026-10-02T00:00:00Z --to 2026-10-03T00:00:00Z --json
msgvault calendar conflicts team --account person@example.com \
  --calendars team,other@example.com \
  --from 2026-10-02T00:00:00Z --to 2026-10-03T00:00:00Z --json
```

Conflicts are overlapping busy periods across the selected calendars. If a
provider returns an error, msgvault reports it instead of treating an
unavailable calendar as free time.

The [HTTP API](../api-server.md#calendar-control) and
[MCP tools](chat.md#calendar-control) run the same daemon checks and archive
path. Delegated grants apply to the exact calendar source identity `gcal` plus
`account-email/calendar-id`: `calendar.read` permits availability,
`calendar.event.read` permits provider-derived event details in delegated plans
and write receipts, `calendar.write` permits event changes, and
`calendar.invite` also permits guest changes. None of these permissions
implies the others. Write-only grants still receive live validation results,
which can reveal timing constraints even when event details are hidden.

## What gets archived

Each event is stored as a searchable record with `message_type = calendar_event`:

- **Organizer and attendees.** The organizer becomes the `from` participant and
  attendees become `to` participants, so they dedupe with your email contacts.
- **Subject and body.** The event summary becomes the subject, and the
  searchable body includes the title, time range, location, description, and
  attendee names.
- **Recurring events.** A series is grouped into one conversation titled by the
  series, while individually edited occurrences keep their own details.
- **Cancelled events.** Cancelled events are kept and marked cancelled rather
  than deleted, so your archive preserves that a meeting once existed.
- **Original record.** The full original event record is retained for fidelity.

## Find events

You can search calendar events like any other message. Pass
`--message-type calendar_event` to restrict results to events:

```bash
# Keyword search across event summaries, locations, descriptions, and attendees
msgvault search "standup" --message-type calendar_event

# Everything on a calendar within a date range
msgvault search "after:2024-01-01 before:2024-04-01" --message-type calendar_event
```

When [vector search](/docs/usage/vector-search/) is enabled, synced events
become eligible for embedding and show up in semantic searches with
`--mode vector` or `--mode hybrid` once the embedding worker processes them.
After a manual `sync-calendar` run, run `msgvault embeddings build`. In the
daemon, scheduled `[[gcal]]` syncs don't trigger the
`[vector.embed.schedule].run_after_sync` hook, which applies to scheduled
account syncs such as Gmail, IMAP, and Teams. Instead, the embed worker picks
up newly synced events on its `[vector.embed.schedule].cron` schedule.

## Scheduled sync (daemon)

To sync your calendar automatically with `msgvault serve`, add a `[[gcal]]`
entry to `config.toml`:

```toml
[[gcal]]
email = "you@gmail.com"
schedule = "0 */6 * * *"   # every 6 hours (5-field cron)
enabled = true
```

The first scheduled run runs a full sync and registers your calendars, and
later runs are incremental. See
[Configuration](/docs/configuration/#google-calendar-sources) for every field.

!!! note
    An `enabled` `[[gcal]]` entry with no `schedule` is never synced by the
    daemon. Set a cron `schedule` so its freshness does not drift stale.

## Headless server setup

A headless server can't complete Google's browser consent, and the OAuth device
flow doesn't support Calendar scopes. Instead, authorize on a machine with a
browser and copy the token to the server. If the server already has a token for
that account, copy it to the browser machine first so re-consent preserves
Drive or other previously granted Google scopes.

1. If a token already exists on the server, copy it to the browser machine:
    ```bash
    mkdir -p ~/.msgvault/tokens
    scp user@server:~/.msgvault/tokens/you@gmail.com.json ~/.msgvault/tokens/
    ```

2. On the machine with a browser, using the same `client_secret.json` as the
    server, authorize Calendar:
    ```bash
    msgvault add-calendar you@gmail.com
    ```
    Keep all existing permissions plus Calendar checked on the consent screen.

3. Copy the updated token back to the server to replace the existing one. It
    now carries Calendar along with the existing Google permissions, so current
    sync jobs keep working:
    ```bash
    ssh user@server mkdir -p ~/.msgvault/tokens
    scp ~/.msgvault/tokens/you@gmail.com.json user@server:~/.msgvault/tokens/
    ```

4. On the server, register the calendars without a browser and run a sync:
    ```bash
    msgvault add-calendar you@gmail.com
    msgvault sync-calendar you@gmail.com
    ```

You can run `msgvault add-calendar you@gmail.com --headless` on the server at
any time to print these steps.

## Google Workspace service accounts

If you are a Workspace admin using domain-wide delegation, you don't need
per-user browser tokens for Calendar. Enable the Google Calendar API, authorize
the service account client ID for
`https://www.googleapis.com/auth/calendar.readonly`, and configure
`[oauth].service_account_key` or `[oauth.apps.<name>].service_account_key` as
described in
[OAuth Setup](/docs/guides/oauth-setup/#google-workspace-service-accounts).

Register the calendars, then sync their events:

```bash
msgvault add-calendar user@example.com --oauth-app example
msgvault sync-calendar user@example.com --oauth-app example
```

Replace `example` with your configured OAuth app name, or omit `--oauth-app` if
you use the default `[oauth]` configuration. `add-calendar` uses delegated
access directly without requesting `client_secrets`, opening a browser, or
creating a per-user refresh token. The usual `--all-calendars`,
`--min-access-role`, and `--calendars` registration filters still apply.

You can also start directly with `sync-calendar`, which registers matching
calendars on its first run, or configure a scheduled `[[gcal]]` entry. If
Google rejects the delegated permission or reports that the Calendar API is
disabled, msgvault reports the error right away. Fix the service account's
Calendar permission or enable the API before trying again.

## Privacy

Calendar sync is read-only and runs only when you invoke it or on the schedule
you configure. msgvault stores OAuth tokens under your msgvault home directory
with owner-only permissions and never writes them into `config.toml`, logs, or
exported data.
