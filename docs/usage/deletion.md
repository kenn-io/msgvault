---
last_edited: "2026-09-08"
title: Deleting Email
description: Staging messages for deletion, reviewing manifests, and executing deletes from Gmail or IMAP.
---


Remove unwanted mail from Gmail or IMAP while keeping the archived message,
raw content, and downloaded attachments. Deletion has three separate steps:

1. **Stage** a precise set of messages in a pending manifest.
2. **Review** that manifest and check the archive.
3. **Execute** with `delete-staged` and explicit remote-deletion consent.

Executing a batch records which messages were deleted at the source. It does
not purge their archived content. You can still open and export those messages:

```bash
msgvault search "old receipt" --deletion-scope deleted
msgvault search "old receipt" --deletion-scope any
```

`deleted` selects source-deleted messages; `any` includes active and
source-deleted messages together. Removing an account or purging local data is
a different operation and can remove archive content.

## Stage from the CLI

Preview matching mail before creating a batch:

```bash
msgvault stage-delete 'from:newsletter@example.com before:2024-01-01' --dry-run
msgvault stage-delete 'from:newsletter@example.com before:2024-01-01'
msgvault show-deletion BATCH_ID
```

Query staging uses the same search language as `msgvault search` and considers
active messages only. It reports and skips matches that this staging path
cannot delete, such as chats, meetings, and non-Gmail mail. The remaining
Gmail targets must belong to one source. Use `--source-id` to narrow a query
when several Gmail accounts match:

```bash
msgvault stage-delete 'label:Newsletters' --source-id 3 --dry-run
```

If you already have internal message IDs, stage them directly:

```bash
msgvault stage-delete --ids 123,456,789 --dry-run
msgvault stage-delete --ids 123,456,789
```

`--ids` accepts positive internal message IDs, not Gmail provider IDs. It cannot
be combined with a query or `--source-id`. Missing, source-deleted, and
unsupported targets are skipped and reported. The explicit-ID path works
without a ready analytical or full-text cache; query staging waits for a
complete search index and requires daemon API schema `2.18.0` or newer.

These CLI staging paths currently resolve Gmail targets. IMAP deletion uses
manifests staged through the TUI. Creating a manifest never executes it.

## Staging in the Web UI

In Everything, select individual rows or all rows matching the current
canonical filter, then press `d` or `D` to open the Deletions workspace. The UI
runs a server-side preflight before it enables staging and requires a separate
confirmation to create the manifest. The Deletions workspace also lists,
inspects, and cancels staged manifests. It never executes remote deletion;
that final step remains the explicit CLI command described below.

