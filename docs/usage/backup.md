---
title: Backup
description: Create incremental, verifiable snapshots of your archive in a local backup repository that is safe to sync off-site.
---

`msgvault backup` captures your entire archive — the SQLite database, every attachment, and optionally your configuration and deletion audit trail — into a **backup repository**: a self-contained directory of immutable, checksummed files. Snapshots are incremental (only changed database pages and new attachments are stored), deduplicated, and verifiable down to the byte.

The repository is append-only and consists entirely of write-once files, which makes it friendly to `rsync`, `rclone`, cloud-drive folders, and filesystem snapshots. You can point any off-site sync tool at it without worrying about files changing underneath the transfer.

!!! note "Encryption and retention are not implemented yet"
    This release ships capture and restore: `init`, `create`, `list`, `verify`, and `restore`. Repository encryption and retention (pruning old snapshots) are planned follow-ups.

## Quick Start

```bash
# One-time: create an empty repository
msgvault backup init --repo ~/Backups/msgvault

# Record the repository in config so you can drop --repo everywhere
# (~/.msgvault/config.toml)
# [backup]
# repo = "~/Backups/msgvault"

# Take a snapshot
msgvault backup create

# See what you have
msgvault backup list

# Prove the latest snapshot is intact, byte for byte
msgvault backup verify

# Materialize a snapshot into a fresh directory and prove the result
msgvault backup restore --target ~/msgvault-restored
```

## What a Snapshot Contains

Each snapshot is a complete, self-consistent picture of the archive at one instant:

- **Database.** The SQLite file is captured at 4 KB page granularity. The first snapshot stores every page; later snapshots store only pages whose content hash changed, typically a small fraction of the file. The database is briefly frozen (see below) so the captured pages are a transactionally consistent image, then verified snapshots materialize to a byte-identical copy of the original file.
- **Attachments.** Every attachment is identified by its content SHA-256, re-hashed from disk during capture (a corrupted file fails the backup loudly rather than being stored wrong), and stored once regardless of how many messages or snapshots reference it. Unchanged attachments cost nothing in later snapshots.
- **Extras (optional).** `--include-config` captures `config.toml` verbatim; the `deletions/` audit directory is captured automatically when present; `--include-tokens` captures OAuth token files. Because config and tokens contain live credentials, capturing either into an unencrypted repository requires the explicit `--allow-plaintext-secrets` flag — the backup fails otherwise rather than quietly leaking secrets.

Attachment and page data is compressed with zstd (level 3 by default, configurable via `zstd_level`); content that does not compress — most images, video, and other already-compressed media — is detected and stored raw instead of wasting CPU.

## Commands

### `backup init --repo DIR`

Creates an empty repository: directory layout, a random repository ID, and a plain-text `config.toml` recording the format version. Runs locally and never touches the archive.

### `backup create`

Takes a snapshot. Flags:

| Flag | Effect |
|------|--------|
| `--repo DIR` | Repository path (overrides `[backup] repo`) |
| `--include-config` | Capture `config.toml` verbatim (may contain API keys) |
| `--include-tokens` | Capture OAuth token files |
| `--allow-plaintext-secrets` | Permit config/tokens in an unencrypted repository |
| `--tag STR` | Free-form label shown by `backup list` |
| `--force-unlock` | Override a fresh repository lock (see Locking) |
| `--jobs N` | Concurrent attachment capture workers (default: one per CPU; use `1` for serial reads on spinning disks or NAS shares) |

When the msgvault daemon is running, `backup create` coordinates with it automatically: the command is proxied through the daemon, which briefly pauses conflicting maintenance operations while the backup pins a consistent read of the database. The pause lasts only as long as it takes to checkpoint the WAL and open a read transaction — normal syncing and reads continue while pages are scanned. A watchdog on the daemon side guarantees a crashed backup can never leave the daemon wedged.

`backup create` requires a SQLite archive; PostgreSQL-backed deployments are not supported.

### `backup list`

Prints every snapshot with its ID, creation time, message count, bytes added, and tag. Read-only and lock-free.

### `backup verify [SNAPSHOT] [--all] [--quick]`

Checks repository integrity and exits non-zero if any problem is found, printing one line per problem and a summary.

