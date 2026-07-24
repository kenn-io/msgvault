---
title: Web UI & API Server
description: Daemon-served analytical Web UI and REST API for your msgvault archive, with optional background sync scheduling.
---


## Overview

`msgvault serve` starts an HTTP server that exposes your archive through the
first-party Web UI at `/` and a REST API under `/api`. It optionally runs a
background sync scheduler to keep accounts up to date on a cron-based schedule.
The complete UI is embedded in the release binary; see [Web UI](/web-ui/) for
browser login, secure remote deployment, search states, and keyboard controls.

The API is registered through Huma and exposes a generated OpenAPI document at `/openapi.json`. You can also run `msgvault openapi` to print the same checked-in contract without starting a daemon or opening the archive database. The OpenAPI `info.version` is the API schema version used for client/server compatibility; the running daemon binary version is exposed separately in the generated document metadata. The API queries the same archive database and attachment store as the CLI, Web UI, and TUI. SQLite is the default archive database; PostgreSQL is supported when `[data].database_url` is a PostgreSQL DSN. Keyword search and ordinary archive reads stay local to that database. If vector search is enabled, semantic and hybrid search also call the embedding endpoint configured in `[vector.embeddings]`. The server is designed for interactive archive use, local integrations, dashboards, and automation scripts.

Go integrations can use the generated client in `pkg/client`. The wrapper
handles msgvault-specific response details such as deletion staging dry-runs
returning `200` while created manifests return `201`.

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

### Health check {#get-health}

**Endpoint:** `GET /health`

Health check endpoint. Does not require authentication.

**Response:**

```json
{"status": "ok"}
```

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
[Vector Search](/usage/vector-search/) for the end-to-end workflow.

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

See [Searching](/usage/searching/) for the full query syntax
reference and [Vector Search](/usage/vector-search/) for vector /
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
  "operation_token": "3q2fF0kaVYlIuXQ8yYb-KzGH5mo2vNc1",
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
to exactly one Gmail account. The resolved account is stamped on the manifest
(`delete-staged` uses it to pick the mailbox) and reported in the response;
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

The same HTTP server backs configured remote CLI access and the local background daemon used by archive-access CLI commands.

!!! note
    Gmail accounts must have completed an initial `msgvault sync-full` before
    scheduled incremental sync. IMAP schedules scan the mailbox and skip known
    messages. Teams and Discord importers detect and checkpoint their own
    first-run history backfills.

`msgvault serve` also runs scheduled SyncTech SMS Backup & Restore Drive sources configured under `[[synctech_sms.sources]]`; see [Configuration](/configuration/#synctech-sms-sources).

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
| `auto_build_cache` | `true` | Build stale or missing Parquet cache files before the daemon opens DuckDB |

`engine = "sql"` forces live SQL for aggregate views. `engine = "duckdb"` requires a usable Parquet cache and fails daemon startup if the cache cannot be built or opened. `auto_build_cache = false` leaves cache rebuilds to explicit `msgvault build-cache` runs. These settings replace the TUI/MCP analytics flags deprecated in 0.17.0; see [Configuration: analytics](/configuration/#analytics).

### `[[accounts]]`

| Key | Default | Description |
|---|---|---|
| `email` | (required) | Gmail/IMAP/Teams identifier or display name, or exact Discord guild ID |
| `schedule` | — | Cron expression for sync schedule |
| `enabled` | `true` | Whether scheduled sync is active |

See the [Configuration](/configuration/) page for the full config file reference.
