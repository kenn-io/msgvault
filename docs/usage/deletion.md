---
title: Deleting Email
description: Staging messages for deletion, reviewing manifests, and executing deletes from Gmail or IMAP.
---


msgvault supports a staged deletion workflow: select messages in the TUI or stage them via an AI assistant, review what will be deleted, then execute against Gmail or your IMAP provider. **Deletion only removes messages from the remote mail server. Your local archive is never modified.** This means you can always search, browse, and export deleted messages from your local copy.

## Bulk Deletion via Aggregate Groups

The fastest way to clean up your inbox is through the TUI's aggregate views. Navigate to the Senders, Domains, or Labels view, find the group you want to remove (e.g., a prolific spam sender or an unwanted mailing list), and press `D` to stage every message in that group for deletion at once.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-deletion.svg" alt="msgvault TUI deletion confirmation dialog showing bulk staging of all messages from a sender" loading="lazy">
</figure>
A confirmation dialog shows exactly how many messages will be staged. Nothing is deleted until you explicitly run `msgvault delete-staged`.

## Selective Deletion

For finer control, drill into any group and use `Space` to select individual rows, then press `d` to stage only the selected messages.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-selection.svg" alt="msgvault TUI with rows selected for deletion staging" loading="lazy">
</figure>
## Staging via MCP (AI-Assisted)

You can also stage deletions through the [MCP server](/usage/chat/) by asking an AI assistant like Claude to find and stage messages for you. For example:

- *"Stage all messages from noreply@linkedin.com for deletion"*
- *"Stage all promotional emails older than 2024-01-01"*

The MCP `stage_deletion` tool creates a manifest through the selected daemon, the same format as TUI-staged deletions. Nothing is deleted until you run `msgvault delete-staged` from the CLI. See [MCP Server](/usage/chat/#staged-deletion-via-mcp) for details.

## Staging via HTTP API

Web dashboards and automation scripts can stage deletion manifests through the
[web API](/api-server/#post-apiv1deletions) without constructing a manifest
themselves. `POST /api/v1/deletions` accepts structured filters and/or
internal message IDs, resolves the Gmail IDs on the server, and supports
`"dry_run": true` to preview the count and a sample before writing anything.

The API can also list and cancel staged manifests:

```bash
curl -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  http://localhost:8080/api/v1/deletions

curl -X DELETE -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  http://localhost:8080/api/v1/deletions/20260706-153000-old-newsletters-a1b2
```

API staging still only creates or cancels local manifests. Remote deletion
continues to require the explicit `delete-staged` execution step and the
`MSGVAULT_ENABLE_REMOTE_DELETE=1` guard.

## Staging Steps

**From the TUI:**

1. Launch the TUI: `msgvault tui`
2. Navigate to the view and group you want to clean up
3. Press `D` to stage all messages in the current group, or use `Space` to select individual rows and press `d`
4. Review the confirmation dialog and confirm

**From an AI assistant:**

1. Ask the assistant to stage messages matching your criteria
2. The assistant calls `stage_deletion`, which creates a pending manifest
3. Review with `msgvault delete-staged --list` or `msgvault show-deletion <batch-id>`

**From the HTTP API:**

1. Send a dry-run `POST /api/v1/deletions` request to confirm the selection
2. Send the same request without `dry_run` to create a pending manifest
3. Review with `GET /api/v1/deletions` or the CLI commands above

## Selection Keys

| Key | Action |
|---|---|
| `Space` | Toggle selection on current row |
| `d` | Stage selected for deletion |
| `D` | Stage all matching current filter |

## Reviewing and Executing

Staged deletions create manifests that record exactly which messages will be affected. Review before executing:

```bash
# List pending deletion batches
msgvault delete-staged --list

# Execute deletion for a Gmail account (moves to trash by default)
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --account you@gmail.com

# Execute deletion for an IMAP account (permanent)
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --account you@fastmail.com

# Permanently delete Gmail messages instead of moving them to trash
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --account you@gmail.com --permanent
```

Gmail deletion moves messages to trash by default, which is recoverable for 30 days in Gmail. Use `--permanent` for Gmail batch-API permanent deletion. For IMAP accounts, msgvault uses `UID STORE \Deleted` followed by `UID EXPUNGE` to permanently remove messages. On large batches it falls back to per-message deletion. Dry-run mode works for both Gmail and IMAP accounts.

## Enabling Remote Deletion

Remote deletion is gated behind an environment variable so it can never run by accident. Set `MSGVAULT_ENABLE_REMOTE_DELETE=1` on the command that executes deletions:

```bash
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --account you@gmail.com
```

Read-only modes (`msgvault delete-staged --list` and `--dry-run`) work without the gate, so you can always review exactly what is staged before you enable execution.

## Permission Upgrade

When you first run `delete-staged --permanent`, your OAuth token likely only has read and modify permissions. Permanent Gmail batch deletion requires full Gmail access (the `mail.google.com` scope). msgvault detects this automatically and prompts you to upgrade:

```
======================================================================
PERMISSION UPGRADE REQUIRED
======================================================================

Batch deletion requires elevated Gmail permissions.

Your current OAuth token was granted with limited permissions that
don't include batch delete. To proceed, msgvault needs to:

  1. Delete your existing OAuth token
  2. Re-authorize with full Gmail access (mail.google.com scope)

This elevated permission allows msgvault to permanently delete
messages in bulk. You can revoke access anytime at:
  https://myaccount.google.com/permissions

Upgrade permissions now? [y/N]:
```

Answering `y` opens your browser for re-authorization. Answering `N` cancels cleanly with no side effects. You can revoke the elevated permission at any time from your [Google Account permissions page](https://myaccount.google.com/permissions).

If you have a legacy token from before scope tracking was added, msgvault falls back to detecting the insufficient scope from the API response and shows the same prompt.

Gmail trash deletion does not require this elevated scope. IMAP accounts do not require a permission upgrade. IMAP credentials already have full mailbox access, so `delete-staged` works without re-authorization.

## Resumable Execution

Deletions are resumable. If execution is interrupted (network error, Ctrl+C, scope error), run the same command again to pick up where it left off. Failed messages are tracked individually and retried automatically on the next run before continuing with remaining messages.

## Cancelling Deletions

Cancel a specific batch or all pending batches:

```bash
# List available batches
msgvault cancel-deletion

# Cancel a specific batch
msgvault cancel-deletion <batch-id>

# Cancel all pending batches
msgvault cancel-deletion --all
```

!!! danger
    Permanent deletion cannot be undone on the remote mail server (Gmail or IMAP provider). Always verify your local archive is complete before permanent deletion. Use `msgvault verify` to check integrity. Your local copy is always preserved regardless of what you delete remotely.

## Safety Notes

- Sync operations are always read-only
- Deletion only affects the remote mail server (Gmail or IMAP provider), never your local archive
- Deletion requires explicit staging and execution as separate steps
- Manifests provide an audit trail of what was deleted
- Review staged manifests carefully before executing, as permanent deletion cannot be undone
