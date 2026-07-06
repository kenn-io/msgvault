# Deletion Staging API

Date: 2026-07-06
Status: Approved design, pending implementation

## Goal

Give web/HTTP consumers a first-class API to stage messages for deletion,
with server-side resolution of message selections. Today the only staging
route is `POST /api/v1/cli/deletion-manifests`, which requires the client
to pre-resolve every Gmail ID and construct a complete `deletion.Manifest`
(the TUI does this via a separate `GET /messages/gmail-ids` call). Nothing
in this design touches Gmail: execution remains exclusively the
`delete-staged` path.

## Endpoints

All routes live in the authenticated `/api/v1` group and are registered in
`internal/api/routes.go` via the existing Huma registrars, so they appear
in the generated OpenAPI spec. Note that raw routes do **not** get
path/query parameters documented automatically: they are manually
declared per operation ID in `rawRouteParameters`
(`internal/api/routes.go`). The new operations need entries there — the
`status` query parameter for the list route and the `{id}` path parameter
(string) for the delete route, mirroring existing entries like
`getMessage`.

### POST /api/v1/deletions — stage messages

Request body:

```json
{
  "filter": {
    "sender": "newsletter@example.com",
    "source_id": 1,
    "after": "2019-01-01",
    "before": "2020-01-01"
  },
  "message_ids": [123, 456],
  "description": "old newsletters",
  "dry_run": false
}
```

All fields are optional (subject to the empty-filter guard below).
Supported `filter` fields: `sender`, `sender_name`, `recipient`,
`recipient_name`, `domain`, `label` (strings, omitted or empty = no
constraint); `source_id` (integer, omitted = no constraint); `after`,
`before` (ISO dates).

Semantics:

- `filter` fields map onto `query.MessageFilter` and deliberately mirror a
  **subset** of the `/messages/filter` query parameters. Excluded, with
  reasons: pagination and sorting (meaningless for staging),
  `time_period`/`time_granularity` (covered by `after`/`before`),
  `empty_targets` (TUI drilldown concept), `hide_deleted` (the Gmail-ID
  resolution path is already live-message scoped), `message_type`
  (redundant — resolution is already scoped to Gmail sources),
  `conversation_id` and `attachments_only` (`GetGmailIDsByFilter` ignores
  them today; no staging use case justifies extending it — thread staging
  can go through `message_ids`).
- **Engine extension required:** `GetGmailIDsByFilter` currently ignores
  `MessageFilter.After`/`.Before` (it only honors `TimeRange.Period`) in
  both `internal/query/sqlite.go` and the DuckDB Parquet fallback in
  `internal/query/duckdb.go`. Date-bounded staging is a core use case, so
  both implementations must be extended to apply `After`/`Before` against
  `sent_at`, with engine tests covering **every** supported filter field
  above on both paths.
- `message_ids` are internal message IDs (what `/messages` and `/search`
  return). Resolved to Gmail IDs via a new engine method
  `GetGmailIDsByMessageIDs(ctx, ids []int64)`. **Contract:** it must
  enforce the same constraints the filter path hardcodes
  (`internal/query/sqlite.go` `GetGmailIDsByFilter`): only live messages
  (`store.LiveMessagesWhere` — excludes remote-deleted and
  dedup-soft-deleted) and only messages from `source_type = 'gmail'`
  sources. Explicit IDs must not be able to stage non-Gmail,
  source-deleted, or dedup-hidden messages; such IDs are silently dropped
  from the result (they contribute to neither the manifest nor
  `message_count`).
- Filter results and `message_ids` results are unioned and deduplicated.
- **Empty-filter guard:** `GetGmailIDsByFilter` with a zero-value filter
  returns every message in the archive
  (`TestDuckDBEngine_GetGmailIDsByFilter_EmptyFilter`,
  `internal/query/duckdb_test.go`). The handler therefore rejects requests
  whose effective criteria are empty — no filter field set AND no
  `message_ids` — with `400 empty_filter`. There is intentionally no
  "stage everything" escape hatch; add one only if a real need appears.
- Zero matches (criteria present but nothing matched) → `400
  no_messages_matched`.
- `dry_run: true` resolves and counts but writes nothing.

Response — a single schema for both outcomes (the Huma helper
`jsonResponsesFor` documents one schema across all success statuses):

```json
{
  "dry_run": false,
  "message_count": 1234,
  "sample_gmail_ids": ["..."],
  "id": "20260706-old-newsletters-abc123",
  "status": "pending"
}
```

- Dry-run → `200` with `dry_run: true`, `message_count`,
  `sample_gmail_ids` (first 10); `id`/`status` omitted.
