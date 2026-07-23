# Generic Single-Meeting Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an authenticated provider-neutral HTTP endpoint that validates and idempotently stores one complete meeting through msgvault's canonical meeting archive path.

**Architecture:** A new `internal/meetingimport` package owns the strict wire contract, normalization, canonical rendering, persistence, and one-item sync lifecycle. `internal/api` exposes it through an optional importer capability, while the daemon adapter supplies the real store, post-source migration, and staleness-aware cache-refresh hooks. Existing search and meeting views consume the resulting `meeting_transcript` message without a new read path.

**Tech Stack:** Go, `net/http`, Huma/OpenAPI, msgvault's SQLite/PostgreSQL store abstractions, generated Go API client, and testify.

## Global Constraints

- Follow `docs/superpowers/specs/2026-07-23-generic-meeting-ingestion-design.md`.
- The endpoint is `POST /api/v1/import/meeting`, accepts one meeting, and requires the existing API key.
- Keep the contract provider-neutral; do not add provider clients, polling, credentials, audio, attachments, batch ingestion, or a packaged local hook.
- Fix source/conversation/message/raw types to `meeting_import`, `meeting`, `meeting_transcript`, and `meeting_json`; callers cannot select them.
- Bound the request to 16 MiB, accept `application/json` only, and reject unknown fields outside `meeting.metadata`.
- Repeated `(source.identifier, meeting.external_id)` delivery updates one record and returns `updated`; do not expose `unchanged`.
- Preserve caller-supplied transcript speaker labels; do not perform diarization, voice recognition, or attendee-to-speaker matching.
- Use existing store abstractions only so SQLite and PostgreSQL remain supported.
- All new Go tests use testify, with expected values first in equality assertions.
- Every Go test command includes `-tags "fts5 sqlite_vec"` or uses `make test`.
- Before each commit run `git diff --check`, inspect the staged diff, and scrub public files for private names, paths, hosts, or personal data.

---

### Task 1: Freeze and validate the provider-neutral request

**Files:**

- Create: `internal/meetingimport/models.go`
- Create: `internal/meetingimport/models_test.go`
- Create: `internal/meetingimport/decode.go`
- Create: `internal/meetingimport/decode_test.go`

**Interfaces:**

- Produces:

  ```go
  const MaxRequestBytes int64 = 16 << 20

  type Request struct {
      Source  Source  `json:"source"`
      Meeting Meeting `json:"meeting"`
  }

  func DecodeRequest(r io.Reader, maxBytes int64) (Request, error)
  func (r Request) Normalize() (NormalizedRequest, error)
  ```

- Typed errors must support `errors.Is` for malformed JSON, oversized input,
  and semantic validation so the API can map them without string matching.

- [ ] **Step 1: Write failing decoder and normalization tests**

  Cover one complete request plus malformed JSON, trailing JSON, duplicate
  top-level values, unknown fields, unknown keys allowed in `meeting.metadata`,
  an oversized body, byte limits, invalid/display-name email forms, missing
  content, timestamp offsets and ordering, mutually exclusive transcript
  forms, non-finite/negative/non-monotonic offsets, attendee deduplication,
  title/date fallback inputs, and normalized lowercase emails.

- [ ] **Step 2: Run the focused tests and verify RED**

  ```bash
  go test -tags "fts5 sqlite_vec" ./internal/meetingimport \
    -run 'Test(DecodeRequest|RequestNormalize)' -count=1
  ```

  Expected: FAIL because `internal/meetingimport` and its public interfaces do
  not exist.

- [ ] **Step 3: Implement the strict decoder and normalized model**

  Use `io.LimitReader(r, maxBytes+1)`, `json.Decoder.DisallowUnknownFields`,
  exactly one decoded object followed by EOF, `time.RFC3339Nano`, `net/mail`,
  UTF-8 validation, byte-counted limits, and `math.IsNaN`/`math.IsInf`.
  Normalize timestamps to UTC, trim outer content whitespace, deduplicate
  attendees case-insensitively in first-seen order, and preserve
  `meeting.metadata` as `map[string]any`.

