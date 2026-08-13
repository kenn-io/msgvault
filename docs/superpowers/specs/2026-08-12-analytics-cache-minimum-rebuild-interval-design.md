# Analytics Cache Minimum Rebuild Interval Design

## Context

Each successful scheduled source sync currently checks the Parquet analytics
cache and immediately rebuilds it when SQLite has changed. Cache builds are
serialized by a file lock, but they are not throttled or coalesced. On an
archive with staggered, frequent source schedules, each sync can therefore
start another archive-scale rebuild shortly after the previous build publishes.

`[analytics].auto_build_cache` is the only post-sync control today. Disabling it
stops all automatic maintenance, while enabling it provides no bound on rebuild
frequency. Issue #600 requests a middle ground that keeps automatic refreshes
but permits an intentionally stale cache between them.

## Goals

- Let operators bound the frequency of automatic post-sync cache rebuilds.
- Preserve the current behavior unless the operator opts into throttling.
- Persist the throttle across daemon restarts without adding new state.
- Keep explicit builds and recovery of an unusable cache immediate.
- Avoid changing sync frequency, provider scheduling, or query behavior.

## Non-goals

- Add an analytics cache cron schedule.
- Make the cache builder proportional to the number of new messages.
- Coalesce arbitrary build requests in memory.
- Hot-reload analytics configuration or add this setting to the settings API.
- Change the cache schema or publication format.

## Configuration

Add a duration to `AnalyticsConfig`:

```toml
[analytics]
auto_build_cache = true
min_rebuild_interval = "6h"
```

The Go field is `MinRebuildInterval time.Duration` with TOML key
`min_rebuild_interval`.

- The default and explicit `"0s"` disable throttling and preserve the current
  rebuild-after-sync behavior.
- Positive durations set the minimum age of the current successful publication
  before a scheduled sync may trigger another cache build.
- Negative durations are invalid configuration and prevent startup with an
  error that names `[analytics].min_rebuild_interval`.
- Like other daemon configuration, a changed value takes effect after the
  daemon restarts.

`auto_build_cache = false` remains the stronger control: automatic post-sync
builds do not run regardless of the interval.

## Rebuild Decision

The minimum interval applies only in `rebuildCacheAfterScheduledSync`, after
the existing backend and `auto_build_cache` checks.

The helper first performs the existing cache staleness inspection. If no build
is needed, it returns as it does today. If the cache is stale but remains a
valid committed publication, it compares the marker's `PublishedAt` with the
current time:

```text
elapsed = now - PublishedAt

if min_rebuild_interval > 0 and elapsed < min_rebuild_interval:
    skip this post-sync rebuild
else:
    run the existing automatic build subprocess
```

The comparison uses the durable `PublishedAt` value rather than process memory
or file modification time. It therefore survives daemon restarts, advances
only after a successful marker-last publication, and cannot be extended by a
failed build.

The age check uses an injectable clock in the small decision helper so tests do
not sleep or mutate filesystem timestamps. Future timestamps use bounded
conservatism: a publication up to one configured interval ahead is treated as
recent, but a timestamp farther ahead is untrusted and bypasses the throttle.
The resulting rebuild restamps `PublishedAt` with the current clock, so a large
clock correction repairs itself in one pass instead of suppressing automatic
maintenance for an unbounded period.

### Throttle eligibility

A valid committed cache may be stale because messages were added, updated,
deleted, hidden, or repaired, or because identity and conversation data
changed. These are data-freshness conditions. They remain eligible for the
minimum interval even when the next build must be a full rebuild. During the
interval, readers continue to use the last valid publication.

An unusable or untrusted cache bypasses the interval and is repaired
immediately. This includes:

- no committed cache;
- interrupted publication;
- incompatible cache schema;
- dataset fingerprint drift;
- unreadable or invalid cache state; and
- failure to inspect the committed cache safely.

This distinction prevents a minimum freshness preference from delaying cache
recovery. The staleness result will expose whether the current publication is
usable, rather than requiring callers to infer safety from human-readable
reason strings.

## Unaffected Build Paths

The interval does not apply to:

- `msgvault build-cache`, including `--full-rebuild`;
- explicit cache builds through the HTTP API;
- daemon startup cache recovery;
- a query path that requires a fresh cache;
- derived-data refreshes directly required by a mutation; or
- PostgreSQL, which does not use the Parquet cache.

Only automatic post-sync refresh calls are throttled. This keeps the setting a
resource-control policy rather than a universal freshness restriction.

## Observability

When an automatic post-sync rebuild is skipped, log one structured information
event with:

- the scheduled source identifier;
- the configured minimum interval;
- the committed publication timestamp; and
- the remaining duration before another post-sync build is eligible.

Existing build-start and build-completion events remain unchanged. No log is
needed when `auto_build_cache` is disabled or the cache is already current.

## Documentation

Update the analytics configuration example and reference table in
`docs/configuration.md`. Explain that:

- the setting limits automatic post-sync rebuild frequency;
- zero preserves rebuild-after-sync behavior;
- the cache may lag SQLite by approximately the configured interval, plus build
  time, on a continuously changing archive;
- explicit builds and unusable-cache recovery are not delayed;
- cache build resources scale with archive size; and
- analytics configuration changes require a daemon restart.

Update the duplicated analytics configuration reference in
`docs/api-server.md` so its defaults and behavior remain consistent.

## Testing

Use focused Go tests with Testify assertions.

Configuration tests cover:

- an omitted interval defaults to zero;
- a positive TOML duration decodes correctly;
- explicit zero is accepted; and
- a negative duration is rejected.

Post-sync refresh tests cover:

- zero interval preserves an immediate stale-cache build;
- a recent valid publication suppresses the build;
- an interval that has elapsed permits the build;
- a failed build does not create or advance throttle state;
- a future publication timestamp within one interval is suppressed;
- a future publication timestamp beyond one interval bypasses throttling and
  repairs the marker;
- every unusable-cache readiness condition bypasses throttling; and
- `auto_build_cache = false` remains a no-op.

Tests exercise the production staleness and marker-reading path with isolated
temporary SQLite and analytics directories. They replace only the subprocess
runner and clock at the external boundaries so no DuckDB build or wall-clock
sleep is required for decision tests.

## Compatibility and Risk

The zero default makes the change backward-compatible. No cache migration is
required because `PublishedAt` already exists in the committed cache marker.
Older or incomplete markers that lack a usable publication timestamp are
classified as unusable and rebuilt immediately.

The main behavioral risk is an operator choosing an interval longer than their
acceptable analytics lag. Documentation must make that tradeoff explicit.
Archive ingestion, full-text search, and direct SQLite-backed reads remain
current; only Parquet-backed analytical views can lag.
