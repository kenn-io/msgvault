# Analytics Cache Publication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make analytics-cache maintenance a mandatory, recoverable commit protocol: exports stay outside live reader globs until verified, readers only accept committed cache state, account removal holds the writer lock through rebuilding, and refresh failures reach the originating operation.

**Architecture:** Add one shared cache-readiness inspector in `internal/query` and one staged-publication helper in `cmd/msgvault/cmd`. Builders export to a same-filesystem sibling directory, verify it, invalidate live state at the publication boundary, rename complete files into place, and write state last. SQLite remains authoritative; invalid or interrupted cache state always causes a stateless full recovery.

**Tech Stack:** Go, SQLite (`go-sqlite3`), DuckDB/Parquet (`duckdb-go/v2`), `gofrs/flock`, Cobra, and Testify.

## Global Constraints

- Follow `CLAUDE.md` and `AGENTS.md`. Every changed Go test uses Testify; never add `t.Error*` or `t.Fatal*` calls.
- Run every Go test command with `-tags "fts5 sqlite_vec"`; `make test` supplies the tags for the full suite.
- Exercise the real SQLite-to-DuckDB export and query paths for cache behavior. Filesystem-only tests are appropriate only for the publication move primitive itself.
- Preserve PostgreSQL behavior: Parquet cache maintenance remains SQLite-only.
- Do not mark a successfully archived `sync_runs` row failed because cache refresh failed.
- Keep the implementation to the two shared abstractions named above. Do not add generation directories, symlinks, a second invalidation marker, or reader-side Parquet repair.
- This work addresses roborev findings #1, #2, and #3 as one consistency change. Keep the implementation uncommitted through the red/green checkpoints and make one final fix commit whose body contains `VALID (fixed): #1, #2, #3`.

---

## Task 1: Centralize cache state and readiness

**Files:**

- Create: `internal/query/cache_state.go`
- Create: `internal/query/cache_state_test.go`
- Modify: `internal/query/cache_lock.go`
- Modify: `internal/query/duckdb.go`
- Modify: `internal/query/duckdb_cache_drift_test.go`
- Modify: `internal/query/testfixtures_test.go`
- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/cache_staleness.go`

- [ ] **Step 1: Write the readiness truth-table tests.**

Add table-driven Testify tests for these exact cases:

| Files | `_last_sync.json` | Want |
|---|---|---|
| none | absent | `CacheAbsent` |
| complete required datasets | valid completed state | `CacheReady` |
| any Parquet file | absent | `CacheInterrupted` |
| none | valid state | `CacheInterrupted` |
| complete datasets | malformed state | `CacheInterrupted` |
| incomplete datasets | valid state | `CacheInterrupted` |
| complete datasets | JSON with zero `last_sync_at` | `CacheInterrupted` |

Use small real Parquet fixtures for the complete-cache cases, reusing the query package's existing fixture helpers where possible. Assert unexpected filesystem errors separately rather than classifying them as Interrupted.

- [ ] **Step 2: Run the focused tests and confirm the red state.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query -run 'TestInspectCacheReadiness'
```

Expected: compile failure because `InspectCacheReadiness` and the readiness constants do not exist.

- [ ] **Step 3: Implement the single shared state model.**

Move the full JSON shape now named `syncState` into `internal/query/cache_state.go` and alias it from the command package so the builder, staleness checker, stats collector, and reader parse exactly one type:

```go
type CacheSyncState struct {
	LastMessageID          int64     `json:"last_message_id"`
	LastSyncAt             time.Time `json:"last_sync_at"`
	SchemaVersion          int       `json:"schema_version,omitempty"`
	LastCompletedSyncRunID int64     `json:"last_completed_sync_run_id,omitempty"`
	LastCacheAdditionCount int64     `json:"last_cache_addition_count,omitempty"`
	LastCacheUpdateCount   int64     `json:"last_cache_update_count,omitempty"`
	LastFailedSyncRunCount int64     `json:"last_failed_sync_run_count,omitempty"`
	LastFailedSyncRunIDSum int64     `json:"last_failed_sync_run_id_sum,omitempty"`
}

type CacheReadiness string

const (
	CacheAbsent      CacheReadiness = "absent"
	CacheReady       CacheReadiness = "ready"
	CacheInterrupted CacheReadiness = "interrupted"
)

var ErrCacheUnavailable = errors.New("analytics cache unavailable")
```

