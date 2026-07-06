# Deletion Staging API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** First-class `/api/v1/deletions` endpoints (stage, list, cancel) with server-side Gmail-ID resolution, per `docs/superpowers/specs/2026-07-06-deletion-staging-api-design.md`.

**Architecture:** New raw Huma routes in `internal/api` resolve message selections server-side via `query.Engine` (`GetGmailIDsByFilter`, extended for After/Before, plus new `GetGmailIDsByMessageIDs`), then drive the existing file-based `deletion.Manager` through optional capability interfaces implemented by `storeAPIAdapter` in `cmd/msgvault/cmd/serve.go`. Nothing here touches Gmail — execution stays with `delete-staged`.

**Tech Stack:** Go, Huma v2 raw routes over net/http, SQLite/DuckDB query engines, testify.

## Global Constraints

- Tests use testify only: `require.X` halts (setup), `assert.X` continues (independent checks). Argument order is `(want, got)`. NEVER `t.Fatalf`/`t.Errorf`.
- After Go changes: `go fmt ./...` and `go vet ./...`; stage ALL resulting changes.
- Commit after every task. Pre-commit hook must pass — never bypass it.
- No real PII in test fixtures (`alice`, `user@example.com`, etc.).
- SQL: EXISTS subqueries, never `SELECT DISTINCT` with JOINs. Never touch `message_bodies`.
- Errors: `fmt.Errorf("doing X: %w", err)`.
- Supported staging filter fields (spec): `sender`, `sender_name`, `recipient`, `recipient_name`, `domain`, `label` (strings); `source_id` (int); `after`, `before` (dates). Nothing else.

**Shared test fixture facts** (`buildStandardTestData`, `internal/query/duckdb_test.go:140`; SQLite `newTestEnv` uses the same data; `makeDate(m, d)` = 2024-mm-dd UTC):
- msg1: 2024-01-15, from alice@example.com, labels INBOX+Work
- msg2: 2024-01-16, from alice, has attachments
- msg3: 2024-02-01, from alice
- msg4: 2024-02-15, from bob@company.org
- msg5: 2024-03-01, from bob
- Single Gmail source `test@gmail.com`; source_message_ids are `"msg1"`…`"msg5"`.

---

### Task 1: `GetGmailIDsByFilter` honors `After`/`Before`

**Files:**
- Modify: `internal/query/sqlite.go` (inside `GetGmailIDsByFilter`, after the `filter.TimeRange.Period` block that ends near line 1379, before the `// Build query` comment)
- Modify: `internal/query/duckdb.go` (inside the Parquet fallback of `GetGmailIDsByFilter`, after its `filter.TimeRange.Period` block near line 2133, before `// Build query`)
- Test: `internal/query/sqlite_crud_test.go`, `internal/query/duckdb_test.go`

**Interfaces:**
- Consumes: existing `MessageFilter.After/Before *time.Time` (`internal/query/models.go:228-229`)
- Produces: `GetGmailIDsByFilter` filters on `sent_at >= After` and `sent_at < Before` on both engines. Task 4's handler relies on this.

- [ ] **Step 1: Write failing SQLite test** in `internal/query/sqlite_crud_test.go` (next to `TestGetGmailIDsByFilter_Label`):

```go
func TestGetGmailIDsByFilter_AfterBefore(t *testing.T) {
	env := newTestEnv(t)
	feb1 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	mar1 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	afterIDs, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{After: &feb1})
	require.NoError(t, err, "after-only")
	assert.ElementsMatch(t, []string{"msg3", "msg4", "msg5"}, afterIDs, "after >= Feb 1 (boundary inclusive)")

	beforeIDs, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{Before: &feb1})
	require.NoError(t, err, "before-only")
	assert.ElementsMatch(t, []string{"msg1", "msg2"}, beforeIDs, "before < Feb 1 (boundary exclusive)")

	rangeIDs, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{After: &feb1, Before: &mar1})
	require.NoError(t, err, "range")
	assert.ElementsMatch(t, []string{"msg3", "msg4"}, rangeIDs, "Feb window")

	combined, err := env.Engine.GetGmailIDsByFilter(env.Ctx, MessageFilter{Sender: "alice@example.com", After: &feb1})
	require.NoError(t, err, "combined with sender")
	assert.ElementsMatch(t, []string{"msg3"}, combined, "sender+after")
}
```

- [ ] **Step 2: Run it — must FAIL** (After/Before currently ignored, so extra rows come back):
`go test ./internal/query/ -run TestGetGmailIDsByFilter_AfterBefore -v` → FAIL

- [ ] **Step 3: Write failing DuckDB Parquet test** in `internal/query/duckdb_test.go` (next to `TestDuckDBEngine_GetGmailIDsByFilter_EmptyFilter`, which shows the `newParquetEngine(t)` pattern). `newParquetEngine` has no SQLite engine attached, so it exercises the Parquet fallback:

```go
func TestDuckDBEngine_GetGmailIDsByFilter_AfterBefore(t *testing.T) {
	engine := newParquetEngine(t)
	ctx := context.Background()
	feb1 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	mar1 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	afterIDs, err := engine.GetGmailIDsByFilter(ctx, MessageFilter{After: &feb1})
	require.NoError(t, err, "after-only")
	assertSetEqual(t, afterIDs, []string{"msg3", "msg4", "msg5"})

	beforeIDs, err := engine.GetGmailIDsByFilter(ctx, MessageFilter{Before: &feb1})
	require.NoError(t, err, "before-only")
	assertSetEqual(t, beforeIDs, []string{"msg1", "msg2"})

	rangeIDs, err := engine.GetGmailIDsByFilter(ctx, MessageFilter{After: &feb1, Before: &mar1})
	require.NoError(t, err, "range")
	assertSetEqual(t, rangeIDs, []string{"msg3", "msg4"})
}
```

- [ ] **Step 4: Run it — must FAIL:**
`go test ./internal/query/ -run TestDuckDBEngine_GetGmailIDsByFilter_AfterBefore -v` → FAIL

- [ ] **Step 5: Implement.** In `internal/query/sqlite.go`, `GetGmailIDsByFilter`, insert after the `filter.TimeRange.Period` block (immediately before the `// Build query` comment):

```go
	if filter.After != nil {
		conditions = append(conditions, "m.sent_at >= ?")
		args = append(args, *filter.After)
	}
	if filter.Before != nil {
		conditions = append(conditions, "m.sent_at < ?")
		args = append(args, *filter.Before)
	}
```

(This mirrors the ListMessages pattern at `internal/query/sqlite.go:334-343`.) In `internal/query/duckdb.go`, Parquet fallback of `GetGmailIDsByFilter`, insert the same block before its `// Build query` comment but with the `msg.` alias:

```go
	if filter.After != nil {
		conditions = append(conditions, "msg.sent_at >= ?")
		args = append(args, *filter.After)
	}
	if filter.Before != nil {
		conditions = append(conditions, "msg.sent_at < ?")
		args = append(args, *filter.Before)
	}
```

- [ ] **Step 6: Run both tests + the package** — all PASS, no regressions:
`go test ./internal/query/ -run 'GetGmailIDsByFilter' -v` then `go test ./internal/query/`

- [ ] **Step 7: fmt, vet, commit:**
```bash
go fmt ./... && go vet ./...
git add -A && git commit -m "Honor After/Before in GetGmailIDsByFilter"
```

