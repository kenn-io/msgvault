---
last_edited: "2026-09-30"
title: Deleting Email
description: Stage messages for deletion, review manifests, and delete mail from Gmail, IMAP, or Microsoft Graph mail.
---


You can remove unwanted mail from Gmail, IMAP, or Microsoft Graph mail while keeping the archived message, raw content, and downloaded attachments. Deletion happens in three separate steps:

1. **Stage** a precise set of messages in a pending manifest.
2. **Review** that manifest and check your archive.
3. **Execute** the batch with `delete-staged` and explicit remote-deletion consent.

Executing a batch records which messages were deleted at the source without purging their archived content. You can still open and export those messages:

```bash
msgvault search "old receipt" --deletion-scope deleted
msgvault search "old receipt" --deletion-scope any
```

Use `deleted` to select source-deleted messages, or `any` to include active and source-deleted messages together. Removing an account or purging local data is a separate operation and can remove archive content.

## Stage from the CLI

Preview matching mail with `--dry-run` before you create a deletion batch:

```bash
msgvault stage-delete 'from:newsletter@example.com before:2024-01-01' --dry-run
msgvault stage-delete 'from:newsletter@example.com before:2024-01-01'
msgvault show-deletion BATCH_ID
```

CLI query staging uses the same search language as `msgvault search` and matches active messages only. If a query hits items that this staging path cannot delete, such as chats, meetings, or mail from IMAP or file imports, msgvault reports and skips them. The remaining targets must belong to one source. When several accounts match, narrow the query with `--source-id`:

```bash
msgvault stage-delete 'label:Newsletters' --source-id 3 --dry-run
```

