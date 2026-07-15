# Analytics Cache Publication Design

**Status:** Approved

## Context

The analytics cache is a disposable Parquet projection of the SQLite archive,
but maintaining it is part of every successful write workflow. A cache build
must never expose files that do not describe one completed export, and a
destructive operation such as account removal must not leave removed data
queryable.

The current builder mutates the live cache table by table. It invalidates
`_last_sync.json` before exporting and relies on the messages table being
exported last to keep earlier partial changes unreachable. Three gaps remain:

1. Account removal releases the exclusive cache lock after the database
   deletion and before rebuilding, allowing readers to serve pre-removal
   Parquet data.
2. A plain `msgvault build-cache` invocation can take the no-op path for an
   empty database with missing state, leaving stale shards in place. The
   daemon's automatic path already avoids this by rechecking staleness under
   the lock.
3. DuckDB writes message partitions directly into the live glob, so a killed
   `COPY` can leave a truncated shard that readers encounter after the OS
   releases the writer's lock.

These are manifestations of one missing abstraction: the cache does not have
an explicit commit boundary shared by builders and readers.

## Goals

- Give the cache one simple committed/uncommitted protocol.
- Preserve the last committed cache when an export fails before publication.
- Keep incremental cache updates; do not force a full rebuild after every
  write.
- Make account removal and its cache rebuild one exclusive cache operation.
- Recover automatically from interruption during publication.
- Preserve truthful archive sync history when cache refresh fails.
- Work on Windows, macOS, and Linux with ordinary Go filesystem operations.

## Non-goals

- Creating versioned cache generations or a symlink-based current pointer.
- Serving queries concurrently with a cache build. Readers continue to wait
  on the existing exclusive build lock.
- Making the Parquet cache authoritative. SQLite remains the system of record.
- Changing PostgreSQL analytics behavior.

## Cache Invariants

1. A ready cache has all required Parquet datasets and a structurally valid
   `_last_sync.json` written by a completed build.
2. A builder never runs `COPY` against a path matched by a live reader.
3. Publication begins by invalidating state and ends by writing new state.
4. Every check that can fail without touching live files runs before state
   invalidation.
5. Once state is invalid, any publication failure leaves the cache
   unavailable until a stateless full build repairs it.
6. A stateless build is never a no-op, even when the source database has no
   exportable messages.
7. Account removal invalidates state before its database commit and holds the
   exclusive cache lock until its forced rebuild succeeds or fails.

## Readiness Model

Cache inspection returns one of three states:

- **Absent:** no sync-state file and no Parquet cache files exist. A fresh
  installation follows the existing SQLite/no-cache fallback, and cache stats
  report `StatusNoCacheFiles`.
- **Ready:** all required Parquet datasets exist and `_last_sync.json` parses
  as a completed cache state. DuckDB readers may proceed.
- **Interrupted:** files exist without valid state, state exists without a
  complete set of files, or the state file is malformed. DuckDB readers return
  a cache-unavailable error and the next build performs stateless recovery.

A successfully built zero-message archive is Ready: it contains the existing
schema-compatible empty messages shard, the required empty dimension files,
and valid state. It is distinct from an Absent fresh installation.

DuckDB checks readiness after acquiring the shared cache lock and before
touching Parquet. This closes the race between a state check and a concurrent
publisher. A daemon chooses SQLite fallback for Absent state at startup; an
already-open DuckDB engine treats any non-Ready state as unavailable.

## Build Structure

Lock acquisition and build execution are separated:

- The normal entry point serializes in-process callers, acquires the
  cross-process exclusive lock, and invokes the lock-held builder.
- The lock-held builder assumes the caller owns the exclusive lock and never
  tries to acquire it recursively.
- Account removal uses the same lock-held builder while retaining the lock it
  acquired before deletion.

The lock remains outside the analytics directory so full publication and
recovery can replace cache directories without unlinking the held lock file.

## Staged Export

Each build creates a uniquely named sibling staging directory on the same
filesystem as the analytics directory. All `COPY` destinations point into
that directory, so abandoned staging output is outside every live Parquet
glob. The staging directory name supplies a filesystem-safe build ID.

For a full or stateless build, staging contains a complete replacement for
every required dataset. For an incremental build:

- messages, message recipients, message-label junctions, and attachments
  contain only rows in the new message-ID batch;
- participants, label definitions, sources, and conversations contain full
  replacement files, matching current behavior;
- `COPY` writes into empty staging paths and therefore does not depend on
  DuckDB choosing names that avoid live-file collisions.

Normal error returns remove their staging directory. A killed process may
leave one behind; later builders remove only sibling directories matching the
cache's private staging prefix while holding the exclusive lock. Readers
always ignore them.

## Pre-publication Verification

The builder completes every fallible validation that does not require a live
mutation before invalidating state:

1. Count staged message rows and compare them with the SQLite snapshot's
   expected exportable count for the full snapshot or incremental ID range.
   A legitimate zero-row incremental batch is allowed; a full/stateless
   zero-message build must contain the empty messages shard.
