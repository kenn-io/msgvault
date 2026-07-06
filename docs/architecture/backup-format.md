---
title: Backup Repository Format
description: How msgvault's backup command coordinates capture with the daemon and materializes restores. The on-disk repository format specification lives with the backup engine, go.kenn.io/kit.
---

The repository/pack format specification — layout, object encodings, versioning, page maps, and the manifest schema — is implemented by [`go.kenn.io/kit`](https://go.kenn.io/kit)'s `backup` and `pack` packages, which msgvault uses as a library. The full specification lives with the engine: see `backup/FORMAT.md` in that module.

msgvault's own schema queries, layout names (`msgvault.db`, the `attachments/` content directory), excluded paths, and manifest stats live in `internal/backupapp`, which implements the engine's `backup.App` interface. The manifest's `msgvault_version` key and `stats` payload are msgvault-defined; their wire encoding is frozen at format v1, so existing repositories stay readable across this change.

This page covers what's specific to msgvault's use of the engine: how `backup create` coordinates with a running msgvault daemon, and what a restore materializes.

## Freeze Protocol

To capture a transactionally consistent database image while the daemon is running:

1. The backup subprocess calls the daemon's same-host-only (loopback, or the daemon's own bind address), authenticated `POST /api/v1/backup/freeze/begin`, which acquires the daemon's serial operation gate (pausing conflicting maintenance work) and returns a token. A 60-second watchdog on the daemon auto-releases the gate if the backup dies.
2. The subprocess opens its own SQLite connection, runs `PRAGMA wal_checkpoint(TRUNCATE)` (with bounded retries) until the WAL is empty, then pins a read transaction — from this point the main database file bytes cannot change under it.
3. It immediately calls `freeze/end` with the token. The gate is released and normal daemon writes resume; the pinned transaction alone keeps the file image stable for the page scan. Database geometry, statistics, and attachment locators are all read inside the pinned transaction.

The freeze window is therefore milliseconds-to-seconds regardless of archive size. `backup create` refuses to run unfrozen against a live daemon: if the daemon's runtime record cannot be resolved, the backup fails rather than risking a torn read.

## Restore

`backup restore` materializes one snapshot into a target directory as a usable archive home: the database written run-by-run at `page × page_size` from the materialized page map, attachments at the storage paths the restored database records for each hash (importers may namespace paths beyond the loose `<hash[:2]>/<hash>` layout; paths are re-validated as local before writing), and captured extras at their recorded relative paths and file modes (tree entry paths are re-validated as local and traversal-free before writing). It refuses a non-empty target without `--overwrite` and refuses the live archive home of a running daemon outright.

Restore is self-proving, in layers. During materialization every blob read re-derives its SHA-256 identity (the pack reader's normal contract) and every database page is additionally checked against the snapshot's page-hash map before it is written — so a page-map bug cannot silently place correct bytes at the wrong offset. After materialization the restored database must pass `PRAGMA integrity_check` and reproduce the manifest's recorded stats through exactly the queries capture ran inside the freeze window; the end-to-end test further proves the restored file is byte-identical to the live database as it existed at capture time, including for parent snapshots restored from an incremental chain. All files, and the directory entries naming them, are fsynced before restore reports success. Pack reads are grouped by pack with a `--jobs` worker bound (1 = strictly serial for spinning-disk repositories); serial and parallel restores produce byte-identical trees. Restoring an old backup onto a newer msgvault goes through normal schema migration at first open, the same path as any upgrade.

## Limitations

- The daemon operation gate is held only through the freeze protocol (checkpoint plus read-transaction pin), not through attachment capture. A gated operation that deletes attachment files (such as `remove-account`) while a backup is still capturing can therefore delete a file the frozen database still references; the backup then fails loudly with a read or hash error and can be retried after the deletion completes. This is a deliberate trade: holding the gate — and with it every daemon write — for the full capture window would be far more disruptive than a rare retryable backup failure. A snapshot that completed is unaffected: it captured every file it references.
- Repository encryption and retention (`forget`/`prune`) are not yet implemented; see the engine's `backup/FORMAT.md` roadmap for the settled design.
