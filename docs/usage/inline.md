---
last_edited: "2026-10-02"
title: Inline
description: Archive all accessible Inline chats through OAuth MCP or the authenticated Inline CLI, with optional chat filters.
---

Archive all [Inline](https://inline.chat) chats your connected account can
currently read, including hidden and archived chats and accessible child
threads. You can restrict capture to specific numeric chat IDs instead.
msgvault stores each signed-in Inline account as a separate `inline` source,
with `message_type = inline` for search and analytics.
OAuth capture remains within the chat contexts approved for that grant.

The default connection uses Inline's MCP server with browser OAuth. An explicit
CLI connection uses an installed, authenticated `inline` executable instead.
Both connections archive the same account and chat identities. msgvault does
not switch between them when a connection fails.

The connector supports production Inline accounts. Custom servers are outside
this integration; a CLI connection must use Inline's production API and
realtime endpoints.

## Connect an account

Authorize the account, check the connection, then sync:

```bash
msgvault add-inline
msgvault sync-inline --probe
msgvault sync-inline
```

OAuth opens a browser and returns through a loopback callback. The connection
requests `messages:read` and `offline_access`; it does not request message-write
scopes. Access and refresh tokens are private files on the daemon host, separate
from `config.toml`. Sync refreshes the authorization without opening a browser.
Re-run `add-inline` if authorization is revoked or can no longer be refreshed.

With a configured remote daemon, OAuth consent runs in the browser on the
machine invoking `add-inline`. The command hands the resulting credentials to
that daemon, which verifies the account and stores them. You do not need a
browser on the remote host. The CLI transport instead requires a separate
Inline CLI installation and login on the daemon host.

`add-inline` verifies the authenticated user and capture scope before
registering the source. The account identifier is derived from that user, for
example `api.inline.chat:user:42`. It is not a label supplied by the caller.

### Limit capture to specific chats

```bash
msgvault add-inline --chat-id 123 --chat-id 456
```

With a chat-ID filter, only the exact configured chats are captured. Selecting
a parent does not implicitly select its children. Re-running with IDs merges them into
an existing explicit filter; supplying IDs to an account in all-chat mode
establishes a filter. Re-run `add-inline` without chat IDs to restore all-chat
capture. Removing an ID while keeping `[[inline.accounts]].chat_ids` nonempty
pauses its capture and keeps the messages already archived. An empty or omitted
filter restores all accessible chats.

To change transports, re-run `add-inline` with the same account and the desired
transport. Each invocation defaults to the MCP transport. Repeat
`--transport cli` when adding chats to an account you want to keep on the CLI
transport. Setup normally confirms the signed-in account as this source's
"me" identity; `--no-default-identity` leaves that confirmation to you.

`--probe` checks the configured connection and read contracts, plus catalog
support in all-chat mode, without importing message bodies or displaying
sampled message text.

### Check all-chat catalog support

All-chat capture requires a complete conversation catalog from the
connected Inline service. It must include accessible child threads, hidden
chats, and archived chats rather than only the visible sidebar.

The MCP connection requires `conversations.list` with `includeSubthreads = true`,
`sort = "id"`, and a `subthreadsIncluded = true` coverage marker. It follows
`nextAfterChatId` as `afterChatId` until the continuation is null.
The CLI connection requires an updated `inline chats list --include-subthreads`
and backend with the corresponding `subthreads_included` marker. msgvault
checks that contract and fails with an update instruction when it is missing;
it does not treat a partial catalog as a complete archive.

The catalog must be available on the backend and CLI you actually use. Changes
in their source repositories do not establish deployed support. Run the probe
before the first all-chat sync. An explicit chat-ID filter can work with an
older compatible CLI because it does not need catalog discovery.

### Use the Inline CLI

Install and authenticate Inline's CLI on the daemon host, then choose it
explicitly:

```bash
inline auth login
msgvault add-inline --transport cli --cli-path inline
msgvault sync-inline --probe
msgvault sync-inline
```

`--cli-path` selects the executable the daemon runs; use an absolute path if it
is outside the daemon's `PATH`. Inline's CLI keeps its own authentication.
msgvault does not copy or inspect the CLI's credentials.
Each read launches a separate CLI process, so keep it signed in to the same
account throughout the run.

The executable must support the JSON account, conversation, message-pagination,
and media contracts used by the connector. The probe reports an incompatible
installed CLI instead of treating incomplete output as a successful archive.

## Sync and refresh

The first sync walks the history currently readable in every discovered chat,
or in the chats selected by an explicit filter. All-chat mode discovers the
current catalog on later runs, so newly accessible chats are included.
Progress is saved after messages are stored, so an interrupted run resumes.
Later runs capture new messages without repeating the completed history.

```bash
# Sync every registered Inline account.
msgvault sync-inline

# Sync one verified account.
msgvault sync-inline 'api.inline.chat:user:42'

# Refresh existing messages, including older edits still served by Inline.
msgvault sync-inline --full

# Archive messages now and defer their media.
msgvault sync-inline --no-media
```

Incremental sync does not continuously revisit older messages for edits or
other changes. Run `--full` to refresh the surviving messages Inline currently
serves. msgvault retains captured messages after they disappear from Inline;
it does not infer deletion from a missing page result. Content deleted before
capture and earlier versions of an edited message cannot be recovered from
this connection.

For scheduled sync, enable `[inline]` and set its cron schedule. The account
entries contain the optional chat filter and connection choice; see
[Inline configuration](../configuration.md#inline). Restart the daemon after
changing scheduled accounts or chat selections.

## Capture scope

- Message text, sender identifiers, timestamps, reply references, and available
  edit timestamps are archived with conversation metadata.
- Replies link to their referenced message when that message is in the archive.
  A reference outside the selected or available history may remain unresolved.
- Attachment metadata and eligible downloaded photos, videos, voice notes, and
  files are stored separately from the searchable message body.
- Each captured message retains a raw JSON projection:
  `inline_mcp_json` or `inline_cli_json`.

All-chat mode discovers accessible child chats as separate conversations;
available parent references remain conversation metadata. With an explicit
filter, select each child chat's ID separately to capture its messages.

The MCP projection does not expose all fields in Inline's native protocol.
Reactions, rich-text entities, forwarding headers, service-message fields, and
some thread metadata may be absent. CLI JSON can retain additional fields the
installed CLI returns, but neither connection is a complete native-protocol
export. An MCP refresh keeps an existing richer CLI raw snapshot while updating
the searchable body and current normalized metadata from MCP. Historical
sender names may also be unavailable; stable user IDs keep
those participants distinguishable.

## Media downloads

Media follows the shared [media policy](../configuration.md#media-policy).
A policy skip or failed download keeps the message and attachment metadata.
The default per-file cap is 250 MiB and the participant cap is 20.

Inline's conversation response may not provide the complete effective group
roster. When the participant count is unknown, a positive participant cap
skips group media rather than assuming the group is small. Set
`[inline].media_max_participants = 0` only if you want eligible media from your
captured chats without a participant-count cap.

```bash
msgvault backfill-inline-media
msgvault backfill-inline-media 'api.inline.chat:user:42'
```

Backfill retries eligible pending media and refreshes signed download URLs
from the source. Downloaded bytes use content-addressed storage, so an already
stored file is reused. Expiring URLs are not durable copies of the media; a
file must still be readable when its download is attempted.

Link-preview images and externally hosted embeds are not fetched
automatically. Missing media at the source does not remove captured message
text or media already stored in the archive.
