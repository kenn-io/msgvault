---
last_edited: "2026-10-02"
title: Matrix
description: Archive joined Matrix rooms, including encrypted history, from any homeserver.
---

Archive joined rooms from any Matrix homeserver with a dedicated msgvault
device. The native source works with accounts used in Element and restores
server-side room-key backups for encrypted history.

All imported messages use `message_type = matrix`. Sync never sends messages,
read receipts, typing notifications, or presence updates. Device login and
encryption setup are the only account mutations. Sync uploads the device and
one-time encryption keys that other devices need to share room keys with the
archive device. It never forwards room keys and ignores incoming key-share
requests without responding.

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
It stores the access token and a randomly generated local encryption key in an
owner-only file under `tokens/`. The recovery key is used once to restore the
homeserver's key backup. It is not written to msgvault configuration or
credentials.

`add-matrix` keeps the backup decryption key that the recovery key unlocks. It
is stored in the device's local crypto store, encrypted with the per-account
key from the credential file. Each sync uses it to fetch room keys that the
account's other devices have since added to the backup, for events still shown
as encrypted placeholders. The trade-off is that anyone who can read both the
credential file and the crypto store can decrypt the account's key backup, so
protect them, and msgvault [backups](backup.md) that include them, like the
recovery key. `remove-account` deletes both. If the account's key backup is
later replaced with a new key, sync reports that the cached key cannot read it
and leaves those events as placeholders.

Use `--recovery-passphrase` when the recovery file contains a passphrase
instead of a recovery key. If the account has no server-side key backup, use
`--skip-key-backup`; old encrypted events without locally available keys remain
searchable placeholders until a later sync can decrypt them. When it does,
msgvault asks the homeserver for the message's latest edit.

To renew a revoked or expired login, run `add-matrix` again for the same user.
It saves the new device's token in place, logs out the old device, and keeps
the archived history and sync state. The new device starts with its own
encryption store and restores the server-side key backup again, so room keys
that only the old device held are not used for later decryption.

For an SSO account, complete the homeserver's SSO flow to obtain a single-use
`m.login.token`, save it to an owner-only file, and use
`--login-token-file` instead of `--password-file`. Availability of this token
flow depends on the homeserver and identity provider.

### Device trust

The dedicated msgvault device remains unverified. Element may show it with an
unverified-device marker. Do not start Element's interactive verification flow:
msgvault does not implement the SAS or QR exchange needed to complete it.

Historical and later messages are decrypted from the server-side key backup,
which the account's other clients keep updating. Senders who refuse to share
keys with unverified devices can still leave events as encrypted placeholders.

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
- Edits, redactions, reactions, and reply relationships.
- Current joined-room membership as conversation participants. The account's
  `m.direct` map distinguishes direct chats from groups.
- The original Matrix event JSON (`raw_format = matrix_json`) for later
  decryption and repair. For a decrypted event, the message keeps the
  decrypted event JSON and msgvault also retains the original
  `m.room.encrypted` event as delivered by the homeserver.

Encrypted events whose room key is unavailable are retained as
`[encrypted message — keys unavailable]`. Every later sync retries those raw
events after processing newly received keys. They are never silently dropped.

Matrix messages use per-message semantic indexing. Once imported, their text is
available to keyword, semantic, and people workflows enabled for the archive.

## Current limits

- Matrix attachment download is not included.
- Only rooms the account has joined are archived. Invited, knocked, and left
  rooms are not imported.
- Room filters accept stable room IDs, not aliases or display names.
- Historical membership is not reconstructed. The current joined-member list
  is the conversation roster used for participants.
- A homeserver or SSO provider that does not expose a login token requires a
  password or app password for the dedicated device.