- Create → `201` with `id`, `status: "pending"`, `message_count`;
  `sample_gmail_ids` omitted.

Manifest construction: `deletion.NewManifest(description, gmailIDs)`, then
set `CreatedBy = "api"`, then `Manager.SaveManifest`. (Do NOT use
`Manager.CreateManifest` — `NewManifest` hardcodes `CreatedBy: "cli"` and
`CreateManifest` provides no override.)

Provenance: `deletion.Filters` cannot represent several request fields
(`sender_name`, `recipient_name`, `source_id`, `message_type`,
`conversation_id`, `attachments_only`). Since `GmailIDs` are what the
executor acts on, `Filters` is display/provenance only. The handler maps
the fields that fit into `Manifest.Filters` best-effort, and a new
additive field records the exact request:

```go
// Manifest gains:
RawFilter json.RawMessage `json:"raw_filter,omitempty"`
```

Backward compatible on disk (omitempty; older manifests simply lack it).

### GET /api/v1/deletions — list staged manifests

Query param `status` (optional): one of `pending`, `in_progress`,
`completed`, `failed`, `cancelled`; validated against that set, default is
all statuses. Response:

```json
{
  "manifests": [
    {
      "id": "...",
      "status": "pending",
      "created_at": "2026-07-06T12:00:00Z",
      "created_by": "api",
      "description": "old newsletters",
      "message_count": 1234
    }
  ]
}
```

### DELETE /api/v1/deletions/{id} — cancel (unstage)

`Manager.CancelManifest` alone cannot distinguish not-found from
non-cancellable (it returns one generic error), so the handler preloads:

1. `ValidateManifestID` → `400 invalid_manifest_id` (path-traversal guard).
2. `Manager.GetManifest(id)` → absent: `404 not_found`.
3. Status not in {`pending`, `in_progress`} (includes `failed`,
   `completed`, `cancelled`): `409 not_cancellable`.
4. Otherwise `Manager.CancelManifest(id)` → `200 {"id": ..., "status":
   "cancelled"}`.

## Plumbing

- Server-side manifest access follows the existing optional-capability
  pattern (`CLIDeletionManifestSaver`, `internal/api/cli_handlers.go`):
  new interfaces `DeletionManifestLister` and `DeletionManifestCanceller`
  (plus reuse of the saver), implemented on `storeAPIAdapter` in
  `cmd/msgvault/cmd/serve.go` by delegating to
  `deletion.NewManager(<DataDir>/deletions)`.
- Gmail-ID resolution uses the server's `query.Engine`
  (`GetGmailIDsByFilter` for filters; new `GetGmailIDsByMessageIDs` for
  explicit IDs).
- Errors use the existing `writeError` envelope
  (`ErrorResponse{error, message}`).
- The TUI's staging path is unchanged.

## Testing

Handler tests in `internal/api` following the established
`httptest.NewRequest` + `Router().ServeHTTP` + mock-store pattern:

- stage by filter, by message IDs, and both (union/dedupe)
- dry-run returns count and writes nothing
- empty effective filter → `400 empty_filter`
- criteria matching nothing → `400 no_messages_matched`
- created manifest has `CreatedBy: "api"`, mapped `Filters`, `RawFilter`
- list: default all statuses, per-status filtering, invalid status → 400
- cancel: happy path, 404 unknown, 409 for each non-cancellable status,
  traversal ID → 400

Engine tests:

- `GetGmailIDsByFilter`: coverage for every supported filter field on
  both the SQLite path and the DuckDB Parquet fallback, including the new
  `After`/`Before` support (boundary dates, after-only, before-only,
  combined with other fields).
- `GetGmailIDsByMessageIDs` (SQLite + Parquet paths): happy path, and the
  constraint contract — IDs of non-Gmail-source, remote-deleted, and
  dedup-soft-deleted messages are silently dropped.

Manifest round-trip test for `RawFilter` (survives save/load, absent on
old manifests).

A daemon-level e2e test (pattern: `cmd/msgvault/cmd/daemon_cli_http_test.go`)
exercising stage → list → cancel through a real listener.

## Workflow notes

- Run `make api-generate` after adding routes — the OpenAPI schemas and
  generated Go client under `api/` and `pkg/client/` are committed
  artifacts (Makefile).
- Bump `APISchemaVersion` (`internal/api/openapi.go`) from 1.1.0 to
  **1.2.0** with a doc comment describing the new endpoints — additive
  change, so a minor bump; the major-version compatibility gate stays
  at 1.