Expose these functions:

```go
func CacheStatePath(analyticsDir string) string
func ReadCacheSyncState(analyticsDir string) (CacheSyncState, error)
func InspectCacheReadiness(analyticsDir string) (CacheReadiness, error)
func AcquireReadyCacheReadLock(ctx context.Context, analyticsDir string) (func(), error)
```

`InspectCacheReadiness` must inspect only the required live dataset directories, never sibling staging directories. It must distinguish absence from filesystem failure, require at least one Parquet file in every `RequiredParquetDirs` entry, parse `CacheSyncState`, and require nonzero `LastSyncAt`. `AcquireReadyCacheReadLock` must acquire the existing shared flock first, inspect under that lock, release on any non-Ready result, and wrap `ErrCacheUnavailable` with the readiness value.

`ReadCacheSyncState` owns JSON loading and parsing for all packages. Callers may apply their own schema-version checks after structural parsing; do not duplicate `os.ReadFile`/`json.Unmarshal` state logic in the builder, staleness checker, or stats collector.

In `build_cache.go`, use:

```go
type syncState = query.CacheSyncState
```

Remove the narrower `ParquetSyncState` and the duplicate stats state type once all consumers use `CacheSyncState`.

- [ ] **Step 4: Gate every DuckDB Parquet touch under the ready read lock.**

Change `DuckDBEngine.acquireCacheRead` to call `AcquireReadyCacheReadLock`. In `NewDuckDBEngine`, acquire that same ready lock around the initial optional-column probe and view registration when `analyticsDir != ""`; close the DuckDB connection and return the cache-unavailable error if readiness is not Ready. The constructor currently touches Parquet before the first query, so changing only `acquireCacheRead` is insufficient.

Add or update a real-engine test that creates a Ready cache, opens an engine, removes state, and asserts:

```go
_, err = engine.Aggregate(context.Background(), ViewSenders, DefaultAggregateOptions())
require.ErrorIs(t, err, ErrCacheUnavailable)
```

Also assert that a new engine cannot be opened on an Interrupted cache.
Update the shared `parquetBuilder.build` test fixture to write a valid completed state by default so existing engine tests continue to model a committed cache; readiness tests can remove or corrupt that file for their negative cases.

- [ ] **Step 5: Make staleness use the same readiness classification.**

At the beginning of `cacheNeedsBuild`, inspect readiness. Return `NeedsBuild: true, FullRebuild: true` for Absent, Interrupted, or inspection errors, with distinct human-readable reasons. Only parse and compare watermarks after readiness is Ready. Preserve schema-version comparison as a staleness concern; structural readiness does not make an old schema current.

- [ ] **Step 6: Run the query and staleness tests.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query ./cmd/msgvault/cmd -run 'Test(InspectCacheReadiness|DuckDB.*Interrupted|CacheNeedsBuild)'
```

Expected: pass.

---

## Task 2: Define and test staged publication

**Files:**

- Create: `cmd/msgvault/cmd/cache_publication.go`
- Create: `cmd/msgvault/cmd/cache_publication_test.go`
- Modify: `cmd/msgvault/cmd/build_cache.go`

- [ ] **Step 1: Write filesystem publication tests first.**

Cover the production helper directly with temporary sibling directories:

1. A full publication replaces every live dataset and removes old-only files.
2. An incremental publication replaces `participants`, `labels`, `sources`, and `conversations` as whole directories.
3. Incremental messages, recipients, message-labels, and attachments are appended with the staging build-ID prefix; two publications with the same DuckDB-produced basename do not collide.
4. Message files retain their `year=*` parent directory.
5. Destination collision detection fails before state invalidation.
6. A hook failure immediately after invalidation leaves no state and never writes replacement state.
7. Replacement does not rely on rename-over-existing behavior: an old-only target file disappears before the staged directory becomes live.

- [ ] **Step 2: Run the focused test and confirm it fails to compile.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'Test(CachePublication|IncrementalPublication)'
```

Expected: compile failure because the staging and publication types do not exist.

- [ ] **Step 3: Implement sibling staging lifecycle.**

Use a same-parent private prefix so rename never crosses filesystems:

```go
type cacheStaging struct {
	root    string
	buildID string
}

func newCacheStaging(analyticsDir string) (*cacheStaging, error)
func cleanupStaleCacheStaging(analyticsDir string) error
func (s *cacheStaging) cleanup() error
```

`cleanupStaleCacheStaging` runs only while the exclusive writer lock is held and removes only directories whose basename begins with `.` + the analytics basename + `.build-`. It must not match the `.build.lock` file. `newCacheStaging` uses `os.MkdirTemp(filepath.Dir(analyticsDir), prefix)` and derives a filename-safe build ID from the generated basename.

- [ ] **Step 4: Construct the complete plan before invalidation.**

Represent publication as explicit moves:

```go
type cachePublishMove struct {
	source      string
	destination string
	replace     bool
}

type cachePublicationPlan struct {
	analyticsDir string
	stateData    []byte
	moves        []cachePublishMove
}

func planCachePublication(staging *cacheStaging, analyticsDir string, replaceAll bool, stateData []byte) (*cachePublicationPlan, error)
func publishCache(plan *cachePublicationPlan) error
```

For a full/stateless build, add one replacement move per required dataset directory. For an incremental build, add replacement moves for the four dimension directories and file moves for every staged append shard. Name append destinations `buildID + "-" + filepath.Base(source)`. Walk messages one partition level deep and retain the `year=*` directory.

During planning, verify every source exists, every destination is contained by the intended live dataset directory, and no append destination exists. Do not create live destination parents during planning; parent creation belongs inside the post-invalidation publication window. No live state or dataset may be changed here.

- [ ] **Step 5: Implement the short publication window.**

`publishCache` performs exactly this order:

```go
if err := invalidateSyncStateFile(query.CacheStatePath(plan.analyticsDir)); err != nil {
	return err
}
if buildCacheAfterStateInvalidationHook != nil {
	if err := buildCacheAfterStateInvalidationHook(); err != nil {
		return err
	}
}
for _, move := range plan.moves {
	if move.replace {
		if err := os.RemoveAll(move.destination); err != nil {
			return fmt.Errorf("remove live cache dataset %s: %w", move.destination, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(move.destination), 0o755); err != nil {
		return fmt.Errorf("create cache publication directory: %w", err)
	}
	if err := os.Rename(move.source, move.destination); err != nil {
		return fmt.Errorf("publish cache file %s: %w", move.destination, err)
	}
}
if err := buildCacheWriteStateFile(query.CacheStatePath(plan.analyticsDir), plan.stateData, 0o600); err != nil {
	return fmt.Errorf("save cache sync state: %w", err)
}
return nil
```

This deliberate delete-then-rename behavior is required on Windows. Do not try to restore old files after invalidation; any error in this window intentionally leaves Interrupted state for stateless recovery.

- [ ] **Step 6: Run publication tests.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'Test(CachePublication|IncrementalPublication)'
```

Expected: pass on the current platform without POSIX overwrite assumptions.

---

## Task 3: Export, verify, then publish

**Files:**

- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/build_cache_test.go`
- Modify: `cmd/msgvault/cmd/build_cache_calendar_test.go`

- [ ] **Step 1: Replace ordering tests with external-invariant tests.**

Update `TestBuildCacheFailedIncrementalStaysServableAndRebuildsCleanly` so a failure before message export now asserts:

- the old `_last_sync.json` bytes are unchanged;
- all old live Parquet file names and hashes are unchanged;
- the old five-message cache remains Ready and queryable;
- the new message is absent;
- retry publishes six messages exactly once.

Add tests for:

- a publish hook failure after invalidation causes `CacheInterrupted` and an already-open DuckDB engine returns `ErrCacheUnavailable`;
- a sync-counter mismatch discards staging while preserving old state and files;
- an exact staged-row mismatch discards staging while preserving old state and files;
- incremental retries use build-ID-prefixed filenames and never duplicate rows;
- a plain non-auto build with missing state, stale shards, and zero exportable SQLite messages does not skip, replaces stale files, writes the empty messages shard, and finishes Ready;
- a failed export leaves no live-tree mutation and normal error cleanup removes its staging directory;
- a successful later build removes an abandoned matching staging directory.