---

### Task 2: New engine method `GetGmailIDsByMessageIDs`

**Files:**
- Modify: `internal/query/sqlite.go` (add method after `GetGmailIDsByFilter`)
- Modify: `internal/query/duckdb.go` (add method after `GetGmailIDsByFilter`)
- Modify: `internal/query/querytest/mock_engine.go` (add func field + method)
- Test: `internal/query/sqlite_crud_test.go`, `internal/query/duckdb_test.go`

**Interfaces:**
- Produces: `func (e *SQLiteEngine) GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error)` and the same signature on `*DuckDBEngine` and `*querytest.MockEngine`. NOT added to the `query.Engine` interface — the API handler type-asserts it (Task 4), so `daemonclient`'s HTTP engine adapter is untouched.
- Contract (spec): same constraints as the filter path — `store.LiveMessagesWhere(alias, true)` and Gmail-source join; non-qualifying IDs silently dropped.

- [ ] **Step 1: Write failing SQLite test** in `internal/query/sqlite_crud_test.go`. The standard fixture has only live Gmail messages, so seed the constraint cases inline via the env's DB (mirror how other tests in this file INSERT extra rows — see `TestMatchEmptySenderName_CombinedWithDomain`'s neighborhood for the `env.DB`/`env.Exec` idiom; if the env helper lacks a raw-exec method, use `env.DB.Exec` directly):

```go
func TestGetGmailIDsByMessageIDs(t *testing.T) {
	env := newTestEnv(t)

	// Happy path: two known fixture messages (msg1=id 1, msg2=id 2).
	ids, err := env.Engine.GetGmailIDsByMessageIDs(env.Ctx, []int64{1, 2})
	require.NoError(t, err, "resolve fixture ids")
	assert.ElementsMatch(t, []string{"msg1", "msg2"}, ids)

	// Unknown IDs are silently dropped.
	ids, err = env.Engine.GetGmailIDsByMessageIDs(env.Ctx, []int64{1, 999999})
	require.NoError(t, err, "unknown id")
	assert.ElementsMatch(t, []string{"msg1"}, ids)

	// Empty input: no query, no results.
	ids, err = env.Engine.GetGmailIDsByMessageIDs(env.Ctx, nil)
	require.NoError(t, err, "empty input")
	assert.Empty(t, ids)
}

func TestGetGmailIDsByMessageIDs_ExcludesNonQualifying(t *testing.T) {
	env := newTestEnv(t)

	// Non-Gmail source message.
	_, err := env.DB.Exec(`INSERT INTO sources (id, source_type, identifier) VALUES (99, 'whatsapp', 'wa@example.com')`)
	require.NoError(t, err, "insert whatsapp source")
	_, err = env.DB.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (901, 99, 'wa-1', '2024-01-01')`)
	require.NoError(t, err, "insert whatsapp message")

	// Remote-deleted and dedup-soft-deleted Gmail messages (source 1 = test@gmail.com).
	_, err = env.DB.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at, deleted_from_source_at) VALUES (902, 1, 'gone-1', '2024-01-02', '2024-06-01')`)
	require.NoError(t, err, "insert source-deleted message")
	_, err = env.DB.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at, deleted_at) VALUES (903, 1, 'dedup-1', '2024-01-03', '2024-06-01')`)
	require.NoError(t, err, "insert dedup-deleted message")

	ids, err := env.Engine.GetGmailIDsByMessageIDs(env.Ctx, []int64{1, 901, 902, 903})
	require.NoError(t, err, "resolve mixed ids")
	assert.ElementsMatch(t, []string{"msg1"}, ids, "non-Gmail, source-deleted, and dedup-deleted must be dropped")
}
```

NOTE for implementer: adjust the INSERT column lists to the actual `schema.sql` NOT NULL columns (check `internal/store/schema.sql` for `sources` and `messages`) — add required columns like `conversation_id` with fixture values if the schema demands them. The assertions must stay as written.

- [ ] **Step 2: Run — must FAIL to compile** (method undefined):
`go test ./internal/query/ -run TestGetGmailIDsByMessageIDs -v`

- [ ] **Step 3: Implement SQLite method** in `internal/query/sqlite.go` directly after `GetGmailIDsByFilter`:

```go
// GetGmailIDsByMessageIDs returns Gmail message IDs (source_message_id)
// for the given internal message IDs. It enforces the same constraints
// as GetGmailIDsByFilter: only live messages (LiveMessagesWhere — not
// remote-deleted, not dedup-soft-deleted) from Gmail sources.
// Non-qualifying IDs are silently dropped, mirroring
// GetMessageSummariesByIDs semantics.
func (e *SQLiteEngine) GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT m.source_message_id
		FROM messages m
		JOIN sources s_gmail ON s_gmail.id = m.source_id AND s_gmail.source_type = 'gmail'
		WHERE %s AND m.id IN (%s)
		ORDER BY m.sent_at DESC, m.id DESC
	`, store.LiveMessagesWhere("m", true), strings.Join(placeholders, ","))

	rows, err := e.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get gmail ids by message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectGmailIDs(rows)
}
```

- [ ] **Step 4: Run SQLite tests — PASS:**
`go test ./internal/query/ -run TestGetGmailIDsByMessageIDs -v`

- [ ] **Step 5: Write failing DuckDB test** in `internal/query/duckdb_test.go`:

```go
func TestDuckDBEngine_GetGmailIDsByMessageIDs(t *testing.T) {
	ctx := context.Background()

	// Parquet fallback path (no SQLite engine attached).
	parquet := newParquetEngine(t)
	ids, err := parquet.GetGmailIDsByMessageIDs(ctx, []int64{1, 2, 999999})
	require.NoError(t, err, "parquet path")
	assertSetEqual(t, ids, []string{"msg1", "msg2"})

	// SQLite delegation path.
	sqlited := newSQLiteEngine(t)
	ids, err = sqlited.GetGmailIDsByMessageIDs(ctx, []int64{1, 2, 999999})
	require.NoError(t, err, "sqlite delegation path")
	assertSetEqual(t, ids, []string{"msg1", "msg2"})
}
```

- [ ] **Step 6: Implement DuckDB method** in `internal/query/duckdb.go` directly after `GetGmailIDsByFilter`, mirroring its delegation and Parquet CTE structure (see `duckdb.go:1996-2160`):

```go
// GetGmailIDsByMessageIDs returns Gmail message IDs for internal message
// IDs, enforcing the same live-message and Gmail-source constraints as
// GetGmailIDsByFilter. Non-qualifying IDs are silently dropped.
func (e *DuckDBEngine) GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error) {
	// Delegate to SQLite for authoritative deletion status.
	if e.sqliteEngine != nil {
		return e.sqliteEngine.GetGmailIDsByMessageIDs(ctx, ids)
	}
	if e.analyticsDir == "" {
		return nil, errors.New("GetGmailIDsByMessageIDs requires SQLite or Parquet data")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		WITH %s
		SELECT msg.source_message_id
		FROM msg
		JOIN src ON src.id = msg.source_id AND COALESCE(src.source_type, 'gmail') = 'gmail'
		WHERE %s AND msg.id IN (%s)
		ORDER BY msg.sent_at DESC, msg.id DESC
	`, e.parquetCTEs(), store.LiveMessagesWhere("msg", true), strings.Join(placeholders, ","))

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get gmail ids by message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectGmailIDs(rows)
}
```

- [ ] **Step 7: Add MockEngine support** in `internal/query/querytest/mock_engine.go` — add to the func-field block:

```go
	GetGmailIDsByMessageIDsFunc func(context.Context, []int64) ([]string, error)
