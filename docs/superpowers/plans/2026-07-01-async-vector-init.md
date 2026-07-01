# Async Vector Backend Initialization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the msgvault daemon's vector backend init (migrations + embed_gen backfill) off the startup critical path so the API serves the TUI immediately, with vector status visible in `/health`, `/api/v1/stats`, and `serve status`.

**Architecture:** `runServe` keeps a cheap synchronous vector-config precheck, starts the API server with vector status `initializing`, then runs `setupVectorFeatures` in a background goroutine serialized by the existing operation gate. On success the goroutine installs the hybrid engine/backend into the running `api.Server` (new RWMutex-guarded state) and registers the embed job; on failure the daemon keeps serving with status `error`.

**Tech Stack:** Go, net/http + Huma routes, testify, existing `SerialOperationGate` / `IdleTracker` / `scheduler.WorkTracker` machinery.

**Spec:** `docs/superpowers/specs/2026-07-01-async-vector-init-design.md`

## Global Constraints

- Repo: `/Users/wesm/.superset/worktrees/msgvault/uncovered-flame`, branch `uncovered-flame`.
- All tests use testify (`require.X` halts, `assert.X` continues; args are `(want, got)`). Never `t.Fatalf`/`t.Errorf`.
- After Go changes run `go fmt ./...` and `go vet ./...`; stage ALL resulting changes.
- Commit after every task; never `--amend`; imperative subject ≤72 chars; end commit body with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Build tags: vector code compiles under `-tags "fts5 sqlite_vec"`. Run cmd-package tests with `go test -tags "fts5 sqlite_vec" ./cmd/... ./internal/api/...` (mirror what `make test` uses — check `Makefile` `test:` target and use its tags).
- No real PII in fixtures — synthetic addresses like `user@example.com` only.
- MCP path (`setupVectorFeatures` callers other than `runServe`) must keep working unchanged.

---

### Task 1: `internal/api` — vector status enum and guarded vector state

**Files:**
- Create: `internal/api/vector_status.go`
- Modify: `internal/api/server.go` (Server struct fields, ServerOptions, NewServerWithOptions)
- Test: `internal/api/vector_status_test.go`

**Interfaces:**
- Produces (later tasks rely on these exact names):
  - `type VectorStatus string` with constants `VectorStatusDisabled`, `VectorStatusInitializing`, `VectorStatusReady`, `VectorStatusError` (values `"disabled"`, `"initializing"`, `"ready"`, `"error"`).
  - `ServerOptions.VectorStatus VectorStatus` (zero value = derive: `Backend != nil` → ready, else disabled).
  - `func (s *Server) SetVectorFeatures(engine *hybrid.Engine, backend vector.Backend, cfg vector.Config)` — installs components, sets status ready.
  - `func (s *Server) SetVectorInitError(err error)` — sets status error + message.
  - `func (s *Server) VectorStatus() (VectorStatus, string)` — returns status and error message (empty unless error).
  - unexported `func (s *Server) vectorComponents() (*hybrid.Engine, vector.Backend, vector.Config)` — read under RLock.

- [ ] **Step 1: Write the failing test**

`internal/api/vector_status_test.go`:

```go
package api

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// fakeVectorBackend is a minimal vector.Backend for status tests. Embed the
// interface so only the methods a test touches need implementations; the
// status tests never call any of them.
type fakeVectorBackend struct {
	vector.Backend
}

func TestVectorStatusDerivedFromOptions(t *testing.T) {
	tests := []struct {
		name string
		opts ServerOptions
		want VectorStatus
	}{
		{"no backend defaults to disabled", testServerOptions(t, nil), VectorStatusDisabled},
		{"backend defaults to ready", testServerOptions(t, &fakeVectorBackend{}), VectorStatusReady},
		{
			"explicit initializing wins",
			func() ServerOptions {
				o := testServerOptions(t, nil)
				o.VectorStatus = VectorStatusInitializing
				return o
			}(),
			VectorStatusInitializing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServerWithOptions(tt.opts)
			status, errMsg := srv.VectorStatus()
			assert.Equal(t, tt.want, status)
			assert.Empty(t, errMsg)
		})
	}
}

func TestSetVectorFeaturesTransitionsToReady(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	backend := &fakeVectorBackend{}
	srv.SetVectorFeatures(nil, backend, vector.Config{})

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusReady, status)
	assert.Empty(t, errMsg)
	_, gotBackend, _ := srv.vectorComponents()
	require.NotNil(t, gotBackend)
}

func TestSetVectorInitErrorTransitionsToError(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	srv.SetVectorInitError(errors.New("migration exploded"))

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusError, status)
	assert.Contains(t, errMsg, "migration exploded")
}

func TestSetVectorFeaturesConcurrentReads(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_, _, _ = srv.vectorComponents()
			_, _ = srv.VectorStatus()
		}
	}()
	srv.SetVectorFeatures(nil, &fakeVectorBackend{}, vector.Config{})
	<-done

	status, _ := srv.VectorStatus()
	assert.Equal(t, VectorStatusReady, status)
}
```

`testServerOptions` helper — add to the same test file (mirror the minimal `ServerOptions` construction used elsewhere in `handlers_test.go`, e.g. around its `NewServerWithOptions` call sites; a `&config.Config{}` plus `slog` logger is what `setupRouter` needs):

```go
func testServerOptions(t *testing.T, backend vector.Backend) ServerOptions {
	t.Helper()
	return ServerOptions{
		Config:  &config.Config{},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backend: backend,
	}
}
```

