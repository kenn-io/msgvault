---
title: Backup
description: Create incremental, verifiable snapshots of your archive in a local backup repository that is safe to sync off-site.
---

Use `msgvault backup` to save your entire archive to a **backup repository**, a self-contained directory of immutable, checksummed files. A snapshot covers your SQLite database, every attachment, and optionally your configuration and deletion audit trail. Snapshots are incremental (storing only changed database pages and new attachments), deduplicated, and verifiable down to the byte.

Because the repository is append-only and holds only write-once files, it works cleanly with `rsync`, `rclone`, cloud-drive folders, and filesystem snapshots. You can point any off-site sync tool at the repository without worrying about files changing underneath a transfer.

!!! note "Encryption and retention are not implemented yet"
    This release ships capture and restore: `init`, `create`, `list`, `verify`, and `restore`. Repository encryption and retention (pruning old snapshots) are planned follow-ups.

## Quick start

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

## What a snapshot contains

Each snapshot is a complete, self-consistent picture of your archive at one instant:

- **Database.** msgvault captures the SQLite file at 4 KB page granularity. The first snapshot stores every page, while later snapshots store only pages whose content hash changed, typically a small fraction of the file. msgvault briefly freezes the database (see below) so the captured pages form a transactionally consistent image, and verified snapshots materialize to a byte-identical copy of the original file.
- **Attachments.** msgvault identifies every attachment by its content SHA-256 and re-hashes it from disk during capture, so a corrupted file fails the backup loudly rather than being stored wrong. Each attachment is stored once regardless of how many messages or snapshots reference it, and unchanged attachments cost nothing in later snapshots.
- **Extras (optional).** `--include-config` captures `config.toml` verbatim, the `deletions/` audit directory is captured automatically when present, and `--include-tokens` captures OAuth token files. Because config and tokens contain live credentials, capturing either into an unencrypted repository requires the explicit `--allow-plaintext-secrets` flag. Without it, the backup fails rather than quietly leaking secrets.

msgvault compresses attachment and page data with zstd (level 3 by default, configurable with `zstd_level`). When content does not compress, such as most images, video, and other already-compressed media, msgvault detects that and stores it raw instead of wasting CPU.

## Commands

### `backup init --repo DIR`

Creates an empty repository with its directory layout, a random repository ID, and a plain-text `config.toml` recording the format version. This command runs locally and never touches your archive.

### `backup create`

Takes a snapshot. Flags:

| Flag | Effect |
|------|--------|
| `--repo DIR` | Repository path (overrides `[backup] repo`) |
| `--include-config` | Capture `config.toml` verbatim (may contain API keys) |
| `--include-tokens` | Capture OAuth tokens, provider credentials, and the server key |
| `--allow-plaintext-secrets` | Permit config/tokens in an unencrypted repository |
| `--tag STR` | Free-form label shown by `backup list` |
| `--force-unlock` | Override a fresh repository lock (see Locking) |
| `--jobs N` | Concurrent attachment capture workers (default: one per CPU; use `1` for serial reads on spinning disks or NAS shares) |

`backup create` always runs through the msgvault daemon. When `[remote].url` is set and you don't pass `--local`, it uses the configured remote daemon and doesn't start a local one. Otherwise `backup create` uses the local daemon and starts it if it isn't already running. The daemon briefly pauses conflicting maintenance operations while the backup pins a consistent read of the database. That pause lasts only as long as checkpointing the WAL and opening a read transaction. Normal syncing and reads continue while pages are scanned. A daemon-side watchdog guarantees that a crashed backup can never leave the daemon wedged.

`backup create` requires a SQLite archive; PostgreSQL-backed deployments are not supported.

### `backup list`

Prints every snapshot with its ID, creation time, message count, whether the snapshot contains normalized document plaintext, bytes added, and tag. Read-only and lock-free.

### `backup verify [SNAPSHOT] [--all] [--quick]`

Checks repository integrity and exits non-zero if it finds any problem, printing one line per problem and a summary.