- [ ] **Step 2: Run the new regression set and confirm the current implementation fails.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestBuildCache(FailedIncremental|PublishInterruption|CounterMismatch|RowCountMismatch|EmptyStateless|StagingCleanup|IncrementalPublication)'
```

Expected: failures showing early state invalidation, live Parquet mutation, whole-analytics deletion, or the incorrect no-op path.

- [ ] **Step 3: Split lock acquisition from the lock-held build body.**

Keep public behavior in the wrapper and make the internal contract explicit:

```go
func buildCacheImpl(dbPath, analyticsDir string, fullRebuild, autoDecided bool) (*buildResult, error) {
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()

	buildLock, err := acquireCacheBuildLock(analyticsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = buildLock.Unlock() }()
	return buildCacheLocked(dbPath, analyticsDir, fullRebuild, autoDecided)
}

func buildCacheLocked(dbPath, analyticsDir string, fullRebuild, autoDecided bool) (*buildResult, error)
```

Factor the existing TryLock/wait message into `acquireCacheBuildLock`. The lock-held body never acquires `buildCacheMu` or a flock. This entry point is later used by account removal.

- [ ] **Step 4: Make missing or invalid prior state stateless.**

Under the writer lock, inspect readiness and load the previous state only when it is Ready and its schema version matches. Set `replaceAll := fullRebuild || !hasPreviousState`; reset `lastMessageID` to zero whenever `replaceAll` is true. The no-op condition must be:

```go
if hasPreviousState && maxID <= lastMessageID && !fullRebuild {
	return &buildResult{Skipped: true, OutputDir: analyticsDir}, nil
}
```

This is the direct fix for the plain-CLI empty stateless case. Do not rely on the daemon's `--auto` staleness gate.

- [ ] **Step 5: Export every dataset into staging.**

After acquiring the writer lock:

1. Remove abandoned private staging directories.
2. Create one `cacheStaging` and defer its cleanup.
3. Point every `COPY` destination at `staging.root`.
4. Always use `OVERWRITE_OR_IGNORE`; staging starts empty, so live-file collision behavior is irrelevant.
5. Use `data.parquet` for staged junction outputs. Publication assigns unique live names.
6. Write the schema-compatible empty message shard into staging for full/stateless zero-message builds.

Delete the live-directory clearing loop, early `invalidateSyncStateFile`, APPEND mode, `incr_<id>.parquet` naming, messages-last safety commentary, and the `discardAttempt` closure. Export order is no longer correctness-sensitive.

- [ ] **Step 6: Capture exact expected counts from the SQLite snapshot.**

Alongside the existing max-ID and sync-counter metadata queries, capture:

```sql
SELECT COUNT(*) FROM messages
WHERE sent_at IS NOT NULL
  AND COALESCE(message_type, '') <> 'calendar_event'
  AND deleted_at IS NULL
  AND id <= ?
  AND id > ?
```

Use lower bound zero for full/stateless builds and `lastMessageID` for incremental builds. Also capture the total exportable count through `maxID` for `buildResult.ExportedCount`, preserving the current public meaning that an incremental result reports the total cache population.

- [ ] **Step 7: Move every fallible check before publication.**

Before calling `planCachePublication`:

1. Build the escaped path with `filepath.Join(staging.root, "messages", "**", "*.parquet")`, query it with `read_parquet(path, hive_partitioning=true)`, and require exact equality with the expected batch count. For a zero-row full/stateless build, require the empty shard and a zero count. For a legitimate zero-row incremental batch, accept that no staged message file exists and treat its staged count as zero.
2. Open every staged full-replacement dataset with DuckDB so existence alone cannot bless a truncated file.
3. Run `buildCacheBeforeStateWriteHook`, re-open SQLite, and require the current sync counters to equal the captured counters.
4. Marshal `query.CacheSyncState`.
5. Build the complete collision-checked publication plan.

All failures return through the deferred staging cleanup. The old cache and state remain untouched.

- [ ] **Step 8: Publish and preserve result semantics.**

Call `publishCache(plan)` only after every check passes. On success return the captured total exportable count, `maxID`, and the live analytics directory. On publication failure, return the error without trying to clean or validate the live tree; missing state is the recovery signal.

- [ ] **Step 9: Run the full build-cache test package.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestBuildCache|TestCacheBuild|TestCacheNeedsBuild'
```

Expected: pass, including existing snapshot, calendar-only, mutation, and incremental-count tests.