- [ ] **Step 4: Run the focused tests and verify GREEN**

  Re-run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/meetingimport/models.go internal/meetingimport/models_test.go \
    internal/meetingimport/decode.go internal/meetingimport/decode_test.go
  git diff --cached --check
  git commit -m "feat(meetings): validate generic imports"
  ```

### Task 2: Build the canonical meeting snapshot

**Files:**

- Create: `internal/meetingimport/format.go`
- Create: `internal/meetingimport/format_test.go`
- Reference: `internal/granola/format.go`
- Reference: `internal/circleback/format.go`

**Interfaces:**

- Consumes: `NormalizedRequest` from Task 1.
- Produces:

  ```go
  type Snapshot struct {
      SourceIdentifier string
      SourceDisplayName string
      AccountEmail      string
      SourceMessageID   string
      Title             string
      StartedAt         time.Time
      Body              string
      Snippet           string
      Metadata          []byte
      Raw               []byte
      Organizer         *Person
      Attendees         []Person
  }

  func BuildSnapshot(req NormalizedRequest) (Snapshot, error)
  ```

- [ ] **Step 1: Write failing formatter tests**

  Assert exact Granola-compatible body output for Markdown-vs-text summary,
  plain transcript line preservation, named and anonymous structured speakers,
  offsets below and above one hour, absent offsets, UTC time ranges, attendee
  display names, empty organizer/attendee sets, date-derived title, 200-rune
  snippets, canonical raw JSON, generated metadata, and arbitrary provider
  metadata nested only under `provider_metadata`.

- [ ] **Step 2: Run the focused tests and verify RED**

  ```bash
  go test -tags "fts5 sqlite_vec" ./internal/meetingimport \
    -run 'TestBuildSnapshot' -count=1
  ```

  Expected: FAIL because `BuildSnapshot` is undefined.

- [ ] **Step 3: Implement deterministic snapshot rendering**

  Follow `internal/granola/format.go`: title, UTC `When:`, named attendees,
  selected summary, and `Transcript:`. Render structured segments as
  `[mm:ss] Speaker: text`, `[h:mm:ss] Speaker: text`, or `Speaker: text`.
  Marshal raw JSON from the normalized `meeting` only and metadata from an
  explicit generated struct; never serialize headers or the full HTTP request.

- [ ] **Step 4: Run the focused tests and verify GREEN**

  Re-run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/meetingimport/format.go internal/meetingimport/format_test.go
  git diff --cached --check
  git commit -m "feat(meetings): format generic snapshots"
  ```

### Task 3: Persist one meeting with sync accounting

**Files:**

- Create: `internal/meetingimport/importer.go`
- Create: `internal/meetingimport/importer_test.go`
- Reference: `internal/granola/importer.go`
- Reference: `internal/store/messages.go`
- Reference: `internal/store/sync.go`

**Interfaces:**

- Consumes: `Request`, `Normalize`, and `BuildSnapshot`.
- Produces:

  ```go
  type Status string

  const (
      StatusCreated Status = "created"
      StatusUpdated Status = "updated"
  )

  type Result struct {
      Status          Status
      SourceID        int64
      MessageID       int64
      SourceMessageID string
  }

  type Hooks struct {
      AfterSourceSetup func() error
      RefreshCache     func(context.Context, string) error
  }

  func NewImporter(s *store.Store, hooks Hooks) *Importer
  func (i *Importer) Import(ctx context.Context, req Request) (Result, error)
  ```

- [ ] **Step 1: Write failing importer tests against the real test store**

  Cover create, identical retry as `updated`, changed retry with the same
  message ID, source-scoped external IDs, source display-name updates,
  confirmed account identity, organizer `is_from_me`, absent organizer,
  attendee replacement with empty, raw/metadata/body/FTS persistence,
  conversation membership and stats, sync counters, failed-run recording,
  post-source hook failure, cache-refresh failure after a durable write, safe
  retry, and atomic rollback on related-row failure.

- [ ] **Step 2: Run the focused tests and verify RED**

  ```bash
  go test -tags "fts5 sqlite_vec" ./internal/meetingimport \
    -run 'TestImporter' -count=1
  ```

  Expected: FAIL because `Importer` is undefined.

- [ ] **Step 3: Implement the importer**

  Resolve `GetOrCreateSource("meeting_import", identifier)`, update its display
  name, add the normalized `account-email` identity, run
  `AfterSourceSetup`, start one sync, check `MessageExistsBatch`, ensure only
  supplied participants, call `PersistMessage` once with replacement
  recipients and conversation membership, checkpoint `processed=1` plus
  added/updated, recompute conversation stats, complete the sync, and run
  `RefreshCache`. Any error after `StartSync` must call
  `FailSyncWithCheckpoint`; cache failure therefore leaves the message durable
  but the run failed and the request retryable.

- [ ] **Step 4: Run the focused tests and verify GREEN**

  Re-run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Run optional PostgreSQL coverage**

  ```bash
  if [ -n "$MSGVAULT_TEST_DB" ]; then
    go test -tags "fts5 sqlite_vec" ./internal/meetingimport \
      -run 'TestImporter' -count=1
  fi
  ```

  Expected: PASS when the configured test database is available; otherwise
  record this merge gate as not run.

- [ ] **Step 6: Commit**

  ```bash
  git add internal/meetingimport/importer.go internal/meetingimport/importer_test.go
  git diff --cached --check
  git commit -m "feat(meetings): persist generic imports"
  ```

