---
last_edited: "2026-10-27"
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

# Sync one registered Matrix user.
msgvault sync-matrix --account @archive:example.org
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
- Edits from the original sender, redactions, reactions, and reply
  relationships.
- Current joined-room membership as conversation participants. The account's
  `m.direct` map distinguishes direct chats from groups.
- The original Matrix event JSON (`raw_format = matrix_json`).

Events whose room keys are available decrypt through the account's key backup. The archived JSON contains the decrypted event and preserves its encrypted source event under `org.msgvault.encrypted_event`. Other encrypted events stay as `[encrypted message — keys unavailable]` with their raw ciphertext.

Events from senders that encrypt their relations under MSC4392 recover without reply or reaction links because the Matrix specification requires msgvault to ignore relations inside encrypted content.

Sync also recovers earlier placeholders in quiet or left rooms that pass the room filters. A backup change makes pending rooms eligible for another attempt. If an attempt recovers nothing, that room waits an hour before another backup change can trigger a retry. Each incremental sync retries up to five previously attempted rooms, starting with those waiting longest. Newly archived placeholders get an immediate attempt, and `--full` forces retries.

Edits waiting for an encrypted original are accepted only from that original's outer sender. Each original keeps at most 32 pending edits, retaining the newest by timestamp and then event ID. Older edits evicted from this queue are not replayed automatically.

Recovery runs after joined-room history finishes. The pending count includes all remaining placeholders in the selected rooms, with or without a saved key. The recovered count includes message rows replaced with decrypted content, excluding edits and reactions.

Matrix messages use per-message semantic indexing. Once imported, their text is
available to keyword, semantic, and people workflows enabled for the archive.

## Current limits

- Legacy recovery keys outside Matrix secret storage are unsupported.
- Decryption uses only the account's key backup. Keys shared directly with a device but absent from the backup remain unavailable. Media download isn't included.
- msgvault checks the decrypted `room_id` and the imported session's ID against the outer event's room and `session_id`. A Megolm session and message index can belong to only one recovered event in a room. If a recovered message has a competing earlier event, the earliest `origin_server_ts` wins, with event ID breaking ties. The losing message returns to its encrypted placeholder and records the winner in `matrix_replay_conflict` metadata. This rule also applies when the earlier event arrives in a later sync. Recovered edits and reactions keep their first claim, so a copied edit or reaction can still take precedence over its original.
- msgvault attributes each recovered message to the outer event's sender and doesn't authenticate devices or bind sessions to them. It trusts the backup's keys. A hostile homeserver can supply its own session or change the claimed sender. The sender's homeserver sets `origin_server_ts`, so a room member on a homeserver they control can backdate a copied ciphertext and win the replay rule. The backup algorithm does not authenticate who created a session entry: anyone with the public key can construct one, and a hostile homeserver can insert it into the backup. See the [Matrix key-backup specification](https://spec.matrix.org/latest/client-server-api/#backup-algorithm-mmegolm_backupv1curve25519-aes-sha2).
- `--recovery-passphrase` uses the server's PBKDF2 iteration count without a local upper bound. A hostile or misconfigured server can make key derivation consume excessive CPU; use a recovery key to avoid passphrase derivation.
- Only rooms the account has joined are archived. Invited, knocked, and left
  rooms are not imported.
- Room filters accept stable room IDs, not aliases or display names.
- Historical membership is not reconstructed. The current joined-member list
  is used for participants.
- A homeserver or SSO provider that does not expose a login token requires a
  password or app password for the dedicated device.
