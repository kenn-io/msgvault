---
last_edited: "2026-10-28"
title: Matrix
description: Archive Matrix room history with keys from your account's backup.
---

Archive messages from joined rooms on any Matrix homeserver with a dedicated msgvault device. Encrypted history becomes searchable when your account's key backup contains the room keys.

All imported messages use `message_type = matrix`. Sync never sends messages,
read receipts, typing notifications, or presence updates. Device login is the
only account mutation.

## Add a Matrix account

Create a password file readable only by your user, then register the account:

```bash
chmod 600 /path/to/matrix-password
msgvault add-matrix \
  --homeserver https://matrix.example.org \
  --user-id @archive:example.org \
  --password-file /path/to/matrix-password \
  --recovery-file /path/to/matrix-recovery-key
```

`add-matrix` creates a separate Matrix device named **msgvault (read-only)**.
It stores the access token and unlocked backup key in an owner-only file under `tokens/`. Anyone holding that file can read this account's backed-up room keys. `remove-account` deletes it; `backup create --include-tokens` captures it. The recovery key or passphrase is never saved.

Use the `--recovery-passphrase` switch with `--recovery-file` for a passphrase file. Without a file, an interactive run prompts for a recovery key or, with `--recovery-passphrase`, a passphrase. Renewals can leave it blank to keep the saved backup key. First setup requires a recovery secret or `--skip-key-backup`, which clears any saved backup key and keeps encrypted placeholders.

To renew a revoked or expired login, run `add-matrix` again for the same user.
It saves the new device's token in place, logs out the old device, and keeps
the archived history and sync state. If renewal stops partway, for example on
a full disk, the new login is kept in a pending file under `tokens/` and the
next `add-matrix` run for that user finishes with that login instead of
creating another device. That run still reads `--password-file` or
`--login-token-file` but doesn't use it, so an already-used SSO token file is
fine.

For an SSO account, complete the homeserver's SSO flow to obtain a single-use
`m.login.token`, save it to an owner-only file, and use
`--login-token-file` instead of `--password-file`. Availability of this token
flow depends on the homeserver and identity provider.

## Sync rooms

```bash
# First run backfills joined rooms; later runs use the saved /sync token.
msgvault sync-matrix

# Re-read complete history to pick up anything missed.
msgvault sync-matrix --full

# Sync one registered Matrix user and defer media bytes.
msgvault sync-matrix --account @archive:example.org --no-media
```

The first run takes a Matrix `/sync` snapshot, then walks backward through each
joined room with `/messages`. It checkpoints each room and saves `next_batch`
for later incremental `/sync` calls. Re-running after interruption skips event
IDs already archived and continues from the saved room cursor. When an
incremental timeline is cut short, msgvault reads `/messages` back to the
previous sync token before committing the new one.