```

and next to `GetGmailIDsByFilter`:

```go
func (m *MockEngine) GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error) {
	if m.GetGmailIDsByMessageIDsFunc != nil {
		return m.GetGmailIDsByMessageIDsFunc(ctx, ids)
	}
	return m.GmailIDs, nil
}
```

- [ ] **Step 8: Run the package — all PASS:** `go test ./internal/query/...`

- [ ] **Step 9: fmt, vet, commit:**
```bash
go fmt ./... && go vet ./...
git add -A && git commit -m "Add GetGmailIDsByMessageIDs engine method"
```

---

### Task 3: deletion package — `RawFilter`, status-aware lookup, list-by-status

**Files:**
- Modify: `internal/deletion/manifest.go`
- Test: `internal/deletion/manifest_test.go`

**Interfaces:**
- Produces (all used by Tasks 4-6):
  - `Manifest.RawFilter json.RawMessage` (json tag `raw_filter,omitempty`)
  - `var ErrManifestNotFound = errors.New("manifest not found")`
  - `func (m *Manager) GetManifestWithStatus(id string) (*Manifest, Status, error)` — status derived from the directory (authoritative), not the inline field; not-found wraps `ErrManifestNotFound`
  - `func (m *Manager) ListByStatus(status Status) ([]*Manifest, error)` — normalizes each returned `Manifest.Status` to the directory status
  - `func IsValidStatus(s Status) bool`
  - `func PersistedStatuses() []Status`

- [ ] **Step 1: Write failing tests** in `internal/deletion/manifest_test.go` (follow the file's existing `NewManager(t.TempDir())` conventions):

```go
func TestManifestRawFilterRoundTrip(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	require.NoError(t, err, "NewManager")

	m := NewManifest("raw filter test", []string{"gm-1"})
	m.RawFilter = json.RawMessage(`{"filter":{"sender":"alice@example.com"},"dry_run":false}`)
	require.NoError(t, mgr.SaveManifest(m), "save")

	loaded, _, err := mgr.GetManifest(m.ID)
	require.NoError(t, err, "reload")
	assert.JSONEq(t, string(m.RawFilter), string(loaded.RawFilter), "raw filter survives round-trip")

	// Old manifests without the field load with a nil RawFilter.
	old := NewManifest("no raw filter", []string{"gm-2"})
	require.NoError(t, mgr.SaveManifest(old), "save old-style")
	loadedOld, _, err := mgr.GetManifest(old.ID)
	require.NoError(t, err, "reload old-style")
	assert.Nil(t, loadedOld.RawFilter, "absent field stays nil")
}

func TestGetManifestWithStatus(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	require.NoError(t, err, "NewManager")

	m := NewManifest("status test", []string{"gm-1"})
	require.NoError(t, mgr.SaveManifest(m), "save pending")

	got, status, err := mgr.GetManifestWithStatus(m.ID)
	require.NoError(t, err, "lookup pending")
	assert.Equal(t, StatusPending, status)
	assert.Equal(t, m.ID, got.ID)

	// Directory is authoritative even when the inline field is stale:
	// move the file to cancelled/ without rewriting the inline status.
	require.NoError(t, mgr.MoveManifest(m.ID, StatusPending, StatusCancelled), "move")
	_, status, err = mgr.GetManifestWithStatus(m.ID)
	require.NoError(t, err, "lookup cancelled")
	assert.Equal(t, StatusCancelled, status, "dir-derived status wins over stale inline field")

	_, _, err = mgr.GetManifestWithStatus("does-not-exist")
	require.Error(t, err, "missing manifest")
	assert.ErrorIs(t, err, ErrManifestNotFound)
}

func TestListByStatus(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	require.NoError(t, err, "NewManager")

	a := NewManifest("batch a", []string{"gm-1"})
	require.NoError(t, mgr.SaveManifest(a), "save a")
	b := NewManifest("batch b", []string{"gm-2", "gm-3"})
	require.NoError(t, mgr.SaveManifest(b), "save b")
	require.NoError(t, mgr.CancelManifest(b.ID), "cancel b")

	pending, err := mgr.ListByStatus(StatusPending)
	require.NoError(t, err, "list pending")
	require.Len(t, pending, 1)
	assert.Equal(t, a.ID, pending[0].ID)
	assert.Equal(t, StatusPending, pending[0].Status, "normalized status")

	cancelled, err := mgr.ListByStatus(StatusCancelled)
	require.NoError(t, err, "list cancelled")
	require.Len(t, cancelled, 1)
	assert.Equal(t, b.ID, cancelled[0].ID)

	_, err = mgr.ListByStatus(Status("bogus"))
	assert.Error(t, err, "invalid status rejected")
}

func TestIsValidStatus(t *testing.T) {
	for _, s := range PersistedStatuses() {
		assert.True(t, IsValidStatus(s), "status %q", s)
	}
	assert.False(t, IsValidStatus(Status("bogus")))
	assert.False(t, IsValidStatus(Status("")))
}
```

- [ ] **Step 2: Run — must FAIL to compile:** `go test ./internal/deletion/ -run 'RawFilter|WithStatus|ListByStatus|IsValidStatus' -v`

- [ ] **Step 3: Implement.** In `internal/deletion/manifest.go`:

Add to the `Manifest` struct (after `Filters`):

```go
	// RawFilter preserves the verbatim staging request from the HTTP
	// API for provenance — Filters cannot represent every request
	// field (sender_name, recipient_name, source_id). Absent on
	// manifests created by the TUI/CLI.
	RawFilter json.RawMessage `json:"raw_filter,omitempty"`
```

(add `"encoding/json"` to imports). Add the sentinel near the top-level vars:

```go
// ErrManifestNotFound reports a manifest ID with no file in any status
// directory. Callers use errors.Is to map it to HTTP 404.
var ErrManifestNotFound = errors.New("manifest not found")
```

Add after `GetManifest`:

```go
// GetManifestWithStatus returns the manifest and its directory-derived
// status. The directory is authoritative over the inline Status field
// (a crash between rename and inline rewrite can leave them disagreeing).
func (m *Manager) GetManifestWithStatus(id string) (*Manifest, Status, error) {
	if strings.TrimSpace(id) == "" {
		return nil, "", errors.New("batch ID is required")
	}
	if err := ValidateManifestID(id); err != nil {
		return nil, "", err
	}
	filename := id + ".json"
	for _, status := range persistedStatuses {
		path := filepath.Join(m.dirForStatus(status), filename)
		if manifest, err := LoadManifest(path); err == nil {
			return manifest, status, nil
		}
	}
	return nil, "", fmt.Errorf("manifest %s: %w", id, ErrManifestNotFound)
}
```

Add after `ListCancelled`:

```go
// ListByStatus returns all manifests currently in the directory for the
// given status, with each Manifest.Status normalized to the
// directory-derived status (the directory is authoritative).
func (m *Manager) ListByStatus(status Status) ([]*Manifest, error) {
	if !isPersistedStatus(status) {
		return nil, fmt.Errorf("invalid manifest status %q", status)
	}
	manifests, err := m.listManifests(m.dirForStatus(status))
	if err != nil {
		return nil, err
	}
	for _, manifest := range manifests {
		manifest.Status = status
	}
	return manifests, nil
}
```

Add near `statusDirMap`/`persistedStatuses`:

```go
// IsValidStatus reports whether s is a persisted manifest status.
func IsValidStatus(s Status) bool { return isPersistedStatus(s) }

