---
last_edited: "2026-09-01"
title: Web UI & API Server
description: Daemon-served analytical Web UI and REST API for your msgvault archive, with optional background sync scheduling.
---


## Overview

`msgvault serve` starts an HTTP server that exposes your archive through the
first-party Web UI at `/` and a REST API under `/api`. It optionally runs a
background sync scheduler to keep accounts up to date on a cron-based schedule.
The complete UI is embedded in the release binary; see [Web UI](/docs/web-ui/) for
browser login, secure remote deployment, search states, and keyboard controls.

The API is registered through Huma and exposes a generated OpenAPI document at `/openapi.json`. You can also run `msgvault openapi` to print the same checked-in contract without starting a daemon or opening the archive database. The OpenAPI `info.version` is the API schema version used for client/server compatibility; the current schema is 2.15.0. Within the unreleased 2.x line, 2.14.0 replaces the CardDAV publication and conflict response shapes with bounded projections that omit raw vCards and resource hrefs. The running daemon binary version is exposed separately in the generated document metadata. The API queries the same archive database and attachment store as the CLI, Web UI, and TUI. SQLite is the default archive database; PostgreSQL is supported when `[data].database_url` is a PostgreSQL DSN. Keyword search and ordinary archive reads stay local to that database. If vector search is enabled, semantic and hybrid search also call the embedding endpoint configured in `[vector.embeddings]`. The server is designed for interactive archive use, local integrations, dashboards, and automation scripts.

Go integrations can use the generated client in `pkg/client`. The wrapper
handles msgvault-specific response details such as deletion staging dry-runs
returning `200` while created manifests return `201`.

The HTTP listener, health endpoint, and API routing start before analytics cache
maintenance. With `engine = "auto"`, aggregate requests initially use live SQL
while the cache is built or opened, then switch to DuckDB after success. A
failed automatic build or open keeps the daemon on live SQL. With
`engine = "duckdb"`, analytics remain unavailable until the required cache is
ready, so analytics routes return `503` during initialization; the daemon does
not fall back to SQL. Cache-dependent routes also return a structured `503`
while automatic cache maintenance is active. Its `readiness` field lets clients
distinguish transient `building` state from an unavailable cache and retry
without requiring user action. Set `auto_build_cache = false` to skip automatic
startup maintenance.

## Quick Start

Add a `[server]` section to your `config.toml`:

```toml
[server]
api_port = 8080
api_key = "your-secret-key"
```

Start the server:

```bash
msgvault serve
```

Test connectivity:

```bash
# Health check (no auth required)
curl http://localhost:8080/health

# Archive stats (auth required)
curl -H "Authorization: Bearer your-secret-key" http://localhost:8080/api/v1/stats

# Generated OpenAPI document
curl http://localhost:8080/openapi.json
```

## Authentication

Archive API endpoints require authentication when `api_key` is set in your
config. The public application shell, `/health`, and the browser-session
bootstrap/login routes remain reachable so the UI can determine whether login
is required. Three API-key authentication methods are supported:

| Method | Header | Example |
|---|---|---|
| Bearer token | `Authorization: Bearer <key>` | `Authorization: Bearer my-secret` |
| API key header | `X-API-Key: <key>` | `X-API-Key: my-secret` |
| Plain auth header | `Authorization: <key>` | `Authorization: my-secret` |

If no `api_key` is configured, authentication is not required regardless of bind address. The separate `allow_insecure` / security validation prevents starting without an API key on non-loopback addresses.

## API Endpoints

### Curated person network {#get-apiv1peopleidnetwork}

**Endpoint:** `GET /api/v1/people/{id}/network`

Returns a read-only, person-centred projection built only from durable typed
relationships and employments. It never derives nodes or edges from messages,
conversations, participant co-occurrence, or the analytical cache.

`depth` defaults to `1` and accepts `1`, `2`, or `3`. `include_ended` defaults
to `false`; set it to `true` to admit ended relationships and employment
records. The deterministic breadth-first response contains at most 250 nodes
and 500 edges. `truncated: true` means the response is a bounded prefix and can
contain fewer than either maximum. The root durable person is returned even
when it has no qualifying connections.

```json
{
  "root_person_id": 42,
  "depth": 2,
  "truncated": false,
  "nodes": [
    {"id": "person:42", "kind": "person", "entity_id": 42, "label": "Example Person", "hop": 0},
    {"id": "organization:21", "kind": "organization", "entity_id": 21, "label": "Example Organization", "hop": 1}
  ],
  "edges": [
    {"id": "employment:7", "kind": "employment", "source_node_id": "person:42", "target_node_id": "organization:21", "label": "Engineer"}
  ]
}
```

Invalid depths return `400`; an unknown durable person returns `404`. The
projection has no ETag because it is not a mutation resource.

---

### Health check {#get-health}

**Endpoint:** `GET /health`

Health check endpoint. Does not require authentication.

**Response:**

```json
{"status": "ok", "analytics_engine": "sql-fallback"}
```

`analytics_engine` reports the active mode: `sql-fallback` while `engine =
"auto"` is waiting for or cannot use a cache, `duckdb` after a successful cache
build/open, `sql` for deliberate live SQL, `postgres` for PostgreSQL, and
`initializing` while required DuckDB analytics are being prepared. Health stays
available during initialization; analytics routes return `503` until the
required engine is ready.

---

### Archive statistics {#get-apiv1stats}

**Endpoint:** `GET /api/v1/stats`

Archive statistics. When vector search is configured on the server,
the response also includes a `vector_search` sub-object describing
the state of the index.

**Response (vector search disabled):**

```json
{
  "total_messages": 142857,
  "total_threads": 48293,
  "total_accounts": 2,
  "total_labels": 47,
  "total_attachments": 31204,
  "database_size_bytes": 8589934592
}
```

**Response (vector search enabled):**

```json
{
  "total_messages": 142857,
  "total_threads": 48293,
  "total_accounts": 2,
  "total_labels": 47,
  "total_attachments": 31204,
  "database_size_bytes": 8589934592,
  "vector_search": {
    "enabled": true,
    "active_generation": {
      "id": 3,
      "model": "nomic-embed-text-v1.5",
      "dimension": 768,
      "fingerprint": "nomic-embed-text-v1.5:768:p1-111111:c32768:e1",
      "state": "active",
      "activated_at": "2026-04-18T15:12:33Z",
      "message_count": 142820
    },
    "building_generation": {
      "id": 4,
      "model": "nomic-embed-text-v2",
      "dimension": 768,
      "started_at": "2026-04-19T09:02:10Z",
      "progress": { "done": 8200, "total": 142857 }
    },
    "missing_embeddings_total": 134657
  }
}
```

`active_generation` is always present in the object (null until the
first build completes). `building_generation` is omitted when no
rebuild is in flight. `missing_embeddings_total` reports live messages
still needing embedding for the generation the worker will target next:
the building generation while a rebuild is in flight, otherwise the active
generation. During a rebuild the old active generation keeps serving vector
and hybrid search, but active-generation top-ups are frozen until the
building generation activates. See
[Vector Search](/docs/usage/vector-search/) for the end-to-end workflow.

---

### List messages {#get-apiv1messages}

**Endpoint:** `GET /api/v1/messages`

Paginated message list.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `page` | int | `1` | Page number |
| `page_size` | int | `20` | Results per page |

**Response:**

```json
{
  "total": 142857,
  "page": 1,
  "page_size": 20,
  "messages": [
    {
      "id": 12345,
      "subject": "Q4 Planning",
      "message_type": "email",
      "from": "alice@example.com",
      "to": ["bob@example.com"],
      "cc": ["carol@example.com"],
      "sent_at": "2024-10-15T09:30:00Z",
      "snippet": "Here's the draft for Q4...",
      "labels": ["INBOX", "IMPORTANT"],
      "has_attachments": true,
      "size_bytes": 52480
    }
  ]
}
```

---

### Filter messages {#get-apiv1messagesfilter}

**Endpoint:** `GET /api/v1/messages/filter`

List messages with structured filters backed by the query engine. This is the
API equivalent of drilling into aggregate/search results and is the endpoint to
use for message-type filtering when you do not need full-text ranking.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `sender` / `sender_name` | string | — | Sender address/phone or display-name filter |
| `recipient` / `recipient_name` | string | — | Recipient address/phone or display-name filter |
| `domain` | string | — | Participant domain filter |
| `label` | string | — | Label filter |
| `message_type` | string | — | Stored message type, for example `email`, `teams`, `discord`, `calendar_event`, or `sms` |
| `source_id` | int | — | Restrict to one source |
| `conversation_id` | int | — | Restrict to one conversation/thread |
| `after` / `before` | date | — | RFC3339 or `YYYY-MM-DD` bounds |
| `attachments_only` | bool | `false` | Only include messages with attachments |
| `hide_deleted` | bool | `false` | Exclude messages marked deleted at the source |
| `offset` | int | `0` | Zero-based row offset |
| `limit` | int | `500` | Maximum rows to return; capped at 500 outside conversation fetches |
| `sort` | enum | `date` | `date`, `size`, or `subject` |
| `direction` | enum | `desc` | `asc` or `desc` |