Room include and exclude lists use exact Matrix room IDs. Configure them with
the [`[matrix]` settings](../configuration.md#matrix). The exclude list wins
when a room appears in both lists. Run `msgvault sync-matrix --full` after
changing either list so newly included quiet rooms are discovered immediately.

Every `/sync` request sends `set_presence=offline`. The Matrix client-server
specification defines that value as "the client is not marked as being online
when it uses this API", so syncing leaves the account's presence unchanged,
including while another client keeps it online. Omitting the parameter would
mark the account online, and `unavailable` would mark it idle.

## What is archived

- Text, notice, and emote events, with HTML formatted bodies converted to
  searchable plain text.
- Images, stickers, video, audio, and files under the shared chat
  [media policy](../configuration.md#media-policy).
- Edits from the original sender, redactions, reactions, and reply
  relationships.
- Current joined-room membership as conversation participants. The account's
  `m.direct` map distinguishes direct chats from groups.
- The original Matrix event JSON (`raw_format = matrix_json`).

Events whose room keys are available decrypt through the account's key backup. Decrypted event JSON replaces the ciphertext in the archive. Other encrypted events stay as `[encrypted message — keys unavailable]` with their raw ciphertext.

Events from senders that encrypt their relations under MSC4392 recover without reply or reaction links because the Matrix specification requires msgvault to ignore relations inside encrypted content.

Sync also recovers earlier placeholders in quiet or left rooms that pass the room filters. A backup change makes pending rooms eligible for another attempt. If an attempt recovers nothing, that room waits an hour before another backup change can trigger a retry. Each incremental sync retries up to five previously attempted rooms, starting with those waiting longest. Newly archived placeholders get an immediate attempt, and `--full` forces retries.

Recovery runs after joined-room history finishes. The pending count includes all remaining placeholders in the selected rooms, with or without a saved key. The recovered count includes message rows replaced with decrypted content, excluding edits and reactions.

Matrix messages use per-message semantic indexing. Once imported, their text is
available to keyword, semantic, and people workflows enabled for the archive.

## Media behavior and limits

After upgrading, each sync scans up to 10,000 previously archived message rows without media markers, including text-only messages, and saves its progress. Later syncs resume the scan until it completes for the account and configured room scope. Temporary lookup failures and unavailable decryption keys get another scan pass. Invalid or permanently unavailable edits are skipped for that pass. Changing `rooms` or `exclude_rooms` restarts discovery. `sync-matrix --full` restarts discovery and re-fetches complete joined-room history.

By default, eligible media is downloaded for direct chats and groups with at most 20 participants, with a 250 MiB per-file cap. `--no-media` records eligible media as pending without downloading bytes. Pending and failed media excluded by current policy becomes skipped, including during `--no-media` runs and retry backoff. The summary separates media downloaded this sync from outstanding pending and failed media for the selected account and rooms. Each media item is attempted once per run; content-addressed storage avoids duplicate files.

Downloads buffer one file in memory up to the configured per-file cap, 250 MiB by default. Set `max_media_mb` to fit the daemon's available memory.

### Retry timing

- Temporary failures such as 408, 425, 429, 5xx, timeouts and connection errors stay pending. Normal syncs retry after 1, 2, 4, 8, 16, then 24 hours. Later failures keep the 24-hour wait.
- File access denied, missing files, hash mismatches and decryption errors stay failed. Normal syncs retry after one week. An edit that changes the media URL or encryption details starts a fresh attempt.
- `--full` bypasses both waits. An invalid or expired account token stops the sync.

Media downloads use the authenticated endpoint added in [Matrix 1.11](https://matrix.org/docs/spec-guides/authed-media-servers/). Older homeservers without that endpoint return `M_UNRECOGNIZED`. Sync prints one warning and continues archiving messages. Media downloads stop for the rest of that run, and eligible items stay pending.

A homeserver may redirect a media download, for example to object storage. msgvault follows redirects only to HTTPS URLs and never sends the access token with them. A refused redirect marks the item failed. Other Matrix requests do not follow redirects.

Three consecutive transient download failures also stop media downloads for the rest of the run. Remaining items stay pending for a later sync.

msgvault retains archived message data after source redaction or deletion. Later media discovery can still fetch a saved URL when current policy allows it and the homeserver retains the file.

Redacting an edit restores the earlier event's media under the current policy. If that media was already attempted in the same run, the restored attachment stays pending until a later sync. Otherwise, sync downloads it immediately. `--no-media` leaves eligible media pending for a later sync. Restoration requires the homeserver to still have the media.

Encrypted Matrix attachments are decrypted locally after download. Temporary download failures leave pending metadata. Permanent download or decryption failures leave a typed failure marker. The message and raw event remain archived. Changing policy affects future downloads. Use
`msgvault purge-excluded-media` to remove stored bytes that a new policy would
exclude.

## Current limits

- Legacy recovery keys outside Matrix secret storage are unsupported.
- Decryption uses only the account's key backup. Keys shared directly with a device but absent from the backup remain unavailable.
- msgvault checks the decrypted `room_id` and the imported session's ID against the outer event's room and `session_id`. When two events in a room carry the same Megolm session and message index, msgvault recovers the one with the earliest server timestamp and leaves the other as a placeholder. A copy recovered in an earlier sync keeps the index, even after `msgvault gc` purges that message. A homeserver that forges an earlier timestamp can still claim another sender's message this way.
- msgvault attributes each recovered message to the outer event's sender and doesn't authenticate devices or bind sessions to them. It trusts the backup's keys. A hostile homeserver can supply its own session or change the claimed sender. The backup algorithm encrypts sessions to a public key; see the [Matrix key-backup specification](https://spec.matrix.org/latest/client-server-api/#backup-algorithm-mmegolm_backupv1curve25519-aes-sha2).
- Only rooms the account has joined are archived. Invited, knocked, and left
  rooms are not imported.
- Room filters accept stable room IDs, not aliases or display names.
- Historical membership is not reconstructed. The current joined-member list
  is the conversation roster used for participant and media-policy decisions.
- A homeserver or SSO provider that does not expose a login token requires a
  password or app password for the dedicated device.