---

## Task 4: Make cache stats honor readiness

**Files:**

- Modify: `internal/cacheops/stats.go`
- Modify: `internal/cacheops/stats_test.go`
- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/cache_stats_test.go`
- Modify: `internal/api/handlers_test.go`

- [ ] **Step 1: Add stats classification tests.**

Using real cache files, assert:

- Absent returns `StatusNoCacheFiles` without opening DuckDB.
- Ready returns statistics.
- Files with missing/malformed state return `StatusInterrupted` without a DuckDB parse error.
- State with incomplete files returns `StatusInterrupted`.
- Stats still waits for the shared cache lock before classifying.

Update the existing empty-directory assertion from `StatusNoCacheData` to `StatusNoCacheFiles`, matching the approved Absent definition (no state and no Parquet files).

- [ ] **Step 2: Confirm the Interrupted test fails.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/cacheops ./cmd/msgvault/cmd -run 'Test(CollectStats|PrintCacheStats).*Interrupted'
```

Expected: compile failure because `StatusInterrupted` does not exist.

- [ ] **Step 3: Inspect readiness under the stats read lock.**

Add:

```go
const StatusInterrupted = "interrupted"
```

`CollectStats` must acquire `query.AcquireCacheReadLock`, call `query.InspectCacheReadiness` while still holding it, and switch before opening DuckDB:

```go
switch readiness {
case query.CacheAbsent:
	return &CacheStats{Status: StatusNoCacheFiles}, nil
case query.CacheInterrupted:
	return &CacheStats{Status: StatusInterrupted}, nil
case query.CacheReady:
	// Continue to the existing real DuckDB stats queries.
}
```

Remove duplicate JSON parsing and populate `LastSyncAt`/`LastMessageID` from `query.CacheSyncState`. Keep `StatusNoCacheData` only if needed for wire compatibility; no readiness path should produce it.

- [ ] **Step 4: Surface a concise repair message through CLI/API.**

Add the CLI branch:

```go
case cacheops.StatusInterrupted:
	if err := writeCacheStatsLine(out, "Analytics cache publication was interrupted.\n"); err != nil {
		return err
	}
	if err := writeCacheStatsLine(out, "Run 'msgvault build-cache' to repair it.\n"); err != nil {
		return err
	}
```

The status field is already a string through the API and daemon client, so do not add a second API model. Extend handler coverage to assert the new string is preserved.

- [ ] **Step 5: Run stats and API tests.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/cacheops ./internal/api ./internal/daemonclient ./cmd/msgvault/cmd -run 'Test(CollectStats|CLICacheStats|CacheStats)'
```

Expected: pass.

---

## Task 5: Hold the cache lock through account removal and recovery

**Files:**

- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/remove_account.go`
- Modify: `cmd/msgvault/cmd/remove_account_test.go`

- [ ] **Step 1: Write removal protocol tests first.**

Add real-store tests for these outcomes:

1. Pause the lock-held full rebuild after the cascade, start a shared reader, and prove it cannot acquire until the rebuild completes. The first aggregate after release contains no rows from the removed source.
2. Force the post-delete state write to fail. The command returns an error containing both `account was removed` and `analytics cache refresh failed`; the source is absent, cleanup ran, state is missing, readiness is Interrupted, and readers get `ErrCacheUnavailable`.
3. Install a SQLite `BEFORE DELETE ON sources` trigger that aborts the cascade. Removal rebuilds the unchanged cache before returning the cascade error; the source remains, state is Ready, and credentials/attachments remain.
4. Combine the aborting trigger with a forced recovery-build failure. The returned error contains both causes and readiness is Interrupted.
5. Make lock or invalidation fail and assert the source remains untouched.