- Default (full) mode reads every blob the snapshot references, re-verifies its content hash, and confirms the reconstructed database page map is complete and matches the recorded geometry. Corruption reports name the damaged blob and the pack file that holds it, so you know exactly which file to restore from another replica.
- `--quick` performs structural checks only (every referenced blob resolves, indexes decode, packs exist) without reading blob contents.
- `--all` verifies every snapshot; shared content is read once and the verdict reused.

Verification takes a shared lock, so multiple verifies can run concurrently, but never during a `create`.

For repositories on spinning disks or NAS shares, `--jobs 1` reads packs strictly one at a time instead of the default one-reader-per-CPU parallelism; `create` accepts the same flag for its attachment reads and changed-page packing.

### `backup restore --target DIR [SNAPSHOT] [--overwrite] [--loose-attachments]`

Materializes a snapshot (the latest by default) into `--target` as a complete archive home — `msgvault.db`, the attachment store, and any captured extras (deletions manifests, config, tokens) at their original relative paths with their recorded file modes. Point msgvault at the restored directory, or swap it into place, to use it.

By default, compatible immutable repository packs are copied directly into the
attachment store. This avoids creating thousands of individual files,
especially on Windows, without skipping content verification: restore reads
and SHA-256 verifies every selected attachment before granting the restored
database authority to read it from a pack. An attachment is restored as a
loose file when it exceeds the target store's configured maintenance limit, or
when its repository pack is structurally valid but uses an incompatible
representation. The completed archive may therefore be packed or mixed; the
command summary reports packed and loose counts plus aggregate fallback
reasons. Corruption remains a hard failure and is never treated as a
compatibility fallback.

Restore does not trust itself: every database page is checked against the snapshot's page-hash map as it is written, every blob read re-derives its SHA-256 identity, and after materialization the restored database must pass SQLite's `PRAGMA integrity_check` and reproduce the manifest's recorded stats (message, conversation, and attachment counts, date range) through exactly the queries capture ran inside the freeze. Any mismatch fails the restore rather than reporting success.

- The target must not exist or must be an empty directory; `--overwrite` permits restoring into a non-empty one. Overwrite **merges**: the database and its SQLite `-wal`/`-shm` sidecars are removed first (a stale WAL would otherwise be replayed over the restored database on its first open), restored files replace same-named ones, and unrelated loose files the snapshot does not carry are left in place. Packed authority is replaced by the snapshot's catalog, so normal maintenance can reclaim old packs containing only non-snapshot content. To guarantee the target contains exactly the snapshot and nothing else, restore into a fresh directory.
- `--loose-attachments` disables direct pack installation and materializes every attachment as an individual file. On an overwritten target, pre-existing pack files can remain uncataloged and can still be discovered by later maintenance; `unpack-attachments` processes only cataloged packs and does not remove those leftovers. A fresh target is therefore the only restore path that currently guarantees a fully loose archive.
- Restoring into the live archive home of a *running* daemon is refused outright — stop the daemon first or restore elsewhere.
- Restoring an old backup onto a newer msgvault goes through normal schema migration the first time the restored database is opened, the same path as any upgrade.
- `--jobs 1` serializes pack reads for repositories on spinning disks or NAS shares.
- The proof's `integrity_check` reads the entire restored database single-threaded inside SQLite, so on large archives the proof stage runs for a while after materialization finishes; the progress line keeps counting elapsed time while it does.

Restore takes a shared repository lock: it can run alongside verifies, never during a `create`.

Direct pack installation still reads and hashes the selected attachment bytes.
Its performance benefit comes primarily from eliminating per-attachment file
creation, directory updates, and antivirus scans rather than from avoiding
repository read I/O.

## Configuration

```toml
[backup]
# Default repository for all backup commands.
repo = "~/Backups/msgvault"

# zstd compression level: 0 uses the built-in default (3); 1-19 otherwise.
zstd_level = 0
```

## How Incremental Capture Works

The repository is a content-addressed store: every piece of data (a run of database pages, an attachment, a metadata object) is a **blob** named by the SHA-256 of its content. Blobs are appended into ~32 MB **pack files** that are sealed once and never modified. A snapshot **manifest** — a small JSON file — is written last, only after everything it references is durably on disk. An interrupted backup therefore never produces a half-snapshot: either the manifest exists and the snapshot is complete, or it doesn't and at worst some unreferenced data sits in the repository until a future cleanup command reclaims it.

