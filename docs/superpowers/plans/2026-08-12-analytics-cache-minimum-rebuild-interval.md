# Analytics Cache Minimum Rebuild Interval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound automatic post-sync Parquet cache rebuild frequency with a durable, opt-in minimum interval while preserving immediate explicit builds and unusable-cache recovery.

**Architecture:** Add a validated `time.Duration` analytics setting and carry committed-publication usability plus `PublishedAt` out of the existing staleness inspection. A pure post-sync decision helper applies the interval only to usable stale publications, treats future timestamps within one interval as recent, and treats larger future skew as untrusted so one rebuild repairs the marker.

**Tech Stack:** Go, BurntSushi TOML decoding, SQLite cache staleness inspection, Testify, Markdown documentation.

## Global Constraints

- The default and explicit `"0s"` preserve current rebuild-after-sync behavior.
- Only `rebuildCacheAfterScheduledSync` is throttled.
- Explicit, startup, query-triggered, mutation-triggered, and PostgreSQL paths remain unchanged.
- Missing, interrupted, incompatible, drifted, or uninspectable cache publications bypass throttling.
- Future `PublishedAt` values more than one configured interval ahead bypass throttling and self-repair through publication.
- All Go tests use Testify and all Go test commands use `-tags "fts5 sqlite_vec"`.
- Do not add the setting to the web settings API in this change.

---

### Task 1: Parse and validate the minimum interval

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.AnalyticsConfig.MinRebuildInterval time.Duration`
- Consumes: BurntSushi TOML's existing duration decoding used by `ServerConfig.DaemonIdleTimeout`

- [ ] **Step 1: Write failing configuration tests**

Add the default assertion and duration tests:

```go
assertions.Zero(cfg.Analytics.MinRebuildInterval)

func TestLoadWithAnalyticsMinRebuildInterval(t *testing.T) {
    tests := []struct {
        name  string
        value string
        want  time.Duration
    }{
        {name: "positive", value: `"6h"`, want: 6 * time.Hour},
        {name: "zero", value: `"0s"`, want: 0},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            configPath := filepath.Join(t.TempDir(), "config.toml")
            content := "[analytics]\nmin_rebuild_interval = " + tt.value + "\n"
            require.NoError(t, os.WriteFile(configPath, []byte(content), 0o600))
            cfg, err := Load(configPath, "")
            require.NoError(t, err)
            assert.Equal(t, tt.want, cfg.Analytics.MinRebuildInterval)
        })
    }
}

func TestLoadRejectsNegativeAnalyticsMinRebuildInterval(t *testing.T) {
    configPath := filepath.Join(t.TempDir(), "config.toml")
    require.NoError(t, os.WriteFile(configPath, []byte(
        "[analytics]\nmin_rebuild_interval = \"-1m\"\n",
    ), 0o600))
    _, err := Load(configPath, "")
    require.Error(t, err)
    assert.Contains(t, err.Error(), "invalid [analytics] min_rebuild_interval")
}
```

- [ ] **Step 2: Run tests and confirm the new field is missing**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/config -run 'TestAnalyticsConfigDefaults|TestLoadWithAnalyticsMinRebuildInterval|TestLoadRejectsNegativeAnalyticsMinRebuildInterval'
```

Expected: build failure because `AnalyticsConfig.MinRebuildInterval` does not exist.

- [ ] **Step 3: Add the setting and validation**

Add this field to `AnalyticsConfig`:

```go
MinRebuildInterval time.Duration `toml:"min_rebuild_interval"`
```

Add this check to `AnalyticsConfig.Validate`:

```go
if a.MinRebuildInterval < 0 {
    return fmt.Errorf("invalid [analytics] min_rebuild_interval %q: must be zero or positive", a.MinRebuildInterval)
}
```

- [ ] **Step 4: Format and verify configuration tests**

Run:

```bash
gofmt -w internal/config/config.go internal/config/config_test.go
go test -tags "fts5 sqlite_vec" ./internal/config -run 'TestAnalyticsConfigDefaults|TestLoadWithAnalyticsMinRebuildInterval|TestLoadRejectsNegativeAnalyticsMinRebuildInterval'
```

Expected: PASS.

- [ ] **Step 5: Commit the configuration contract**

Stage the two configuration files and use the repository commit workflow with subject `feat(config): set a minimum analytics rebuild interval`.

### Task 2: Throttle only usable post-sync cache rebuilds