// PersistedStatuses returns all statuses that have on-disk directories.
func PersistedStatuses() []Status { return slices.Clone(persistedStatuses) }
```

(add `"slices"` to imports; if `isPersistedStatus` doesn't already exist as shown in `SaveManifest`, adapt to whatever the file's status-validity helper is called).

- [ ] **Step 4: Run package tests — all PASS:** `go test ./internal/deletion/`

- [ ] **Step 5: fmt, vet, commit:**
```bash
go fmt ./... && go vet ./...
git add -A && git commit -m "Add RawFilter and status-aware manifest lookups to deletion package"
```

---

### Task 4: `POST /api/v1/deletions` — stage handler

**Files:**
- Create: `internal/api/deletions.go`
- Modify: `internal/api/routes.go` (route registration in `registerHumaRoutes` near the backup-freeze block at line ~296; `rawRouteParameters` switch at line ~402)
- Test: create `internal/api/deletions_test.go`

**Interfaces:**
- Consumes: `s.engine.GetGmailIDsByFilter` (Task 1 semantics), type-asserted `GetGmailIDsByMessageIDs` (Task 2), `CLIDeletionManifestSaver` (existing, `internal/api/cli_handlers.go:93`), `deletion.NewManifest`, `parseAPITime` (`internal/api/params.go`), `writeError`/`writeJSON`/`writeAPIHTTPError`/`newAPIHTTPError`/`cliStoreUnavailableError` (existing helpers).
- Produces (Tasks 5-7 rely on these exact names): `StageDeletionRequest`, `StageDeletionFilter`, `StageDeletionResponse`, `s.handleStageDeletion`, capability interface `deletionMessageIDResolver`.

- [ ] **Step 1: Write failing handler tests** in `internal/api/deletions_test.go`:

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// deletionMockStore layers manifest-capability fakes over mockStore.
type deletionMockStore struct {
	mockStore
	saved     []*deletion.Manifest
	manifests map[deletion.Status][]*deletion.Manifest
	getStatus deletion.Status
	getErr    error
	cancelled []string
	cancelErr error
}

func (s *deletionMockStore) SaveCLIDeletionManifest(_ context.Context, m *deletion.Manifest) error {
	s.saved = append(s.saved, m)
	return nil
}

func (s *deletionMockStore) ListDeletionManifests(_ context.Context, status deletion.Status) ([]*deletion.Manifest, error) {
	if status != "" {
		return s.manifests[status], nil
	}
	var all []*deletion.Manifest
	for _, st := range deletion.PersistedStatuses() {
		all = append(all, s.manifests[st]...)
	}
	return all, nil
}

func (s *deletionMockStore) GetDeletionManifest(_ context.Context, id string) (*deletion.Manifest, deletion.Status, error) {
	if s.getErr != nil {
		return nil, "", s.getErr
	}
	return &deletion.Manifest{ID: id, Status: s.getStatus}, s.getStatus, nil
}

func (s *deletionMockStore) CancelDeletionManifest(_ context.Context, id string) error {
	if s.cancelErr != nil {
		return s.cancelErr
	}
	s.cancelled = append(s.cancelled, id)
	return nil
}

func newDeletionTestServer(t *testing.T, st MessageStore, engine query.Engine) *Server {
	t.Helper()
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})
}

func postDeletions(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deletions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func TestStageDeletionByFilter(t *testing.T) {
	st := &deletionMockStore{}
	var gotFilter query.MessageFilter
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, f query.MessageFilter) ([]string, error) {
			gotFilter = f
			return []string{"gm-1", "gm-2"}, nil
		},
	}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{
		"filter": {"sender": "alice@example.com", "after": "2019-01-01", "before": "2020-01-01"},
		"description": "old alice mail"
	}`)

	require.Equal(t, http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(t, "alice@example.com", gotFilter.Sender)
	require.NotNil(t, gotFilter.After, "after parsed")
	assert.Equal(t, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), gotFilter.After.UTC())
	require.NotNil(t, gotFilter.Before, "before parsed")

	var resp StageDeletionResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.False(t, resp.DryRun)
	assert.Equal(t, 2, resp.MessageCount)
	assert.NotEmpty(t, resp.ID)
	assert.Equal(t, "pending", resp.Status)
	assert.Empty(t, resp.SampleGmailIDs, "create response has no sample")

	require.Len(t, st.saved, 1, "manifest saved")
	m := st.saved[0]
	assert.Equal(t, "api", m.CreatedBy)
	assert.Equal(t, []string{"gm-1", "gm-2"}, m.GmailIDs)
	assert.Equal(t, []string{"alice@example.com"}, m.Filters.Senders)
	assert.Equal(t, "2019-01-01", m.Filters.After)
	assert.NotEmpty(t, m.RawFilter, "raw provenance recorded")
}

func TestStageDeletionByMessageIDsUnionsAndDedupes(t *testing.T) {
	st := &deletionMockStore{}
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, _ query.MessageFilter) ([]string, error) {
			return []string{"gm-1", "gm-2"}, nil
		},
		GetGmailIDsByMessageIDsFunc: func(_ context.Context, ids []int64) ([]string, error) {
			assert.Equal(t, []int64{7, 8}, ids)
			return []string{"gm-2", "gm-3"}, nil
		},
	}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{"filter": {"sender": "alice@example.com"}, "message_ids": [7, 8]}`)

	require.Equal(t, http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())
	require.Len(t, st.saved, 1)
	assert.Equal(t, []string{"gm-1", "gm-2", "gm-3"}, st.saved[0].GmailIDs, "union, deduped, order-preserving")
}

func TestStageDeletionDryRun(t *testing.T) {
	st := &deletionMockStore{}
	ids := make([]string, 25)
	for i := range ids {
		ids[i] = "gm-" + string(rune('a'+i))
	}
	engine := &querytest.MockEngine{GmailIDs: ids}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{"filter": {"domain": "example.com"}, "dry_run": true}`)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp StageDeletionResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.True(t, resp.DryRun)
	assert.Equal(t, 25, resp.MessageCount)
	assert.Len(t, resp.SampleGmailIDs, 10, "sample capped at 10")
	assert.Empty(t, resp.ID, "dry run stages nothing")
	assert.Empty(t, st.saved, "dry run writes nothing")
}

func TestStageDeletionRejectsEmptyFilter(t *testing.T) {
	st := &deletionMockStore{}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	for _, body := range []string{`{}`, `{"filter": {}}`, `{"filter": {"sender": ""}, "message_ids": []}`} {
		w := postDeletions(t, srv, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "body %s -> status", body)
		assert.Contains(t, w.Body.String(), "empty_filter", "body %s -> error code", body)
	}
	assert.Empty(t, st.saved)
}

func TestStageDeletionNoMatches(t *testing.T) {
	st := &deletionMockStore{}
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, _ query.MessageFilter) ([]string, error) {
			return nil, nil
		},
	}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{"filter": {"sender": "nobody@example.com"}}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "no_messages_matched")
	assert.Empty(t, st.saved)
}