Between snapshots, msgvault keeps a per-page hash map of the database. At backup time it re-hashes the (frozen) database, diffs against the previous snapshot's map, and stores only the changed pages — large contiguous changed regions as dedicated blobs, scattered small changes grouped together. Every ~30 snapshots (or sooner, once deltas outweigh a fresh baseline) it writes a new full baseline so restore and verify never walk long chains.

## Locking

`create` takes an exclusive lock on the repository; `verify` takes a shared lock. Locks carry the hostname, process ID, and operation, heartbeat every 30 seconds, and are considered stale after 30 minutes, at which point they are reaped automatically. If a lock is genuinely orphaned but still fresh (for example, after a hard power cut within the stale window), `--force-unlock` overrides it — never use it while another msgvault process might actually be running against the repository.

## Operating Recommendations

- **Schedule it.** `backup create` is designed for unattended nightly runs (cron, launchd, systemd timers). Repeat runs with no changes are cheap and produce a small snapshot recording that nothing changed.
- **Sync it off-site.** Everything in the repository is write-once; point `rclone`, `rsync`, or a cloud-drive client at it. Sync after the backup completes, not during.
- **Verify on a schedule too.** `backup verify --all --quick` is fast enough to run alongside every backup; run a full `backup verify --all` weekly or monthly to catch bit rot on the storage medium itself.
- **Keep the cache.** msgvault stores a small per-repository page-hash cache under `~/.msgvault/backup-cache/`. Losing it is harmless — the next backup just re-derives it — but keeping it makes nightly backups faster.

## Restoring to a New Machine

This is the scenario `backup` exists for: a drive dies, or you're setting up msgvault on a new Mac, Linux box, or Windows PC, and you need the archive back exactly as it was.

!!! note "Windows: use `$HOME`, not `~`"
    The examples below are written for a Unix shell, where the shell expands `~` before msgvault sees it. PowerShell does not expand `~` in arguments to native programs, and msgvault does not expand it either — a literal `~` in `--target` creates a directory actually named `~` under the current directory. On Windows, run the examples in PowerShell with `$HOME\.msgvault` and `$HOME\Backups\msgvault` in place of the `~/` paths; the Windows tabs below show the exact commands.

1. **Install msgvault**, but don't run any command that touches `~/.msgvault` yet — no `init-db`, no `add-account`. Restoring works best into a directory that doesn't exist yet; see the footgun below for why.
2. **Get the repository onto the new machine.** However you synced it off-site — `rclone`, `rsync`, a cloud-drive client, an external drive — copy or mount it locally:
   ```bash
   rclone copy remote:msgvault-backups ~/Backups/msgvault
   ```
3. **List snapshots** to confirm the repository came across intact and pick one (usually the latest):
   ```bash
   msgvault backup list --repo ~/Backups/msgvault
   ```
   There's no `config.toml` on this machine yet, so pass `--repo` explicitly here and below rather than relying on `[backup] repo`.
4. **Restore into the default location**, `~/.msgvault`:

    === "macOS / Linux"

        ```bash
        msgvault backup restore --repo ~/Backups/msgvault --target ~/.msgvault
        ```

    === "Windows (PowerShell)"

        ```powershell
        msgvault backup restore --repo $HOME\Backups\msgvault --target $HOME\.msgvault
        ```

    Since nothing has touched `~/.msgvault` yet, the directory doesn't exist and restore creates it fresh — the path that guarantees the target ends up with exactly the snapshot's contents (see `--overwrite` above for what changes once the target already exists).
5. **Check for a restored config.** If the snapshot was taken with `--include-config`, the restore just placed the *old machine's* `config.toml` into `~/.msgvault` — and settings in it can point msgvault away from the archive you just restored. Open it and remove or update anything machine-specific before running other commands:
    - `[data] database_url` — takes precedence over `data_dir`; a restored absolute SQLite path or PostgreSQL DSN sends every command to the old machine's database, not the restored one.
    - `[data] data_dir` — an absolute path from the old machine points at a directory that doesn't exist here.
    - `[remote] url` — sends `tui` to a remote server instead of the local archive.

    If the snapshot did not capture config, there is nothing to check — msgvault's defaults already point at `~/.msgvault`.
