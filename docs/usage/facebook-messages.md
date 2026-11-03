---
last_edited: 2026-11-03
title: Messenger Inbox
description: Archive a Facebook Page's recent inbox messages and live webhook events.
---

Archive a Page's Messenger inbox beside your other communications and link
its participants to existing people. This integration is unreleased.
Backfill covers **at most the 20 most recent messages per thread**. Run the
Meta webhook receiver for new messages and events between backfills.

## Register and backfill a Page

Use a Page access token issued to a person with the Page's `MESSAGING` or
`MODERATE` task. It needs `pages_messaging`, `pages_manage_metadata`, and
`pages_read_engagement`. The app and token must have access to the selected
Page; configuring an app ID does not grant access. Keep the token in the named
environment variable on the daemon host.

Add a [`[[facebook_messages]]` profile](../configuration.md#facebook-messages),
then restart the daemon with that environment:

```toml
[[facebook_messages]]
identifier = "example-inbox"
app_id = "9"
page_id = "100"
token_env = "EXAMPLE_PAGE_TOKEN"
enabled = true
schedule = "17 */6 * * *"
actions = []
```

```bash
msgvault source facebook-messages add example-inbox
msgvault source facebook-messages sync example-inbox --max-requests 1000
```

Registration verifies the Page before creating the source. It never enumerates
other Pages, subscribes an app, or changes conversation routing. Manual commands
also work when `enabled = false`; that setting disables scheduled capture and
webhook routing for the profile.

## Resume and search

Each sync has a default budget of 1,000 reads, including Page verification.
A budget pause saves the remaining threads and message IDs for the next run.
Accepted partial writes remain searchable even if a later read fails. Use
`--full` to restart with fresh cursors after a continuation error, preserving
archived history. Missing or inaccessible messages never erase stored history.
The command prints `threads`, `messages_added`, `requests`, `partial`, and the
20-message coverage note. See the [CLI reference](../cli-reference.md#source-facebook-messages-sync).

Each customer thread is a `direct_chat` Conversation in source type
`facebook_messages`. Participant identities include the app, Page, and
Page-scoped ID (PSID). Matching names or digits never link people automatically.
Use the existing [person linking tools](people.md) to link a participant by
hand. Download Your Information archives (`fbmessenger`) remain separate:
those exports lack PSIDs, so messages are not deduplicated against them.

Attachments are metadata only: kinds and available sticker IDs, share counts,
and story references. Media is not downloaded. Structured metadata and search
text omit temporary media URLs; raw provider records may retain them.
Conversations in the Requests folder that have been inactive for 30 days are
not returned by Meta, and the archive cannot recover older messages beyond
the backfill cap.

## Add live events

Configure the [Meta webhook receiver](meta-webhooks.md) and the app's permissions separately.
Request the Page fields
`messages,message_echoes,message_reactions,message_edits,standby` through the
receiver's subscription workflow. Only enable live use after access and
routing have been reviewed. Registration itself does not subscribe anything.

The subscription command accepts the inbox profile's identifier. If that
identifier also names another source, qualify it as
`facebook_messages:example-inbox`:

```sh
msgvault meta-webhooks subscribe facebook_messages:example-inbox --fields messages,message_echoes,message_reactions,message_edits,standby
```

The handler archives inbound messages, Page echoes, reactions, and Messenger
edits. Higher `num_edit` values update the archived text; older edits and a
later backfill preserve the edit. `standby` deliveries can be archived while
another app is the primary receiver; msgvault never requests thread control.
Messages and inline replies use their provider IDs, including replies whose
parent arrives later.

Deliveries received before registration remain held as `unrouted`. After
registering and enabling the profile, run the receiver's
`meta-webhooks replay --state held` command. Unsupported shapes remain held as
`unhandled`. A missing reaction or edit target retries within the receiver's
limits; a busy source retries without counting an attempt. The receiver's spool
contains private message data and is separate from the archive backup.

## Provider behavior awaiting live verification

Fixture tests exercise the handler and importer. The following assumptions
still need an owner-run check before treating live coverage as proven:

| Question | Current behavior |
|---|---|
| Does webhook `mid` equal the backfill message `id`? (U1) | Equal IDs reuse one row; different IDs would create duplicates |
| Does nested message expansion work? (U2) | Backfill uses documented message-ID reads followed by individual node reads |
| Which Messenger referral fields arrive? (U6) | Raw referral data is retained; a string `ad_id` is extracted when present |
| Is the 20-message limit identical on both platforms? (U7) | Both backfills enforce the same cap |
| Which events reach a secondary app on `standby`? (U8) | The same messaging decoder is used; malformed items are held as invalid |
| Are conversation lists ordered by update time? (U9) | The importer pages the entire list and skips unchanged thread reads |
| Is a PSID shared across apps on one Page? (U10) | App-scoped keys keep participants separate until explicitly linked |

For linked professional Instagram inboxes, see [Instagram inbox](instagram-messages.md).