2. Verify every required staged full-replacement dataset exists and is
   readable.
3. Re-read the source sync counters and reject the build if they changed
   during export.
4. Marshal the replacement sync state and construct the complete publication
   plan, including destination names and collision checks.

Any failure here discards staging and returns without modifying the old cache
or its state. This replaces the current `discardAttempt` behavior that removes
the live analytics directory on a consistency mismatch.

## Publication Protocol

Publication runs under the already-held exclusive cache lock:

1. Invalidate the live `_last_sync.json`. This is the first live-tree
   mutation for ordinary builds.
2. Publish staged files.
3. Write the prepared replacement state last.

Full and stateless builds remove each required live dataset directory and
rename its staged replacement into place. The destination is absent before
each rename, making the operation portable across supported platforms.

Incremental builds use two publication rules:

- Full-replacement dimension files are deliberately deleted and then renamed
  from staging. Go's `os.Rename` is not relied upon to overwrite an existing
  Windows file.
- Append-only junction and message shards receive names prefixed with the
  unique build ID before they are renamed into live directories. Message
  shards retain their `year=*` partition directories. The prefix makes
  collisions with existing live shards impossible without depending on
  DuckDB's staging-local filename selection.

Each rename publishes a complete file. The sequence is not globally atomic,
but state remains invalid throughout it, so readers cannot observe a mixed
set. A crash between delete and rename, between shard renames, or during the
final state write leaves Interrupted state and triggers full recovery.

## Empty and Stateless Recovery

The no-op path requires valid prior state in addition to an unchanged message
watermark. Missing or malformed state always proceeds through a stateless full
build.

Consequently, plain `msgvault build-cache` repairs the specific empty-database
case that the daemon's `--auto` path already self-heals: stale live shards are
replaced, a schema-compatible empty messages shard is published, and fresh
state records the empty snapshot.

## Account Removal

For SQLite archives, account removal performs this sequence:

1. Acquire the exclusive cache lock.
2. Invalidate cache state before deleting the source.
3. Commit the serialized source cascade.
4. Invoke the full lock-held cache builder immediately.
5. Release the cache lock after publication succeeds or fails.
6. Complete best-effort credential and attachment cleanup.

Readers block for the cascade plus full rebuild. This is an intentional
behavior change for a rare destructive operation: no normal-success window
may expose the removed account's cached data.

If cache locking or pre-delete invalidation fails, removal aborts before the
database mutation. If rebuilding fails after deletion commits, state remains
invalid and the command returns an explicit partial-success error stating that
the account was removed but cache refresh failed. Cleanup still runs before
the error is returned.

## Refresh Failure Semantics

Cache maintenance is part of successful write-command completion:

- Manual CLI and API operations return an annotated error when their archive
  write succeeded but the required cache refresh failed.
- Scheduled operations surface cache refresh failure through their job result
  and daemon logging/status path.
- A successfully completed archive sync remains completed in `sync_runs`.
  Cache failure never rewrites the sync run as failed, because the mail was
  archived successfully and those counters are themselves cache-staleness
  inputs.

The next scheduled refresh, explicit `build-cache`, or daemon startup can
repair an Interrupted cache. The error remains visible to the operation that
failed to finish cache maintenance.

## Simplification

The commit protocol makes several current workarounds unnecessary:

- exporting messages last to make earlier partial live writes unreachable;
- keeping partially changed live Parquet files readable after an export
  failure;
- deleting the whole analytics directory after a pre-state consistency
  failure;
- treating `_last_sync.json` only as builder metadata rather than the shared
  commit marker.

Tests for those implementation orderings are replaced by tests of the stable
external invariants: old committed cache before publish, unavailable mixed
cache during interrupted publish, and deterministic stateless recovery.

## Test Strategy

All Go tests use testify and exercise the real SQLite-to-DuckDB export path.

- A failed staged export leaves live files and state unchanged and queryable.
- A simulated failure after invalidation but during publication causes readers
  to reject the cache.
- Incremental publication produces build-ID-prefixed, collision-free shards
  and no duplicates after retry.
- Pre-publication row-count and sync-counter failures preserve the old cache.
- Plain, non-auto `build-cache` repairs stale shards for an empty stateless
  database and publishes the empty shard.
- Absent cache inspection retains SQLite/no-cache fallback behavior.
- Incomplete files/state combinations are classified as Interrupted.
- Account removal blocks a shared reader until its forced rebuild finishes,
  and the first subsequent query contains no removed-account rows.
- A failed post-removal rebuild returns partial-success error text and leaves
  readers unable to use the cache.
- A cache refresh failure propagates through command/job results without
  changing a completed `sync_runs` row to failed.
- Replacement publication follows delete-then-rename semantics suitable for
  Windows.

The final quality gate is `go fmt ./...`, `go vet ./...`, `make test`, and the
repository lint target, with the required `fts5 sqlite_vec` tags supplied by
the Makefile.