6. **Use it.** `msgvault stats` and `msgvault tui` read `~/.msgvault` by default on every platform, so once no leftover config overrides that, the restored archive is the active one.

### Footgun: restoring over an existing msgvault

`backup restore --target DIR` refuses a non-empty `DIR` unless you pass `--overwrite` — but `--overwrite` **merges**, it doesn't wipe first (see the flag's description above). Reach for it only when you deliberately want to merge a snapshot into a target you understand, such as re-running a restore that was interrupted partway through.

!!! warning "A non-empty `~/.msgvault` is not a blank slate"
    If `~/.msgvault` already has anything in it — a `config.toml` left over from once running `msgvault init-db` to try something, tokens for an account the snapshot doesn't include, loose attachment files from an earlier experiment — restoring with `--overwrite` layers the snapshot on top rather than replacing the directory outright. The previous `msgvault.db` is removed and replaced, but unrelated files the snapshot doesn't carry are left in place. The result is a target that is *not* provably identical to the snapshot, which defeats the point of restoring in the first place.

    Check before you restore, and if the directory is non-empty and you're not deliberately merging, move it aside rather than deleting it — it may be an archive you still want:

    === "macOS / Linux"

        ```bash
        ls -la ~/.msgvault
        mv ~/.msgvault ~/.msgvault.bak
        msgvault backup restore --repo ~/Backups/msgvault --target ~/.msgvault
        ```

    === "Windows (PowerShell)"

        ```powershell
        Get-ChildItem -Force $HOME\.msgvault
        Move-Item $HOME\.msgvault $HOME\.msgvault.bak
        msgvault backup restore --repo $HOME\Backups\msgvault --target $HOME\.msgvault
        ```

## Deleted and Purged Messages

msgvault distinguishes hiding a message (flagging it deleted) from purging it (removing the rows and attachment files from disk). Backups interact with each differently:

- **Flag-deleted messages** are a database change like any other. The message and its attachments are still in the archive, so new snapshots continue to capture them; the changed database pages are stored incrementally. Restoring any snapshot restores the flag state as of that snapshot.
- **Purged messages** disappear *logically* from subsequent snapshots. Once the rows and attachment files are gone, the next `backup create` no longer references them: the purged attachment content is not in the snapshot's attachment list, and no query against a restored database returns the purged rows. Physically, though, backups capture the database byte-for-byte at page granularity — and SQLite keeps deleted row payloads in free pages until those pages are rewritten or the database is vacuumed. Fragments of purged content can therefore persist inside later snapshots' database pages, and inside a *fresh* repository seeded from the same unvacuumed archive.
- **Older snapshots keep everything.** The repository is append-only: a snapshot taken before a purge still references the purged content, and that content remains in the repository's pack files. This is deliberate — it is what makes a backup protection against an accidental or malicious purge.

The flip side is a privacy consideration: purging a message from the archive does **not** expunge it from backups taken earlier. Until the planned `forget`/`prune` commands ship (delete old snapshots, then reclaim unreferenced data), expunging content from a backup repository takes two steps: rewrite the archive database so free-page remnants are gone (`VACUUM`, or any msgvault compact operation that rewrites the file), then recreate the repository and take a fresh snapshot. Recreating the repository without vacuuming first can carry purged fragments along in the new snapshot's pages. If you sync the repository off-site, the same applies to the replica.

## Compatibility

The repository records a format version and a **minimum reader version**. A newer msgvault that changes the format in a way old readers cannot safely handle will raise the minimum reader version; an older msgvault opening such a repository refuses cleanly with an upgrade message instead of misreading it. Every binary object in the repository additionally carries its own magic number, version, and SHA-256 integrity trailer, and each snapshot manifest records the msgvault version that wrote it. See [Backup Repository Format](/architecture/backup-format/) for how msgvault integrates with the backup engine; the full on-disk format specification lives with the engine, in `backup/FORMAT.md` of [`go.kenn.io/kit`](https://go.kenn.io/kit).
