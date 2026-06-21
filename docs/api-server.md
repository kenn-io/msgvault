---
title: Web Server
description: REST API for programmatic access to your msgvault archive, with optional background sync scheduling.
---


## Overview

`msgvault serve` starts an HTTP server that exposes your email archive over a REST API. It optionally runs a background sync scheduler to keep accounts up to date on a cron-based schedule.

The API queries the same archive database and attachment store as the CLI and TUI. SQLite is the default archive database; PostgreSQL is supported when `[data].database_url` is a PostgreSQL DSN. Keyword search and ordinary archive reads stay local to that database. If vector search is enabled, semantic and hybrid search also call the embedding endpoint configured in `[vector.embeddings]`. The server is designed for local integrations, dashboards, and automation scripts.

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
```

## Authentication

All endpoints except `/health` require authentication when `api_key` is set in your config. Three authentication methods are supported:

| Method | Header | Example |
|---|---|---|
| Bearer token | `Authorization: Bearer <key>` | `Authorization: Bearer my-secret` |
| API key header | `X-API-Key: <key>` | `X-API-Key: my-secret` |
| Plain auth header | `Authorization: <key>` | `Authorization: my-secret` |

If no `api_key` is configured, authentication is not required regardless of bind address. The separate `allow_insecure` / security validation prevents starting without an API key on non-loopback addresses.

## API Endpoints

### GET `/health`

Health check endpoint. Does not require authentication.

**Response:**

```json
{"status": "ok"}
```

---

### GET `/api/v1/stats`

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
    "pending_embeddings_total": 134657
  }
}
```

`active_generation` is always present in the object (null until the
first build completes). `building_generation` is omitted when no
rebuild is in flight. `pending_embeddings_total` is the sum of rows
still pending across the active and building generations. See
[Vector Search](/usage/vector-search/) for the end-to-end workflow.

---

### GET `/api/v1/messages`

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

### GET `/api/v1/messages/{id}`

Full message details including body and attachment metadata.

**Response:**

```json
{
  "id": 12345,
  "subject": "Q4 Planning",
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
      "filename": "q4-plan.pdf",
      "mime_type": "application/pdf",
      "size_bytes": 204800
    }
  ]
}
```

The `cc`, `bcc`, and `body_html` fields are included only when present. `body` is the plain-text body when one exists; for HTML-only messages, it falls back to the HTML body so callers still receive message content.

---

### GET `/api/v1/messages/{id}/inline?cid=<content-id>`

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

### GET `/api/v1/search`

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
| `explain` | 0/1 | `0` | When `1` and `mode=vector|hybrid`, include per-signal scores |

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

### GET `/api/v1/accounts`

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

### GET `/api/v1/sources/status`

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

### POST `/api/v1/auth/token/{email}`

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

### POST `/api/v1/accounts`

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

### POST `/api/v1/sync/{account}`

Trigger a manual sync for an account. Returns immediately with a 202 status while the sync runs in the background.

**Response (202 Accepted):**

```json
{
  "status": "accepted",
  "message": "Sync started for you@gmail.com"
}
```

---

### GET `/api/v1/scheduler/status`

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

The server can automatically sync Gmail and IMAP accounts on a cron-based schedule. Add `[[accounts]]` sections to your config:

```toml
[[accounts]]
email = "you@gmail.com"
schedule = "0 * * * *"    # every hour
enabled = true

[[accounts]]
email = "you@fastmail.com"
schedule = "*/15 * * * *" # every 15 minutes
enabled = true
```

The scheduler starts automatically with `msgvault serve` when account schedules are configured. It resolves each entry to a syncable source type, including Gmail and IMAP. Use the `/api/v1/scheduler/status` endpoint to monitor schedule state, and `/api/v1/sync/{account}` to trigger a manual sync outside the schedule.

!!! note
    Each account must have completed an initial full sync (`msgvault sync-full`) before scheduled sync will work. Gmail scheduled syncs use incremental history sync. IMAP scheduled syncs run the IMAP sync path and skip messages already present locally.

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
| `api_port` | `8080` | Port the server listens on |
| `bind_addr` | `127.0.0.1` | Bind address |
| `api_key` | — | API key for authentication |
| `allow_insecure` | `false` | Allow non-loopback binding without `api_key` |
| `cors_origins` | `[]` | Allowed CORS origins |
| `cors_credentials` | `false` | Allow credentials in CORS requests |
| `cors_max_age` | `0` | CORS preflight cache duration in seconds (defaults to `86400` when `cors_origins` is set) |

### `[[accounts]]`

| Key | Default | Description |
|---|---|---|
| `email` | (required) | Gmail/IMAP account identifier or display name |
| `schedule` | — | Cron expression for sync schedule |
| `enabled` | `true` | Whether scheduled sync is active |

See the [Configuration](/configuration/) page for the full config file reference.