### Task 4: Expose and wire the authenticated endpoint

**Files:**

- Create: `internal/api/meeting_import.go`
- Create: `internal/api/meeting_import_test.go`
- Modify: `internal/api/routes.go`
- Modify: `internal/api/server.go`
- Modify: `cmd/msgvault/cmd/serve.go`
- Modify: `cmd/msgvault/cmd/store_adapter_test.go`
- Create: `cmd/msgvault/cmd/meeting_import_e2e_test.go`

**Interfaces:**

- Produces:

  ```go
  type MeetingImporter interface {
      ImportMeeting(context.Context, meetingimport.Request) (meetingimport.Result, error)
  }

  type MeetingImportResponse struct {
      Status          meetingimport.Status `json:"status"`
      SourceID        int64                `json:"source_id"`
      MessageID       int64                `json:"message_id"`
      SourceMessageID string               `json:"source_message_id"`
  }
  ```

- [ ] **Step 1: Write failing HTTP contract tests**

  Test authentication, missing capability, JSON media type including charset,
  wrong media type, malformed/trailing/unknown JSON, 16 MiB limit, semantic
  422 responses without meeting content, 201 create, 200 update, internal
  errors, context cancellation, and OpenAPI route registration.

- [ ] **Step 2: Write the failing API-to-store integration test**

  Post a synthetic meeting through a real `storeAPIAdapter`, read it through
  `GET /api/v1/messages/{id}`, redeliver replacement content, and prove the
  same message ID exposes the replacement body and participants.

- [ ] **Step 3: Run focused API and command tests and verify RED**

  ```bash
  go test -tags "fts5 sqlite_vec" ./internal/api ./cmd/msgvault/cmd \
    -run 'Test(MeetingImport|StoreAPIAdapterMeetingImport)' -count=1
  ```

  Expected: FAIL because the route and adapter capability do not exist.

- [ ] **Step 4: Implement the handler and daemon wiring**

  Register `POST /import/meeting` with Huma request/response schemas and
  explicit 200/201 responses. The raw handler validates media type, delegates
  bounded strict decoding to `meetingimport.DecodeRequest`, maps typed errors,
  and calls the optional `MeetingImporter`. Construct the daemon importer with
  `runPostSourceCreateMigrations` and
  `rebuildCacheAfterScheduledSync(ctx, "meeting_import:"+identifier)`.
  The generic operation-gate middleware already serializes this mutating POST.

- [ ] **Step 5: Run focused tests and verify GREEN**

  Re-run the command from Step 3. Expected: PASS.

- [ ] **Step 6: Commit**

  ```bash
  git add internal/api/meeting_import.go internal/api/meeting_import_test.go \
    internal/api/routes.go internal/api/server.go cmd/msgvault/cmd/serve.go \
    cmd/msgvault/cmd/store_adapter_test.go \
    cmd/msgvault/cmd/meeting_import_e2e_test.go
  git diff --cached --check
  git commit -m "feat(api): ingest single meetings"
  ```

### Task 5: Surface imported meetings in the TUI

**Files:**

- Modify: `internal/tui/meeting_state.go`
- Modify: `internal/tui/meeting_view.go`
- Modify: `internal/tui/meeting_mode_test.go`
- Modify: `internal/tui/meeting_view_test.go`

**Interfaces:**

- Consumes: sources with `source_type == "meeting_import"`.
- Produces: imported-source filtering and display in the existing Meetings
  mode.

- [ ] **Step 1: Write failing TUI tests**

  Assert imported sources appear in the selector, use sanitized display name
  then identifier then `Imported`, remain searchable with
  `meeting_transcript`, and update empty-state copy to mention provider and
  imported meetings.

- [ ] **Step 2: Run focused TUI tests and verify RED**

  ```bash
  go test -tags "fts5 sqlite_vec" ./internal/tui \
    -run 'TestMeeting.*(Source|Empty|Import)' -count=1
  ```

  Expected: FAIL because `meeting_import` is not recognized.

- [ ] **Step 3: Add the imported source type and neutral copy**

  Include `meeting_import` in `meetingAccounts`; preserve existing Granola and
  Circleback labels; resolve imported labels from display name, identifier,
  then `Imported`; replace provider-only empty-state instructions.