(Adjust imports: `io`, `log/slog`, `go.kenn.io/msgvault/internal/config`. If existing tests already have an equivalent minimal-options helper, reuse it instead of adding this one.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags "fts5 sqlite_vec" ./internal/api/ -run 'TestVectorStatus|TestSetVector' -v`
Expected: compile FAIL — `VectorStatus`, `SetVectorFeatures`, etc. undefined.

- [ ] **Step 3: Implement**

Create `internal/api/vector_status.go`:

```go
package api

import (
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// VectorStatus describes the daemon's vector-search subsystem state. The
// serve daemon starts with `initializing` and flips to `ready` or `error`
// when the background init finishes; non-daemon servers derive `ready` or
// `disabled` from whether a backend was supplied at construction.
type VectorStatus string

const (
	VectorStatusDisabled     VectorStatus = "disabled"
	VectorStatusInitializing VectorStatus = "initializing"
	VectorStatusReady        VectorStatus = "ready"
	VectorStatusError        VectorStatus = "error"
)

// SetVectorFeatures installs the vector components into a running server.
// The serve daemon calls this from its background init goroutine once
// migrations and the embed_gen backfill complete.
func (s *Server) SetVectorFeatures(engine *hybrid.Engine, backend vector.Backend, cfg vector.Config) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.hybridEngine = engine
	s.backend = backend
	s.vectorCfg = cfg
	s.vectorStatus = VectorStatusReady
	s.vectorErr = ""
}

// SetVectorInitError marks the vector subsystem as failed. The daemon keeps
// serving; vector endpoints return 503 carrying the message.
func (s *Server) SetVectorInitError(err error) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorStatus = VectorStatusError
	if err != nil {
		s.vectorErr = err.Error()
	}
}

// VectorStatus returns the vector subsystem status and, when the status is
// VectorStatusError, the failure message.
func (s *Server) VectorStatus() (VectorStatus, string) {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.vectorStatus, s.vectorErr
}

func (s *Server) vectorComponents() (*hybrid.Engine, vector.Backend, vector.Config) {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.hybridEngine, s.backend, s.vectorCfg
}
```

Modify `internal/api/server.go`:

1. Server struct — replace the three bare fields and add guarded state (keep field order/grouping tidy):

```go
	// vectorMu guards the vector subsystem state: the daemon installs
	// hybridEngine/backend/vectorCfg from a background init goroutine
	// after the server is already handling requests.
	vectorMu     sync.RWMutex
	hybridEngine *hybrid.Engine
	vectorCfg    vector.Config
	backend      vector.Backend
	vectorStatus VectorStatus
	vectorErr    string
```

2. `ServerOptions` — add below `Backend`:

```go
	// VectorStatus is the initial vector subsystem status. Zero value
	// derives it: ready when Backend is non-nil, disabled otherwise. The
	// serve daemon passes VectorStatusInitializing and installs the
	// components later via SetVectorFeatures.
	VectorStatus VectorStatus
```

3. `NewServerWithOptions` — after building `s`, before `s.router = s.setupRouter()`:

```go
	s.vectorStatus = opts.VectorStatus
	if s.vectorStatus == "" {
		if opts.Backend != nil {
			s.vectorStatus = VectorStatusReady
		} else {
			s.vectorStatus = VectorStatusDisabled
		}
	}
```

- [ ] **Step 4: Run tests (with race detector)**

Run: `go test -tags "fts5 sqlite_vec" -race ./internal/api/ -run 'TestVectorStatus|TestSetVector' -v`
Expected: PASS

- [ ] **Step 5: Run the whole api package + fmt/vet, commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/api/ && go fmt ./... && go vet -tags "fts5 sqlite_vec" ./...
git add -A
git commit -m "feat(api): add guarded vector state with late install"
```

---

### Task 2: `internal/api` — handlers read guarded state and return status-aware 503s

**Files:**
- Modify: `internal/api/handlers.go` (`handleStats` ~411, `handleSearch` ~600, `handleHybridSearch` ~676, `handleSimilarSearch` ~808)
- Test: `internal/api/vector_status_test.go` (extend)

**Interfaces:**
- Consumes: Task 1's `vectorComponents()`, `VectorStatus()`.
- Produces: unexported `func (s *Server) writeVectorUnavailable(w http.ResponseWriter)` writing 503 with code `vector_initializing`, `vector_init_failed`, or `vector_not_enabled` depending on status.

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/vector_status_test.go`. Use the similar-search endpoint (backend is an interface, so no real engine needed) and drive requests through `srv.Router()` with `httptest`, following the request pattern at `handlers_test.go:4226`:

```go
func TestSimilarSearchStatusAware503(t *testing.T) {
	tests := []struct {
		name        string
		status      VectorStatus
		initErr     error
		wantCode    string
		wantMessage string
	}{
		{"initializing", VectorStatusInitializing, nil, "vector_initializing", "initializing"},
		{"error", VectorStatusError, errors.New("migration exploded"), "vector_init_failed", "migration exploded"},
		{"disabled", VectorStatusDisabled, nil, "vector_not_enabled", "not configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testServerOptions(t, nil)
			opts.VectorStatus = tt.status
			srv := NewServerWithOptions(opts)
			if tt.initErr != nil {
				srv.SetVectorInitError(tt.initErr)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1", nil)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			var body struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tt.wantCode, body.Error)
			assert.Contains(t, body.Message, tt.wantMessage)
		})
	}
}

func TestHybridSearchInitializing503(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=hello&mode=hybrid", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "vector_initializing", body.Error)
}
```

Note: `handleSearch` requires a store for hybrid mode? Check the handler: it returns `store_unavailable` when `s.store == nil` BEFORE the mode branch. If so, set `opts.Store = &mockStore{}` (the existing mock in `handlers_test.go`) in `TestHybridSearchInitializing503`. Verify against the actual code and existing tests.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags "fts5 sqlite_vec" ./internal/api/ -run 'TestSimilarSearchStatusAware503|TestHybridSearchInitializing503' -v`
Expected: FAIL — body.Error is `vector_not_enabled` for the initializing/error cases (current hardcoded message).

- [ ] **Step 3: Implement**

Add to `internal/api/vector_status.go`:

```go
// writeVectorUnavailable reports why vector search cannot serve a request
// right now: still initializing (daemon background migration), failed to
// initialize, or simply not enabled.
func (s *Server) writeVectorUnavailable(w http.ResponseWriter) {
	status, errMsg := s.VectorStatus()
	switch status {
	case VectorStatusInitializing:
		writeError(w, http.StatusServiceUnavailable, "vector_initializing",
			"vector search is initializing (schema migration or backfill in progress); retry shortly")
	case VectorStatusError:
		writeError(w, http.StatusServiceUnavailable, "vector_init_failed",
			"vector search failed to initialize: "+errMsg)
	default:
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
			"vector search is not configured on this server")
	}
}
```

(add `net/http` import to that file)

In `internal/api/handlers.go`:

1. `handleHybridSearch` (~line 676) — replace:

```go
	if s.hybridEngine == nil {
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
			"vector search is not configured on this server")
		return
	}
```

with:

```go
	hybridEngine, _, vectorCfg := s.vectorComponents()
	if hybridEngine == nil {
		s.writeVectorUnavailable(w)
		return
	}
```

then replace every subsequent `s.hybridEngine` in the function with `hybridEngine` (BuildFilter ~701, Search ~717). `vectorCfg` is unused here unless the function references `s.vectorCfg` — it doesn't today; drop it from the assignment if so (`hybridEngine, _, _ :=`).

2. `handleSearch` (~line 600) — the clamp reads `s.vectorCfg` directly:

```go
		if maxPage := s.vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && pageSize > maxPage {
```

becomes:

```go
		_, _, vectorCfg := s.vectorComponents()
		if maxPage := vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && pageSize > maxPage {
```

3. `handleSimilarSearch` (~line 808) — replace the leading nil-check:

```go
	if s.backend == nil {
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
			"vector search is not configured on this server")
		return
	}
```

with:

```go
	_, backend, vectorCfg := s.vectorComponents()
	if backend == nil {
		s.writeVectorUnavailable(w)
		return
	}
```

and replace all later `s.backend` → `backend`, `s.vectorCfg` → `vectorCfg` within the function (clamp ~829, ResolveActiveForFingerprint ~840, ValidateBuildScope ~845, LoadVector ~850, Search ~856).

4. `handleStats` (~line 427) — replace `vector.CollectStats(r.Context(), s.backend)`:

```go
	_, backend, _ := s.vectorComponents()
	vs, vsErr := vector.CollectStats(r.Context(), backend)
```

5. Grep for any remaining direct reads outside `NewServerWithOptions`/`vector_status.go`:
   `rg -n 's\.(hybridEngine|backend|vectorCfg)' internal/api/ --type go | grep -v _test | grep -v vector_status.go | grep -v server.go` — convert any stragglers (e.g. `similarSearchFilter`, `writeVectorSearchError`, CLI handlers) the same way.

- [ ] **Step 4: Run tests**

Run: `go test -tags "fts5 sqlite_vec" -race ./internal/api/ -run 'TestSimilarSearch|TestHybridSearch|TestVectorStatus|TestSetVector' -v`
Expected: PASS