**Response:**

```json
{
  "count": 1,
  "has_more": false,
  "offset": 0,
  "limit": 500,
  "messages": [
    {
      "id": 12345,
      "subject": "Incident review",
      "message_type": "teams",
      "from": "alice@example.com",
      "to": [],
      "sent_at": "2026-07-01T15:30:00Z",
      "snippet": "Follow-up from the channel discussion...",
      "labels": [],
      "has_attachments": false,
      "size_bytes": 0
    }
  ]
}
```

The companion `GET /api/v1/messages/gmail-ids` endpoint returns matching Gmail
source message IDs for email workflows such as deletion staging. It honors a
subset of these parameters: `sender` / `sender_name`, `recipient` /
`recipient_name`, `domain`, `label`, `source_id`, `after` / `before`, and
`limit`. Results are always restricted to Gmail sources, exclude deleted
messages, and are ordered newest-first; the remaining `/messages/filter`
parameters (`message_type`, `conversation_id`, `attachments_only`,
`hide_deleted`, `offset`, `sort`, `direction`) are ignored.

---

### Changed messages {#get-apiv1messageschanges}

**Endpoint:** `GET /api/v1/messages/changes`

Lists messages whose content changed at or after a cursor, oldest change first.
Poll it, re-read the rows it returns, and store the cursor it hands back. Unlike
`/messages/filter` it is ordered by when a message *changed*, not by when it was
sent, so a mailbox imported today with ten-year-old mail shows up in the very
next page.

**What it is.** An invalidation feed over one projection of a message: the
columns this endpoint returns, plus a body edit and two columns it does not
return. It tells you *that* a message changed and hands you the current value of
those columns. It does not tell you how many times it changed, what it held in
between, or that anything outside that projection changed.

**What it is not.** It is not a way to keep a general copy of the archive
current. Labels, recipients, attachment metadata and content, raw MIME, body
deletions, participant edits, and conversation metadata are all outside it — and
several of those, label re-sync above all, are the most frequent changes a mail
archive sees. Hard deletions are outside it too: a row removed from the database
leaves nothing behind to report.