**Files:**
- Modify: `cmd/msgvault/cmd/cache_staleness.go`
- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/build_cache_test.go`
- Modify: `cmd/msgvault/cmd/cache_refresh_test.go`

**Interfaces:**
- Consumes: `config.AnalyticsConfig.MinRebuildInterval`
- Produces: `cacheStaleness.HasUsablePublication bool`
- Produces: `cacheStaleness.PublishedAt time.Time`
- Produces: `scheduledCacheBuildDelay(cacheStaleness, time.Duration, time.Time) (time.Duration, bool)`

- [ ] **Step 1: Write failing staleness classification assertions**

Extend `TestCacheNeedsBuild` with `wantUsable bool`. Mark valid committed cache fixtures as usable and absent, corrupt, incomplete, drifted, and database-inspection failures as unusable. Assert:

```go
assert.Equal(t, tt.wantUsable, got.HasUsablePublication)
```

Also assert `TestCacheNeedsBuild_DriftedPublicationForcesFullRebuild` returns `HasUsablePublication == false`.

- [ ] **Step 2: Write failing pure decision tests**

Add a table test for `scheduledCacheBuildDelay` with a six-hour interval and fixed `now`:

```go
tests := []struct {
    name         string
    staleness    cacheStaleness
    interval     time.Duration
    wantThrottle bool
}{
    {name: "disabled interval", staleness: cacheStaleness{HasUsablePublication: true, PublishedAt: now.Add(-time.Minute)}},
    {name: "unusable publication", staleness: cacheStaleness{PublishedAt: now.Add(-time.Minute)}, interval: 6 * time.Hour},
    {name: "recent publication", staleness: cacheStaleness{HasUsablePublication: true, PublishedAt: now.Add(-time.Hour)}, interval: 6 * time.Hour, wantThrottle: true},
    {name: "elapsed interval", staleness: cacheStaleness{HasUsablePublication: true, PublishedAt: now.Add(-6 * time.Hour)}, interval: 6 * time.Hour},
    {name: "small future skew", staleness: cacheStaleness{HasUsablePublication: true, PublishedAt: now.Add(time.Hour)}, interval: 6 * time.Hour, wantThrottle: true},
    {name: "large future skew", staleness: cacheStaleness{HasUsablePublication: true, PublishedAt: now.Add(6*time.Hour + time.Nanosecond)}, interval: 6 * time.Hour},
}
```

For throttled cases assert the returned delay is positive; otherwise assert it is zero.

- [ ] **Step 3: Write failing production-path post-sync tests**

Create an isolated SQLite database with `setupTestSQLiteEmpty`, a complete fake committed publication with `writeSyncStateAt` plus `createFakeParquet`, and then insert one new exportable message. Configure the global test config with the database path, analytics data directory, and a six-hour minimum interval. Replace only `scheduledCacheBuildNow` and `runBuildCacheSubprocess` at external boundaries.

Add subtests for:

- a publication one hour old, which suppresses the runner;
- a publication older than six hours, which calls the runner once;
- a publication seven hours in the future, which calls the runner once; and
- a runner failure, which leaves the marker's `PublishedAt` unchanged.

- [ ] **Step 4: Run tests and confirm the new classification/helper are missing**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestCacheNeedsBuild|TestScheduledCacheBuildDelay|TestScheduledCacheRefreshMinimumInterval'
```

Expected: build failure because the new staleness fields and helper do not exist.

- [ ] **Step 5: Expose usable committed publication state**

Add fields to `cacheStaleness`:

```go
HasUsablePublication bool
PublishedAt           time.Time
```

After readiness, marker, schema, and database checks succeed, initialize the accumulated result as:

```go
result := cacheStaleness{
    HasUsablePublication: true,
    PublishedAt:           state.PublishedAt,
}
```

Keep every existing early recovery/error return at the zero value so unusable publications bypass throttling.

- [ ] **Step 6: Implement the bounded decision helper**

Add an injectable clock and pure helper:

```go
var scheduledCacheBuildNow = time.Now

func scheduledCacheBuildDelay(staleness cacheStaleness, interval time.Duration, now time.Time) (time.Duration, bool) {
    if interval <= 0 || !staleness.HasUsablePublication || staleness.PublishedAt.IsZero() {
        return 0, false
    }
    if staleness.PublishedAt.After(now.Add(interval)) {
        return 0, false
    }
    eligibleAt := staleness.PublishedAt.Add(interval)
    if !eligibleAt.After(now) {
        return 0, false
    }
    return eligibleAt.Sub(now), true
}
```

In `rebuildCacheAfterScheduledSync`, call it after `cacheNeedsBuild` reports staleness and before spawning the child. Log `identifier`, `min_rebuild_interval`, `published_at`, and `remaining` when skipping.