- [ ] **Step 2: Run the removal tests and confirm the lock-gap failure.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestRemoveAccountCmd_(HoldsCacheLock|FailedCacheRebuild|CascadeFailure|LockFailure)'
```

Expected: failures because removal releases before rebuilding, swallows cache failure, and does not repair after cascade failure.

- [ ] **Step 3: Make lock/invalidation acquisition fail closed.**

Change the helper contract to return the held flock and an error:

```go
func lockCacheAndInvalidateSyncState(analyticsDir string) (*flock.Flock, error)
```

Acquire the exclusive lock, invalidate `query.CacheStatePath(analyticsDir)`, and return the still-held lock. If either operation fails, unlock and return an annotated error. Remove the warning-only behavior; destructive database mutation is not allowed without this protection.

- [ ] **Step 4: Keep one lock across cascade and `buildCacheLocked`.**

For SQLite removal:

1. Acquire/invalidate before calling `RemoveSourceSerialized`.
2. If the cascade succeeds, invoke `buildCacheLocked(dbPath, analyticsDir, true, false)` immediately, then unlock.
3. If the cascade fails, invoke the same full lock-held builder against the unchanged database, then unlock and return the cascade error.
4. If recovery also fails, return `errors.Join(cascadeErr, recoveryErr, unlockErr)`.
5. Do not clean credentials or attachments after cascade failure because the source still exists.

On successful deletion, retain any rebuild error, perform the existing best-effort credential and attachment cleanup, then return an explicit partial-success error. Do not print the unconditional success footer before returning that error.

- [ ] **Step 5: Run all account-removal tests.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestRemoveAccount'
```

Expected: pass, including last-account empty-cache and attachment/credential cleanup coverage.

---

## Task 6: Propagate mandatory refresh failures without falsifying sync history

**Files:**

- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/add_synctech_sms_drive.go`
- Modify: `cmd/msgvault/cmd/backfill_beeper_media.go`
- Modify: `cmd/msgvault/cmd/backfill_teams_media.go`
- Modify: `cmd/msgvault/cmd/calendar.go`
- Modify: `cmd/msgvault/cmd/circleback.go`
- Modify: `cmd/msgvault/cmd/deletions.go`
- Modify: `cmd/msgvault/cmd/import.go`
- Modify: `cmd/msgvault/cmd/import_emlx.go`
- Modify: `cmd/msgvault/cmd/import_gvoice.go`
- Modify: `cmd/msgvault/cmd/import_imessage.go`
- Modify: `cmd/msgvault/cmd/import_mbox.go`
- Modify: `cmd/msgvault/cmd/import_messenger.go`
- Modify: `cmd/msgvault/cmd/serve.go`
- Modify: `cmd/msgvault/cmd/sync.go`
- Modify: `cmd/msgvault/cmd/sync_beeper.go`
- Modify: `cmd/msgvault/cmd/sync_teams.go`
- Modify: `cmd/msgvault/cmd/syncfull.go`
- Modify: `cmd/msgvault/cmd/circleback_test.go`
- Modify: `cmd/msgvault/cmd/sync_beeper_routing_test.go`
- Create: `cmd/msgvault/cmd/cache_refresh_test.go`
- Modify: `cmd/msgvault/cmd/serve_test.go`

- [ ] **Step 1: Add error-propagation tests.**

Cover three levels:

1. `rebuildCacheAfterWrite` returns an annotated sentinel build error from a real stale-cache build.
2. `runScheduledBeeperAttempts` and `finishScheduledCirclebackImport` join refresh failure with import/cancellation failure and also return refresh failure after an otherwise successful import.
3. A scheduled refresh failure is returned to `runScheduledSync`/job status while a real completed `sync_runs` row remains `completed` with its original counters.

Use a narrow subprocess function seam for the scheduled helper:

```go
var runBuildCacheSubprocess = buildCacheSubprocess
```

Do not stub SQLite writes, the sync-run record, or cache staleness detection.

- [ ] **Step 2: Confirm current warning-only behavior fails the tests.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'Test(RebuildCacheAfterWriteReturnsError|Scheduled.*CacheRefresh|FinishScheduledCircleback.*RefreshError|ScheduledBeeper.*RefreshError)'
```

Expected: failures because both refresh helpers currently return no error.

- [ ] **Step 3: Return errors from both refresh helpers.**

Use these signatures:

```go
func rebuildCacheAfterWrite(dbPath string) error
func rebuildCacheAfterScheduledSync(ctx context.Context, identifier string) error
```

Return nil for PostgreSQL, no-staleness, skipped builds, and successful builds. Return `fmt.Errorf("refresh analytics cache: %w", err)` on failure. The scheduled helper may log the failure, but logging is not a substitute for returning it.

- [ ] **Step 4: Update manual write commands mechanically and preserve primary errors.**

At a clean-success return, use:

```go
return rebuildCacheAfterWrite(dbPath)
```

Where an import or multi-account operation already has an error, use:

```go
cacheErr := rebuildCacheAfterWrite(dbPath)
return errors.Join(operationErr, cacheErr)
```

Apply this to every current `rebuildCacheAfterWrite` call site listed in the Files section. Change `finishImessageImport` and Circleback refresh callbacks to return `error`. Preserve the commands' existing cancellation semantics when refresh succeeds, but never turn a refresh failure into nil. Forced full rebuilds for iMessage participant/conversation mutations must also return their failure instead of printing a warning.

- [ ] **Step 5: Join scheduled refresh errors into job results.**

- `runScheduledSync`: append the refresh error after all source attempts and return it through `errors.Join`.
- configured Synctech SMS and Calendar: return the refresh helper directly after successful import.
- `runScheduledBeeperAttempts`: change `rebuild func()` to `rebuild func() error` and append its error.
- `finishScheduledCirclebackImport`: change `refreshCache` to return error and join it with the import/cancel error.

Do not call `FailSync`, update `sync_runs.status`, or change message counters in any of these paths. The scheduler records the returned job failure separately.

- [ ] **Step 6: Prove no fire-and-forget call remains.**

Run:

```bash
rg -n 'rebuildCacheAfter(Write|ScheduledSync)\(' cmd/msgvault/cmd --glob '*.go'
```

Inspect every result. Each call must be returned, assigned to an error that is later returned/joined, or used as an error-returning callback. There must be no bare invocation statement.

- [ ] **Step 7: Run command and scheduler tests.**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd
```

Expected: pass.

---

## Task 7: Simplify obsolete recovery logic and verify end to end

**Files:**

- Modify: comments and tests in files changed above
- Verify: `docs/superpowers/specs/2026-07-15-analytics-cache-publication-design.md`

- [ ] **Step 1: Remove obsolete concepts and search for residue.**

Run:

```bash
rg -n 'messages.*LAST|discardAttempt|incr_|APPEND|partial.*servable|Warning: cache rebuild failed|must proceed' cmd/msgvault/cmd internal/query internal/cacheops
```

Remove only cache-publication workarounds superseded by staging. Keep unrelated uses of DuckDB APPEND or warning text. Ensure comments describe the commit marker, not export ordering.

- [ ] **Step 2: Run formatting and static checks.**

Run:

```bash
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
make lint-ci
```

Expected: all pass with no new warnings.

- [ ] **Step 3: Run the full build and test gate.**

Run:

```bash
make build
make test
```

Expected: both pass. Record the command outputs for handoff evidence.

- [ ] **Step 4: Exercise the failure/recovery path as one integration scenario.**

Run the focused real-path tests together so lock, publication, reader rejection, empty recovery, removal, stats, and job error behavior coexist in one process:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query ./internal/cacheops ./cmd/msgvault/cmd -run 'Test(InspectCacheReadiness|BuildCacheFailedIncremental|BuildCachePublishInterruption|BuildCacheEmptyStateless|RemoveAccountCmd_HoldsCacheLock|RemoveAccountCmd_CascadeFailure|CollectStats.*Interrupted|Scheduled.*CacheRefresh)'
```

Expected: pass.

- [ ] **Step 5: Review the final diff against every approved invariant.**

Run:

```bash
git diff --check
git diff --stat
git status --short
```

Confirm explicitly:

- no `COPY` destination is under the live analytics directory;
- old state survives every pre-publication failure;
- invalidation is the first live mutation;
- state write is the last publication step;
- Absent falls back and Interrupted rejects;
- plain empty stateless build cannot skip;
- account removal never releases between cascade and rebuild;
- cascade failure attempts recovery before return;
- cache refresh errors reach command/job results;
- no cache error rewrites a completed sync run.

- [ ] **Step 6: Scrub public content and make the single implementation commit.**

Run the repository's private-data scrub over the branch diff, then follow the mandatory commit skill. Commit all implementation and test changes together with a message such as:

```text
Make analytics cache publication transactional

VALID (fixed): #1, #2, #3
```

- [ ] **Step 7: Verify the committed state.**

Run:

```bash
git status --short
git log -1 --oneline
```

Expected: clean worktree and the analytics-cache implementation commit at `HEAD`.