func TestStageDeletionInvalidDate(t *testing.T) {
	srv := newDeletionTestServer(t, &deletionMockStore{}, &querytest.MockEngine{})
	w := postDeletions(t, srv, `{"filter": {"after": "not-a-date"}}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_date")
}

func TestStageDeletionEngineUnavailable(t *testing.T) {
	srv := newDeletionTestServer(t, &deletionMockStore{}, nil)
	w := postDeletions(t, srv, `{"filter": {"sender": "alice@example.com"}}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "engine_unavailable")
}
```

- [ ] **Step 2: Run — must FAIL to compile:** `go test ./internal/api/ -run TestStageDeletion -v`

- [ ] **Step 3: Implement** `internal/api/deletions.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
)

// stageDeletionSampleSize caps the dry-run Gmail-ID preview.
const stageDeletionSampleSize = 10

// deletionMessageIDResolver is the optional engine capability for
// resolving internal message IDs to Gmail IDs. SQLite/DuckDB engines
// implement it; the daemonclient HTTP engine does not need to.
type deletionMessageIDResolver interface {
	GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error)
}

// DeletionManifestLister lists staged deletion manifests. Implemented by
// the serve daemon's store adapter; status "" means all statuses.
type DeletionManifestLister interface {
	ListDeletionManifests(ctx context.Context, status deletion.Status) ([]*deletion.Manifest, error)
}

// DeletionManifestCanceller resolves and cancels staged deletion
// manifests. GetDeletionManifest returns the directory-derived status;
// not-found errors wrap deletion.ErrManifestNotFound.
type DeletionManifestCanceller interface {
	GetDeletionManifest(ctx context.Context, id string) (*deletion.Manifest, deletion.Status, error)
	CancelDeletionManifest(ctx context.Context, id string) error
}

// StageDeletionFilter selects messages to stage. All fields optional,
// but the effective request must contain at least one criterion.
type StageDeletionFilter struct {
	Sender        string `json:"sender,omitempty"`
	SenderName    string `json:"sender_name,omitempty"`
	Recipient     string `json:"recipient,omitempty"`
	RecipientName string `json:"recipient_name,omitempty"`
	Domain        string `json:"domain,omitempty"`
	Label         string `json:"label,omitempty"`
	SourceID      *int64 `json:"source_id,omitempty"`
	After         string `json:"after,omitempty"`
	Before        string `json:"before,omitempty"`
}

func (f *StageDeletionFilter) isEmpty() bool {
	return f == nil || (f.Sender == "" && f.SenderName == "" && f.Recipient == "" &&
		f.RecipientName == "" && f.Domain == "" && f.Label == "" &&
		f.SourceID == nil && f.After == "" && f.Before == "")
}

func (f *StageDeletionFilter) toMessageFilter() (query.MessageFilter, *apiHTTPError) {
	var mf query.MessageFilter
	mf.Sender = f.Sender
	mf.SenderName = f.SenderName
	mf.Recipient = f.Recipient
	mf.RecipientName = f.RecipientName
	mf.Domain = f.Domain
	mf.Label = f.Label
	mf.SourceID = f.SourceID
	if f.After != "" {
		ts, err := parseAPITime(f.After)
		if err != nil {
			return mf, newAPIHTTPError(http.StatusBadRequest, "invalid_date",
				fmt.Sprintf("filter field %q must be an RFC3339 or YYYY-MM-DD date, got %q", "after", f.After))
		}
		mf.After = &ts
	}
	if f.Before != "" {
		ts, err := parseAPITime(f.Before)
		if err != nil {
			return mf, newAPIHTTPError(http.StatusBadRequest, "invalid_date",
				fmt.Sprintf("filter field %q must be an RFC3339 or YYYY-MM-DD date, got %q", "before", f.Before))
		}
		mf.Before = &ts
	}
	return mf, nil
}

// StageDeletionRequest is the POST /api/v1/deletions body.
type StageDeletionRequest struct {
	Filter      *StageDeletionFilter `json:"filter,omitempty"`
	MessageIDs  []int64              `json:"message_ids,omitempty"`
	Description string               `json:"description,omitempty"`
	DryRun      bool                 `json:"dry_run,omitempty"`
}

// StageDeletionResponse covers both dry-run (200) and create (201).
type StageDeletionResponse struct {
	DryRun         bool     `json:"dry_run"`
	MessageCount   int      `json:"message_count"`
	SampleGmailIDs []string `json:"sample_gmail_ids,omitempty"`
	ID             string   `json:"id,omitempty"`
	Status         string   `json:"status,omitempty"`
}

