---
last_edited: "2026-09-15"
title: IMAP Sync and Repair
description: Archive IMAP mail efficiently, choose folders, and repair stored labels.
---

Archive mail from an IMAP account, then keep it current without downloading
unchanged messages again. Start with [IMAP account setup](/docs/setup/#add-an-imap-account)
if you have not connected the account yet.

```bash
msgvault sync-full you@example.com
msgvault sync you@example.com
```

Sync reads the provider. It preserves messages already in your local archive
when their server copies disappear. Remote deletion is a separate
[staged workflow](/docs/usage/deletion/).

## How later syncs find changes

msgvault chooses the sync method from the server's capabilities:

| Server behavior | What msgvault does |
|---|---|
| Supports QRESYNC, the IMAP change-tracking extension | Uses saved mailbox state to fetch changes and track messages removed from folders |
| Does not support QRESYNC | Compares mailbox counts and saved message-number boundaries; skips unchanged folders and fetches new messages where possible |
| State is missing, inconsistent, or no longer valid | Enumerates the affected folders again to establish current membership |

A failed QRESYNC attempt reconnects and falls back to full enumeration. An
incomplete server response is not accepted as a complete mailbox snapshot.
Temporary connection failures receive bounded retries; a persistent failure
still ends the run with an error. Rerun the command after correcting the
connection problem.

## Choose folders

By default, msgvault scans every selectable folder. Folder filters let you
start with a small part of an account or leave out folders you do not need.
They work with both `sync-full` and `sync` and affect IMAP sources only.

## Find the Folder Names

Ask the IMAP server for its folder names before creating a filter:

```bash
msgvault list-folders you@example.com
```

The command shows each selectable folder and its approximate message count:

```text
Account: you@example.com

  Folder                                Messages
  ----------------------------------------------
  INBOX                                     1240
  Archive                                  18342
  Projects/Alpha                             217
  Trash                                       36
```

Leave out the account name to list folders for every configured IMAP account:

```bash
msgvault list-folders
```

Some servers do not provide a message count for every folder. In that case,
msgvault shows `??`, but you can still use the folder name in a filter.

## Sync Only Selected Folders

Repeat `--folder` once for each folder you want to include:

```bash
msgvault sync-full you@example.com \
  --folder INBOX \
  --folder Archive
```

To scan the same folders during a later sync:

```bash
msgvault sync you@example.com \
  --folder INBOX \
  --folder Archive
```

Each flag takes one complete folder name. Repeat the flag instead of joining
names with commas. This also means a folder whose name contains a comma works
without special handling:

```bash
msgvault sync-full you@example.com --folder "Receipts, 2025"
```

## Skip Selected Folders

Use `--skip-folder` to scan every folder except the ones you name:

```bash
msgvault sync-full you@example.com \
  --skip-folder Trash \
  --skip-folder Spam
```

You can combine include and exclude filters. msgvault first keeps the folders
named by `--folder`, then removes any named by `--skip-folder`:

```bash
msgvault sync-full you@example.com \
  --folder INBOX \
  --folder Archive \
  --folder "Archive/Newsletters" \
  --skip-folder "Archive/Newsletters"
```

That example scans `INBOX` and `Archive`.

## Matching Rules

- Folder names are matched exactly, without wildcards or prefix matching.
- Matching is case-insensitive.
- Nested folders use the full name shown by `list-folders`, such as
  `Projects/Alpha`.
- With no folder flags, msgvault scans every selectable folder.
- Folder flags apply to one command invocation. Repeat them in later commands
  when you want the same filter.
- If a command syncs several account types, folder flags affect only its IMAP
  accounts.

## What Filtering Changes

A folder filter limits which remote IMAP folders msgvault scans during that
run. It does not delete messages from the server or remove messages already in
the local archive.

An email can appear in more than one IMAP folder. During a filtered scan,
msgvault keeps the stable identity and folder labels learned by earlier,
broader scans while adding information from the selected folders. A later sync
without folder flags scans the complete account again.

Folder filtering works the same whether the CLI uses a local daemon or a
configured remote msgvault server.

## Repair stored labels

Use `repair-labels` when an archived message still shows a folder label that
no longer belongs to it. The command rebuilds labels from the folder
memberships already stored in msgvault. It does not contact the provider.

1. Preview the repair for one source:

    ```bash
    msgvault repair-labels you@example.com
    ```

2. Review the `scanned` and `changed` counts, then apply it:

    ```bash
    msgvault repair-labels you@example.com --apply
    ```

Omit the identifier to check or repair every IMAP source. Applying a repair
also refreshes the analytical cache.

A sync with incomplete folder information only adds labels; it does not
remove labels it cannot disprove. If the stored memberships later become
complete but never change again, an old label can remain until this repair.

If the stored memberships themselves need refreshing, enumerate the server
again first:

```bash
msgvault sync-full you@example.com --noresume
```

Leave out folder filters for a complete account scan. `repair-labels` cannot
recover memberships that the archive has never observed.

## Reply drafts

Create a reply in your IMAP Drafts folder, then review and send it from your
usual mail application. Msgvault never sends email. Draft creation is disabled
until an operator grants it for one exact IMAP source on the daemon host.

1. Run `msgvault list-accounts` to find the source ID. Confirm the Drafts
    folder's exact name with `msgvault list-folders <account>`.

1. Add the grant to the daemon host's `config.toml`, using that source ID and
    folder name:

    ```toml
    [[imap.drafts]]
    source_id = 42
    enabled = true
    mailbox = "Drafts"
    ```

1. Restart the daemon. The host policy applies per source. Owner callers
    using an API key, browser session, or keyless loopback can create drafts
    on a granted source. Delegated callers also need that source in their
    [agent token grant](../cli-reference.md#agent-token). Client configuration,
    request fields, and environment variables cannot grant access or choose a
    different folder.

1. Check the source's confirmed sender identities:

    ```bash
    msgvault identity list --source-id 42
    ```

    If your address is missing, confirm it with
    `msgvault identity add --source-id 42 you@example.com`.

1. Find the parent email's local message ID with search, then create the draft:

    ```bash
    msgvault draft-reply 123 --from you@example.com \
      --body 'Thanks for the update. I will review it tomorrow.' --json
    ```

The parent must belong to the granted IMAP source and have its original email
stored in the archive. Msgvault composes a plain-text reply using the parent's
threading headers. `--from` must be a confirmed identity for that source, and
`--body` is required; `--body=` creates an empty draft. The IMAP server must
support UIDPLUS, which returns a receipt that identifies the stored draft.

A successful result reports `status: "created"`, the archived `message_id`, and
the remote mailbox receipt. The draft is marked `\Draft` and stored locally with
its original email content. Later syncs advance the mailbox cursor and reconcile
any mailbox identifier changes.

### If draft creation does not finish

The command does not retry the remote write automatically. Use the reported
outcome to decide what to do next:

| Result                                      | Next step                                                                                                                                            |
| ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sync_active`                               | Wait for this source's sync to finish, then retry.                                                                                                   |
| `uidplus_required`                          | Use a server that advertises UIDPLUS; no draft was appended.                                                                                         |
| `append_rejected`                           | Check that the configured folder exists and permits writes.                                                                                          |
| `remote_unknown` or `accepted_unidentified` | Inspect the Drafts folder before retrying; the draft may already exist.                                                                              |
| `remote_accepted_local_failed`              | The server accepted the draft, but the local save failed. Use the reported `operation_ref` and mailbox receipt to inspect it before another request. |

## Manage a created draft

The creation result includes an opaque `draft_id` and revision `1`. Read the
archived draft and its current revision through the daemon:

```bash
msgvault draft-get <draft-id> --json
```

`draft-get` never connects to IMAP or changes pending operations. It also reads
discarded drafts and works when the source's draft mutation grant is disabled
or its provider configuration is unavailable.

### Edit or delete the provider draft

Use the current revision from `draft-get` or the last successful operation:

```bash
msgvault draft-edit <draft-id> --revision 1 --body 'Updated text' --json
msgvault draft-delete <draft-id> --revision 2 --json
```

Edit and delete require the same daemon-host `[[imap.drafts]]` grant and provider
configuration as draft creation. Policy changes take effect after a daemon
restart. Neither command sends mail.

Editing supports plain-text drafts without attachments. `--body=` sets an empty
body. The edit preserves the From, To, Cc, Bcc, Reply-To, Subject, In-Reply-To, and
References headers. Msgvault appends one replacement, records the new revision,
removes the exact old UID, and confirms that it is absent.

Delete removes the exact provider draft and marks the managed draft
`discarded` after confirming its absence. The local archived content remains
readable with `draft-get`; ordinary archive garbage collection retains it.
A discarded draft cannot be edited. Repeating delete with its current revision
returns `already_discarded`.

### Provider checks before a change

Msgvault refuses stale revisions, UIDVALIDITY changes, external moves, a missing
`\Draft` flag, an existing `\Deleted` flag, or missing UIDPLUS before writing.
Edits also reject multipart drafts. An active source sync returns `sync_active`;
retry after it finishes.

When the mailbox supplies CONDSTORE metadata, msgvault uses the exact UID's
positive MODSEQ to guard its singleton `UID STORE`, then verifies that UID's
flags. If the server does not advertise CONDSTORE, msgvault runs a fresh `SELECT`
and exact-UID `FETCH` immediately before the nonconditional `UID STORE`.
When CONDSTORE is advertised, missing or zero mailbox or message MODSEQ metadata
refuses the change before writing with `modseq_unusable`. This also applies to
`NOMODSEQ` mailboxes: the current IMAP parser cannot distinguish that response
from missing metadata.

Every removal path checks the mailbox generation and requires both `\Draft` and
`\Deleted` again before `UID EXPUNGE`, then confirms exact-UID absence.
`UID EXPUNGE` itself is not conditional, so another client can still change flags
after the last check.

### If an edit or delete does not finish

- A delete failure before any remote write clears the pending claim. After
  resolving the reported problem, retry with the same revision.
- An uncertain APPEND or removal returns a nonzero result with the saved
  candidate and available receipt evidence. The operation stays pending and
  blocks further changes. Inspect the provider state before any manual
  reconciliation; general recovery for uncertain writes is not available.
- If `draft-get <draft-id> --json` reports `pending_code: "removed"`, removal was
  confirmed and saved, but local completion is still pending. Repeat the matching
  `draft-edit` or `draft-delete` command with the revision from that read to finish
  locally without another remote write. The source policy still applies. For an
  edit, `--body` must match the already published replacement after MIME
  normalization. A `removed` observation in an error response alone is not enough;
  `draft-get` must report the saved pending code.

## Keep edited outgoing mail current

After you edit or send a draft in your mail application, IMAP sync can update
its archived body, recipients, attachments, and search text while retaining the
local message ID. A newer Sent copy takes precedence over a stale Drafts copy.

This replacement is limited to trusted outgoing folders. Msgvault trusts
unambiguous server-advertised `\Sent` and `\Drafts` roles. If your server does
not advertise Sent correctly, configure the exact account and folder under
[`sync.trusted_imap_sent_mailboxes`](../configuration.md#sync). Only name
folders used for sent mail; never include folders that receive incoming mail
through filters or filing rules. Ordinary received-mail and All Mail copies
preserve the archived content instead of replacing it.

These rules apply during later syncs. They do not automatically repair older
archive rows that already lost the location information needed to identify the
outgoing copy.