Preflight validates the selection itself (that it still matches, that the
cache/search revision hasn't moved) but does not check which accounts the
matched messages belong to. Each deletion manifest executes against exactly
one mailbox, so if you stage "all matching" across an unfiltered or
multi-account view, confirming the stage fails with a `multi_account_selection`
error even though preflight succeeded. Filter to one source/account
(the `A` key in the TUI, or the equivalent source filter in the Web UI) before
staging an all-matching selection that could span more than one account.

## Bulk Deletion via Aggregate Groups

The fastest way to clean up your inbox is through the TUI's aggregate views. Navigate to the Senders, Domains, or Labels view, find the group you want to remove (e.g., a prolific spam sender or an unwanted mailing list), and press `D` to stage every message in that group for deletion at once.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-deletion.svg" alt="msgvault TUI deletion confirmation dialog showing bulk staging of all messages from a sender" loading="lazy">
</figure>
A confirmation dialog shows exactly how many messages will be staged. Nothing is deleted until you explicitly run `msgvault delete-staged`.

## Selective Deletion

For finer control, drill into any group and use `Space` to select individual rows, then press `d` to stage only the selected messages.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-selection.svg" alt="msgvault TUI with rows selected for deletion staging" loading="lazy">
</figure>
## Staging via MCP (AI-Assisted)

You can also stage deletions through the [MCP server](/docs/usage/chat/) by asking an AI assistant like Claude to find and stage messages for you. For example:

- *"Stage all messages from noreply@linkedin.com for deletion"*
- *"Stage all promotional emails older than 2024-01-01"*

The MCP `stage_deletion` tool creates a manifest through the selected daemon, the same format as TUI-staged deletions. Nothing is deleted until you run `msgvault delete-staged` from the CLI. See [MCP Server](/docs/usage/chat/#staged-deletion-via-mcp) for details.

## Staging via HTTP API

Web dashboards and automation scripts can stage deletion manifests through the
[web API](/docs/api-server/#post-apiv1deletions) without constructing a manifest
themselves. `POST /api/v1/deletions` accepts structured filters and/or
internal message IDs, resolves the Gmail IDs on the server, and supports
`"dry_run": true` to preview the count and a sample before writing anything.
IDs that do not resolve to live deletable Gmail messages with provider message
IDs are omitted, and the
reported count is the number of targets resolved by the daemon.

Actually staging depends on the request shape. An explicit `message_ids` list
stages directly. Filter-based staging requires a preflighted selection: run
the reviewed predicate through
[`POST /api/v1/explore/preflight`](/docs/api-server/#post-apiv1explorepreflight)
to get a single-use `operation_token` (valid for five minutes), then stage
with the same selection and token. A non-dry-run filter request without a
preflighted selection is rejected with `428 preflight_required`.

The API can also list and cancel staged manifests:

```bash
curl -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  http://localhost:8080/api/v1/deletions

curl -X DELETE -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  http://localhost:8080/api/v1/deletions/20260706-153000-old-newsletters-a1b2
```

API staging still only creates or cancels local manifests. Remote deletion
continues to require the explicit `delete-staged` execution step and invoking
CLI consent, configured durably or supplied for one command as described
below.

## Reviewing and Executing

Staged deletions create manifests that record exactly which messages will be affected. Review before executing:

```bash
# List pending deletion batches
msgvault delete-staged --list

# Execute deletion for a Gmail account (moves to trash by default)
msgvault delete-staged --account you@gmail.com

# Move staged IMAP messages to the server's Trash folder
msgvault delete-staged --account you@fastmail.com

# Permanently delete Gmail messages instead of moving them to trash
msgvault delete-staged --account you@gmail.com --permanent
```

Gmail and IMAP both default to moving messages to Trash. Gmail retains trash
for 30 days; IMAP recovery and retention depend on the server. IMAP uses the
server's discovered Trash folder, falling back to a folder named `Trash`.
If that move fails, the batch records the failure rather than switching to
permanent deletion.

`--permanent` requests irreversible deletion: Gmail uses its batch API; IMAP
uses `UID STORE \Deleted` followed by `UID EXPUNGE` for each message. Permanent
IMAP deletion requires UIDPLUS and is refused when the server lacks it.
Dry-run mode works for both providers.

## Enabling Remote Deletion

Remote deletion is opt-in. Enable it in the invoking CLI's configuration, or
provide consent for one command. Staging and review do not require this
setting.

Durable consent belongs in the invoking CLI's `config.toml`:

```toml
[deletion]
remote_enabled = true
```

As a one-command alternative:

```bash
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --account you@gmail.com
```

When remote mode is configured, the invoking CLI forwards its effective
consent for that operation. The remote daemon's own `[deletion]` section is
not server policy for a command invoked elsewhere. Staging, listing,
inspecting, and dry-running deletion batches remain ungated, so you can always
review exactly what is staged before enabling execution.

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
    Permanent deletion cannot be undone on the remote mail server (Gmail or IMAP provider). Always verify your local archive is complete before permanent deletion. Use `msgvault verify` to check integrity. Executing a remote-deletion manifest preserves the archived copy.


## Permanently purge source-deleted mail from the local archive

Use `gc` only when you also want to discard local copies of messages already
marked deleted at their source. It purges those messages across the entire
SQLite archive, compacts the database, and removes attachment files that no
remaining message references. Active messages and messages hidden only by
deduplication remain in the archive.

`gc` does not contact providers or execute pending deletion batches. It has no
account filter, date filter, or dry-run flag, and it is not available for
PostgreSQL archives.

1. If you need a recoverable copy, back up the archive database **and attachment
   storage** before purging. The automatic GC backup contains only SQLite data;
   it cannot restore attachment bytes removed during cleanup.
2. Run the command and confirm the local purge:

   ```bash
   msgvault gc
   ```

3. After a purge, rebuild each cache you use:

   ```bash
   msgvault build-cache --full-rebuild
   msgvault embeddings build --full-rebuild
   ```

The second rebuild is needed only if you use embeddings. By default, GC writes
`msgvault.db.gc-backup-<timestamp>` beside the database before deleting rows.
`--yes` skips the confirmation prompt; `--no-backup` explicitly skips that
SQLite backup. A backup failure stops deletion.

If attachment cleanup fails after rows have been deleted, the command reports
the partial result. Rerunning GC retries unreferenced loose files even when no
source-deleted messages remain.
