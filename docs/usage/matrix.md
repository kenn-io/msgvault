---
last_edited: "2026-11-04"
title: Matrix
description: Archive Matrix room history with keys from your account's backup.
---

Archive messages from joined rooms on any Matrix homeserver with a dedicated
msgvault device. When your account's key backup holds the room keys, encrypted
history becomes searchable too.

Imported messages use `message_type = matrix`. Sync never sends messages, read
receipts, typing notifications, or presence updates; logging in the device is
the only change made to your account.

## Add a Matrix account

Save your password in a file readable only by your user, then register the
account:

```bash
chmod 600 /path/to/matrix-password
msgvault add-matrix \
  --homeserver https://matrix.example.org \
  --user-id @archive:example.org \
  --password-file /path/to/matrix-password \
  --recovery-file /path/to/matrix-recovery-key
```

`add-matrix` creates a separate Matrix device named **msgvault (read-only)**
and stores its access token and unlocked backup key in an owner-only file under
`tokens/`. Anyone holding that file can read this account's backed-up room
keys. `remove-account` deletes the file, and `backup create --include-tokens`
captures it. msgvault never saves your recovery key or passphrase.

If you use a passphrase file instead of a recovery key, pass
`--recovery-passphrase` with `--recovery-file`. Without a file, an interactive
run prompts for a recovery key, or for a passphrase when you pass
`--recovery-passphrase`. During a renewal, you can leave the prompt blank to
keep the saved backup key. First setup requires a recovery secret or
`--skip-key-backup`, which clears any saved backup key and keeps encrypted
placeholders.

To renew a revoked or expired login, run `add-matrix` again for the same user.
It saves the new device's token in place, logs out the old device, and keeps
your archived history and sync state. If renewal stops partway, for example on
a full disk, msgvault keeps the new login in a pending file under `tokens/`,
and the next `add-matrix` run for that user finishes with that login instead of
creating another device. That run still reads `--password-file` or
`--login-token-file` without using it, so an already-used SSO token file works
fine.

For an SSO account, complete your homeserver's SSO flow to get a single-use
`m.login.token`, save it to an owner-only file, and use `--login-token-file`
instead of `--password-file`. Whether this token flow is available depends on
your homeserver and identity provider.

## Sync rooms

```bash
# First run backfills joined rooms; later runs use the saved /sync token.
msgvault sync-matrix

# Re-read complete history to pick up anything missed.
msgvault sync-matrix --full

# Sync one registered Matrix user and defer media bytes.
msgvault sync-matrix --account @archive:example.org --no-media
```

On the first run, msgvault takes a Matrix `/sync` snapshot and walks backward
through each joined room with `/messages`. It checkpoints each room and saves
`next_batch` for later incremental `/sync` calls. If a run is interrupted,
re-running skips event IDs already archived and picks up from the saved room
cursor. When an incremental timeline is cut short, msgvault reads `/messages`
back to the previous sync token before committing the new one.