- Default (full) mode reads every blob the snapshot references, re-verifies its content hash, and confirms the reconstructed database page map is complete and matches the recorded geometry. Corruption reports name the damaged blob and the pack file that holds it, so you know which file to restore from another replica.
- `--quick` runs structural checks only (every referenced blob resolves, indexes decode, and packs exist) without reading blob contents.
- `--all` verifies every snapshot, reading shared content once and reusing the verdict.

Verification takes a shared lock, so multiple verifies can run at the same time, though never during a `create`.

For repositories on spinning disks or NAS shares, pass `--jobs 1` to read packs strictly one at a time instead of using the default one-reader-per-CPU parallelism. `create` accepts the same flag for its attachment reads and changed-page packing.

### `backup restore --target DIR [SNAPSHOT] [--overwrite] [--integrity-check] [--loose-attachments] [--permissions-only]`

Materializes a snapshot (the latest by default) into `--target` as a complete archive home containing `msgvault.db`, the attachment store, and any captured extras (deletions manifests, config, and tokens) at their original relative paths with their recorded file modes. Point msgvault at the restored directory, or swap it into place, to use it.

By default, restore copies compatible immutable repository packs directly into the attachment store. This avoids creating thousands of individual files, especially on Windows, without skipping content verification: restore still reads and SHA-256 verifies every selected attachment before granting the restored database authority to read it from a pack. An attachment is restored as a loose file when it exceeds the target store's configured maintenance limit, or when its repository pack is structurally valid but uses an incompatible representation. The completed archive can therefore be packed or mixed, and the command summary reports packed and loose counts along with aggregate fallback reasons. Corruption remains a hard failure and is never treated as a compatibility fallback.

Restore verifies its own work. It checks every database page against the snapshot's page-hash map as it writes, re-derives the SHA-256 identity of every blob it reads, and after materialization checks that the restored database reproduces the manifest's recorded stats (message, conversation, and attachment counts, plus date range) using the exact queries capture ran inside the freeze. Any mismatch fails the restore rather than reporting success.

- The target must not exist or must be an empty directory, unless you pass `--overwrite` to restore into a non-empty directory. Overwrite **merges**: it removes the database and its SQLite `-wal`/`-shm` sidecars first (otherwise a stale WAL would be replayed over the restored database on its first open), replaces same-named files with the restored ones, and leaves unrelated loose files that the snapshot does not carry in place. Packed authority is replaced by the snapshot's catalog, so normal maintenance can reclaim old packs containing only non-snapshot content. To guarantee the target contains only the snapshot and nothing else, restore into a fresh directory.
- On Windows, restore secures token files after publishing the snapshot. If omitted entries would lose access, it reports a conflict after securing the selected files and leaves token-directory permissions unchanged. Move the omitted entries aside and retry with `--permissions-only`; other permission errors can leave a partial repair, which the same command can retry.
- `--loose-attachments` disables direct pack installation and materializes every attachment as an individual file. On an overwritten target, pre-existing pack files can remain uncataloged and can still be discovered by later maintenance; `unpack-attachments` processes only cataloged packs and does not remove those leftovers. A fresh target is therefore the only restore path that currently guarantees a fully loose archive.
- Restore refuses to write into the live archive home of a *running* daemon. Stop the daemon first or restore elsewhere.
- When you restore an old backup onto a newer msgvault, opening the restored database for the first time runs normal schema migration, the same path as any upgrade.
- `--jobs 1` serializes pack reads for repositories on spinning disks or NAS shares.
- `--integrity-check` adds SQLite's full `PRAGMA integrity_check` after materialization. It reads the entire restored database single-threaded and can take much longer than the default hash and manifest-statistics verification on large archives; the `SQLite check` progress line keeps counting elapsed time while it runs.

Restore takes a shared repository lock, so it can run alongside verifies, but never during a `create`.

Direct pack installation still reads and hashes the selected attachment bytes. Its performance benefit comes primarily from eliminating per-attachment file creation, directory updates, and antivirus scans rather than avoiding repository read I/O.

## Configuration

```toml
[backup]
# Default repository for all backup commands.
repo = "~/Backups/msgvault"

# zstd compression level: 0 uses the built-in default (3); 1-19 otherwise.
zstd_level = 0
```