So build on it a consumer that keeps a projection of the tracked columns fresh,
or one that reacts to messages changing. Do not build on it a mirror that has to
match the archive, unless you also refresh the untracked surfaces on your own
schedule and reconcile deletions independently. The exact boundaries are
[What this feed does not report](#what-this-feed-does-not-report) and the
[delivery contract](#delivery-contract).

Messages hidden by deduplication and messages deleted at the source are
included, with their `deleted_at` and `deleted_from_source_at` timestamps set.
A consumer keeping a copy of the tracked columns needs both to learn that a
message it already holds has been hidden or deleted.

The dedup-hidden ones are what this feed is alone in returning: every other
message listing filters them out with `deleted_at IS NULL`, and no parameter
turns that off. Source-deleted messages are not unique to it —
`/messages/filter` returns those by default too, and `hide_deleted=true` is what
suppresses them there — but this feed returns them unconditionally, with no
parameter that hides them.

The watermark moves for every mutable field the feed returns, for message-body
edits, and for two columns the feed does not return: the sender pointer
(`messages.sender_id`) and the platform metadata payload. It never moves for
anything else — see
[What this feed does not report](#what-this-feed-does-not-report).

Part of what moves it is readable only from `/api/v1/messages/{id}`, so the feed
*does* invalidate some of that response. When a message reaches you through the
feed, re-fetch its detail response if you cache either of these:

* `body` and `body_html`. An added or edited body moves the watermark. A body
  *deletion* does not, so a body you already hold can still go stale — that one
  is in the exception list below.
* `from`. It is derived from the sender pointer, and a change of pointer moves
  the watermark. Renaming or correcting the participant the pointer names does
  not, so the address or display name can still change under you.

The rest of the detail response the feed genuinely never invalidates: `to`,
`cc`, `bcc`, `labels`, and the `attachments` array — filenames, hashes, sizes
and all — can all change without the message ever reappearing in the feed.
Refresh those on your own schedule.

> **Existing archives are modified the first time they are opened, and the index
> build inside that stops writes.** Support for this feed adds a
> `content_changed_at`
> column to `messages`, runs a one-time full-table backfill of it, builds an
> index on it, and installs the watermark triggers. Trigger installation is a
> versioned migration, so it runs once for each trigger definition instead of
> taking trigger locks on every open. On PostgreSQL the backfill bumps
> `last_modified` on every row once, at upgrade.
>
> The backfill commits in batches and the watermark triggers are installed before
> it starts, so a concurrent write is safe throughout and, on PostgreSQL,
> unrelated rows can be written between batches. The index is the part that
> stops writes: it is built with a plain `CREATE INDEX`, which on PostgreSQL
> blocks every write to `messages` for as long as the build takes, and on SQLite
> holds the database's single writer slot for the same stretch. The two
> directions both bite: an import already writing holds the startup waiting, and
> an import that starts during the build waits for it. On a large archive treat
> the first open as a planned write outage rather than a restart — for the length
> of the index build, not of the whole upgrade — and note that the daemon does
> not serve requests until all of it finishes.
>
> The upgrade is deliberately **not atomic**. The column addition, each committed
> batch of the backfill, the ledger entry that records the backfill as done, and
> the index are four separate durable steps. If one fails — or an operator
> interrupts it — startup aborts, everything already committed stays, and the
> next open resumes from where it stopped instead of starting over.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `cursor` | string | — | The `next_cursor` of the previous response, sent back verbatim. Omit — or send it empty — to start from the beginning of the archive |
| `limit` | int | `100` | Maximum rows to return; capped at 500. Values below 1 fall back to the default |

> **Polling cost — the feed takes the SQLite write lock.** On the SQLite backend
> each request briefly acquires the database's single write lock to establish how
> far writes have committed, so unlike an ordinary read it competes with an
> in-progress import. Measured on one machine against three concurrent writers:
> eight clients paging the feed in a tight loop cut writer throughput to **15%**
> of the unloaded rate, where eight clients running an equivalent plain `SELECT`
> left **49%**. One consumer polling once a second is free. The endpoint limits
> each client IP to two requests per second with a burst of four; honor a `429`
> response's `Retry-After` header. Poll on an interval, and drain a backlog with
> `has_more` or a larger `limit` rather than a tighter poll. PostgreSQL
> establishes the same bound without taking a lock.

**The cursor is opaque.** Store the `next_cursor` a response hands you and send
it back unchanged; do not parse it, construct one, compare two of them, or order
them. What it encodes is the server's business and may change without notice.
Omit it, or send it empty, to start from the beginning of the archive.

The token is not authenticated. The server holds no secret and does not sign it,
so it cannot tell a cursor it issued from a well-formed one you built yourself —
and it does not try. A fabricated cursor that names this archive is accepted; all
it does is move your own position in your own feed, and it reaches no message you
could not already ask for. What the server *does* refuse, with a
`400 invalid_cursor` rather than a silent restart from the beginning of the
archive, is a token it cannot read at all, one carrying a cursor format this
build does not speak, and one issued against a **different archive**.

That last one is worth planning for. A cursor is a position in one specific
archive — the archive it names, and no other — so a stored cursor keeps working
across a restart, an upgrade, or a file-level restore of the same archive, but is
rejected if you point the daemon at a different `--db` or at an archive rebuilt
from scratch. Surviving a restore is not the same as being correct across one: a
restore that rolls the archive *back* past your cursor loses what it undid, which
the [delivery contract](#delivery-contract) sets out. The rejection is the point: honouring it would resume the walk at
an unrelated position and silently never deliver the records before it. Restart
the sync from the beginning of the archive instead.

**Response:**

```json
{
  "messages": [
    {
      "id": 918,
      "source_id": 1,
      "source_message_id": "18f2c9d0a1b3",
      "conversation_id": 44,
      "message_type": "email",
      "subject": "Q4 planning",
      "snippet": "Here's the draft for Q4...",
      "sent_at": "2026-03-01T10:00:00Z",
      "size_estimate": 8412,
      "has_attachments": false,
      "attachment_count": 0,
      "content_changed_at": "2026-07-26T10:00:00.731123Z"
    }
  ],
  "count": 1,
  "has_more": true,
  "next_cursor": "1.eyJ0IjoiMjAyNi0wNy0yNlQxMDowMDowMC43MzExMjNaIiwiaSI6OTE4LCJhIjoiM2YwYjljMmU3ZDUxNDg2YWIwYzRlMjlmN2E2MWQ4NTM0YzllMGIxN2QyYTZmMzg1MTllNGM3YjBhMjVkNjMxOCJ9",
  "server_time": "2026-07-26T10:00:03.114500Z",
  "complete_through": "2026-07-26T10:00:03.114488Z"
}
```

Each row is a complete snapshot, never a patch, so an unset or empty field is
left out of the row entirely rather than sent as `null` or `""`. Read an absent
key as "empty" and overwrite whatever you had cached for it. Always present:
`id`, `source_id`, `conversation_id`, `size_estimate`, `has_attachments`,
`attachment_count`, `content_changed_at`. `messages` is always an array, never
`null`.

`server_time` is the database server's clock at the moment the page was read,
not the client's and not the daemon process's. `has_more` reports whether more
rows are already waiting; when it is `true`, request the next page immediately
instead of waiting for the next poll.

`next_cursor` is where the next request resumes. It is always present and never
empty, so you always have something to send back — on an empty page and at the
very beginning of the archive alike.

`complete_through` is how far the feed is caught up, which is a different
question from what time it is. Every change committed strictly before it, at or
after the cursor you sent, is now *reachable* — in this page, or in a page you
can fetch right now — barring the exceptions the
[delivery contract](#delivery-contract) sets out. It is **not a cursor**: the
only cursor is `next_cursor`, and a consumer that resumes from
`complete_through` while `has_more` is `true` skips every row in between.

`complete_through` is never after `server_time`, and on a healthy server trails
it by microseconds; see [Delivery contract](#delivery-contract) for when it stops
tracking and what that means. A page never reaches it — rows stamped in that same
instant are held back — so the very newest changes arrive on the following poll.

On a server that has only just started, `complete_through` can be `null`. No
bound has been established yet, so the feed is complete through nothing. It
happens when every attempt to read the bound so far has been beaten by a writer,
and it resolves by itself on the first quiet moment.
Such a page carries no rows and echoes your cursor back unchanged, so it is safe
to keep polling; the whole archive from your cursor onward is delivered once the
bound is established.

On an empty page the response normally echoes the cursor you sent. The exception
is a cursor standing *above* the database clock: it matches nothing, and echoing
it would leave you polling a feed that answers "caught up" forever while the
archive changes, so the response moves it down to the start of
`complete_through` instead — the start, so that a row stamped at that very
instant is still ahead of the cursor whatever its message id. It
lands on the commit bound rather than on the clock because a change that is
stamped but not yet committed sits between the two, and a cursor at the clock
would step over it. A server that has not yet established a bound has no
proven-safe point to move to, so there the cursor is echoed unchanged.

You can reach that state through no fault of your own — an NTP correction, a
resumed VM, a restore onto slower hardware. Being moved gets you going again; it
does not undo whatever put you above the clock. If that was a clock stepped
backwards, the changes already stamped below the new position are gone from your
walk — see the [delivery contract](#delivery-contract).

#### Walking the feed

First request — no cursor, so the feed starts at the beginning of the archive:

```bash
curl -H "X-API-Key: $MSGVAULT_API_KEY" \
  "http://localhost:8080/api/v1/messages/changes?limit=100"
```

```json
{
  "messages": ["... 100 messages ..."],
  "count": 100,
  "has_more": true,
  "next_cursor": "1.eyJ0IjoiMjAyNi0wNy0yMFQxODowNDoxMS45MDIzMTdaIiwiaSI6NDQ3MSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ",
  "server_time": "2026-07-26T10:00:03.114500Z",
  "complete_through": "2026-07-26T10:00:03.114488Z"
}
```

`has_more` is `true`, so send the next request immediately. It comes from a
one-row lookahead: the server asked for one row more than your `limit` and got
it, so a further row was already there to be read. Expect the next page to carry
rows.

Second request — the cursor from the first response, verbatim:

```bash
curl -H "X-API-Key: $MSGVAULT_API_KEY" \
  "http://localhost:8080/api/v1/messages/changes?cursor=1.eyJ0IjoiMjAyNi0wNy0yMFQxODowNDoxMS45MDIzMTdaIiwiaSI6NDQ3MSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ&limit=100"
```

```json
{
  "messages": ["... 12 messages ..."],
  "count": 12,
  "has_more": false,
  "next_cursor": "1.eyJ0IjoiMjAyNi0wNy0yNFQwOToxMjo1Ny40MTgwMDRaIiwiaSI6NTEwOSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ",
  "server_time": "2026-07-26T10:00:04.550118Z",
  "complete_through": "2026-07-26T10:00:04.550102Z"
}
```

Third request — `has_more` was `false`, so this one waits for your poll interval.
Nothing changed in between, so the page is empty and the cursor comes back
unchanged:

```bash
curl -H "X-API-Key: $MSGVAULT_API_KEY" \
  "http://localhost:8080/api/v1/messages/changes?cursor=1.eyJ0IjoiMjAyNi0wNy0yNFQwOToxMjo1Ny40MTgwMDRaIiwiaSI6NTEwOSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ&limit=100"
```

```json
{
  "messages": [],
  "count": 0,
  "has_more": false,
  "next_cursor": "1.eyJ0IjoiMjAyNi0wNy0yNFQwOToxMjo1Ny40MTgwMDRaIiwiaSI6NTEwOSwiYSI6IjNmMGI5YzJlN2Q1MTQ4NmFiMGM0ZTI5ZjdhNjFkODUzNGM5ZTBiMTdkMmE2ZjM4NTE5ZTRjN2IwYTI1ZDYzMTgifQ",
  "server_time": "2026-07-26T10:00:34.771205Z",
  "complete_through": "2026-07-26T10:00:34.771190Z"
}
```

The loop is: re-read the rows, take `next_cursor` from the response, send it as
the next request, and repeat — immediately while `has_more` is `true`, on your
poll interval when it is not. An empty page echoes the cursor you sent, so a
caught-up consumer can send the response straight back forever without
re-reading the archive.

A page that reports `has_more: true` and is then followed by an empty one is not
the ordinary case: the lookahead row existed when the first page was read, so
something has to have happened to it since. Either it was hard-deleted, or its
watermark moved — forward to at or above the next page's
[commit bound](#delivery-contract), which holds it back until the bound catches
up, or backward below the cursor you are holding, which is the clock-step case
in the delivery contract. Only the backward step loses a change you would
otherwise have been handed — a hard deletion was never reportable at all — and
none of the three is a reason to stop polling.

#### Delivery contract {#delivery-contract}

**What the feed guarantees.** After a change to a tracked column, written the
ordinary way — through the application, with the watermark left to the database
triggers — the message is delivered by following `next_cursor`, in as many
further pages as `has_more` calls for, however long its writing transaction took.
The row you get carries the message's current values as of the moment the page
was read.

**What kind of feed that makes it.** Latest-state invalidation, not an event
stream. The archive holds one mutable watermark per message and keeps no change
history, so what a page delivers is "this message is stale, re-read it" — never
how many times it changed or what it held in between. Two consequences follow,
and neither is a defect to be worked around:

* Several changes to one message between two of your polls arrive as **one** row,
  carrying the last of them. The intermediate states are gone from the archive,
  so no cursor and no re-read can recover them.
* The same change can arrive **more than once** — resuming from an earlier cursor
  re-delivers rather than erroring or skipping — and nothing distinguishes a
  re-delivery from a fresh change.

So a consumer must be able to re-read a message it already holds, and must not
count feed rows as changes or drive anything off their number.

**Every exception to that guarantee is in "What is still best-effort" below.**
That list is the complete one and the only place that enumerates them; every
other passage in this document and in the source points here instead of
restating it.

`complete_through` is what makes the guarantee hold: the page stops below the
oldest write that could still commit, not below the clock, so — for every write
the bound can see — the cursor cannot come to rest above a change that has been
stamped but not yet published. The guarantee is about the cursor, not about any
single response: keep following `next_cursor` and nothing outside the exception
list is lost; substitute `complete_through` for it and the rows between it and
`next_cursor` are lost as well.

**What it costs.** The feed cannot advance past the start of any open transaction
that holds — or is queued for — a write lock on the message table. A batch
import, a source-deletion run, or a client that wrote a message and then sat on
its `BEGIN` freezes `complete_through` for as long as that lasts. Only
transactions bearing on the message table count, and only for as long as they
last: a long read, an idle connection, a batch writing some other table, and
autovacuum's routine work do not hold the feed back, whichever database role
they belong to.

On PostgreSQL the test is the lock, not the write, and that is deliberate. A
transaction still *waiting* to acquire a write lock on `messages` — behind
someone else's `ALTER TABLE`, say — counts from the moment it queues, before it
has written anything, and it holds the bound back to its own transaction start
rather than to the moment it began waiting. Waiting to write is the state that
immediately precedes writing, and the bound has to be below a write before it
happens rather than after; counting a transaction that turns out never to write
costs a visible gap, while missing one that does costs a silently skipped row.

On SQLite, where there is no way to ask which transaction is open, any write
transaction held longer than a moment has the same effect.

During the freeze the feed keeps serving: a consumer with a backlog goes on
draining every row committed below the frozen bound, page after page, exactly as
it would otherwise. What the freeze costs is the far end of the walk — once your
cursor reaches the bound, the feed returns no rows and `has_more: false`, which
reads exactly like being caught up. The difference is that `complete_through` has
stopped tracking `server_time` while `server_time` keeps moving. **That gap is
the signal.** If it grows past a few seconds, something is holding a write
transaction open. Once it passes a minute the server logs a warning
(`message change feed is not advancing`, with the lag and a cause), repeated at
most once a minute while the condition lasts. A normal batch write causes a gap
for as long as the batch runs and then closes it; that is the mechanism working,
not a fault.

There is one case with no gap to measure and no minute to wait for: a server
that has never established a bound at all, because a write transaction was open
on every attempt since it started. `complete_through` is `null` then, so there
is no lag to report, and the same warning is logged on the very first request,
under the same once-a-minute throttle — carrying `lag: "unknown"`,
`complete_through: "none"`, and its own cause. Read it as "this server has never
seen the message table quiescent", which on a freshly started daemon usually
means an import was already running when it came up.

Read the gap as "how stale the bound is", not as "how long that transaction has
been open" — the two are the same number only on PostgreSQL, where the bound is
the open transaction's own start time. SQLite has no way to ask when another
connection's transaction began; its bound is the last moment the server caught
the database with the write lock free, so the gap measures the age of that
observation instead. A writer is genuinely in flight whenever the gap is open,
but on SQLite it may have started seconds ago and still show a gap of hours,
because time in which nothing polled the endpoint is time in which no reading was
taken. On SQLite the gap is an upper bound on the writer's age; on PostgreSQL it
is a measurement of it.

**What is still best-effort.** This list is the canonical one: every exception to
the guarantee above is here, and no other passage in this document or in the
source enumerates them. The feed does not promise that a change reaches a
consumer once and only once, it does not promise that a change reaches one
promptly, and there are surfaces it cannot see at all:

* Rows may be delivered more than once. Resuming from an earlier cursor
  re-delivers rather than erroring or skipping.
* Changes to one message coalesce. Several changes between two of your polls
  arrive as a single row carrying the last of them, because the archive holds one
  mutable watermark per message and no change history. Nothing on the wire says
  how many changes a row stands for, and the intermediate states are not stored,
  so no cursor recovers them. A consumer that must observe every individual
  change cannot get that here.
* Hard deletions are never reported. A row removed from the database outright
  leaves nothing behind for the feed to report.
* Changes to anything outside the tracked columns are not reported — see
  [What this feed does not report](#what-this-feed-does-not-report).
* **PostgreSQL only:** PostgreSQL hides other roles' connections from a role that
  is neither a superuser nor a member of `pg_read_all_stats`, and a writer the
  server cannot see cannot hold the bound back. Rather than quietly resume losing
  rows, the feed stops advancing while a hidden connection is *writing to the
  message table*: `complete_through` freezes at the last reading taken while every
  such writer was visible. So this shows up as a stalled feed, not as missing
  changes. A hidden connection that is idle, reading, or writing something else
  changes nothing. msgvault uses one role, so this arises only if something else
  writes the same message table; if it does and the feed stalls, grant the
  msgvault role `pg_read_all_stats`. Where there is nothing to fall back to — a
  server that started while a hidden writer was already inside its transaction —
  the endpoint returns `500` rather than a page and the server log names the
  grant. It clears by itself when that transaction ends.
* **PostgreSQL only:** a *prepared* transaction (two-phase commit) holds its locks
  without an owning session, so the feed cannot see when it began, and one that
  wrote to the message table and then committed could publish its change behind a
  cursor that had already moved past it. It needs `max_prepared_transactions > 0`,
  which is off by default, and msgvault never uses two-phase commit. If another
  application runs prepared transactions against the same database, reconcile
  independently rather than relying on the feed.
* **A stamp at or above the current bound waits for the bound to reach it.** The
  feed orders by the watermark stored on the row, not by the instant the write
  committed, and a page stops strictly below `complete_through`. So a row stamped
  at or above the bound is not returned, and while it is the only thing
  outstanding the feed reports `has_more: false` — which reads exactly like being
  caught up. **That is a delay, not a loss:** no cursor this page hands you gets
  past the waiting stamp, so the row arrives on the first page read after the
  bound clears it. (The moved cursor an empty page can hand you stands exactly
  *on* the bound; that is still safe, because it names the *start* of that
  instant rather than a position after some row in it, so every row stamped
  there is still ahead of it whatever its message id.) A watermark
  written by hand for a future instant waits until that instant genuinely arrives;
  a server with no bound yet is this case at its limit, selecting nothing until
  the first bound reading. A clock stepped backwards leaves you holding a cursor
  above the clock too and looks identical, but what it strands is stamped *below*
  your cursor, where no bound will ever bring it back — that is the next bullet,
  and it is a loss rather than a wait. On SQLite the column can also hold a value
  no bound will ever reach, and there the wait never ends and the change really is
  lost; that is case 2 below.
* **A database clock that steps backwards loses the changes committed below your
  cursor while it climbs back.** The feed orders by the wall-clock watermark
  stored on the row and your cursor only ever moves forward, so both rely on the
  database's clock being monotonic. When it is not — an NTP step, a resumed or
  migrated VM, a restore onto a host whose clock is behind — the writes that
  follow the step are stamped in clock time your walk has already passed, and
  every one of them stamped below the cursor you hold fails the keyset comparison
  on that poll and on every poll after it. Both backends stamp from the database
  server's own wall clock, so both are affected. **This is a loss, not a delay** —
  the row is not waiting for anything, and nothing on the wire distinguishes it
  from a healthy feed. It is bounded by the size of the step: what is lost is the
  changes committed during the stretch of clock time the step re-runs, and normal
  delivery resumes once the clock passes your cursor again. Moving a cursor that
  stands above the clock narrows the window but cannot close it, and does not
  happen at all for a consumer that polls late enough for the clock to have
  climbed back above its cursor. The cursor being opaque changes none of this. No
  cursor the server can hand you reaches back below itself; a full re-read from an
  empty cursor does, because these rows are perfectly selectable — see the
  reconciliation note at the end of this list.
* **Restoring the archive from an older snapshot loses everything the rollback
  undid.** A cursor names the archive that issued it, and a file-level restore of
  the same archive keeps that name — deliberately, because a restore of the same
  archive is exactly the case a stored cursor is meant to survive. But a cursor
  issued *after* the snapshot was taken still validates against the restored
  database, and everything the rollback undid now sits below it: a row reverted to
  an older watermark fails the keyset comparison on that poll and on every poll
  after it, and a row the rollback removed produces no event at all. Your mirror
  goes on holding content the archive no longer has. **This is a loss, not a
  delay**, and nothing on the wire distinguishes it from a healthy feed — the same
  shape as the backward clock step above, and the same underlying cause: the walk
  is ordered by a wall-clock watermark with no monotonic sequence behind it.
  Copying the archive **file** — `cp`, a filesystem snapshot, a backup restored
  under a different path — forks it the same way and keeps the original's
  identity, so a cursor from either copy is accepted by the other even as the two
  drift apart. A subset built with `msgvault create-subset` does not: it is a new
  database with an identity of its own, so a cursor issued against the original
  is rejected there with `400 invalid_cursor`, and a consumer of the subset
  starts from an empty cursor. The repair is a full
  re-read from an empty cursor: it restores the tracked fields of every row the
  restored archive still has, though a row the rollback removed is
  indistinguishable from a hard deletion — see the reconciliation note at the end
  of this list. **After restoring an archive from a snapshot, restart your
  consumers from an empty cursor.**
* **A `NULL` watermark is invisible to the feed, on either backend.** The page
  compares `content_changed_at` against both of its bounds and a `NULL` satisfies
  neither, so the row is not returned from any cursor. No write path produces one
  — every writer stamps the column and the first-run backfill fills in a database
  that predates it — so this takes direct SQL, and whatever content change the
  same statement made goes unreported with it. The next ordinary change to one of
  the row's tracked columns stamps a fresh watermark and the row rejoins the feed
  carrying its current content. If no such change ever comes, it stays invisible.
* **SQLite only:** direct SQL can store a `NULL` or malformed
  `content_changed_at`; normal write paths do not. The feed returns an error for
  a `NULL` watermark or a malformed value selected by the page instead of
  inventing a timestamp and moving the cursor. SQLite orders non-date values by
  their raw type and text, so a corrupt value outside the requested range can
  still be unreachable. Repair the stored watermark directly before resuming.
* A consumer that must not diverge from the archive should still reconcile
  periodically — for example, a scheduled full re-read from an empty cursor. It
  re-delivers every row the feed can select, so it restores the tracked fields of a
  malformed row that remains selectable and a row stranded by a backward clock
  step. What it does not do: it cannot reach a malformed watermark that sorts
  outside the feed range, and it recovers nothing outside the tracked columns.
  Nor does it identify a hard
  deletion: a row your mirror has and the walk never returns is simply a row no
  cursor can reach, which is what a hard deletion and an out-of-range corrupt
  watermark both look like from outside. Separating them takes the same direct read
  — a row absent from a direct `SELECT` over `messages` was hard-deleted, and one
  still there has a watermark the feed cannot reach.

Re-reading an overlapping window is safe, so a consumer that wants extra margin
can keep an older `next_cursor` of its own and resume from that instead, trading
duplicates for margin. Build that margin out of a cursor the feed handed you,
never out of `complete_through`.

#### What this feed does not report {#what-this-feed-does-not-report}

The watermark is maintained by triggers on the `messages` table and on message
bodies. The trigger on `messages` covers every column the feed returns, plus the
sender pointer and the platform metadata payload, which it does not return.
Everything else a message has — including the remaining columns of `messages` —
is outside it:

| Surface | Where it lives | Does changing it move the message into the feed? |
|---|---|---|
| Labels | `message_labels` | No — and label re-sync is the most frequent change in a mail archive |
| Recipients (to/cc/bcc) | `message_recipients` | No |
| Attachment metadata (filenames, hashes, sizes, storage paths) | `attachments` | No — but `has_attachments` and `attachment_count` are message columns, so an attachment set that changes those does appear |
| Raw MIME | `message_raw` | No — including raw MIME added after the message row |
| Read state and platform flags (`is_read`, `read_at`, `is_edited`, `archived_at`) | `messages` | No |
| Threading and identity pointers (`reply_to_message_id`, `rfc822_message_id`) | `messages` | No |
| Conversation metadata (thread title) | `conversations` | No |
| Message body | `message_bodies` | Yes for an added or edited body (see the deletion row below), but the feed reports only `snippet`; fetch the body from `/api/v1/messages/{id}` |
| Message body deletion | `message_bodies` | No — the body triggers fire on INSERT and UPDATE only. Adding or editing a body moves the watermark; deleting one does not |
| Sender *pointer* (`messages.sender_id`) | `messages` | Yes — but the feed does not return it, and only the pointer is tracked. Renaming or correcting the participant it points at changes `participants`, not `sender_id`, so it does **not** move the watermark |
| Platform metadata payload (`metadata`) | `messages` | Yes — but no endpoint returns it, so the wake-up is all you get. It is tracked so that a metadata change still invalidates your cached copy of the fields that *are* returned |

A consumer that caches any of the untracked surfaces has to refresh them on its
own schedule; nothing in this feed will invalidate them.

#### Errors

| Status | `error` | Meaning |
|---|---|---|
| 400 | `invalid_cursor`, `invalid_limit` | A parameter is present but could not be used. A cursor this API cannot use — damaged in transit, left over from an incompatible server version, or issued against a different archive — is rejected rather than read as the beginning of the archive; the message says which of those it was, and in each case the only repair is to restart the sync from the beginning. A hand-built cursor is *not* rejected on those grounds alone: the token is unauthenticated, so a well-formed one naming this archive is honoured (see [the cursor is opaque](#get-apiv1messageschanges)). An *empty* value (`?cursor=`) is read as absent, exactly like omitting the parameter, so `?cursor=&limit=10` starts from the beginning of the archive |
| 401 | `unauthorized` | No API key, or one this server rejects. Every request to this endpoint is authenticated the same way the rest of the API is (see [Authentication](#authentication)) |
| 429 | `rate_limit_exceeded` | The caller has outrun the [rate limit](#rate-limiting); `Retry-After` says how long to wait. Worth handling here more than anywhere else, because polling is what this endpoint is for. Polling faster does not make the feed advance sooner — `complete_through` moves with the database, not with your poll rate — and on SQLite it costs the importer write throughput |
| 500 | `internal_error` | The watermark query failed. **PostgreSQL only:** this is also how the no-visibility-floor case above surfaces — a server that has never once read the bound while every writer of the message table was visible has no safe bound to report, so the endpoint refuses rather than returning a page that would step your cursor over an invisible writer's change. The server log names the `pg_read_all_stats` grant that fixes it; it also clears by itself when the foreign transaction ends |
| 503 | `feature_unavailable`, `query_timeout` | `feature_unavailable`: the configured store cannot answer the watermark query, or cannot say which archive it is, so it cannot issue a cursor you could safely resume from. `query_timeout`: the request outran the server's per-request time limit. Both are retryable from the same cursor. (A request the caller abandoned is answered `query_canceled` on the same status, which by definition nobody is left to read.) |

---

### Message details {#get-apiv1messagesid}

**Endpoint:** `GET /api/v1/messages/{id}`

Full message details including body and attachment metadata.

**Response:**

```json
{
  "id": 12345,
  "subject": "Q4 Planning",
  "message_type": "email",
  "from": "alice@example.com",
  "to": ["bob@example.com"],
  "cc": ["carol@example.com"],
  "bcc": ["dave@example.com"],
  "sent_at": "2024-10-15T09:30:00Z",
  "snippet": "Here's the draft for Q4...",
  "labels": ["INBOX", "IMPORTANT"],
  "has_attachments": true,
  "size_bytes": 52480,
  "body": "<plain text body, or HTML when no plain text body exists>",
  "body_html": "<html><body><p>Full HTML body</p></body></html>",
  "attachments": [
    {
      "id": 987,
      "filename": "q4-plan.pdf",
      "mime_type": "application/pdf",
      "size_bytes": 204800,
      "content_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    }
  ]
}
```

The `cc`, `bcc`, and `body_html` fields are included only when present. `body` is the plain-text body when one exists; for HTML-only messages, it falls back to the HTML body so callers still receive message content.

---

### Attachment metadata {#get-apiv1attachmentsid}

**Endpoint:** `GET /api/v1/attachments/{id}`

Returns metadata for the numeric attachment ID exposed by message details.
The response includes the SHA-256 `content_hash` used by the content endpoint:

```json
{
  "id": 987,
  "filename": "q4-plan.pdf",
  "mime_type": "application/pdf",
  "size_bytes": 204800,
  "content_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
```

---

### Attachment content by hash {#get-apiv1attachmentshashcontent}

**Endpoint:** `GET /api/v1/attachments/{hash}/content`

Streams the raw bytes of an archived attachment from either loose or packed
storage. `{hash}` must be the 64-character SHA-256 `content_hash` returned by
message details or the attachment metadata endpoint.

```bash
curl -H "X-API-Key: $MSGVAULT_API_KEY" \
  --output attachment.bin \
  "http://localhost:8080/api/v1/attachments/$HASH/content"
```

Successful responses set the archived MIME type as `Content-Type` (falling
back to `application/octet-stream`), the archived filename in
`Content-Disposition`, `Content-Length`, and `X-Content-Type-Options: nosniff`.
An invalid hash returns `400`; a hash with no attachment row, or content that
is no longer available, returns `404`. The generated Go client's
`GetAttachmentContent` wrapper verifies the returned bytes against the
requested hash before returning them.

---

### Inline image content {#get-apiv1messagesidinlinecidcontent-id}

**Endpoint:** `GET /api/v1/messages/{id}/inline?cid=<content-id>`

Fetch an inline MIME image part by content ID. This is intended for rendering `cid:` images referenced by `body_html`.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `cid` | string | (required) | MIME `Content-ID` to fetch |

Only inline image parts are served. SVG images and non-image inline parts are rejected with `415 unsupported_type`. If the query engine cannot fetch raw MIME, the endpoint returns `501 not_supported`.

Successful responses set:

| Header | Description |
|---|---|
| `Content-Type` | Inline image content type |
| `Content-Disposition` | `inline` |
| `Cache-Control` | `private, max-age=31536000, immutable` |
| `X-Content-Type-Options` | `nosniff` |

---

### Search messages {#get-apiv1search}

**Endpoint:** `GET /api/v1/search`

Search messages. The default mode is full-text search (FTS5 with
LIKE fallback). When the server is configured for vector search,
`mode=vector` runs semantic-only search and `mode=hybrid` fuses BM25
and vector ranking via Reciprocal Rank Fusion.

`mode=vector` and `mode=hybrid` both require at least one free-text
term in `q` — the free text is what gets embedded as the query
vector. Operator-only queries such as `q=from:alice` have nothing to
embed and return `400 missing_free_text`; route filter-only requests
to `mode=fts` instead.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `q` | string | (required) | Search query |
| `mode` | enum | `fts` | `fts`, `vector`, or `hybrid` |
| `page` | int | `1` | Page number (FTS only — vector/hybrid reject `page>1`) |
| `page_size` | int | `20` | Results per page (max 100 for FTS, max `[vector].search.max_page_size_hybrid` for vector/hybrid) |
| `message_type` | string | — | Message-type filter; repeat or comma-separate for multiple values |
| `account` | string | — | Restrict to one account/source |
| `collection` | string | — | Restrict to one collection |
| `explain` | 0/1 | `0` | When `1` and `mode=vector|hybrid`, include per-signal scores |

`message_type` uses the same values as local search: `email`,
`calendar_event`, `meeting_transcript`, `beeper`, `teams`, `discord`, `sms`,
`mms`, `whatsapp`, `imessage`, `fbmessenger`, `synctech_sms_call`,
`google_voice_text`, `google_voice_call`, and `google_voice_voicemail`. The
query string can also carry `message_type:` / `message_type=` operators inside
`q`.

**Response (mode=fts, default):**

```json
{
  "query": "quarterly report",
  "total": 23,
  "page": 1,
  "page_size": 20,
  "messages": [
    {
      "id": 12345,
      "subject": "Q4 Planning",
      "message_type": "email",
      "from": "alice@example.com",
      "to": ["bob@example.com"],
      "cc": ["carol@example.com"],
      "sent_at": "2024-10-15T09:30:00Z",
      "snippet": "Here's the draft for Q4...",
      "labels": ["INBOX", "IMPORTANT"],
      "has_attachments": true,
      "size_bytes": 52480
    }
  ]
}
```

**Response (mode=vector or mode=hybrid):**

```json
{
  "query": "when is the planning offsite",
  "mode": "hybrid",
  "returned": 12,
  "pool_saturated": false,
  "generation": {
      "id": 3,
      "model": "nomic-embed-text-v1.5",
      "dimension": 768,
      "fingerprint": "nomic-embed-text-v1.5:768:p1-111111:c32768:e1",
      "state": "active"
    },
  "took_ms": 84,
  "results": [
    {
      "id": 12345,
      "subject": "Q2 planning offsite agenda",
      "message_type": "email",
      "from": "alice@example.com",
      "to": ["team@example.com"],
      "sent_at": "2024-01-15T10:30:00Z",
      "snippet": "Proposed agenda for the offsite on...",
      "labels": ["INBOX"],
      "has_attachments": false,
      "size_bytes": 2048
    }
  ]
}
```

Vector and hybrid responses expose `returned` instead of `total`
(ANN search does not have a meaningful total count), add a
`generation` sub-object naming the index generation that answered
the query, and include `took_ms`. The top-level `results` array
replaces `messages`. `pool_saturated` is true when a vector or BM25
candidate pool hit its configured cap (or pure vector search returned
as many hits as requested), hinting that increasing the limit or
narrowing the query may expose more relevant results.

When `explain=1`, each element of `results` carries an extra `score`
object exposing the fused-score components:

```json
{
  "id": 12345,
  "subject": "...",
  "score": {
    "rrf": 0.032,
    "bm25": 7.4,
    "vector": 0.82,
    "subject_boosted": true
  }
}
```

`bm25` and `vector` are omitted when the message did not appear in
that signal (BM25 missed it or the ANN pool did not include it).
`rrf` is omitted in `mode=vector` (only one signal — there is
nothing to fuse). `subject_boosted` is true when the subject-line
boost was applied.

See [Searching](/docs/usage/searching/) for the full query syntax
reference and [Vector Search](/docs/usage/vector-search/) for vector /
hybrid setup.

---

### Accounts summary {#get-apiv1accounts}

**Endpoint:** `GET /api/v1/accounts`

List configured accounts with sync status.

**Response:**

```json
{
  "accounts": [
    {
      "email": "you@gmail.com",
      "display_name": "Your Name",
      "last_sync_at": "2024-10-15T08:00:00Z",
      "next_sync_at": "2024-10-15T09:00:00Z",
      "schedule": "0 * * * *",
      "enabled": true
    }
  ]
}
```

---

### Source sync status {#get-apiv1sourcesstatus}

**Endpoint:** `GET /api/v1/sources/status`

Read sync status for all sources, or filter to one source type with
`source_type`. This endpoint is useful for dashboards and remote
deployments because it exposes active, latest, and last-successful
sync runs without triggering a sync.

| Parameter | Type | Default | Description |
|---|---|---|---|
| `source_type` | string | — | Optional source-type filter, for example `gmail`, `imap`, or `synctech_sms` |

**Response:**

```json
{
  "sources": [
    {
      "id": 1,
      "source_type": "gmail",
      "identifier": "you@gmail.com",
      "display_name": "Personal Gmail",
      "last_sync_at": "2026-06-18T13:02:11Z",
      "updated_at": "2026-06-18T13:02:11Z",
      "active_sync": null,
      "latest_sync": {
        "id": 42,
        "source_id": 1,
        "started_at": "2026-06-18T13:00:00Z",
        "completed_at": "2026-06-18T13:02:11Z",
        "status": "completed",
        "messages_processed": 250,
        "messages_added": 12,
        "messages_updated": 3,
        "errors_count": 1,
        "error_message": null,
        "cursor_before": "745391",
        "cursor_after": "745406",
        "skipped_count": 2,
        "item_errors": [
          {
            "source_message_id": "18fedcba12345678",
            "phase": "ingest",
            "error_kind": "ingest_error",
            "error_message": "parse MIME: malformed header",
            "created_at": "2026-06-18T13:01:44Z"
          }
        ]
      },
      "last_successful_sync": {
        "id": 41,
        "source_id": 1,
        "started_at": "2026-06-18T12:00:00Z",
        "completed_at": "2026-06-18T12:01:18Z",
        "status": "completed",
        "messages_processed": 33,
        "messages_added": 0,
        "messages_updated": 1,
        "errors_count": 0,
        "error_message": null
      }
    }
  ]
}
```

`active_sync`, `latest_sync`, and `last_successful_sync` are `null`
when no matching run exists. `item_errors` contains up to the 10 most
recent per-item errors for that run. `skipped_count` counts expected
per-item skips, such as Gmail messages that disappeared between list
and fetch. `error_message` is `null` unless the sync run itself failed
with a run-level error.

---

### Import a meeting {#post-apiv1importmeeting}

**Endpoint:** `POST /api/v1/import/meeting`

Import one provider-neutral meeting without configuring a provider account.
The authenticated, on-demand endpoint accepts at most 16 MiB of JSON and is
idempotent on `source.identifier` plus `meeting.external_id`: the first import
returns `201` / `created`, and retries return `200` / `updated`.

```bash
curl http://localhost:8080/api/v1/import/meeting \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  --data '{
    "source": {
      "identifier": "local-meetings",
      "display_name": "Local Meetings",
      "account_email": "you@example.com"
    },
    "meeting": {
      "external_id": "planning-2026-07-29",
      "title": "Planning",
      "started_at": "2026-07-29T09:00:00-04:00",
      "summary_text": "Reviewed the launch plan.",
      "organizer": {"name": "You", "email": "you@example.com"},
      "attendees": [{"name": "Teammate", "email": "teammate@example.com"}]
    }
  }'
```

```json
{
  "status": "created",
  "source_id": 12,
  "message_id": 901,
  "source_message_id": "meeting:planning-2026-07-29"
}
```

Timestamps must be RFC 3339 values with explicit offsets. A meeting must
contain at least one non-empty `summary_markdown`, `summary_text`, `transcript`,
or `transcript_segments` value; plain and segmented transcripts are mutually
exclusive. Segment offsets must be finite, non-negative, and non-decreasing.
Unknown fields are rejected except within `meeting.metadata`.

---

### OAuth token exchange {#post-apiv1authtokenemail}

**Endpoint:** `POST /api/v1/auth/token/{email}`

Upload an OAuth token JSON file generated by a local `msgvault` client.

This endpoint is used by `msgvault export-token` during remote/headless deployment workflows.

**Request headers:**

- `X-API-Key: <api-key>` (or any supported auth header)
- `Content-Type: application/json`

**Example request body (`/api/v1/auth/token/you@gmail.com`):**

```json
{
  "access_token": "ya29...",
  "token_type": "Bearer",
  "refresh_token": "1//0g...",
  "expiry": "2024-12-31T23:59:59Z",
  "scopes": ["https://www.googleapis.com/auth/gmail.modify"]
}
```

**Successful response (`201 Created`):**

```json
{
  "status": "created",
  "message": "Token saved for you@gmail.com"
}
```

---

### Create account {#post-apiv1accounts}

**Endpoint:** `POST /api/v1/accounts`

Register or ensure an account is scheduled for sync on the remote server.

`msgvault export-token` posts to this endpoint automatically after uploading a token.

```json
{
  "email": "you@gmail.com",
  "schedule": "0 2 * * *"
}
```

The `enabled` field is always set to `true` server-side.

**If the account already exists (200 OK):**

```json
{
  "status": "exists",
  "message": "Account already configured for you@gmail.com"
}
```

**On success (201 Created):**

```json
{
  "status": "created",
  "message": "Account added for you@gmail.com"
}
```

---

### Start account sync {#post-apiv1syncaccount}

**Endpoint:** `POST /api/v1/sync/{account}`

Trigger a manual sync for an account. Returns immediately with a 202 status while the sync runs in the background.

**Response (202 Accepted):**

```json
{
  "status": "accepted",
  "message": "Sync started for you@gmail.com"
}
```

---

### Scheduler status {#get-apiv1schedulerstatus}

**Endpoint:** `GET /api/v1/scheduler/status`

Scheduler state and per-account schedule details.

**Response:**

```json
{
  "running": true,
  "accounts": [
    {
      "email": "you@gmail.com",
      "running": false,
      "last_run": "2024-10-15T08:00:00Z",
      "next_run": "2024-10-15T09:00:00Z",
      "schedule": "0 * * * *"
    }
  ]
}
```

---

### Preflight an analytical selection {#post-apiv1explorepreflight}

**Endpoint:** `POST /api/v1/explore/preflight`

Validates a revision-pinned selection of canonical archive entries and issues
the single-use `operation_token` required to stage that selection for
deletion. Selections are built from the analytical explore endpoints
(`POST /api/v1/explore` and `POST /api/v1/explore/groups`), whose responses
carry the `cache_revision`, `search_provenance`, and — for semantic/hybrid
search — `candidate_snapshot_id` values a selection must echo back. The full
explore contract is in the generated OpenAPI document (`/openapi.json`).

**Request:**

```json
{
  "selection": {
    "mode": "all_matching",
    "predicate": {
      "filters": [
        { "dimension": "domain", "values": ["example.com"] },
        { "dimension": "before", "values": ["2020-01-01"] }
      ]
    },
    "cache_revision": "<cache_revision from the explore response>",
    "search_provenance": {}
  }
}
```

`selection` fields:

| Field | Description |
|---|---|
| `mode` | `all_matching` selects everything the predicate matches; `explicit` selects only the listed `row_keys` |
| `predicate` | The explore request being acted on: `filters` (dimensions `source`, `participant`, `domain`, `message_type`, `after`, `before`, `deletion`), plus `query` / `search_mode` for search-backed selections. Cursors are rejected |
| `row_keys` | Explore row keys (the `key` field of explore rows) to include; required when `mode` is `explicit` |
| `exclusions` | Row keys to exclude from an `all_matching` selection |
| `cache_revision` | Required. The `cache_revision` from the explore response being reviewed |
| `search_provenance` | The `search_provenance` from the explore response; checked when the predicate has a `search_mode` |
| `candidate_snapshot_id` | The snapshot ID from the explore response; required for `semantic` / `hybrid` predicates (`400 candidate_snapshot_required` otherwise) |

**Response (200):**

```json
{
  "count": 1234,
  "estimated_bytes": 52428800,
  "cache_revision": "<current cache revision>",
  "search_provenance": {},
  "unavailable_actions": [
    { "action": "export", "reason": "browser_export_requires_single_message" },
    { "action": "open_in_source", "reason": "trusted_source_link_unavailable" }
  ],
  "action_targets": [],
  "operation_token": "<operation_token>",
  "expires_at": "2026-07-06T15:35:00Z"
}
```

`unavailable_actions` lists actions this selection does not support. A
`stage_deletion` entry means the selection includes items that cannot be
deleted from their source; staging it would fail with
`409 selection_not_deletable`.

The `operation_token` is bound to this exact selection, its match count, and
the current cache revision. It expires at `expires_at` (five minutes after
issue) and is single-use: staging a manifest consumes it, while a staging
attempt that fails before the manifest persists leaves it valid for retry. A
reused or expired token is rejected with `409 operation_token_invalid`.

Malformed selections return `400` with `invalid_selection` (bad `mode`,
missing `cache_revision`, or `mode: "explicit"` without `row_keys`) or
`invalid_selection_predicate`. If the analytical cache or search index changes
after the explore response was produced, preflight fails with
`409 archive_revision_changed` or `409 search_revision_changed` — re-run
explore and preflight against the new revision. Staging repeats all of these
checks.

---

### Stage messages for deletion {#post-apiv1deletions}

**Endpoint:** `POST /api/v1/deletions`

Stages messages for deletion by writing a pending deletion manifest. Staging
never touches Gmail — execution remains exclusively the `delete-staged`
command. The endpoint accepts three request shapes:

- **Dry run** — `"dry_run": true` with a `filter` and/or `message_ids`
  resolves and counts without staging anything.
- **Explicit message IDs** — `message_ids` alone (internal IDs as returned by
  `/messages` and `/search`) stages directly.
- **Preflighted selection** — a `selection` plus the `operation_token` issued
  by [`/api/v1/explore/preflight`](#post-apiv1explorepreflight). This is the
  only way to stage a filter-based deletion: a non-dry-run request with a
  `filter` is rejected with `428 preflight_required`.

In every shape, resolution is restricted to live Gmail-source messages, and a
staged manifest executes against a single mailbox: the selection must resolve
to exactly one Gmail source. The durable source tuple (`type` and `identifier`,
plus the local numeric `id`) is stamped on the manifest and reported as
`source` in dry-run and created responses; `delete-staged` resolves that tuple
again before it claims the manifest. The account identifier is also reported;
selections spanning multiple accounts are rejected with
`400 multi_account_selection` — scope the request (for example with
`filter.source_id` or a `source` filter dimension) or stage per account.
Unknown JSON fields are rejected with `400 invalid_request` so a typo'd filter
key cannot silently widen the selection, and requests with no criteria at all
are rejected with `400 empty_filter`, so the entire archive cannot be staged.

#### Dry run

```json
{
  "filter": {
    "sender": "newsletter@example.com",
    "source_id": 1,
    "after": "2019-01-01",
    "before": "2020-01-01"
  },
  "dry_run": true
}
```

Supported `filter` fields: `sender`, `sender_name`, `recipient`,
`recipient_name`, `domain`, `label` (strings), `source_id` (integer), and
`after` / `before` (RFC3339 or `YYYY-MM-DD` dates). The filter can be combined
with `message_ids`; matches are unioned and deduplicated. The server resolves
the request and returns `200` without writing anything:

```json
{
  "dry_run": true,
  "message_count": 1234,
  "account": "you@gmail.com",
  "source": {"id": 1, "type": "gmail", "identifier": "you@gmail.com"},
  "sample_gmail_ids": ["18c2f5a1b2c3d4e5", "..."]
}
```

#### Staging explicit message IDs

A non-dry-run request whose only criterion is `message_ids` stages directly —
the IDs are already an explicit, reviewed list:

```json
{
  "message_ids": [123, 456],
  "description": "old newsletters"
}
```

A pending manifest is written and `201` returned:

```json
{
  "dry_run": false,
  "message_count": 2,
  "account": "you@gmail.com",
  "source": {"id": 1, "type": "gmail", "identifier": "you@gmail.com"},
  "id": "20260706-153000-old-newsletters-a1b2",
  "status": "pending"
}
```

#### Staging a preflighted selection

Filter-based staging goes through preflight so the reviewed selection — not a
re-evaluated filter — is what gets staged:

1. `POST /api/v1/explore` with the predicate; review the rows and note
   `cache_revision` (plus `search_provenance` and `candidate_snapshot_id` for
   search-backed predicates).
2. `POST /api/v1/explore/preflight` with the `selection`; review `count` and
   `estimated_bytes`, and keep the `operation_token`.
3. `POST /api/v1/deletions` with the same `selection` and the token:

```json
{
  "selection": {
    "mode": "all_matching",
    "predicate": {
      "filters": [
        { "dimension": "domain", "values": ["example.com"] },
        { "dimension": "before", "values": ["2020-01-01"] }
      ]
    },
    "cache_revision": "<cache_revision from the explore response>",
    "search_provenance": {}
  },
  "operation_token": "<operation_token from preflight>",
  "description": "old example.com mail"
}
```

The response is the same `201` manifest shape as above. `selection` cannot be
combined with `filter` or `message_ids` (`400 invalid_request`). The server
re-validates the selection against the preflight grant before staging: the
selection, its match count, and the cache/search revisions must be unchanged,
and every selected item must be deletable. `"dry_run": true` may be combined
with a selection to preview the resolved count and sample; the token is
validated but not consumed.

#### Errors

| Status | Code | When |
|---|---|---|
| `400` | `empty_filter` | No filter criterion, `message_ids` entry, or `selection` |
| `400` | `invalid_request` | Unknown JSON fields, or `selection` combined with `filter` / `message_ids` |
| `400` | `invalid_date` | `after` / `before` is not RFC3339 or `YYYY-MM-DD` |
| `400` | `no_messages_matched` | The criteria or reviewed selection match nothing |
| `400` | `multi_account_selection` | The selection spans more than one Gmail account |
| `428` | `preflight_required` | Non-dry-run `filter` request without a preflighted selection, or `selection` without `operation_token` |
| `409` | `operation_token_invalid` | Token expired, already used, or does not match the selection, count, and revision |
| `409` | `archive_revision_changed` | The analytical cache changed since preflight |
| `409` | `search_revision_changed` | The search index revision changed since preflight |
| `409` | `selection_not_deletable` | The selection contains items that cannot be deleted from their source |
| `409` | `selection_changed` | The matching messages changed between preflight and staging |

---

### List staged deletions {#get-apiv1deletions}

**Endpoint:** `GET /api/v1/deletions`

Lists deletion manifests, newest first. The optional `status` parameter
filters by one of `pending`, `in_progress`, `completed`, `failed`, or
`cancelled`; omitting it returns all statuses.

**Response:**

```json
{
  "manifests": [
    {
      "id": "20260706-153000-old-newsletters-a1b2",
      "status": "pending",
      "created_at": "2026-07-06T15:30:00Z",
      "created_by": "api",
      "description": "old newsletters",
      "message_count": 1234
    }
  ]
}
```

---

### Cancel a staged deletion {#delete-apiv1deletionsid}

**Endpoint:** `DELETE /api/v1/deletions/{id}`

Cancels (unstages) a pending or in-progress deletion manifest. Returns `404`
for an unknown manifest ID and `409 not_cancellable` for manifests that are
already completed, failed, or cancelled.

**Response:**

```json
{
  "id": "20260706-153000-old-newsletters-a1b2",
  "status": "cancelled"
}
```

## Rate Limiting

The API enforces rate limiting of 10 requests per second per client IP, with a burst allowance of 20 requests. When the limit is exceeded, the server responds with HTTP 429 and includes a `Retry-After` header indicating how long to wait before retrying.

## CORS

Cross-Origin Resource Sharing is disabled by default. To allow browser-based clients, configure allowed origins in your `config.toml`:

```toml
[server]
cors_origins = ["http://localhost:3000", "https://my-dashboard.example.com"]
cors_credentials = true
cors_max_age = 3600
```

## Scheduled Sync

The server can automatically sync Gmail, IMAP, Microsoft Teams, and registered
Discord guild sources on a cron-based schedule. Add `[[accounts]]` sections to
your config:

```toml
[[accounts]]
email = "you@gmail.com"
schedule = "0 * * * *"    # every hour
enabled = true

[[accounts]]
email = "user@example.com"
schedule = "*/15 * * * *" # every 15 minutes
enabled = true

[[accounts]]
email = "123456789012345678" # exact registered Discord guild ID
schedule = "*/30 * * * *"
enabled = true
```

The scheduler starts automatically with `msgvault serve` when account schedules
are configured. Discord schedules require the exact guild ID because display
names are mutable and can be duplicated. Use `/api/v1/scheduler/status` to
monitor schedule state and `/api/v1/sync/{account}` to trigger supported
account syncs outside the schedule. Discord's dedicated manual command is
`msgvault sync-discord`.

The same HTTP server backs configured remote CLI access and the local background daemon used by archive-access CLI commands. In API schema 2.4.0, the CLI sync transport accepts `source_id`, allowing `sync` and `sync-full` to select one source exactly even when several source types share an identifier.

!!! note
    Gmail accounts must have completed an initial `msgvault sync-full` before
    scheduled incremental sync. IMAP schedules scan the mailbox and skip known
    messages. Teams and Discord importers detect and checkpoint their own
    first-run history backfills.

`msgvault serve` also runs scheduled SyncTech SMS Backup & Restore Drive sources configured under `[[synctech_sms.sources]]`; see [Configuration](/docs/configuration/#synctech-sms-sources).

## Security Model

The server is designed for local use:

- **Loopback-only by default.** The default bind address is `127.0.0.1`, restricting access to the local machine.
- **API key required for non-loopback.** If you bind to a non-loopback address (e.g., `0.0.0.0`), the server requires `api_key` to be set and will refuse to start without it.
- **Opt-in for insecure binding.** To bind to a non-loopback address without an API key (not recommended), set `allow_insecure = true`.

!!! warning
    Exposing the server on a network without authentication gives anyone on that network access to your entire email archive. Always set an `api_key` when binding to non-loopback addresses.

## Configuration Reference

All server settings go in the `[server]` section of `config.toml`. Account schedules use `[[accounts]]` sections.

### `[server]`

| Key | Default | Description |
|---|---|---|
| `api_port` | `0` (auto-select) | Port the server listens on; `0` picks an open port at startup and clients discover it automatically. Set a fixed port for remote/NAS deployments. |
| `bind_addr` | `127.0.0.1` | Bind address |
| `api_key` | — | API key for authentication |
| `allow_insecure` | `false` | Allow non-loopback binding without `api_key` |
| `cors_origins` | `[]` | Allowed CORS origins |
| `cors_credentials` | `false` | Allow credentials in CORS requests |
| `cors_max_age` | `0` | CORS preflight cache duration in seconds (defaults to `86400` when `cors_origins` is set) |
| `daemon_idle_timeout` | `20m` | Idle timeout for lifecycle-managed background daemons; set to `"0s"` to disable |
| `daemon_auto_restart` | `newer` | Local daemon restart policy when the CLI finds a different daemon binary version: `newer`, `never`, or `always` |

`daemon_idle_timeout` only affects daemons started by `msgvault daemon start` or auto-started by a CLI command. A foreground `msgvault serve` runs until interrupted. `MSGVAULT_DAEMON_IDLE_TIMEOUT` can override the configured timeout for lifecycle-managed background daemons.

`daemon_auto_restart` only affects local lifecycle-managed daemons. The default `newer` replaces older compatible daemons with the current CLI binary, `never` leaves restarts to an external supervisor, and `always` restarts on any safe version mismatch. Remote servers are never restarted by CLI clients.

### `[analytics]`

| Key | Default | Description |
|---|---|---|
| `engine` | `auto` | Aggregate engine for Web UI, TUI, and aggregate HTTP views: `auto`, `sql`, or `duckdb` |
| `auto_build_cache` | `true` | Build stale or missing Parquet cache files during daemon startup and after scheduled syncs; `false` skips both automatic paths |
| `min_rebuild_interval` | `0s` | Minimum age of a usable cache before a scheduled sync may rebuild it; zero preserves rebuilding after each sync |

`engine = "sql"` forces live SQL for aggregate views. `engine = "duckdb"`
requires a usable Parquet cache and keeps analytics unavailable until it is
ready; a build or open failure is fatal rather than a silent SQL fallback.
`auto_build_cache = false` leaves cache rebuilds to explicit
`msgvault build-cache` runs. These settings replace the TUI/MCP analytics flags
deprecated in 0.17.0; see [Configuration: analytics](/docs/configuration/#analytics).

`min_rebuild_interval` limits only automatic post-sync rebuilds. Explicit
builds, startup maintenance, query-required builds, and unusable-cache recovery
remain immediate. On a continuously changing archive, Parquet analytics can lag
SQLite by approximately the interval plus cache build time. Cache builder memory
and temporary disk usage scale with archive size, so the interval can prevent
repeated archive-scale work on frequently synced archives. Changes under
`[analytics]` take effect after the daemon restarts.

### `[[accounts]]`

| Key | Default | Description |
|---|---|---|
| `email` | (required) | Gmail/IMAP/Teams identifier or display name, or exact Discord guild ID |
| `schedule` | — | Cron expression for sync schedule |
| `enabled` | `true` | Whether scheduled sync is active |

See the [Configuration](/docs/configuration/) page for the full config file reference.