To filter rooms, configure include and exclude lists with exact Matrix room IDs
in the [`[matrix]` settings](../configuration.md#matrix). If a room appears in
both lists, the exclude list wins. After changing either list, run
`msgvault sync-matrix --full` so msgvault discovers newly included quiet rooms
right away.

Every `/sync` request sends `set_presence=offline`. The Matrix client-server
specification defines that value as "the client is not marked as being online
when it uses this API", so syncing leaves your account's presence unchanged
even while another client keeps it online. Omitting the parameter would mark
the account online, and `unavailable` would mark it idle.

## What is archived

- Text, notice, and emote events, with HTML formatted bodies converted to
  searchable plain text.
- Images, stickers, video, audio, and files under the shared chat
  [media policy](../configuration.md#media-policy).
- Edits from the original sender, redactions, reactions, and reply
  relationships.
- Current joined-room membership as conversation participants, using the
  account's `m.direct` map to tell direct chats apart from groups.
- The original Matrix event JSON (`raw_format = matrix_json`).

When room keys are available in your account's key backup, msgvault decrypts
those events and stores the decrypted event in the archived JSON. It keeps the
encrypted source event separately so a replay conflict can return a message to
its placeholder. Other encrypted events stay as
`[encrypted message — keys unavailable]` with their raw ciphertext.

Events from senders that encrypt their relations under MSC4392 recover without
reply or reaction links, because the Matrix specification requires msgvault to
ignore relations inside encrypted content.

Sync also recovers earlier placeholders in quiet or left rooms that pass your
room filters. Newly archived placeholders get an immediate attempt, while a
placeholder whose key was missing gets another attempt after the backup
changes, at most once an hour. If reading a room's backup fails, the next sync
tries again, and `--full` retries every placeholder.

Edits waiting for an encrypted original are accepted only from that original's
outer sender. Each original keeps at most 32 pending edits, retaining the
newest by timestamp and then event ID; older edits evicted from this queue are
not replayed automatically.

Recovery runs after joined-room history finishes. The pending count includes
all remaining placeholders in the selected rooms, with or without a saved key,
while the recovered count includes messages replaced with decrypted content,
excluding edits and reactions.

Matrix messages use per-message semantic indexing. Once imported, their text is
available to any keyword, semantic, and people workflows enabled for your
archive.

## Media behavior and limits

After upgrading, each sync scans up to 10,000 previously archived message rows
without media markers, including text-only messages, and saves its progress.
Later syncs resume the scan until it finishes for the account and configured
room scope. Temporary lookup failures and unavailable decryption keys get
another scan pass, while invalid or permanently unavailable edits are skipped
for that pass. Changing `rooms` or `exclude_rooms` restarts discovery, and
`sync-matrix --full` restarts discovery while re-fetching complete joined-room
history.

By default, msgvault downloads eligible media for direct chats and groups with
at most 20 participants, with a 250 MiB per-file cap. `--no-media` records
eligible media as pending without downloading bytes. Pending and failed media
excluded by your current policy becomes skipped, including during `--no-media`
runs and retry backoff. The summary separates media downloaded this sync from
outstanding pending and failed media for the selected account and rooms. Each
media item is attempted once per run, and content-addressed storage avoids
duplicate files.

Downloads buffer one file in memory up to your configured per-file cap, 250 MiB
by default. Set `max_media_mb` to fit the daemon's available memory.

### Retry timing

- Temporary failures such as 408, 425, 429, 5xx, timeouts, and connection
  errors stay pending. Normal syncs retry after 1, 2, 4, 8, 16, and then 24
  hours, keeping the 24-hour wait for later failures.
- File access denied, missing files, hash mismatches, and decryption errors
  stay failed. Normal syncs retry after one week, and an edit that changes the
  media URL or encryption details starts a fresh attempt.
- `--full` bypasses both waits. An invalid or expired account token stops the
  sync.

Media downloads use the authenticated endpoint added in
[Matrix 1.11](https://matrix.org/docs/spec-guides/authed-media-servers/). Older
homeservers without that endpoint return `M_UNRECOGNIZED`; when that happens,
sync prints one warning, stops media downloads for the rest of the run while
leaving eligible items pending, and continues archiving messages.

A homeserver may redirect a media download, for example to object storage.
msgvault follows redirects only to HTTPS URLs and never sends your access token
with them. A refused redirect, or a redirect target that answers 401, marks
only that item as failed. Other Matrix requests do not follow redirects.

Three consecutive transient download failures also stop media downloads for the
rest of the run, leaving remaining items pending for a later sync.

msgvault retains archived message data after source redaction or deletion.
Later media discovery can still fetch a saved URL when your current policy
allows it and the homeserver still has the file.

Redacting an edit restores the earlier event's media under your current policy,
as long as the homeserver still has the file. If that media was already
attempted in the same run, or if you passed `--no-media`, the restored
attachment stays pending until a later sync; otherwise, sync downloads it right
away.

Encrypted Matrix attachments are decrypted locally after download. Temporary
download failures leave pending metadata, while permanent download or
decryption failures leave a typed failure marker, and the message and raw event
stay archived. Changing policy affects future downloads. To remove stored bytes
that a new policy would exclude, run `msgvault purge-excluded-media`.

## WhatsApp through mautrix-whatsapp

A [mautrix-whatsapp](https://github.com/mautrix/whatsapp) bridge puts each
WhatsApp chat in a Matrix portal room. Archived as ordinary Matrix rooms,
those messages would name senders by Matrix ghost IDs, without phone numbers,
and would never match messages that `import-whatsapp` stores from WhatsApp
Desktop. When the bridge runs on the same host as msgvault, point `[matrix]`
at its database instead:

```toml
[matrix]
whatsapp_bridge_db = "/var/lib/mautrix-whatsapp/mautrix-whatsapp.db"
```

Each Matrix sync then skips the portal rooms and archives them into one
`whatsapp` source per number the account linked to the bridge, the same
source that `import-whatsapp --phone` uses for that number:

- The message ID is the WhatsApp message ID, so a message that both the bridge
  and a WhatsApp Desktop import see is stored once. A row that another
  importer stored first stays as it is.
- Conversations use the Desktop importer's keys (`<digits>@s.whatsapp.net`
  for direct chats, the group JID for groups).
- Senders resolve to phone numbers through the bridge's LID map and carry
  `whatsapp` identifiers. A contact name from the linked phone only fills a
  blank display name.
- Media messages are stored as a label and caption (for example
  `[image] caption`). The bridge import does not download media, so the
  `[matrix]` media settings do not apply to these messages.
- Edits from the original sender replace the body; replies link to the
  message they quote.
- The raw form (`raw_format = matrix_whatsapp_json`) holds the Matrix event,
  the edit that supplied the body, and the WhatsApp chat, sender and message
  IDs.

msgvault opens the bridge database read-only. The first run for a number
imports everything the bridge has; later runs revisit the last seven days, and
`sync-matrix --full` revisits everything. The bridge only knows messages from
the moment a number was linked (or what its history sync delivered), so keep
an earlier WhatsApp Desktop import for older history.

## Current limits

- Legacy recovery keys outside Matrix secret storage are not supported.
- Decryption uses only your account's key backup. Keys shared directly with a
  device that are absent from the backup stay unavailable.
- msgvault checks the decrypted `room_id` and imported session ID against the
  outer event's room and `session_id`, and a Megolm session and message index
  can belong to only one recovered event in a room. If a recovered message has
  a competing earlier event, the earliest `origin_server_ts` wins, with event
  ID breaking ties. The losing message returns to its encrypted placeholder and
  records the winner in `matrix_replay_conflict` metadata. The same rule applies
  when the earlier event arrives in a later sync. When restoring a redacted edit,
  msgvault checks for an earlier archived copy before claiming an unclaimed
  index. Recovered edits and reactions keep their first claim, so a copied edit
  or reaction can still take precedence over its original.
- msgvault attributes each recovered message to the outer event's sender,
  trusts the backup's keys, and does not authenticate devices or bind sessions
  to them. A hostile homeserver can supply its own session or change the
  claimed sender. Because the sender's homeserver sets `origin_server_ts`, a
  room member on a homeserver they control can backdate a copied ciphertext and
  win the replay rule. The backup algorithm does not authenticate who created a
  session entry: anyone with the public key can construct one, and a hostile
  homeserver can insert it into the backup. See the
  [Matrix key-backup specification](https://spec.matrix.org/latest/client-server-api/#backup-algorithm-mmegolm_backupv1curve25519-aes-sha2).
- `--recovery-passphrase` uses the server's PBKDF2 iteration count without a
  local upper bound. A hostile or misconfigured server can make key derivation
  consume excessive CPU; to avoid passphrase derivation, use a recovery key.
- Only rooms the account has joined are archived; invited, knocked, and left
  rooms are not imported.
- Room filters accept stable room IDs, not aliases or display names.
- Historical membership is not reconstructed. msgvault uses the current
  joined-member list as the conversation roster for participant and
  media-policy decisions.
- If your homeserver or SSO provider does not expose a login token, you need a
  password or app password for the dedicated device.