## How incremental capture works

The repository is a content-addressed store where every piece of data (a run of database pages, an attachment, or a metadata object) is a **blob** named by the SHA-256 of its content. msgvault appends blobs into ~32 MB **pack files** that are sealed once and never modified. It writes the snapshot **manifest**, a small JSON file, last, only after everything it references is durably on disk. Because of that order, an interrupted backup never produces a half-snapshot: either the manifest exists and the snapshot is complete, or it doesn't and at worst some unreferenced data sits in the repository until a future cleanup command reclaims it.

Between snapshots, msgvault keeps a per-page hash map of the database. At backup time, it re-hashes the frozen database, diffs against the previous snapshot's map, and stores only the changed pages, packing large contiguous changed regions as dedicated blobs and grouping scattered small changes together. Every ~30 snapshots, or sooner once deltas outweigh a fresh baseline, it writes a new full baseline so restore and verify never walk long chains.

## Locking

`create` takes an exclusive lock on the repository, and `verify` takes a shared lock. Locks carry the hostname, process ID, and operation, heartbeat every 30 seconds, and become stale after 30 minutes, when msgvault reaps them automatically. If a lock is genuinely orphaned but still fresh, for example after a hard power cut within the stale window, you can override it with `--force-unlock`. Never use `--force-unlock` while another msgvault process might still be running against the repository.

## Operating recommendations

- **Schedule it.** `backup create` is built for unattended nightly runs with cron, launchd, or systemd timers. Repeat runs with no changes are cheap and produce a small snapshot recording that nothing changed.
- **Sync it off-site.** Everything in the repository is write-once, so you can point `rclone`, `rsync`, or a cloud-drive client at it. Sync after the backup completes, not during.
- **Verify on a schedule too.** `backup verify --all --quick` is fast enough to run alongside every backup. Run a full `backup verify --all` weekly or monthly to catch bit rot on the storage medium itself.
- **Keep the cache.** msgvault stores a small per-repository page-hash cache under `~/.msgvault/backup-cache/`. Losing it is harmless because the next backup re-derives it, and keeping it makes nightly backups faster.

## Restoring to a New Machine

If a drive dies, or you're setting up msgvault on a new Mac, Linux box, or Windows PC, follow these steps to bring your archive back exactly as it was.

!!! note "Windows: use `$HOME`, not `~`"
    The examples below are written for a Unix shell, where the shell expands `~` before msgvault sees it. PowerShell does not expand `~` in arguments to native programs, and msgvault does not expand it either; a literal `~` in `--target` creates a directory actually named `~` under the current directory. On Windows, run the examples in PowerShell with `$HOME\.msgvault` and `$HOME\Backups\msgvault` in place of the `~/` paths. The Windows tabs below show the exact commands.

1. **Install msgvault**, without running any command that touches `~/.msgvault` yet (no `init-db`, no `add-account`). Restoring works best into a directory that doesn't exist yet; see [Overwriting an existing msgvault](#overwriting-an-existing-msgvault) for why.
2. **Copy or mount the repository locally.** Use whatever method you synced it off-site with (`rclone`, `rsync`, a cloud-drive client, or an external drive):
   ```bash
   rclone copy remote:msgvault-backups ~/Backups/msgvault
   ```
3. **List snapshots** to confirm the repository arrived intact and choose a snapshot (usually the latest):
   ```bash
   msgvault backup list --repo ~/Backups/msgvault
   ```
   Because there is no `config.toml` on this machine yet, pass `--repo` explicitly here and in the commands below instead of relying on `[backup] repo`.
4. **Restore into the default location**, `~/.msgvault`:

    === "macOS / Linux"

        ```bash
        msgvault backup restore --repo ~/Backups/msgvault --target ~/.msgvault
        ```

    === "Windows (PowerShell)"

        ```powershell
        msgvault backup restore --repo $HOME\Backups\msgvault --target $HOME\.msgvault
        ```

    Because nothing has touched `~/.msgvault` yet, the directory doesn't exist and restore creates it fresh, which guarantees the target ends up with only the snapshot's contents (see `--overwrite` above for what changes when the target already exists).