- [ ] **Step 4: Run focused tests and verify GREEN**

  Re-run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/tui/meeting_state.go internal/tui/meeting_view.go \
    internal/tui/meeting_mode_test.go internal/tui/meeting_view_test.go
  git diff --cached --check
  git commit -m "feat(tui): show imported meetings"
  ```

### Task 6: Publish the API contract and usage docs

**Files:**

- Modify: `internal/api/openapi.go`
- Modify: `internal/api/openapi_test.go`
- Modify: `api/openapi.yaml`
- Modify: `pkg/client/openapi.yaml`
- Modify: `pkg/client/generated/`
- Modify: `docs/usage/meetings.md`
- Modify: `docs/api-server.md`
- Modify: `docs/changelog.md`

**Interfaces:**

- Produces: API schema version `1.6.0`, checked-in 3.1/3.0 schemas, generated
  client request/response types, and public setup guidance.

- [ ] **Step 1: Write failing OpenAPI assertions**

  Assert schema version `1.6.0`, the meeting import path, API-key security,
  required fields, strict object schemas, extensible metadata, response
  statuses, and generated client method/type presence.

- [ ] **Step 2: Run OpenAPI tests and verify RED**

  ```bash
  go test -tags "fts5 sqlite_vec" ./internal/api ./cmd/msgvault/cmd \
    -run 'TestOpenAPI.*MeetingImport' -count=1
  ```

  Expected: FAIL until the schema version and generated artifacts are updated.

- [ ] **Step 3: Bump and regenerate the contract**

  ```bash
  make api-generate
  ```

  Expected: updated `api/openapi.yaml`, `pkg/client/openapi.yaml`, and generated
  Go client files containing `ImportMeeting`.

- [ ] **Step 4: Document the generic endpoint**

  Add a synthetic `curl` example, response examples, field behavior,
  idempotency scope, speaker-label preservation, caller-owned privacy
  filtering, safe retry after cache errors, and a short local-adapter example.
  Do not name any private downstream host, project, or real person.

- [ ] **Step 5: Run focused contract and docs checks**

  ```bash
  make openapi-check
  make docs-check
  ```

  Expected: PASS.

- [ ] **Step 6: Commit**

  ```bash
  git add internal/api/openapi.go internal/api/openapi_test.go \
    api/openapi.yaml pkg/client/openapi.yaml pkg/client/generated \
    docs/usage/meetings.md docs/api-server.md docs/changelog.md
  git diff --cached --check
  git commit -m "docs(api): publish meeting ingestion"
  ```

### Task 7: Final verification, review, and pull request

**Files:**

- Review: all files changed from `origin/main`

- [ ] **Step 1: Run formatting and static checks**

  ```bash
  go fmt ./...
  go vet -tags "fts5 sqlite_vec" ./...
  make lint-ci
  ```

  Expected: PASS with no new warnings or unstaged formatting.

- [ ] **Step 2: Run the full test and artifact suites**

  ```bash
  make test
  make openapi-check
  make docs-check
  ```

  Expected: PASS.

- [ ] **Step 3: Inspect and scrub the complete branch**

  ```bash
  git diff --check origin/main...HEAD
  git diff --stat origin/main...HEAD
  git diff origin/main...HEAD
  rg -n -i '/users/|umbrel\\.local|\\.ts\\.net|api[_-]?key[=:][^[:space:]]+' \
    internal api pkg/client docs/superpowers/specs/2026-07-23-generic-meeting-ingestion-design.md \
    docs/usage/meetings.md docs/api-server.md docs/changelog.md
  ```

  Expected: no private downstream names, personal paths, secrets, or unrelated
  changes in public artifacts. The historical superseded design/plan documents
  are not part of the new PR's public implementation narrative.

- [ ] **Step 4: Request code review and resolve findings**

  Review `origin/main..HEAD` against the approved generic spec. Fix every
  Critical or Important issue with a failing regression test first, then rerun
  the affected focused and full checks.

- [ ] **Step 5: Commit any review fixes**

  ```bash
  git add internal/meetingimport internal/api/meeting_import.go \
    internal/api/meeting_import_test.go internal/api/routes.go \
    internal/api/server.go internal/api/openapi.go internal/api/openapi_test.go \
    internal/tui/meeting_state.go internal/tui/meeting_view.go \
    internal/tui/meeting_mode_test.go internal/tui/meeting_view_test.go \
    cmd/msgvault/cmd/serve.go cmd/msgvault/cmd/store_adapter_test.go \
    cmd/msgvault/cmd/meeting_import_e2e_test.go api/openapi.yaml \
    pkg/client/openapi.yaml pkg/client/generated docs/usage/meetings.md \
    docs/api-server.md docs/changelog.md
  git diff --cached --check
  git commit -m "fix(meetings): address ingestion review"
  ```

  Skip only when review produced no changes.

- [ ] **Step 6: Push through the authenticated fork and open the PR**

  Push the feature branch to the authenticated contributor fork and open a PR
  against `kenn-io/msgvault:main`. Keep the PR description
  changelog-oriented: what changed, why, and how to call the endpoint. Do not
  include a test plan or private deployment details.