- [ ] **Step 7: Format and verify the focused cache tests**

Run:

```bash
gofmt -w cmd/msgvault/cmd/cache_staleness.go cmd/msgvault/cmd/build_cache.go cmd/msgvault/cmd/build_cache_test.go cmd/msgvault/cmd/cache_refresh_test.go
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestCacheNeedsBuild|TestScheduledCacheBuildDelay|TestScheduledCacheRefreshMinimumInterval|TestScheduledCacheRefreshSkipsWhenAutoBuildCacheDisabled'
```

Expected: PASS.

- [ ] **Step 8: Commit the post-sync throttle**

Stage the four cache files and use the repository commit workflow with subject `fix(analytics): bound post-sync cache rebuild frequency`.

### Task 3: Document operator behavior and verify the branch

**Files:**
- Modify: `docs/configuration.md`
- Modify: `docs/api-server.md`

**Interfaces:**
- Consumes: `[analytics].min_rebuild_interval` from Tasks 1-2
- Produces: operator-facing configuration examples and restart/freshness guidance

- [ ] **Step 1: Update the main configuration example and table**

Add the commented example:

```toml
# Minimum age of a usable cache before a scheduled sync may rebuild it again.
# min_rebuild_interval = "6h"
```

Change `auto_build_cache` wording so it explicitly covers startup and post-sync maintenance. Add the new table row with default `0s`, and explain that a busy archive can lag by the interval plus build time while explicit builds and unusable-cache recovery remain immediate.

- [ ] **Step 2: Update the API server configuration reference**

Mirror the corrected `auto_build_cache` wording and the new setting. State that analytics configuration changes take effect after daemon restart and that cache-builder memory and temporary disk usage scale with archive size.

- [ ] **Step 3: Run documentation and diff checks**

Run:

```bash
git diff --check
rg -n "auto_build_cache|min_rebuild_interval|restart|archive size" docs/configuration.md docs/api-server.md
```

Expected: no whitespace errors and consistent descriptions in both documents.

- [ ] **Step 4: Run required Go formatting and static checks**

Run in isolated scratch state:

```bash
go fmt ./...
go vet ./...
make lint-ci
```

Expected: all commands exit zero.

- [ ] **Step 5: Run focused and full tests**

Run in isolated scratch state:

```bash
go test -tags "fts5 sqlite_vec" ./internal/config ./cmd/msgvault/cmd -run 'Analytics|Cache|Scheduled'
make test
```

Expected: all tests pass.

- [ ] **Step 6: Review scope, scrub public content, and commit docs**

Review `git diff`, `git diff --stat`, and `git status --short`; run the repository private-data scrub across pending documentation and branch history. Stage the documentation and use the repository commit workflow with subject `docs: explain bounded automatic cache maintenance`.

### Task 4: Review, repair findings, and publish

**Files:**
- Modify: only files required by review findings

**Interfaces:**
- Consumes: the complete implementation branch
- Produces: closed original Roborev findings and a GitHub pull request targeting the repository default branch

- [ ] **Step 1: Review the completed diff against the design**

Compare the branch against `origin/main`, verify every design requirement has a corresponding test or documentation statement, and inspect the public diff for unrelated scope.

- [ ] **Step 2: Run the explicitly requested `$roborev-fix` discovery cycle**

Run `roborev fix --list`. Record the job IDs it reports, fetch each original
actionable failing job with `roborev show --job "$job_id" --json`, address all
findings in severity order, run the relevant focused and full checks, comment
with a heredoc, close each original job, commit any fixes, and audit every
recorded ID with the same `roborev show` command until `closed=true`.

- [ ] **Step 3: Run final verification after all review fixes**

Run fresh in isolated scratch state:

```bash
go fmt ./...
go vet ./...
make lint-ci
make test
git diff --check origin/main...HEAD
git status --short
```

Expected: every command exits zero and the working tree is clean.

- [ ] **Step 4: Scrub the complete public branch**

Inspect all commits, patches, introduced objects, and the proposed PR text against the private-terms denylist plus structural secret/private-data heuristics. Stop if any unresolved hit remains.

- [ ] **Step 5: Push and open the PR**

Push the issue branch to `origin` and create a rationale-first pull request against the resolved default branch. Explain the continuous-rebuild problem, the opt-in durable throttle, the immediate recovery exceptions, and bounded future-clock repair. Do not add a validation/test-plan section because repository guidance reserves routine evidence for CI.