5. **Check for a restored config.** If the snapshot was taken with `--include-config`, restore places the *old machine's* `config.toml` into `~/.msgvault`, and settings in it can point msgvault away from the archive you restored. Open it and remove or update anything machine-specific before running other commands:
    - `[data] database_url` takes precedence over `data_dir`, so a restored absolute SQLite path or PostgreSQL DSN sends every command to the old machine's database instead of the restored one.
    - `[data] data_dir` with an absolute path from the old machine points at a directory that doesn't exist here.
    - `[remote] url` sends `tui` to a remote server instead of the local archive.

    If the snapshot did not capture config, there is nothing to check because msgvault's defaults already point at `~/.msgvault`.
6. **Use your archive.** `msgvault stats` and `msgvault tui` read `~/.msgvault` by default on every platform, so once no leftover config overrides that path, the restored archive is the active one.

### Overwriting an existing msgvault

`backup restore --target DIR` refuses a non-empty `DIR` unless you pass `--overwrite`, and `--overwrite` **merges** rather than wiping the directory first (see the flag's description above). Use it only when you deliberately want to merge a snapshot into a target you understand, such as re-running a restore that was interrupted partway through.

!!! warning "A non-empty `~/.msgvault` is not a blank slate"
    If `~/.msgvault` already has files in it, such as a `config.toml` left over from running `msgvault init-db`, tokens for an account the snapshot doesn't include, or loose attachment files from an earlier experiment, restoring with `--overwrite` layers the snapshot on top rather than replacing the directory outright. Restore removes and replaces the previous `msgvault.db`, while leaving unrelated files that the snapshot doesn't carry in place. The result is a target that is *not* provably identical to the snapshot.

    Check before you restore. If the directory is non-empty and you're not deliberately merging, move it aside rather than deleting it in case it holds an archive you still want:

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

## Deleted and purged messages

msgvault treats hiding a message (flagging it deleted) differently from purging it (removing the rows and attachment files from disk), and backups reflect that difference:

- **Flag-deleted messages.** Flagging a message is a database change like any other. The message and its attachments stay in the archive, so new snapshots continue to capture them and store the changed database pages incrementally. Restoring any snapshot restores the flag state as of that snapshot.
- **Purged messages.** Purged messages disappear *logically* from subsequent snapshots. Once the rows and attachment files are gone, the next `backup create` no longer references them: the purged attachment content is not in the snapshot's attachment list, and no query against a restored database returns the purged rows. Physically, however, backups capture the database byte-for-byte at page granularity, and SQLite keeps deleted row payloads in free pages until those pages are rewritten or the database is vacuumed. Fragments of purged content can therefore persist inside later snapshots' database pages, and inside a *fresh* repository seeded from the same unvacuumed archive.
- **Older snapshots.** The repository is append-only, so a snapshot taken before a purge still references the purged content, and that content remains in the repository's pack files. This behavior is deliberate and protects you against an accidental or malicious purge.

From a privacy standpoint, purging a message from the archive does **not** expunge it from backups taken earlier. Until the planned `forget`/`prune` commands ship (to delete old snapshots and reclaim unreferenced data), expunging content from a backup repository takes two steps: rewrite the archive database so free-page remnants are gone (`VACUUM`, or any msgvault compact operation that rewrites the file), then recreate the repository and take a fresh snapshot. Recreating the repository without vacuuming first can carry purged fragments along in the new snapshot's pages. If you sync the repository off-site, apply the same steps to the replica.

## Compatibility

The repository records a format version and a **minimum reader version**. A newer msgvault that changes the format in a way old readers cannot safely handle raises the minimum reader version; an older msgvault opening that repository refuses cleanly with an upgrade message instead of misreading it. Every binary object in the repository also carries its own magic number, version, and SHA-256 integrity trailer, and each snapshot manifest records the msgvault version that wrote it. See [Backup Repository Format](/docs/architecture/backup-format/) for how msgvault integrates with the backup engine; the full on-disk format specification lives with the engine, in `backup/FORMAT.md` of [`go.kenn.io/kit`](https://go.kenn.io/kit).
