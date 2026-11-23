---
last_edited: "2026-10-12"
title: Inline
description: Archive all accessible Inline chats through OAuth MCP or the authenticated Inline CLI, with optional chat filters.
---

Archive all [Inline](https://inline.chat) chats your connected account can
currently read, including hidden chats, archived chats, and accessible child
threads, or restrict capture to specific numeric chat IDs. msgvault stores each
signed-in Inline account as a separate `inline` source with
`message_type = inline` for search and analytics. If you connect through OAuth,
capture stays within the chat contexts approved for that grant.

By default, msgvault connects to Inline's MCP server using browser OAuth. You
can explicitly choose a CLI connection instead, which uses an installed,
authenticated `inline` executable. Both connections archive the same account
and chat identities, and msgvault does not switch between them when a
connection fails.

This connector supports production Inline accounts. Custom servers are outside
this integration, and a CLI connection must use Inline's production API and
realtime endpoints.

## Connect an account

Authorize your account, check the connection, and run your first sync:

```bash
msgvault add-inline
msgvault sync-inline --probe
msgvault sync-inline
```

OAuth opens your browser and completes through a loopback callback. msgvault
requests `messages:read` and `offline_access` and does not request
message-write scopes. The daemon stores access and refresh tokens in private
files on the daemon host, separate from `config.toml`, and sync refreshes
authorization without opening a browser. If authorization is revoked or can no
longer be refreshed, run `add-inline` again.

If you use a configured remote daemon, OAuth consent runs in the browser on the
machine where you invoke `add-inline`. The command hands the credentials to
that daemon, which verifies the account and stores them. You do not need a
browser on the remote host. By contrast, the CLI transport requires a separate
Inline CLI installation and login on the daemon host.

Before registering the source, `add-inline` verifies the authenticated user and
capture scope. msgvault derives the account identifier from that user, for
example `api.inline.chat:user:42`, rather than taking a label supplied by the
caller.

### Limit capture to specific chats

To archive only specific chats instead of every accessible conversation, pass
their numeric IDs:

```bash
msgvault add-inline --chat-id 123 --chat-id 456
```

When a chat-ID filter is set, msgvault captures only the exact configured
chats, and selecting a parent does not implicitly select its children.
Re-running `add-inline` with chat IDs merges them into an existing explicit
filter, or establishes a filter if the account was in all-chat mode. To restore
all-chat capture, re-run `add-inline` without chat IDs, or leave
`[[inline.accounts]].chat_ids` empty or omitted. Removing an ID while keeping
`[[inline.accounts]].chat_ids` nonempty pauses its capture and keeps the
messages already archived.

To change transports, re-run `add-inline` with the same account and the
transport you want. Each invocation defaults to the MCP transport. Repeat
`--transport cli` when adding chats to an account you want to keep on the CLI
transport. Setup normally confirms the signed-in account as this source's "me"
identity; pass `--no-default-identity` to leave that confirmation to you.

Running `--probe` checks the configured connection and read contracts, along
with catalog support in all-chat mode, without importing message bodies or
displaying sampled message text.

### Check all-chat catalog support

All-chat capture requires a complete conversation catalog from the connected
Inline service, including accessible child threads, hidden chats, and archived
chats rather than only the visible sidebar.

Over MCP, msgvault requests `conversations.list` with `includeSubthreads = true`
and `sort = "id"`, following `nextAfterChatId` as `afterChatId` until the
continuation is null. Over the CLI, it runs
`inline chats list --include-subthreads --json --compact`.

Deploy the complete-catalog backend before updating or using the corresponding
MCP server or CLI. An older backend can ignore the request flag, and there is
no response capability marker to detect that mismatch. msgvault rejects
unsupported CLI commands and malformed MCP pagination, but a successful probe
does not prove that an older backend included every child thread, and changes
in source repositories do not establish deployed support. An explicit chat-ID
filter can still work with an older compatible CLI because it does not need
catalog discovery.

### Use the Inline CLI

To use the CLI transport, install and authenticate Inline's CLI on the daemon
host, then select it explicitly:

```bash
inline auth login
msgvault add-inline --transport cli --cli-path inline
msgvault sync-inline --probe
msgvault sync-inline
```

Use `--cli-path` to select the executable the daemon runs, providing an
absolute path if it is outside the daemon's `PATH`. Inline's CLI keeps its own
authentication, and msgvault does not copy or inspect its credentials. Because
each read launches a separate CLI process, keep it signed in to the same
account throughout the run.

The installed executable must support the JSON account, conversation,
message-pagination, and media contracts used by the connector. If the installed
CLI is incompatible, the probe reports it instead of treating incomplete output
as a successful archive.

## Sync and refresh

The first sync walks the history currently readable in every discovered chat,
or in the chats selected by an explicit filter. In all-chat mode, later runs
discover the current catalog again, so newly accessible chats are included.
msgvault saves progress after storing messages, so an interrupted run resumes
where it left off. Limited runs rotate through the current chat selection using
that checkpoint, so arrivals in an earlier chat do not prevent later chats from
being archived. Within a limited run, unfinished older history takes priority
over new arrivals. After that backlog finishes, incremental scans capture newer
messages, and later runs capture new messages without repeating completed
history. In all-chat mode, a later run reads only the chats whose newest message
or title differs from the archive, using the newest message ID that discovery
lists for each chat. A chat filter, or a connection whose catalog does not list
newest message IDs, reads every selected chat on each run.

```bash
# Sync every registered Inline account.
msgvault sync-inline

# Sync one verified account.
msgvault sync-inline 'api.inline.chat:user:42'

# Refresh existing messages, including older edits still served by Inline.
msgvault sync-inline --full

# Archive messages now and defer their media.
msgvault sync-inline --no-media

# Force or skip the normal analytics cache refresh.
msgvault sync-inline --build-cache
msgvault backfill-inline-media --no-build-cache
```

Incremental sync does not continuously revisit older messages for edits or
other changes. To refresh the surviving messages Inline currently serves, run
`--full`. msgvault keeps captured messages after they disappear from Inline and
does not infer deletion from a missing page result. Content deleted before
capture and earlier versions of an edited message cannot be recovered from this
connection.

To sync on a schedule, enable `[inline]` and set its cron schedule. Account
entries hold the optional chat filter and connection choice; see
[Inline configuration](../configuration.md#inline). Restart the daemon after
changing scheduled accounts or chat selections.

## Capture scope

- msgvault archives message text, sender identifiers, timestamps, reply
  references, and available edit timestamps with conversation metadata.
- Replies link to their referenced message when that message is in the archive.
  A reference outside the selected or available history may remain unresolved.
- Attachment metadata and eligible downloaded photos, videos, voice notes, and
  files are stored separately from the searchable message body.
- Each captured message retains a raw JSON projection, either
  `inline_mcp_json` or `inline_cli_json`.
- When reaction snapshots are available, msgvault stores each actor and emoji.
  An explicit empty MCP snapshot clears archived reactions, while a missing or
  null snapshot keeps the previous one. MCP does not provide reaction
  timestamps.

In all-chat mode, msgvault discovers accessible child chats as separate
conversations and keeps available parent references in conversation metadata.
With an explicit filter, select each child chat's ID separately to capture its
messages.

The MCP projection does not expose all fields in Inline's native protocol.
Rich-text entities, forwarding headers, service-message fields, and some thread
metadata may be absent, and older MCP projections may also omit reactions. CLI
JSON can retain additional fields the installed CLI returns, though neither
connection is a complete native-protocol export. An MCP refresh keeps an
existing richer CLI raw snapshot while updating the searchable body and current
normalized metadata from MCP. Historical sender names may also be unavailable,
and stable user IDs keep those participants distinguishable.

## Media downloads

Media downloads follow the shared [media policy](../configuration.md#media-policy).
If a file is skipped by policy or fails to download, msgvault still keeps the
message and attachment metadata. Inline uses Slack's shared defaults, with a
250 MiB per-file cap and a participant cap of 20. Discord uses the same
participant cap with a smaller per-file cap.

Inline's conversation response may not provide the complete effective group
roster. When the participant count is unknown, a positive participant cap skips
group media rather than assuming the group is small. Set
`[inline].media_max_participants = 0` only if you want eligible media from your
captured chats without a participant-count cap.

```bash
msgvault backfill-inline-media
msgvault backfill-inline-media 'api.inline.chat:user:42'
```

Backfill retries eligible pending media and refreshes signed download URLs from
the source. Because downloaded bytes use content-addressed storage, msgvault
reuses any file that is already stored. Expiring URLs are not durable copies of
the media, and a file must still be readable when its download is attempted.

Link-preview images and externally hosted embeds are not fetched automatically.
If media is missing at the source, msgvault keeps your captured message text
and any media already stored in the archive.
