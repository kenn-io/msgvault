# Async Vector Backend Initialization

**Date:** 2026-07-01
**Status:** Approved

## Problem

`runServe` (cmd/msgvault/cmd/serve.go) initializes the vector backend
synchronously before the HTTP API server starts listening. That call chain —
`setupVectorFeatures` → `sqlitevec.Open` → `Migrate` (vec-table rebuilds,
chunked-layout migrations) plus the one-time `BackfillEmbedGenForUpgrade`
(writes `messages.embed_gen` in msgvault.db) — can take minutes on large
archives. The TUI and every other CLI client autostart the daemon and wait for
the API to respond, so optional vector maintenance blocks archive access that
does not need it.

## Design

### Startup sequencing

1. Keep the existing early steps unchanged: reserve API port, claim ownership,
   open store, `InitSchema`, analytics engine, scheduler setup, scheduler
   start.
2. When `cfg.Vector.Enabled`, run only cheap validation synchronously so
   misconfiguration still fails startup fast:
   - `cfg.Vector.Validate()`
   - embed-cron validation (`scheduler.ValidateCronExpr` when a schedule is set)
   - the "binary built without vector support" stub-build error path
   - a dialect-aware backend-availability check: a `postgres://` DSN requires
     the pgvector tag compiled in (`pgvector.Available()`), a SQLite path
     requires the sqlite_vec tag (`sqlitevec.Available()`); either mismatch
     fails fast with rebuild guidance instead of surfacing later in the
     background init goroutine.
   This is a new per-build-tag `precheckVectorFeatures` function with the
   same build-tag split as `setupVectorFeatures`.
3. Start the API server on the reserved listener with vector status
   `initializing` (or `disabled` when `cfg.Vector.Enabled` is false).
4. Launch a background goroutine that:
   - acquires the `SerialOperationGate` (`BeginWorkContext(ctx)`) and touches
     the idle tracker, so the init serializes with scheduled syncs and the
     background daemon cannot idle-shutdown mid-migration;
   - runs `setupVectorFeatures(ctx, ...)` (open, migrate, backfill);
   - on success: installs the features into the API server via
     `SetVectorFeatures`, registers the embed job via `sched.SetEmbedJob`
     (mutex-protected, safe after `Start()`), sets status `ready`;
   - on failure: sets status `error` with the failure message, logs it, and
     leaves the daemon serving (vector endpoints stay 503).
5. The gate is held for the whole init, including the `Migrate` portion that
   only writes vectors.db. Splitting the gate to cover just the msgvault.db
   writes would require plumbing into `sqlitevec.Open`; not worth it.

### API server changes (internal/api)

- `Server.hybridEngine`, `Server.backend`, `Server.vectorCfg` become
  RWMutex-guarded state installed either at construction (tests, current
  callers) or later via a new `SetVectorFeatures(hybrid *hybrid.Engine,
  backend vector.Backend, cfg vector.Config)` method.
- New vector status enum: `disabled`, `initializing`, `ready`, `error`
  (with message). Constructed servers with a non-nil backend start `ready`;
  with vector disabled start `disabled`; runServe passes `initializing`.
- Hybrid search (`/api/v1/search`) and similar search
  (`/api/v1/search/similar`) return 503 with a status-specific message:
  - `initializing`: vector search is initializing (schema migration /
    backfill in progress); retry later.
  - `error`: vector search failed to initialize, with the error detail.
  - `disabled`: current behavior (vector search not enabled).
- `/api/v1/stats` includes the vector status alongside the existing
  `vector_search` stats block (which is only populated once ready).
- `/health` (`HealthResponse`) gains an optional `vector` object with
  `status` and `error` fields so the daemon status is never blind to an
  in-progress or failed vector init. Omitted when vector is disabled.
- OpenAPI artifacts (`api/openapi.yaml`, `pkg/client/*`) are regenerated
  (`make openapi`) since `HealthResponse` and `StatsResponse` change.

### Daemon status CLI

`msgvault serve status` currently prints from the kit ping probe
(`daemon.PingInfo`), which is an external kit type we cannot extend. After
printing the runtime lines, it additionally fetches `GET /health` from the
running daemon's URL (short timeout, best-effort) and prints a
`vector:  <status>` line, including the error detail when status is `error`.
No line is printed when vector is disabled or `/health` is unreachable.

### Shutdown

- Daemon shutdown cancels the shared context; migrations honor ctx.
- `runServe` waits for the init goroutine to finish before closing the store,
  then closes vectors.db (`vf.Close`) if the backend was opened.

### Unchanged

- MCP path: `setupVectorFeatures` stays synchronous and read-only there.
- Scheduler, analytics engine, sync paths.

## Failure semantics (decided)

Background init failure does NOT exit the daemon. The daemon keeps serving
archive/API traffic; vector endpoints return 503 with the failure message;
the status endpoint and logs carry the error.

## Out of scope (flagged follow-up)

The analytics cache auto-build (`openDaemonAnalyticsEngine` →
`buildCacheSubprocess`) also blocks startup, but only when explicitly
configured with `engine = "duckdb"` + `auto_build_cache = true`; default
`auto` mode falls back to the live SQL engine without building. Moving that
build off the critical path (with a live-SQL engine served meanwhile and a
late engine swap) is a follow-up.

## Testing

- `runServe`-level test: with vector enabled and a slow/blocked
  `setupVectorFeatures` (injectable seam), the API answers `/health` and
  archive endpoints before vector init completes; vector endpoints return 503
  `initializing`; after init completes they succeed.
- Failure-path test: init error leaves daemon serving, vector endpoints 503
  with error detail, status `error`.
- `api.Server` unit tests: `SetVectorFeatures` visible to concurrent handler
  reads (race detector); status transitions disabled/initializing/ready/error
  reflected in handler responses, `/health`, and stats.
- `serve status` test: prints the vector line from a live `/health` response.
- Existing `TestSetupVectorFeatures_Disabled` and port-reservation tests stay
  green.