- [ ] **Step 5: Full api package, fmt/vet, commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/api/ && go fmt ./... && go vet -tags "fts5 sqlite_vec" ./...
git add -A
git commit -m "feat(api): status-aware vector 503s via guarded reads"
```

---

### Task 3: `internal/api` — expose vector status in `/health` and `/api/v1/stats`, regenerate OpenAPI

**Files:**
- Modify: `internal/api/handlers.go` (`HealthResponse` ~122, `StatsResponse` ~37, `handleStats` ~432), `internal/api/server.go` (`handleHealth` ~451)
- Regenerate: `api/openapi.yaml`, `pkg/client/openapi.yaml`, `pkg/client/generated/*` via `make openapi`
- Test: `internal/api/vector_status_test.go` (extend)

**Interfaces:**
- Produces:
  - `type VectorHealth struct { Status string `json:"status"`; Error string `json:"error,omitempty"` }`
  - `HealthResponse.Vector *VectorHealth` (`json:"vector,omitempty"`) — nil when vector disabled.
  - `StatsResponse.VectorStatus string` (`json:"vector_status,omitempty"`) — empty when disabled.
  - `func (s *Server) vectorHealth() *VectorHealth` — nil when disabled. Task 7's CLI decodes this shape.

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/vector_status_test.go`:

```go
func TestHealthReportsVectorStatus(t *testing.T) {
	tests := []struct {
		name       string
		status     VectorStatus
		initErr    error
		wantVector *VectorHealth
	}{
		{"disabled omits vector", VectorStatusDisabled, nil, nil},
		{"initializing", VectorStatusInitializing, nil, &VectorHealth{Status: "initializing"}},
		{"error carries message", VectorStatusError, errors.New("migration exploded"),
			&VectorHealth{Status: "error", Error: "migration exploded"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testServerOptions(t, nil)
			opts.VectorStatus = tt.status
			srv := NewServerWithOptions(opts)
			if tt.initErr != nil {
				srv.SetVectorInitError(tt.initErr)
			}

			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			var body HealthResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "ok", body.Status)
			assert.Equal(t, tt.wantVector, body.Vector)
		})
	}
}
```

And a stats assertion — extend or mirror the existing stats handler test (see `handlers_test.go:153` neighborhood for how stats tests build the server with a mock store):

```go
func TestStatsReportsVectorStatus(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t) // reuse existing helper at handlers_test.go:50
	srv.vectorMu.Lock()
	srv.vectorStatus = VectorStatusInitializing
	srv.vectorMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body StatsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "initializing", body.VectorStatus)
}
```

(If `newTestServerWithMockStore` sets options incompatible with this, construct via `NewServerWithOptions` + `mockStore` directly, per the existing stats tests.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags "fts5 sqlite_vec" ./internal/api/ -run 'TestHealthReportsVectorStatus|TestStatsReportsVectorStatus' -v`
Expected: compile FAIL — `VectorHealth`, `body.Vector`, `body.VectorStatus` undefined.

- [ ] **Step 3: Implement**

`internal/api/handlers.go`:

```go
// VectorHealth reports the vector subsystem state in health responses so
// daemon status is visible while background init runs (or after it fails).
type VectorHealth struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type HealthResponse struct {
	Status string        `json:"status"`
	Vector *VectorHealth `json:"vector,omitempty"`
}
```

`StatsResponse` (~line 37): add field

```go
	VectorStatus  string            `json:"vector_status,omitempty"`
```

`handleStats` (~line 432): after `resp.VectorSearch = vs`:

```go
	if status, _ := s.VectorStatus(); status != VectorStatusDisabled {
		resp.VectorStatus = string(status)
	}
```

`internal/api/vector_status.go`:

```go
// vectorHealth returns the health-response view of the vector subsystem,
// or nil when vector search is disabled.
func (s *Server) vectorHealth() *VectorHealth {
	status, errMsg := s.VectorStatus()
	if status == VectorStatusDisabled {
		return nil
	}
	return &VectorHealth{Status: string(status), Error: errMsg}
}
```

`internal/api/server.go` `handleHealth` (~line 451):

```go
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok", Vector: s.vectorHealth()})
}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags "fts5 sqlite_vec" ./internal/api/ -v -run 'TestHealth|TestStats'`
Expected: PASS (existing health/stats tests must also still pass — the new fields are omitempty/nil for them).

- [ ] **Step 5: Regenerate OpenAPI artifacts**

Run: `make openapi`
Then: `go test ./internal/api/ -run 'OpenAPI' -v` and `go build ./pkg/...`
Expected: artifacts updated, generated client compiles. If `make openapi` needs build tags, check the Makefile target and follow it exactly.

- [ ] **Step 6: fmt/vet, commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/api/ && go fmt ./... && go vet -tags "fts5 sqlite_vec" ./...
git add -A
git commit -m "feat(api): report vector status in health and stats"
```

---

### Task 4: `cmd` — synchronous vector precheck per build tag

**Files:**
- Modify: `cmd/msgvault/cmd/serve_vector.go`, `cmd/msgvault/cmd/serve_vector_stub.go`
- Test: `cmd/msgvault/cmd/serve_vector_precheck_test.go` (new; build-tagged `sqlite_vec || pgvector`)
- Test helper: `cmd/msgvault/cmd/vector_test_helpers_test.go` (new; NO build tags — Task 5's untagged tests reuse `withTestConfig`)

**Interfaces:**
- Produces: `func precheckVectorFeatures(mainPath string) error` — nil when vector disabled; in vector builds validates `cfg.Vector.Validate()` + embed cron expression; in stub builds returns the existing built-without-tags error.

- [ ] **Step 1: Write the failing test**

`cmd/msgvault/cmd/serve_vector_precheck_test.go` (copy the build tag line from `serve_vector.go`: `//go:build sqlite_vec || pgvector`). Look at `serve_test.go` / `serve_vector_stub_test.go` for how tests swap the package-level `cfg` — follow that pattern (there is a global `cfg *config.Config`; tests set it and restore via `t.Cleanup`):

First, `cmd/msgvault/cmd/vector_test_helpers_test.go` (no build tags, so it
compiles in every build; check first whether an equivalent cfg-swap helper
already exists in the package and reuse it if so):

```go
package cmd

import (
	"testing"

	"go.kenn.io/msgvault/internal/config"
)

func withTestConfig(t *testing.T, c *config.Config) {
	t.Helper()
	prev := cfg
	cfg = c
	t.Cleanup(func() { cfg = prev })
}
```

Then the precheck tests:

```go
//go:build sqlite_vec || pgvector

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestPrecheckVectorFeaturesDisabled(t *testing.T) {
	c := config.Default() // or &config.Config{} — match how serve_test.go builds configs
	c.Vector.Enabled = false
	withTestConfig(t, c)

	assert.NoError(t, precheckVectorFeatures("/tmp/msgvault.db"))
}

func TestPrecheckVectorFeaturesRejectsBadCron(t *testing.T) {
	c := config.Default()
	c.Vector.Enabled = true
	// Fill the minimum valid [vector] config the way TestSetupVectorFeatures_*
	// tests do (endpoint/model/dimension), then break only the cron:
	c.Vector.Embed.Schedule.Cron = "not a cron"
	withTestConfig(t, c)

	err := precheckVectorFeatures("/tmp/msgvault.db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cron")
}

func TestPrecheckVectorFeaturesRejectsInvalidConfig(t *testing.T) {
	c := config.Default()
	c.Vector.Enabled = true
	// leave required embeddings fields empty so Validate() fails
	withTestConfig(t, c)

	assert.Error(t, precheckVectorFeatures("/tmp/msgvault.db"))
}
```

**Adjust the config construction to reality:** read `internal/config` for the actual `Vector` struct fields and what `Validate()` requires; read `serve_test.go:574` (`TestSetupVectorFeatures_Disabled`) for the established pattern of building test configs in this package. If a `withTestConfig`-style helper already exists, reuse it.

Also add a stub-build test in `serve_vector_stub_test.go` (tagged `!sqlite_vec && !pgvector`) asserting `precheckVectorFeatures("/tmp/x.db")` errors mentioning `sqlite_vec` when enabled and is nil when disabled.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/ -run TestPrecheckVectorFeatures -v`
Expected: compile FAIL — `precheckVectorFeatures` undefined.

- [ ] **Step 3: Implement**

`cmd/msgvault/cmd/serve_vector.go` — add:

```go
// precheckVectorFeatures validates vector configuration cheaply so runServe
// can fail fast on misconfiguration while deferring the expensive backend
// open/migrate/backfill to the background init task. Returns nil when
// vector search is disabled. The mainPath parameter is unused in vector
// builds; the stub build uses it to pick the right rebuild guidance.
func precheckVectorFeatures(_ string) error {
	if !cfg.Vector.Enabled {
		return nil
	}
	if err := cfg.Vector.Validate(); err != nil {
		return fmt.Errorf("vector config: %w", err)
	}
	if cronExpr := cfg.Vector.Embed.Schedule.Cron; cronExpr != "" {
		if err := scheduler.ValidateCronExpr(cronExpr); err != nil {
			return fmt.Errorf("invalid embed cron expression %q: %w", cronExpr, err)
		}
	}
	return nil
}
```

(import `go.kenn.io/msgvault/internal/scheduler`)

`cmd/msgvault/cmd/serve_vector_stub.go` — extract the two error constructions from `setupVectorFeatures` into:

```go
func errVectorBuildUnsupported(mainPath string) error {
	if store.IsPostgresURL(mainPath) {
		return errors.New("vector search is enabled in config but this binary was built without vector support; " +
			"to use vector search on PostgreSQL, rebuild with `go build -tags \"fts5 sqlite_vec pgvector\"` " +
			"or set [vector] enabled = false")
	}
	return errors.New("vector search is enabled in config but this binary was built without -tags sqlite_vec; " +
		"rebuild with `make build` (or `go build -tags \"fts5 sqlite_vec\"`) " +
		"or set [vector] enabled = false")
}

func precheckVectorFeatures(mainPath string) error {
	if !cfg.Vector.Enabled {
		return nil
	}
	return errVectorBuildUnsupported(mainPath)
}
```

and make the stub `setupVectorFeatures` return `errVectorBuildUnsupported(mainPath)` instead of the inline errors.

- [ ] **Step 4: Run tests under both build configurations**

Run: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/ -run 'TestPrecheck|TestSetupVectorFeatures' -v`
Run: `go test ./cmd/msgvault/cmd/ -run 'TestPrecheck|TestSetupVectorFeatures' -v` (stub build — confirm it compiles and stub tests pass)
Expected: PASS both.

- [ ] **Step 5: fmt/vet, commit**

```bash
go fmt ./... && go vet -tags "fts5 sqlite_vec" ./...
git add -A
git commit -m "feat(daemon): add cheap vector config precheck"
```

---

### Task 5: `cmd` — background vector init task

**Files:**
- Create: `cmd/msgvault/cmd/serve_vector_init.go` (NO build tags — `vectorFeatures` and `setupVectorFeatures` exist in all builds)
- Test: `cmd/msgvault/cmd/serve_vector_init_test.go` (no build tags)

**Interfaces:**
- Consumes: `setupVectorFeatures` (via new seam `setupVectorFeaturesForRun`), `api.Server.SetVectorFeatures`/`SetVectorInitError` (Task 1), `scheduler.WorkTracker`, `combineWorkTrackers` (work_tracker.go:11).
- Produces:
  - `var setupVectorFeaturesForRun = setupVectorFeatures` (seam, same pattern as `buildCacheSubprocessForRun` at serve.go:63).
  - `type vectorInitHandle struct` with methods `WaitTimeout(d time.Duration) bool` and `CloseFeatures()`.
  - `func startVectorInit(ctx context.Context, s *store.Store, dbPath string, tracker scheduler.WorkTracker, apiServer *api.Server, sched *scheduler.Scheduler) *vectorInitHandle`
  - `func registerEmbedJob(sched *scheduler.Scheduler, vf *vectorFeatures, s *store.Store) error` (extracted from serve.go:266-286).

- [ ] **Step 1: Write the failing tests**

`cmd/msgvault/cmd/serve_vector_init_test.go`:

```go
package cmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

func newVectorInitTestServer(t *testing.T) *api.Server {
	t.Helper()
	return api.NewServerWithOptions(api.ServerOptions{
		Config:       &config.Config{},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		VectorStatus: api.VectorStatusInitializing,
	})
}

func overrideSetupVectorFeatures(t *testing.T, fn func(context.Context, *store.Store, string, bool) (*vectorFeatures, error)) {
	t.Helper()
	prev := setupVectorFeaturesForRun
	setupVectorFeaturesForRun = fn
	t.Cleanup(func() { setupVectorFeaturesForRun = prev })
}

func waitForVectorStatus(t *testing.T, srv *api.Server, want api.VectorStatus) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, msg := srv.VectorStatus()
		if status == want {
			return msg
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, _ := srv.VectorStatus()
	require.Equal(t, want, status, "vector status never reached %s", want)
	return ""
}

func TestStartVectorInitDisabledFinishesImmediately(t *testing.T) {
	c := config.Default()
	c.Vector.Enabled = false
	withTestConfig(t, c)

	h := startVectorInit(context.Background(), nil, "", nil, nil, nil)
	assert.True(t, h.WaitTimeout(time.Second))
}

func TestStartVectorInitInstallsFeaturesOnSuccess(t *testing.T) {
	c := config.Default()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	closed := false
	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return &vectorFeatures{
			Backend: &fakeCmdVectorBackend{},
			Close:   func() error { closed = true; return nil },
		}, nil
	})

	srv := newVectorInitTestServer(t)
	sched := scheduler.New(nil)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil, srv, sched)

	require.True(t, h.WaitTimeout(5*time.Second))
	waitForVectorStatus(t, srv, api.VectorStatusReady)
	h.CloseFeatures()
	assert.True(t, closed, "CloseFeatures must close the opened backend")
}

func TestStartVectorInitReportsError(t *testing.T) {
	c := config.Default()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return nil, errors.New("migration exploded")
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))

	require.True(t, h.WaitTimeout(5*time.Second))
	msg := waitForVectorStatus(t, srv, api.VectorStatusError)
	assert.Contains(t, msg, "migration exploded")
}

func TestStartVectorInitHoldsWorkTracker(t *testing.T) {
	c := config.Default()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	gate := api.NewSerialOperationGate()
	release := make(chan struct{})
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		<-release
		return nil, ctx.Err()
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", gate, srv, scheduler.New(nil))

	// While init runs, the gate must be held: BeginWorkContext with an
	// already-cancelled context must fail rather than acquire.
	assert.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		done, ok := gate.BeginWorkContext(ctx)
		if ok {
			done()
		}
		return !ok
	}, 2*time.Second, 10*time.Millisecond, "gate should be held during init")

	close(release)
	require.True(t, h.WaitTimeout(5*time.Second))
	done, ok := gate.BeginWork()
	require.True(t, ok, "gate must be released after init")
	done()
}

func TestStartVectorInitAbortsQuietlyOnCancel(t *testing.T) {
	c := config.Default()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(ctx, nil, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))
	cancel()

	require.True(t, h.WaitTimeout(5*time.Second))
	status, _ := srv.VectorStatus()
	assert.Equal(t, api.VectorStatusInitializing, status,
		"shutdown-cancelled init must not flip status to error")
}
```

Add `fakeCmdVectorBackend` (embed `vector.Backend` like Task 1's fake) and the missing `store` import. `withTestConfig` comes from Task 4's untagged `vector_test_helpers_test.go`.

Embed-job note: `registerEmbedJob` reads `vf.Worker` (nil in fakes) and `vf.Cfg` — with `scheduler.New(nil)` and empty cron it must not panic; the success test exercises that path implicitly.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/ -run TestStartVectorInit -v`
Expected: compile FAIL — `startVectorInit`, `setupVectorFeaturesForRun` undefined.

- [ ] **Step 3: Implement**

`cmd/msgvault/cmd/serve_vector_init.go`:

```go
package cmd

import (
	"context"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// setupVectorFeaturesForRun is a test seam for the build-tag-selected
// setupVectorFeatures implementation.
var setupVectorFeaturesForRun = setupVectorFeatures

// vectorInitHandle tracks the background vector init goroutine so shutdown
// can wait for it and close the opened backend.
type vectorInitHandle struct {
	done chan struct{}
	mu   sync.Mutex
	vf   *vectorFeatures
}

// WaitTimeout blocks until the init goroutine finishes or d elapses.
// Returns false on timeout.
func (h *vectorInitHandle) WaitTimeout(d time.Duration) bool {
	select {
	case <-h.done:
		return true
	case <-time.After(d):
		return false
	}
}

// CloseFeatures closes the vector backend if the init goroutine opened one.
// Only call after WaitTimeout reports the goroutine finished.
func (h *vectorInitHandle) CloseFeatures() {
	h.mu.Lock()
	vf := h.vf
	h.vf = nil
	h.mu.Unlock()
	if vf != nil && vf.Close != nil {
		if err := vf.Close(); err != nil {
			logger.Warn("closing vectors.db failed", "error", err)
		}
	}
}

// startVectorInit runs the expensive vector backend setup (open, schema
// migrations, embed_gen backfill) in the background so the daemon API can
// serve archive requests immediately. The tracker (idle tracker + operation
// gate) serializes the init's msgvault.db writes against scheduled syncs
// and keeps a background daemon from idle-stopping mid-migration. On
// success the components are installed into apiServer and the embed job is
// registered; on failure the daemon keeps serving with vector endpoints
// reporting the error.
func startVectorInit(
	ctx context.Context,
	s *store.Store,
	dbPath string,
	tracker scheduler.WorkTracker,
	apiServer *api.Server,
	sched *scheduler.Scheduler,
) *vectorInitHandle {
	h := &vectorInitHandle{done: make(chan struct{})}
	if !cfg.Vector.Enabled {
		close(h.done)
		return h
	}
	go func() {
		defer close(h.done)
		logger.Info("daemon startup step",
			"step", "init_vector_backend",
			"detail", "running in background; may run vector schema migrations and embed_gen backfill on large archives")
		if tracker != nil {
			release, ok := tracker.BeginWorkContext(ctx)
			if !ok {
				logger.Info("vector init aborted", "reason", "daemon shutting down")
				return
			}
			defer release()
		}
		vf, err := setupVectorFeaturesForRun(ctx, s, dbPath, false)
		if err != nil {
			if ctx.Err() != nil {
				logger.Info("vector init cancelled during daemon shutdown")
				return
			}
			logger.Error("vector init failed; vector search unavailable until fixed",
				"error", err)
			apiServer.SetVectorInitError(err)
			return
		}
		h.mu.Lock()
		h.vf = vf
		h.mu.Unlock()
		apiServer.SetVectorFeatures(vf.HybridEngine, vf.Backend, vf.Cfg)
		if err := registerEmbedJob(sched, vf, s); err != nil {
			// Cron was validated in precheckVectorFeatures, so this is an
			// invariant violation, not user error; vector search still works.
			logger.Error("register embed job failed", "error", err)
		}
		logger.Info("daemon startup step complete", "step", "init_vector_backend")
	}()
	return h
}
```

(`setupVectorFeatures` returns non-nil whenever `cfg.Vector.Enabled` is true and
err is nil, so no nil-vf branch is needed. Add `fmt` to imports for
`registerEmbedJob` below.)

`registerEmbedJob` — extracted from serve.go:266-286 verbatim:

```go
// registerEmbedJob wires the embed worker into the scheduler (cron-driven
// plus optional post-sync hook). Extracted from runServe so the background
// vector init can register it once the backend is ready.
func registerEmbedJob(sched *scheduler.Scheduler, vf *vectorFeatures, s *store.Store) error {
	embedJob := &scheduler.EmbedJob{
		Worker:           vf.Worker,
		Backend:          vf.Backend,
		Store:            s,
		Fingerprint:      vf.Cfg.GenerationFingerprint(),
		BackstopInterval: vf.Cfg.Embed.BackstopInterval,
		BuildScope:       vf.Cfg.Embed.Scope.BuildScope(),
		Log:              logger,
	}
	schedule := cfg.Vector.Embed.Schedule.Cron
	if err := sched.SetEmbedJob(embedJob, schedule, cfg.Vector.Embed.Schedule.RunAfterSync); err != nil {
		return fmt.Errorf("register embed job: %w", err)
	}
	logger.Info("embed scheduled",
		"cron", schedule,
		"run_after_sync", cfg.Vector.Embed.Schedule.RunAfterSync,
	)
	return nil
}
```

- [ ] **Step 4: Run tests (race detector)**

Run: `go test -tags "fts5 sqlite_vec" -race ./cmd/msgvault/cmd/ -run TestStartVectorInit -v`
Expected: PASS. Also run the untagged build: `go test -race ./cmd/msgvault/cmd/ -run TestStartVectorInit -v` — PASS (seam makes it build-independent).

- [ ] **Step 5: fmt/vet, commit**

```bash
go fmt ./... && go vet -tags "fts5 sqlite_vec" ./...
git add -A
git commit -m "feat(daemon): add background vector init task"
```

---

### Task 6: `cmd` — wire runServe to async init

**Files:**
- Modify: `cmd/msgvault/cmd/serve.go` (`runServe`, lines ~162-186 vector block, ~264-286 embed block, ~299-330 API opts/start, ~351-375 shutdown)
- Test: `cmd/msgvault/cmd/serve_test.go` (new integration test)

**Interfaces:**
- Consumes: `precheckVectorFeatures` (Task 4), `startVectorInit`/`vectorInitHandle` (Task 5), `api.ServerOptions.VectorStatus` (Task 1).

- [ ] **Step 1: Write the failing integration test**

Add to `cmd/msgvault/cmd/serve_test.go`, modeled on `TestRunServeFailsBeforeArchiveWorkWhenAPIPortInUse` (serve_test.go:167) — reuse its temp-dir/config/port scaffolding (read that test first and copy its setup helpers exactly; it shows how to build `cfg`, pick a free port, and invoke `runServe` via the cobra command):

```go
func TestRunServeServesHealthWhileVectorInitBlocked(t *testing.T) {
	// Build a config with vector enabled, pointing at a temp data dir.
	// (copy the config/temp-dir setup from TestRunServeFailsBeforeArchiveWork...,
	// then set Vector.Enabled = true plus the minimal valid [vector] fields
	// used by the precheck tests.)

	release := make(chan struct{})
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, ctx.Err()
	})

	// Run runServe in a goroutine (same pattern as existing serve tests that
	// exercise the full command; capture the returned error).
	// Then poll GET http://127.0.0.1:<port>/health until it answers 200 —
	// while setupVectorFeatures is still blocked on `release`.

	var health struct {
		Status string `json:"status"`
		Vector *struct {
			Status string `json:"status"`
		} `json:"vector"`
	}
	require.Eventually(t, func() bool {
		resp, err := http.Get(healthURL)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		return json.NewDecoder(resp.Body).Decode(&health) == nil
	}, 10*time.Second, 25*time.Millisecond, "health must answer while vector init is blocked")
	require.NotNil(t, health.Vector)
	assert.Equal(t, "initializing", health.Vector.Status)

	// Shut down: close(release) then send the shutdown (existing tests use
	// the shutdown token POST or context cancellation — follow the pattern
	// in TestShutdownServeRuntimeDrainsGate... / other runServe tests).
	// Assert runServe returns nil.
}
```

This is intentionally sketched at the seams that must copy existing scaffolding — the assertions (health 200 + vector.status=initializing while init is blocked, clean shutdown) are the contract. Do not weaken them.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/ -run TestRunServeServesHealthWhileVectorInitBlocked -v`
Expected: FAIL — health never answers while init is blocked (current code blocks before `StartOnListener`), so the Eventually times out. (If config validation fails first, fix the test config, not the assertion.)

- [ ] **Step 3: Modify runServe**

In `cmd/msgvault/cmd/serve.go`:

1. Replace the synchronous vector block (lines ~162-186: the `if cfg.Vector.Enabled` logging, `setupVectorFeatures` call, error return, completion log, and the `defer vf.Close` block) with:

```go
	// Vector misconfiguration still fails startup fast; the expensive
	// backend open/migrate/backfill runs in the background after the API
	// server is listening (startVectorInit below), so the TUI and other
	// clients are not blocked by vector maintenance.
	if err := precheckVectorFeatures(dbPath); err != nil {
		return fmt.Errorf("vector features: %w", err)
	}
	if !cfg.Vector.Enabled {
		logger.Info("daemon startup step", "step", "skip_vector_backend", "enabled", false)
	}
```

2. Delete the embed-job registration block (lines ~264-286, `if vf != nil { ... }`) — `startVectorInit` handles it.

3. API options (~line 314): replace

```go
	if vf != nil {
		apiOpts.HybridEngine = vf.HybridEngine
		apiOpts.Backend = vf.Backend
		apiOpts.VectorCfg = vf.Cfg
	}
```

with:

```go
	if cfg.Vector.Enabled {
		apiOpts.VectorStatus = api.VectorStatusInitializing
	}
```

4. After the API server goroutine + idle tracker start (after line ~335), add:

```go
	vectorInit := startVectorInit(
		ctx, s, dbPath,
		combineWorkTrackers(idleTracker, operationGate),
		apiServer, sched,
	)
```

Note `combineWorkTrackers` filters nil trackers, but `idleTracker` is a typed `*api.IdleTracker` — check how line ~208 passes the same pair; it's the identical call shape, so reuse it verbatim.

5. Shutdown path — after the `select` (line ~353-363), before `shutdownServeRuntime`:

```go
	// Stop background work first: vector init honors ctx, so cancelling
	// lets the operation-gate drain inside shutdownServeRuntime complete.
	cancel()
```

and after `shutdownServeRuntime` succeeds (before the `serverStartupErr` check):

```go
	if vectorInit.WaitTimeout(serveOperationDrainTimeout) {
		vectorInit.CloseFeatures()
	} else {
		logger.Warn("vector init did not stop within the shutdown drain timeout; skipping vectors.db close")
	}
```

6. Confirm nothing else references the removed `vf` variable (`go build -tags "fts5 sqlite_vec" ./cmd/...`).

- [ ] **Step 4: Run the new test and the whole cmd package**

Run: `go test -tags "fts5 sqlite_vec" -race ./cmd/msgvault/cmd/ -run TestRunServe -v`
Then: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/`
Expected: PASS, including the pre-existing runServe tests (port reservation, read-only OAuth, shutdown drain).

- [ ] **Step 5: fmt/vet, commit**

```bash
go fmt ./... && go vet -tags "fts5 sqlite_vec" ./...
git add -A
git commit -m "feat(daemon): serve API before vector init completes"
```

---

### Task 7: `serve status` shows the vector line

**Files:**
- Modify: `cmd/msgvault/cmd/serve_lifecycle.go` (`serveStatusLines` ~96, `runServeStatus` ~73)
- Test: `cmd/msgvault/cmd/serve_lifecycle_test.go` (or wherever `serveStatusLines` is currently tested — `rg -n serveStatusLines cmd/msgvault/cmd/*_test.go`)

**Interfaces:**
- Consumes: `HealthResponse`/`VectorHealth` JSON shape from Task 3, `urlFromDaemonRuntime` (daemon_runtime.go:334).
- Produces: `func fetchDaemonVectorHealth(ctx context.Context, baseURL string) *api.VectorHealth` (nil on any failure — best-effort).

- [ ] **Step 1: Write the failing test**

```go
func TestServeStatusPrintsVectorLine(t *testing.T) {
	tests := []struct {
		name     string
		health   string
		wantLine string
		wantNone bool
	}{
		{"initializing", `{"status":"ok","vector":{"status":"initializing"}}`,
			"vector:  initializing", false},
		{"error with detail", `{"status":"ok","vector":{"status":"error","error":"migration exploded"}}`,
			"vector:  error (migration exploded)", false},
		{"disabled omits line", `{"status":"ok"}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/health", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.health))
			}))
			defer srv.Close()

			vh := fetchDaemonVectorHealth(context.Background(), srv.URL)
			lines := vectorStatusLines(vh)
			if tt.wantNone {
				assert.Empty(t, lines)
				return
			}
			require.Len(t, lines, 1)
			assert.Contains(t, lines[0], tt.wantLine)
		})
	}
}
```

(Also produces `vectorStatusLines(vh *api.VectorHealth) []string` — pure formatting, keeps `serveStatusLines`'s current signature untouched for its existing tests.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/ -run TestServeStatusPrintsVectorLine -v`
Expected: compile FAIL — `fetchDaemonVectorHealth`, `vectorStatusLines` undefined.

- [ ] **Step 3: Implement**

In `cmd/msgvault/cmd/serve_lifecycle.go`:

```go
// fetchDaemonVectorHealth fetches /health from a running daemon and returns
// its vector block. Best-effort: any transport/decode failure returns nil
// and the status output simply omits the vector line.
func fetchDaemonVectorHealth(ctx context.Context, baseURL string) *api.VectorHealth {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil
	}
	return health.Vector
}

func vectorStatusLines(vh *api.VectorHealth) []string {
	if vh == nil {
		return nil
	}
	line := "  vector:  " + vh.Status
	if vh.Error != "" {
		line += " (" + vh.Error + ")"
	}
	return []string{line}
}
```

(add `encoding/json` import)

In `runServeStatus` (~line 75), extend the running branch:

```go
	if rt := findDaemonRuntime(dataDir); rt != nil {
		lines := serveStatusLines(rt)
		lines = append(lines, vectorStatusLines(
			fetchDaemonVectorHealth(cmd.Context(), urlFromDaemonRuntime(rt)))...)
		for _, line := range lines {
			_, _ = fmt.Fprintln(out, line)
		}
		return nil
	}
```

Check whether `/health` requires the API key: look at how auth middleware is applied in `internal/api` (the kit ping at `/api/v1/kit/ping` is explicitly unauthenticated; `/health` is registered the same raw way at routes.go:158). If a test shows 401, add the configured key header (`X-Api-Key: cfg.Server.APIKey`) to the request.

- [ ] **Step 4: Run tests**

Run: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/ -run 'TestServeStatus' -v`
Expected: PASS (including any pre-existing serve-status tests).

- [ ] **Step 5: fmt/vet, commit**

```bash
go fmt ./... && go vet -tags "fts5 sqlite_vec" ./...
git add -A
git commit -m "feat(cli): show vector status in serve status output"
```

---

### Task 8: Full verification sweep

**Files:** none new — verification + any fallout fixes.

- [ ] **Step 1: Full test suite**

Run: `make test`
Expected: PASS. Fix any fallout (e.g. tests that asserted the old synchronous startup log order, or OpenAPI artifact drift — rerun `make openapi` if a handler type changed after Task 3).

- [ ] **Step 2: Lint**

Run: `make lint-ci`
Expected: clean. Fix everything (zero-warnings policy).

- [ ] **Step 3: Stub build check**

Run: `go build ./... && go test ./cmd/msgvault/cmd/ -run 'TestPrecheck|TestStartVectorInit|TestSetupVectorFeatures'`
Expected: compiles and passes without vector tags.

- [ ] **Step 4: Commit any fixes**

```bash
git add -A
git commit -m "test: fix fallout from async vector init"
```

(Skip if no changes.)
