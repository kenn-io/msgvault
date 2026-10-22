---
last_edited: "2026-10-08"
title: Matrix
description: Archive plaintext history from joined Matrix rooms on any homeserver.
---

Archive plaintext events from joined rooms on any Matrix homeserver with a
dedicated msgvault device.

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
  --password-file /path/to/matrix-password
```

`add-matrix` creates a separate Matrix device named **msgvault (read-only)**.
It stores the access token in an owner-only file under `tokens/`.

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

Encrypted events are retained as `[encrypted message — keys unavailable]`
with their raw ciphertext. They are not silently dropped.

Matrix messages use per-message semantic indexing. Once imported, their text is
available to keyword, semantic, and people workflows enabled for the archive.

## Message notifications

On unreleased `main`, native Matrix sync can publish `msgvault.message_archived` Events for new
readable messages in an exact archived room conversation. Enable Events and
explicitly include `matrix` in the daemon configuration:

```toml
[mcp.events]
enabled = true
sources = ["matrix"]
```

Restart the daemon after changing these settings. Use the archive's numeric
`conversation_id` when subscribing, not a Matrix room ID. Events remain off
by default, and Matrix is not in the default source list. Notifications arrive
when sync archives a message; they are not a direct homeserver push feed.

The first sync, full history refreshes, newly included or rejoined rooms, and
older checkpoints without Events coverage are silent. A completed history
pass establishes coverage for later syncs, including rooms with no messages.
An interrupted pass keeps its original classification. Missing pages from a
covered incremental timeline can publish arrivals; an interrupted page walk
retains that decision when it resumes. Re-reading an archived message does
not publish it again. Edits, reactions, redactions, and reply repairs do not
publish message arrivals.

Before publishing an occurrence, msgvault commits the readable body, raw
event, current room title and membership, and any available reply link
together. A reply whose parent is unavailable can still publish its own
arrival. Its delivery receipt grants access to that message, not its parent
or another room. Use receipt-bound `get_message` pagination to read the
body. The [MCP Events guide](chat.md#events) owns authentication, signed
callbacks, receipt expiry, and subscription renewal.

Own messages are excluded unless the subscription sets `include_from_me`.
Encrypted placeholders remain archived but never publish a readable-message
notification. These Events do not promise decryption, downloaded media,
transcripts, reactions, drafts, source-wide subscriptions, or sending.

## Current limits

- End-to-end encrypted event decryption and media download are not included.
- Only rooms the account has joined are archived. Invited, knocked, and left
  rooms are not imported.
- Room filters accept stable room IDs, not aliases or display names.
- Historical membership is not reconstructed. The current joined-member list
  is used for participants.
- A homeserver or SSO provider that does not expose a login token requires a
  password or app password for the dedicated device.
