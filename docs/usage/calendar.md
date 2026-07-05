---
title: Google Calendar
description: Archive Google Calendar events alongside your email, with full-text and semantic search over meetings, organizers, and attendees.
---

msgvault can archive your Google Calendar events into the same local database as
your email. Events become searchable by keyword (and semantically, when vector
search is enabled), and their organizers and attendees join the same contact
graph as the people you email — so a meeting with `alice@example.com` dedupes
against the messages you exchanged with her.

Calendar sync is **read-only**: msgvault never creates, edits, or deletes
anything on your Google Calendar.

## Prerequisites

- An OAuth client already configured for Gmail (see [OAuth Setup](/guides/oauth-setup/)).
  Calendar reuses the same `client_secret.json`.
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

When [vector search](/usage/vector-search/) is enabled, events become eligible
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
incremental. See [Configuration](/configuration/#google-calendar-sources) for
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
configure `[oauth].service_account_key` or `[oauth.apps.<name>].service_account_key`
as described in [OAuth Setup](/guides/oauth-setup/#google-workspace-service-accounts).

Then sync the account directly or add a scheduled `[[gcal]]` entry:

```bash
msgvault sync-calendar user@domain.com --oauth-app acme
```

The first sync registers matching calendars and stores their sync cursors.

## Privacy

Calendar sync is read-only and runs only when you invoke it (or on the schedule
you configure). OAuth tokens are stored under your msgvault home directory with
owner-only permissions and are never written into `config.toml`, logs, or
exported data.