MCP query staging also includes IMAP mail; see [staged deletion via MCP](chat.md#staged-deletion-via-mcp).

If you already have internal message IDs, stage them directly:

```bash
msgvault stage-delete --ids 123,456,789 --dry-run
msgvault stage-delete --ids 123,456,789
```

`--ids` accepts positive internal message IDs, not Gmail provider IDs, and cannot be combined with a query or `--source-id`. Missing, source-deleted, and unsupported targets are skipped and reported. Staging by explicit ID works without a ready analytical or full-text cache, while query staging waits for a complete search index and requires daemon API schema `2.18.0` or newer.

Both CLI staging paths resolve Gmail and Microsoft Graph mail targets. Graph mail accounts are the ones added with `add-o365 --graph`. Creating a manifest never executes it.

## Stage in the Web UI

You can review and stage deletions directly from the **Messages** view:

1. In **Messages**, select individual rows or all rows matching the current filter.
2. Choose **Review deletion…**, or press `d` or `D`. The review dialog displays server preflight counts, exclusions, and expiration.
3. Optional: select **Dry run** to see what would be staged without creating a batch.
4. Select **Stage N messages** to create the batch and view its ID and counts, then select **Done** to return to **Messages**.

The **Staged deletions** workspace lists batches by date, message count, and status. Select a row to inspect its account, description, and failed message IDs. If the account cannot be read, the panel shows "Account unavailable". Select **Cancel batch** and confirm to cancel the batch immediately. The sidebar shows **Staged deletions** while a listed batch is pending, running, or failed, and **Go to Staged deletions** in the command palette always opens the history. Executing a staged batch still requires the CLI command described below.

Preflight validates that your selection still matches and that the cache or search revision has not moved, but it does not check which accounts the matched messages belong to. Because each deletion manifest executes against exactly one mailbox, staging "all matching" across an unfiltered or multi-account view fails with a `multi_account_selection` error even after preflight succeeds. Before you stage an all-matching selection that could span more than one account, filter to a single source or account using `A` in the TUI or the source filter in the Web UI.

## Stage bulk deletions from aggregate groups

To clean up an inbox quickly in the TUI, use the aggregate views. Open **Senders**, **Domains**, or **Labels**, highlight the group you want to remove (for example, a prolific spam sender or an unwanted mailing list), and press `D` to stage every message in that group at once.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-deletion.svg" alt="msgvault TUI deletion confirmation dialog showing bulk staging of all messages from a sender" loading="lazy">
</figure>
A confirmation dialog shows exactly how many messages are about to be staged. Nothing is deleted until you explicitly run `msgvault delete-staged`.

## Stage selected messages in the TUI

For finer control, drill into any group, press `Space` to select individual rows, and press `d` to stage only the selected messages.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-selection.svg" alt="msgvault TUI with rows selected for deletion staging" loading="lazy">
</figure>

## Stage with MCP

You can stage deletions through the [MCP server](/docs/usage/chat/) by asking an AI assistant such as Claude to find and stage messages for you. For example:

- *"Stage all messages from noreply@linkedin.com for deletion"*
- *"Stage all promotional emails older than 2024-01-01"*

Owner MCP sessions can also list batches and read lifecycle receipts without enabling writes. The MCP `stage_deletion` tool creates a manifest through the selected daemon in the same format as TUI-staged deletions. Nothing is deleted until you run `msgvault delete-staged` from the CLI. See [staged deletion via MCP](chat.md#staged-deletion-via-mcp) for details.

## Stage with the HTTP API

Web dashboards and automation scripts can stage deletion manifests through the [web API](/docs/api-server/#post-apiv1deletions) without building a manifest by hand. `POST /api/v1/deletions` accepts structured filters, internal message IDs, or both, resolves the provider IDs on the server, and supports `"dry_run": true` to preview the count and a sample before writing anything. Any ID that does not resolve to a live deletable Gmail or Microsoft Graph mail message with a provider message ID is omitted, and the reported count reflects the targets resolved by the daemon.

How you stage depends on the request shape. An explicit `message_ids` list stages directly. Filter-based staging requires a preflighted selection: send the reviewed predicate to [`POST /api/v1/explore/preflight`](/docs/api-server/#post-apiv1explorepreflight) to obtain a single-use `operation_token` (valid for five minutes), then stage with the same selection and token. A non-dry-run filter request without a preflighted selection is rejected with `428 preflight_required`.

You can also list and cancel staged manifests through the API:

```bash
curl -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  http://localhost:8080/api/v1/deletions

curl -X DELETE -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  http://localhost:8080/api/v1/deletions/20260706-153000-old-newsletters-a1b2
```

Staging through the API still only creates or cancels local manifests. Remote deletion still requires running `delete-staged` with CLI consent, configured durably or supplied for one command as described below.

## Review and execute staged deletions

Each staged deletion creates a manifest that records exactly which messages are affected. Review pending batches before you execute them:

```bash
# List pending deletion batches
msgvault delete-staged --list

# Execute deletion for a Gmail account (moves to trash by default)
msgvault delete-staged --account you@gmail.com

# Move staged IMAP messages to the server's Trash folder
msgvault delete-staged --account you@fastmail.com

# Move staged Microsoft Graph mail messages to Deleted Items
msgvault delete-staged --account you@company.com

# Permanently delete Gmail messages instead of moving them to trash
msgvault delete-staged --account you@gmail.com --permanent
```

By default, Gmail and IMAP move messages to Trash. Gmail keeps trash for 30 days; IMAP recovery and retention depend on your server. IMAP uses the server's discovered Trash folder and falls back to a folder named `Trash`. If that move fails, the batch records the failure instead of switching to permanent deletion. Microsoft Graph mail moves messages to Deleted Items, where you can restore them in Outlook. A later sync keeps those messages marked as deleted while they stay in Deleted Items. Gmail and IMAP follow the same rule: sync clears the deleted mark when it confirms the message is outside Trash.

For IMAP messages without a Message-ID, a restored copy is ingested as a new message and the original remains marked.

Pass `--permanent` to request irreversible deletion. Gmail uses its batch API, and IMAP runs `UID STORE \Deleted` followed by `UID EXPUNGE` for each message. Permanent IMAP deletion requires UIDPLUS and is refused when the server lacks it. Microsoft Graph mail calls `permanentDelete` for each message, and you cannot restore those messages in Outlook. Dry-run mode works for all providers.

## Enable remote deletion

Remote deletion is opt-in. Before running `delete-staged`, enable it in the invoking CLI's configuration or pass consent for a single command. Staging and review do not require this setting.

For durable consent, add a `[deletion]` section to the invoking CLI's `config.toml`:

```toml
[deletion]
remote_enabled = true
```

To allow remote deletion for one command instead, set the environment variable:

```bash
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --account you@gmail.com
```

When remote mode is configured, the invoking CLI forwards its effective consent for that operation. The remote daemon's own `[deletion]` section is not server policy for a command invoked elsewhere. Staging, listing, inspecting, and dry-running deletion batches remain ungated, so you can always inspect exactly what is staged before enabling execution.

## Upgrade OAuth permissions

When you first run `delete-staged --permanent` on a Gmail account, your OAuth token likely has only read and modify permissions. Permanent Gmail batch deletion requires full Gmail access (the `mail.google.com` scope). msgvault detects this automatically and prompts you to upgrade:

```
======================================================================
PERMISSION UPGRADE REQUIRED
======================================================================

Batch deletion requires elevated Gmail permissions.

Your current OAuth token was granted with limited permissions that
don't include batch delete. To proceed, msgvault will re-authorize
this account with full Gmail access (mail.google.com scope). Your
existing token keeps working until the new grant succeeds.

This elevated permission allows msgvault to permanently delete
messages in bulk. You can revoke access anytime at:
  https://myaccount.google.com/permissions

Upgrade permissions now? [y/N]:
```

If you answer `y`, msgvault opens your browser for re-authorization. If you answer `N`, it cancels cleanly with no side effects. You can revoke the elevated permission at any time from your [Google Account permissions page](https://myaccount.google.com/permissions).

If you have a legacy token from before scope tracking was added, msgvault detects the insufficient scope from the API response and shows the same prompt. Moving Gmail messages to Trash does not require this elevated scope.

Microsoft Graph mail accounts sync with the `Mail.Read` permission. The first time you run `delete-staged` on a Microsoft Graph account, msgvault asks to upgrade the token to `Mail.ReadWrite` for both Trash and `--permanent`. If you answer `y`, msgvault opens your browser to grant it, and your existing token keeps working until the new grant succeeds. Your Entra app registration must list the delegated `Mail.ReadWrite` permission.

On a host without a browser, use device-code sign-in:

```bash
msgvault delete-staged BATCH_ID --headless
```

Open the printed sign-in URL on another device and enter the code. The `--headless` flag applies to Microsoft Graph permission upgrades only.

With a local daemon, the CLI completes the Gmail or Microsoft Graph permission upgrade before starting the deletion worker. With a remote daemon, sign-in runs on the daemon host because that host owns the token, and `--headless` is forwarded to that worker for Microsoft Graph accounts.

IMAP accounts do not need a permission upgrade. IMAP credentials already have full mailbox access, so `delete-staged` works without re-authorization.

## Resume interrupted deletions

If execution stops partway through due to a network error, Ctrl+C, or a scope error, run the same command again to pick up where it left off. msgvault tracks failed messages individually and retries them automatically on the next run before continuing with the remaining messages.

## Cancel deletions

You can cancel a specific batch or cancel all pending and in-progress batches:

```bash
# List available batches
msgvault cancel-deletion

# Cancel a specific batch
msgvault cancel-deletion <batch-id>

# Cancel all pending and in-progress batches
msgvault cancel-deletion --all
```

!!! danger
    Permanent deletion cannot be undone on the remote mail server (Gmail or IMAP provider). Always verify your local archive is complete before permanent deletion. Use `msgvault verify` to check integrity. Executing a remote-deletion manifest preserves the archived copy.


## Permanently purge source-deleted mail from the local archive

Use `gc` only when you also want to discard local copies of messages already marked deleted at their source. It purges those messages across the entire SQLite archive, compacts the database, and removes attachment files that no remaining message references. Active messages and messages hidden only by deduplication stay in the archive.

`gc` does not contact providers or execute pending deletion batches. It has no account filter, date filter, or dry-run flag, and it is not available for PostgreSQL archives.

1. If you need a recoverable copy, back up the archive database **and attachment storage** before purging. The automatic GC backup contains only SQLite data and cannot restore attachment bytes removed during cleanup.
2. Run the command and confirm the local purge:

   ```bash
   msgvault gc
   ```

3. After a purge, rebuild each cache you use:

   ```bash
   msgvault build-cache --full-rebuild
   msgvault embeddings build --full-rebuild
   ```

You need the second rebuild only if you use embeddings. By default, GC writes `msgvault.db.gc-backup-<timestamp>` beside the database before deleting rows. Pass `--yes` to skip the confirmation prompt; pass `--no-backup` to skip that SQLite backup. A backup failure stops deletion.

If attachment cleanup fails after rows have been deleted, `gc` reports the partial result. Running `gc` again retries unreferenced loose files even when no source-deleted messages remain.