// DeletionManifestSummary is one row of GET /api/v1/deletions.
type DeletionManifestSummary struct {
	ID           string    `json:"id"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedBy    string    `json:"created_by"`
	Description  string    `json:"description"`
	MessageCount int       `json:"message_count"`
}

// ListDeletionsResponse is the GET /api/v1/deletions body.
type ListDeletionsResponse struct {
	Manifests []DeletionManifestSummary `json:"manifests"`
}

// CancelDeletionResponse is the DELETE /api/v1/deletions/{id} body.
type CancelDeletionResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (s *Server) handleStageDeletion(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}
	saver, ok := s.store.(CLIDeletionManifestSaver)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}

	var req StageDeletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}
	if req.Filter.isEmpty() && len(req.MessageIDs) == 0 {
		writeError(w, http.StatusBadRequest, "empty_filter",
			"At least one filter criterion or message_ids entry is required; staging the entire archive is not supported")
		return
	}

	gmailIDs, httpErr := s.resolveStageDeletionIDs(r.Context(), &req)
	if httpErr != nil {
		writeAPIHTTPError(w, httpErr)
		return
	}
	if len(gmailIDs) == 0 {
		writeError(w, http.StatusBadRequest, "no_messages_matched", "No messages matched the given criteria")
		return
	}

	if req.DryRun {
		sample := gmailIDs
		if len(sample) > stageDeletionSampleSize {
			sample = sample[:stageDeletionSampleSize]
		}
		writeJSON(w, http.StatusOK, StageDeletionResponse{
			DryRun:         true,
			MessageCount:   len(gmailIDs),
			SampleGmailIDs: sample,
		})
		return
	}

	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "staged via API"
	}
	manifest := deletion.NewManifest(description, gmailIDs)
	manifest.CreatedBy = "api"
	manifest.Filters = manifestFiltersFromRequest(req.Filter)
	raw, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request is not serializable")
		return
	}
	manifest.RawFilter = raw

	if err := saver.SaveCLIDeletionManifest(r.Context(), manifest); err != nil {
		s.logger.Error("failed to save staged deletion manifest", "id", manifest.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "stage_deletion_failed", "Failed to save deletion manifest")
		return
	}
	writeJSON(w, http.StatusCreated, StageDeletionResponse{
		MessageCount: len(gmailIDs),
		ID:           manifest.ID,
		Status:       string(manifest.Status),
	})
}

// resolveStageDeletionIDs unions filter-resolved and explicitly listed
// message IDs into a deduplicated, order-preserving Gmail-ID list.
func (s *Server) resolveStageDeletionIDs(ctx context.Context, req *StageDeletionRequest) ([]string, *apiHTTPError) {
	var out []string
	seen := make(map[string]struct{})
	appendIDs := func(ids []string) {
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}

	if !req.Filter.isEmpty() {
		mf, httpErr := req.Filter.toMessageFilter()
		if httpErr != nil {
			return nil, httpErr
		}
		ids, err := s.engine.GetGmailIDsByFilter(ctx, mf)
		if err != nil {
			s.logger.Error("stage deletion filter query failed", "error", err)
			return nil, newAPIHTTPError(http.StatusInternalServerError, "internal_error", "Gmail ID query failed")
		}
		appendIDs(ids)
	}
	if len(req.MessageIDs) > 0 {
		resolver, ok := s.engine.(deletionMessageIDResolver)
		if !ok {
			return nil, newAPIHTTPError(http.StatusServiceUnavailable, "engine_unavailable",
				"message_ids staging is not supported by this query engine")
		}
		ids, err := resolver.GetGmailIDsByMessageIDs(ctx, req.MessageIDs)
		if err != nil {
			s.logger.Error("stage deletion message-id query failed", "error", err)
			return nil, newAPIHTTPError(http.StatusInternalServerError, "internal_error", "Gmail ID query failed")
		}
		appendIDs(ids)
	}
	return out, nil
}

// manifestFiltersFromRequest maps the request fields that
// deletion.Filters can represent; RawFilter preserves the rest.
func manifestFiltersFromRequest(f *StageDeletionFilter) deletion.Filters {
	var out deletion.Filters
	if f == nil {
		return out
	}
	if f.Sender != "" {
		out.Senders = []string{f.Sender}
	}
	if f.Domain != "" {
		out.SenderDomains = []string{f.Domain}
	}
	if f.Recipient != "" {
		out.Recipients = []string{f.Recipient}
	}
	if f.Label != "" {
		out.Labels = []string{f.Label}
	}
	out.After = f.After
	out.Before = f.Before
	return out
}
```

(Task 5 adds `errors` and `sort` to this import block when its handlers need them — do NOT add them now, they'd be unused imports and fail the build.)

- [ ] **Step 4: Register the route.** In `internal/api/routes.go`, `registerHumaRoutes`, after the backup-freeze registrations (~line 297):

```go
	registerAPIV1RawHumaJSONRouteWithRequest[StageDeletionRequest, StageDeletionResponse](
		apiV1, "stageDeletion", http.MethodPost, "/deletions",
		"Stage messages for deletion", s.handleStageDeletion,
		http.StatusOK, http.StatusCreated)
```

- [ ] **Step 5: Run tests — all PASS:** `go test ./internal/api/ -run TestStageDeletion -v`

- [ ] **Step 6: Run the whole api package** (OpenAPI contract tests must still pass): `go test ./internal/api/`

- [ ] **Step 7: fmt, vet, commit:**
```bash
go fmt ./... && go vet ./...
git add -A && git commit -m "Add POST /api/v1/deletions staging endpoint"
```

---

### Task 5: `GET /api/v1/deletions` and `DELETE /api/v1/deletions/{id}`

**Files:**
- Modify: `internal/api/deletions.go` (add two handlers)
- Modify: `internal/api/routes.go` (two more registrations + `rawRouteParameters` cases)
- Test: `internal/api/deletions_test.go`

**Interfaces:**
- Consumes: `DeletionManifestLister`, `DeletionManifestCanceller`, `deletion.IsValidStatus`, `deletion.ValidateManifestID`, `deletion.ErrManifestNotFound` (Task 3), types from Task 4.
- Produces: `s.handleListDeletions`, `s.handleCancelDeletion`.

- [ ] **Step 1: Write failing tests** (append to `internal/api/deletions_test.go`; reuses `deletionMockStore`):

```go
func TestListDeletions(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st := &deletionMockStore{manifests: map[deletion.Status][]*deletion.Manifest{
		deletion.StatusPending: {{
			ID: "batch-1", Status: deletion.StatusPending, CreatedAt: now,
			CreatedBy: "api", Description: "old mail", GmailIDs: []string{"gm-1", "gm-2"},
		}},
		deletion.StatusCancelled: {{
			ID: "batch-2", Status: deletion.StatusCancelled, CreatedAt: now.Add(time.Hour),
			CreatedBy: "tui", Description: "cancelled batch", GmailIDs: []string{"gm-3"},
		}},
	}}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	// All statuses, newest first.
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deletions", nil))
	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp ListDeletionsResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	require.Len(t, resp.Manifests, 2)
	assert.Equal(t, "batch-2", resp.Manifests[0].ID, "newest first")
	assert.Equal(t, 2, resp.Manifests[1].MessageCount)

	// Filtered by status.
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deletions?status=pending", nil))
	require.Equal(t, http.StatusOK, w.Code)
	resp = ListDeletionsResponse{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode filtered")
	require.Len(t, resp.Manifests, 1)
	assert.Equal(t, "batch-1", resp.Manifests[0].ID)

	// Invalid status.
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deletions?status=bogus", nil))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_status")
}

func deleteDeletion(srv *Server, id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/deletions/"+id, nil))
	return w
}

func TestCancelDeletion(t *testing.T) {
	st := &deletionMockStore{getStatus: deletion.StatusPending}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	w := deleteDeletion(srv, "batch-1")
	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp CancelDeletionResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal(t, "batch-1", resp.ID)
	assert.Equal(t, "cancelled", resp.Status)
	assert.Equal(t, []string{"batch-1"}, st.cancelled)
}

func TestCancelDeletionNotFound(t *testing.T) {
	st := &deletionMockStore{getErr: fmt.Errorf("manifest batch-x: %w", deletion.ErrManifestNotFound)}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	w := deleteDeletion(srv, "batch-x")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "not_found")
	assert.Empty(t, st.cancelled)
}

func TestCancelDeletionNotCancellable(t *testing.T) {
	for _, status := range []deletion.Status{deletion.StatusCompleted, deletion.StatusFailed, deletion.StatusCancelled} {
		st := &deletionMockStore{getStatus: status}
		srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

		w := deleteDeletion(srv, "batch-1")
		assert.Equal(t, http.StatusConflict, w.Code, "status %s", status)
		assert.Contains(t, w.Body.String(), "not_cancellable", "status %s", status)
		assert.Empty(t, st.cancelled, "status %s", status)
	}
}

func TestCancelDeletionRejectsTraversalID(t *testing.T) {
	st := &deletionMockStore{getStatus: deletion.StatusPending}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	// Encoded traversal — must be rejected by ValidateManifestID.
	w := deleteDeletion(srv, "..%2F..%2Fetc")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_manifest_id")
	assert.Empty(t, st.cancelled)
}
```

(Add `"fmt"` to the test imports.)

- [ ] **Step 2: Run — must FAIL** (404s from the router): `go test ./internal/api/ -run 'TestListDeletions|TestCancelDeletion' -v`

- [ ] **Step 3: Implement handlers** (append to `internal/api/deletions.go`; add `"errors"` and `"sort"` to its import block):

```go
func (s *Server) handleListDeletions(w http.ResponseWriter, r *http.Request) {
	lister, ok := s.store.(DeletionManifestLister)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	var status deletion.Status
	if raw := r.URL.Query().Get("status"); raw != "" {
		status = deletion.Status(raw)
		if !deletion.IsValidStatus(status) {
			writeError(w, http.StatusBadRequest, "invalid_status",
				"status must be one of pending, in_progress, completed, failed, cancelled")
			return
		}
	}
	manifests, err := lister.ListDeletionManifests(r.Context(), status)
	if err != nil {
		s.logger.Error("failed to list deletion manifests", "error", err)
		writeError(w, http.StatusInternalServerError, "list_deletions_failed", "Failed to list deletion manifests")
		return
	}
	summaries := make([]DeletionManifestSummary, 0, len(manifests))
	for _, m := range manifests {
		summaries = append(summaries, DeletionManifestSummary{
			ID:           m.ID,
			Status:       string(m.Status),
			CreatedAt:    m.CreatedAt,
			CreatedBy:    m.CreatedBy,
			Description:  m.Description,
			MessageCount: len(m.GmailIDs),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt.After(summaries[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, ListDeletionsResponse{Manifests: summaries})
}

func (s *Server) handleCancelDeletion(w http.ResponseWriter, r *http.Request) {
	canceller, ok := s.store.(DeletionManifestCanceller)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	id := r.PathValue("id")
	if err := deletion.ValidateManifestID(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_manifest_id", err.Error())
		return
	}
	_, status, err := canceller.GetDeletionManifest(r.Context(), id)
	if errors.Is(err, deletion.ErrManifestNotFound) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("deletion manifest %q not found", id))
		return
	}
	if err != nil {
		s.logger.Error("failed to load deletion manifest", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load deletion manifest")
		return
	}
	if status != deletion.StatusPending && status != deletion.StatusInProgress {
		writeError(w, http.StatusConflict, "not_cancellable",
			fmt.Sprintf("deletion manifest %q has status %q and cannot be cancelled", id, status))
		return
	}
	if err := canceller.CancelDeletionManifest(r.Context(), id); err != nil {
		s.logger.Error("failed to cancel deletion manifest", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "cancel_deletion_failed", "Failed to cancel deletion manifest")
		return
	}
	writeJSON(w, http.StatusOK, CancelDeletionResponse{ID: id, Status: string(deletion.StatusCancelled)})
}
```

- [ ] **Step 4: Register routes + document params.** In `registerHumaRoutes`, after the Task 4 registration:

```go
	registerAPIV1RawHumaJSONRoute[ListDeletionsResponse](
		apiV1, "listDeletions", http.MethodGet, "/deletions",
		"List staged deletion manifests", s.handleListDeletions)
	registerAPIV1RawHumaJSONRoute[CancelDeletionResponse](
		apiV1, "cancelDeletion", http.MethodDelete, "/deletions/{id}",
		"Cancel a staged deletion manifest", s.handleCancelDeletion)
```

In the `rawRouteParameters` switch (near the `getMessage` case):

```go
	case "listDeletions":
		return []*huma.Param{queryStringParam("status",
			"Filter manifests by status (pending, in_progress, completed, failed, cancelled)", false)}
	case "cancelDeletion":
		return []*huma.Param{pathStringParam("id", "Deletion manifest ID")}
```

- [ ] **Step 5: Run tests — all PASS:** `go test ./internal/api/ -run 'TestListDeletions|TestCancelDeletion' -v` then `go test ./internal/api/`

- [ ] **Step 6: fmt, vet, commit:**
```bash
go fmt ./... && go vet ./...
git add -A && git commit -m "Add list and cancel endpoints for staged deletions"
```

---

### Task 6: serve daemon adapter — manifest capabilities

**Files:**
- Modify: `cmd/msgvault/cmd/serve.go` (near `SaveCLIDeletionManifest` at ~line 853 and the `var _ api.X` assertion block at ~line 634)
- Test: create `cmd/msgvault/cmd/serve_deletions_test.go`

**Interfaces:**
- Consumes: `deletion.Manager` methods from Task 3, api interfaces from Tasks 4-5, package-global `cfg` (set by root command; tests override it).
- Produces: `storeAPIAdapter` satisfies `api.DeletionManifestLister` and `api.DeletionManifestCanceller`.

- [ ] **Step 1: Write failing test** in `cmd/msgvault/cmd/serve_deletions_test.go` (cmd tests override the global `cfg` with save/restore — same idiom as `search_test.go:463-468`):

```go
package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/deletion"
)

func TestStoreAPIAdapterDeletionManifests(t *testing.T) {
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}

	adapter := &storeAPIAdapter{}
	var _ api.DeletionManifestLister = adapter
	var _ api.DeletionManifestCanceller = adapter

	ctx := context.Background()

	// Save through the existing saver path.
	m := deletion.NewManifest("adapter test", []string{"gm-1"})
	m.CreatedBy = "api"
	require.NoError(t, adapter.SaveCLIDeletionManifest(ctx, m), "save")

	// List all and by status.
	all, err := adapter.ListDeletionManifests(ctx, "")
	require.NoError(t, err, "list all")
	require.Len(t, all, 1)
	assert.Equal(t, m.ID, all[0].ID)

	pending, err := adapter.ListDeletionManifests(ctx, deletion.StatusPending)
	require.NoError(t, err, "list pending")
	require.Len(t, pending, 1)

	// Get with status, cancel, verify.
	_, status, err := adapter.GetDeletionManifest(ctx, m.ID)
	require.NoError(t, err, "get")
	assert.Equal(t, deletion.StatusPending, status)

	require.NoError(t, adapter.CancelDeletionManifest(ctx, m.ID), "cancel")
	_, status, err = adapter.GetDeletionManifest(ctx, m.ID)
	require.NoError(t, err, "get after cancel")
	assert.Equal(t, deletion.StatusCancelled, status)

	cancelled, err := adapter.ListDeletionManifests(ctx, deletion.StatusCancelled)
	require.NoError(t, err, "list cancelled")
	assert.Len(t, cancelled, 1)
}
```

- [ ] **Step 2: Run — must FAIL to compile:** `go test ./cmd/msgvault/cmd/ -run TestStoreAPIAdapterDeletionManifests -v`

- [ ] **Step 3: Implement.** In `cmd/msgvault/cmd/serve.go`, next to `SaveCLIDeletionManifest`:

```go
func (a *storeAPIAdapter) deletionManager() (*deletion.Manager, error) {
	mgr, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	if err != nil {
		return nil, fmt.Errorf("create deletion manager: %w", err)
	}
	return mgr, nil
}

func (a *storeAPIAdapter) ListDeletionManifests(_ context.Context, status deletion.Status) ([]*deletion.Manifest, error) {
	mgr, err := a.deletionManager()
	if err != nil {
		return nil, err
	}
	if status != "" {
		return mgr.ListByStatus(status)
	}
	var all []*deletion.Manifest
	for _, s := range deletion.PersistedStatuses() {
		manifests, err := mgr.ListByStatus(s)
		if err != nil {
			return nil, err
		}
		all = append(all, manifests...)
	}
	return all, nil
}

func (a *storeAPIAdapter) GetDeletionManifest(_ context.Context, id string) (*deletion.Manifest, deletion.Status, error) {
	mgr, err := a.deletionManager()
	if err != nil {
		return nil, "", err
	}
	return mgr.GetManifestWithStatus(id)
}

func (a *storeAPIAdapter) CancelDeletionManifest(_ context.Context, id string) error {
	mgr, err := a.deletionManager()
	if err != nil {
		return err
	}
	return mgr.CancelManifest(id)
}
```

Refactor the existing `SaveCLIDeletionManifest` to use the new helper:

```go
func (a *storeAPIAdapter) SaveCLIDeletionManifest(_ context.Context, manifest *deletion.Manifest) error {
	mgr, err := a.deletionManager()
	if err != nil {
		return err
	}
	return mgr.SaveManifest(manifest)
}
```

Add to the `var _ api.X` assertion block:

```go
var _ api.DeletionManifestLister = (*storeAPIAdapter)(nil)
var _ api.DeletionManifestCanceller = (*storeAPIAdapter)(nil)
```

- [ ] **Step 4: Run — PASS:** `go test ./cmd/msgvault/cmd/ -run TestStoreAPIAdapterDeletionManifests -v`

- [ ] **Step 5: fmt, vet, commit:**
```bash
go fmt ./... && go vet ./...
git add -A && git commit -m "Wire deletion manifest capabilities into serve daemon adapter"
```

---

### Task 7: Daemon-level e2e test — stage → list → cancel over a real listener

**Files:**
- Test: create `cmd/msgvault/cmd/deletions_api_e2e_test.go`

**Interfaces:**
- Consumes: everything above; `store.Open`/`InitSchema`/`GetOrCreateSource`/`EnsureConversation`/`UpsertMessage` seeding (same idiom as `search_test.go:430-461`); `api.NewServerWithOptions` + `httptest.NewServer` (same idiom as `api_daemon_test.go:14-36`, but with `Store: &storeAPIAdapter{store: s}` so the manifest capabilities are present).

- [ ] **Step 1: Write the e2e test:**

```go
package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestDeletionStagingEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	tmpDir := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: tmpDir}}

	s, err := store.Open(tmpDir + "/msgvault.db")
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	t.Cleanup(func() { _ = s.Close() })

	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	conv, err := s.EnsureConversation(src.ID, "c1", "")
	require.NoError(err, "create conversation")
	msgID, err := s.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv,
		SourceMessageID: "gm-e2e-1", MessageType: "email",
		Subject:      sql.NullString{String: "Stage me", Valid: true},
		SizeEstimate: 100,
	})
	require.NoError(err, "insert message")

	engine := query.NewEngine(s.DB(), s.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Data: config.DataConfig{DataDir: tmpDir}},
		Store:  &storeAPIAdapter{store: s},
		Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
	})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	// Stage by message ID.
	body, err := json.Marshal(map[string]any{
		"message_ids": []int64{msgID},
		"description": "e2e staging",
	})
	require.NoError(err, "marshal request")
	resp, err := http.Post(httpSrv.URL+"/api/v1/deletions", "application/json", bytes.NewReader(body))
	require.NoError(err, "POST /deletions")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(http.StatusCreated, resp.StatusCode, "stage status")
	var staged api.StageDeletionResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&staged), "decode stage response")
	assert.Equal(1, staged.MessageCount)
	require.NotEmpty(staged.ID, "manifest id")

	// List pending — the staged manifest appears.
	listResp, err := http.Get(httpSrv.URL + "/api/v1/deletions?status=pending")
	require.NoError(err, "GET /deletions")
	defer func() { _ = listResp.Body.Close() }()
	require.Equal(http.StatusOK, listResp.StatusCode, "list status")
	var listed api.ListDeletionsResponse
	require.NoError(json.NewDecoder(listResp.Body).Decode(&listed), "decode list response")
	require.Len(listed.Manifests, 1)
	assert.Equal(staged.ID, listed.Manifests[0].ID)
	assert.Equal("api", listed.Manifests[0].CreatedBy)

	// Cancel it.
	req, err := http.NewRequest(http.MethodDelete, httpSrv.URL+"/api/v1/deletions/"+staged.ID, nil)
	require.NoError(err, "build DELETE request")
	cancelResp, err := http.DefaultClient.Do(req)
	require.NoError(err, "DELETE /deletions/{id}")
	defer func() { _ = cancelResp.Body.Close() }()
	require.Equal(http.StatusOK, cancelResp.StatusCode, "cancel status")

	// Second cancel conflicts.
	req2, err := http.NewRequest(http.MethodDelete, httpSrv.URL+"/api/v1/deletions/"+staged.ID, nil)
	require.NoError(err, "build second DELETE request")
	conflictResp, err := http.DefaultClient.Do(req2)
	require.NoError(err, "second DELETE")
	defer func() { _ = conflictResp.Body.Close() }()
	assert.Equal(http.StatusConflict, conflictResp.StatusCode, "second cancel conflicts")

	// Pending list is empty again.
	list2, err := http.Get(httpSrv.URL + "/api/v1/deletions?status=pending")
	require.NoError(err, "GET /deletions after cancel")
	defer func() { _ = list2.Body.Close() }()
	var listedAfter api.ListDeletionsResponse
	require.NoError(json.NewDecoder(list2.Body).Decode(&listedAfter), "decode second list")
	assert.Empty(listedAfter.Manifests, "no pending manifests after cancel")
}
```

NOTE for implementer: check `UpsertMessage`'s actual return values (`search_test.go` discards the first) and the `store.Message` field for sent-at; set a sent-at value if the schema requires one. If `query.NewEngine` has a different signature, copy the exact call from `api_daemon_test.go:41`.

- [ ] **Step 2: Run — PASS:** `go test ./cmd/msgvault/cmd/ -run TestDeletionStagingEndToEnd -v`
(If it fails, fix the seeding/field details per the NOTE — the HTTP assertions themselves must not be weakened.)

- [ ] **Step 3: fmt, vet, commit:**
```bash
go fmt ./... && go vet ./...
git add -A && git commit -m "Add end-to-end test for deletion staging API"
```

---

### Task 8: Schema version bump, OpenAPI regeneration, full verification

**Files:**
- Modify: `internal/api/openapi.go:24` (`APISchemaVersion`)
- Regenerate: `api/openapi.yaml`, `pkg/client/openapi.yaml`, `pkg/client/generated/*` (committed artifacts)

- [ ] **Step 1: Bump the schema version.** In `internal/api/openapi.go`, replace the constant and extend its doc comment following the existing 1.1.0 entry's style:

```go
// 1.2.0: adds the deletion staging endpoints — POST /api/v1/deletions
// (server-side Gmail-ID resolution, dry-run preview), GET /api/v1/deletions
// (list staged manifests by status), and DELETE /api/v1/deletions/{id}
// (cancel a pending/in-progress manifest). Additive (minor bump): the
// major-version compatibility gate stays at 1.
const APISchemaVersion = "1.2.0"
```

(Keep the existing 1.1.0 paragraph above it.)

- [ ] **Step 2: Regenerate committed API artifacts:**
`make api-generate`
Expected: `api/openapi.yaml`, `pkg/client/openapi.yaml`, and files under `pkg/client/generated/` change; the new `stageDeletion`/`listDeletions`/`cancelDeletion` operations appear.

- [ ] **Step 3: Full verification:**
```bash
make lint
go vet ./...
make test
```
Expected: zero warnings, all tests pass (OpenAPI contract tests in `internal/api/openapi_test.go` validate the regenerated schema).

- [ ] **Step 4: Commit everything, including generated files:**
```bash
go fmt ./...
git add -A && git commit -m "Bump API schema to 1.2.0 and regenerate client for deletion endpoints"
```
