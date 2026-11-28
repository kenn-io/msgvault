---
last_edited: "2026-11-05"
title: IMAP Sync and Repair
description: Archive IMAP mail efficiently, choose folders, and repair stored labels.
---

Archive mail from an IMAP account and keep it up to date without downloading
unchanged messages again. If you have not connected the account yet, start with
[IMAP account setup](../setup.md#add-an-imap-account).

```bash
msgvault sync-full you@example.com
msgvault sync you@example.com
```

Sync only reads from the provider and keeps messages in your local archive even
if their server copies disappear. Removing messages from the server happens in a
separate [staged workflow](/docs/usage/deletion/).

## How later syncs find changes

msgvault picks its sync strategy based on what your IMAP server supports:

| Server behavior | What msgvault does |
|---|---|
| Supports QRESYNC, the IMAP change-tracking extension | Uses saved mailbox state to fetch changes and track messages removed from folders |
| Does not support QRESYNC | Compares mailbox counts and saved message-number boundaries; skips unchanged folders and fetches new messages where possible |
| Does not support CONDSTORE either, so reports no mod-sequences | Also rereads the flags of the 5,000 newest archived messages in each folder, without downloading them, and saves only flags that changed |
| State is missing, inconsistent, or no longer valid | Enumerates the affected folders again to establish current membership |

Without mod-sequences, a flag change on an older message (such as marking it
read or answered) only reaches the archive when you run a full scan with
`msgvault sync-full you@example.com --noresume`. If a flag read fails, msgvault
keeps the saved flags and continues the sync.

If a QRESYNC attempt fails, msgvault reconnects and falls back to full
enumeration. It never accepts an incomplete server response as a complete
mailbox snapshot. Temporary connection errors get bounded retries, while a
persistent failure still stops the run with an error. Fix the connection problem
and rerun the command.

Servers that advertise both IMAP4rev1 and IMAP4rev2 require explicit negotiation
before using UTF-8 mailbox names. msgvault enables IMAP4rev2 after login on
every connection, including reconnects, so folders with non-ASCII names can be
included in a complete incremental baseline. Folder names are then exchanged
as plain UTF-8, so characters such as `&` need no escaping. If a server refuses
that negotiation, the connection ends with an error.

## Handle server command limits

When a server replies to a read command with `NO [LIMIT]` or a `NO` containing a
`retry in N seconds` hint, sync retries the command. It waits for the full
hinted delay, or one second when `LIMIT` has no usable hint, and returns the
command error immediately if a hint is too large to represent as a duration.
Each command gets up to three retries, and cancellation interrupts the wait.
Because an existing transport reconnect can start one fresh command budget, a
FETCH can make up to eight attempts across both connections. Persistent limits
still surface as errors.

To hit a server's command quota less often, set
[`sync.imap_max_commands_per_minute`](../configuration.md#sync) and restart
the daemon:

```toml
[sync]
imap_max_commands_per_minute = 1800
```

The default is `0` (disabled). Any positive value evenly paces explicit sync
reads, including retries, and authentication attempts for each client. Pacing
survives reconnects and applies to both manual and scheduled syncs, though it
does not coordinate quotas across separate clients or processes. Internal
go-imap `CAPABILITY` commands and transport negotiation are outside this cap,
and msgvault never retries authentication or commands that modify server
messages automatically.

## Choose folders

By default, msgvault scans every selectable folder. Folder filters let you start
with a subset of an account or skip folders you do not need. They work with both
`sync-full` and `sync` and apply to IMAP sources only.

## Find the Folder Names

Before writing a filter, ask the IMAP server for its folder names:

```bash
msgvault list-folders you@example.com
```

The output lists each selectable folder and its approximate message count:

```text
Account: you@example.com

  Folder                                Messages
  ----------------------------------------------
  INBOX                                     1240
  Archive                                  18342
  Projects/Alpha                             217
  Trash                                       36
```

To list folders for every configured IMAP account, omit the account name:

```bash
msgvault list-folders
```

Some servers do not report a message count for every folder. When that happens,
msgvault displays `??`, and you can still use the folder name in a filter.

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

Each flag takes one complete folder name. Repeat the flag rather than joining
names with commas. That also means folder names with commas work without
escaping:

```bash
msgvault sync-full you@example.com --folder "Receipts, 2025"
```

## Skip Selected Folders

Pass `--skip-folder` to scan every folder except the ones you list:

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

This example scans `INBOX` and `Archive`.

To skip folders on every scheduled and manual sync, set
[`sync.imap_skip_folders`](../configuration.md#sync) in `config.toml` and
restart the daemon. Copy the exact `ACCOUNT` identifier from
`msgvault list-accounts`:

```toml
[sync]
imap_skip_folders = { "imaps://user@example.com@imap.example.com:993" = ["Trash", "Junk"] }
```

Passing either folder flag replaces the saved filter for that run. With the
configuration above, `--folder Trash` scans Trash, and `--skip-folder Archive`
skips Archive while allowing Trash and Junk.

## Matching Rules

- Folder names match exactly, with no wildcards or prefix matching.
- Matching is case-insensitive.
- For nested folders, use the full name printed by `list-folders`, such as
  `Projects/Alpha`.
- When you omit folder flags, msgvault applies the account's saved exclusions
  and stored folder allow list, if configured.
- Folder flags apply only to the current command. Repeat them in later runs if
  you want the same filter.
- If a command syncs multiple account types, folder flags affect only the IMAP
  accounts.

## What Filtering Changes

A folder filter limits which remote IMAP folders msgvault scans during that run.
It never deletes messages from the server or removes messages already stored in
your local archive.

An email can live in more than one IMAP folder. During a filtered scan, msgvault
keeps the stable identity and folder labels learned from earlier, broader scans
while adding details from the selected folders. A later sync without folder
filters scans the full account again. Filtered scans preserve existing archive
membership and leave each message's source-deleted status unchanged.

Saved exclusions pause deletion tracking even in folders that are still being
scanned, and they also pause folder cursor advancement. Expunged messages keep
their prior deletion markers, and messages previously marked present stay
ineligible for `gc` until you remove the exclusions and a complete sync resumes
tracking. Changed mailboxes can be listed again on later runs while exclusions
remain active.

Folder filtering behaves the same way whether the CLI talks to a local daemon or
a configured remote msgvault server.

## Edit keyword tags

Use [`message-tags`](../cli-reference.md#message-tags) to read, add, or remove
custom keywords on a message's recorded server copy:

```bash
msgvault message-tags 42 --mailbox INBOX --json
msgvault message-tags 42 --mailbox INBOX --add Next --remove Old
```

Keywords are separate from the folder labels in your archive, and each folder
copy has its own flags. The daemon keeps existing system flags and other
keywords intact, verifies your requested changes with a readback, and saves the
observed flags on that exact membership. If you omit `--mailbox`, msgvault uses
the current original copy or the sole current membership. If the original copy
is gone and multiple copies remain, specify `--mailbox`.

Keywords can be up to 255 ASCII bytes and are compared without case. If the
stored UID or UIDVALIDITY is missing or stale, sync first. Your server must
also allow persistent keywords: its `PERMANENTFLAGS` response must include
`\*` or the keyword itself, or be absent (which IMAP treats as all flags
permanent).

This command works on every IMAP account, including Microsoft 365, Outlook.com,
Fastmail, and other providers, regardless of host or authentication method.
Fastmail's custom keywords are
[shared with JMAP](https://www.rfc-editor.org/rfc/rfc8621.html#section-4.1.1),
and its optional JMAP address-discovery token does not change how mail is
archived. Microsoft Outlook categories are separate from IMAP keywords and use
the [Microsoft Graph tag backend](../cli-reference.md#message-tags). A server
that disallows persistent keywords cannot accept an IMAP tag edit.

A dry run checks keyword syntax, identity, and advertised support, but it cannot
prove mailbox write access. The server's STORE response and readback decide
whether a later edit succeeded. After a partial or uncertain write, read the
current tags before retrying.

## Repair stored labels

If an archived message still shows a folder label that no longer belongs to it,
run `repair-labels`. The command rebuilds labels from the folder memberships
already stored in msgvault without contacting your mail provider.

1. Preview the repair for one source:

    ```bash
    msgvault repair-labels you@example.com
    ```

2. Check the `scanned` and `changed` counts, then apply the repair:

    ```bash
    msgvault repair-labels you@example.com --apply
    ```

To check or repair every IMAP source at once, omit the account identifier.
Applying a repair also refreshes the analytical cache.

A sync with incomplete folder information only adds labels and never removes
labels it cannot disprove. If the stored memberships later become complete and
never change again, an old label can stay behind until you run this repair.

If the stored memberships themselves are out of date, enumerate the server
again first:

```bash
msgvault sync-full you@example.com --noresume
```

Omit folder filters for a complete account scan. `repair-labels` cannot
recover memberships that the archive has never seen.

## Drafts

You can create a new message or a reply in your IMAP Drafts folder, then review
and send it from your usual mail app. msgvault never sends email. Draft creation
stays disabled until an operator grants it for one exact IMAP source on the
daemon host.

1. Find the source ID with `msgvault list-accounts`, and check the exact Drafts
    folder name with `msgvault list-folders <account>`.

1. Add the grant to the daemon host's `config.toml` using that source ID and
    folder name:

    ```toml
    [[imap.drafts]]
    source_id = 42
    enabled = true
    mailbox = "Drafts"
    ```

1. Restart the daemon. Host policy applies per source. Owner callers using an
    API key, browser session, or keyless loopback can create drafts on a granted
    source, while delegated callers also need that source in their
    [agent token grant](../cli-reference.md#agent-token). Client configuration,
    request fields, and environment variables cannot grant access or pick a
    different folder.

1. Check the source's confirmed sender identities:

    ```bash
    msgvault identity list --source-id 42
    ```

    If your address is missing, add it with
    `msgvault identity add --source-id 42 you@example.com`.

1. Find the parent email's local message ID with search, then create a reply:

    ```bash
    msgvault draft-reply 123 --all --from you@example.com \
      --body 'Thanks for the update. I will review it tomorrow.' --json
    ```

    Leave out `--all` for a direct reply. To target a different live IMAP
    destination, pass `--account <account>` or `--source-id <id>`. If the parent
    is offline, one of those selectors is required.

1. Create a new message by picking its live source and repeating recipient flags
    as needed:

    ```bash
    msgvault draft-compose --source-id 42 --from you@example.com \
      --to recipient@example.com --cc copy@example.com \
      --bcc private@example.com --subject 'Project update' --body 'Draft text'
    ```

The parent message must have its original email stored in the archive. Reply-all
uses Reply-To or From along with visible To and Cc recipients, removes confirmed
identities for the selected destination, and never reads a parent's Bcc as a
reply recipient. Your IMAP server must support UIDPLUS, which returns a receipt
identifying the stored draft.

msgvault chooses the draft's From address in this order:

1. An explicit `--from`, which must be a confirmed identity for the destination.
2. For replies, the confirmed identity the parent was addressed to. msgvault
    checks the parent's archived To, Cc, and Bcc recipients as well as every To
    and Cc header in the original email. If several identities match, an agent's
    grant must allow exactly one of them; otherwise pass `--from`. If a sole
    match falls outside the agent's grant, msgvault returns an authorization
    error.
3. The destination's only eligible confirmed identity. Compose and forward start
    here.

For delegated callers, the token freezes the allowed sender identities when an
owner issues it. A draft's From header is a local choice and does not prove
send-as permission with your provider.

On success, the command returns `status: "created"`, the archived `message_id`,
and the remote mailbox receipt. The draft is marked `\Draft` and saved locally
with its original email content. Later syncs advance the mailbox cursor and
reconcile any mailbox identifier changes.

### If draft creation does not finish

msgvault does not retry the remote write automatically. Check the reported
result to decide what to do next:

| Result                                      | Next step                                                                                                                                            |
| ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sync_active`                               | Wait for this source's sync to finish, then retry.                                                                                                   |
| `uidplus_required`                          | Use a server that advertises UIDPLUS; no draft was appended.                                                                                         |
| `append_rejected`                           | Check that the configured folder exists and permits writes.                                                                                          |
| `remote_unknown` or `accepted_unidentified` | Inspect the Drafts folder before retrying; the draft may already exist.                                                                              |
| `remote_accepted_local_failed`              | The server accepted the draft, but the local save failed. Use the reported `operation_ref` and mailbox receipt to inspect it before another request. |

## Manage a created draft

When you create a draft, the result includes an opaque `draft_id` and revision
`1`. Read the archived draft and its current revision through the daemon:

```bash
msgvault draft-get <draft-id> --json
```

`draft-get` never connects to IMAP or changes pending operations. It also reads
discarded drafts and works even when the source's draft mutation grant is
disabled or its provider configuration is unavailable.

### Edit or delete the provider draft

Pass the current revision from `draft-get` or from the last successful
operation:

```bash
msgvault draft-edit <draft-id> --revision 1 --body 'Updated text' --json
msgvault draft-delete <draft-id> --revision 2 --json
msgvault draft-recover <draft-id> --revision 1 --json
```

Editing and deleting require the same daemon-host `[[imap.drafts]]` grant and
provider configuration as draft creation, and policy changes take effect after
you restart the daemon. Recovery uses the same policy after checking the
delegated action grant. None of these commands sends mail.

Editing supports plain-text drafts without attachments, and `--body=` sets an
empty body. An edit keeps the From, To, Cc, Bcc, Reply-To, Subject,
In-Reply-To, and References headers. msgvault appends one replacement, records
the new revision, removes the exact old UID, and confirms that it is gone.

Deleting removes the exact provider draft and marks the managed draft
`discarded` once its absence is confirmed. The local archived copy stays
readable with `draft-get`, and ordinary archive garbage collection keeps it. You
cannot edit a discarded draft, and repeating delete with its current revision
returns `already_discarded`.

### Provider checks before a change

Before writing anything, msgvault rejects stale revisions, UIDVALIDITY changes,
external moves, a missing `\Draft` flag, an existing `\Deleted` flag, or missing
UIDPLUS. Edits also reject multipart drafts. If a source sync is running, the
command returns `sync_active`; retry after the sync finishes.

When the mailbox provides CONDSTORE metadata, msgvault guards its singleton
`UID STORE` with the exact UID's positive MODSEQ, then verifies that UID's
flags. If the server does not advertise CONDSTORE, msgvault runs a fresh
`SELECT` and exact-UID `FETCH` right before the nonconditional `UID STORE`.
When CONDSTORE is advertised, missing or zero mailbox or message MODSEQ metadata
refuses the change before writing with `modseq_unusable`. This also happens on
`NOMODSEQ` mailboxes. The current IMAP parser cannot tell that response apart
from missing metadata.

Every removal path checks the mailbox generation and requires both `\Draft` and
`\Deleted` again before `UID EXPUNGE`, then confirms exact-UID absence. Because
`UID EXPUNGE` itself is not conditional, another client can still change flags
after that last check.

### Recover an interrupted edit or delete

Recovery relies only on the original receipt and any replacement receipt already
saved on the managed draft row. Before publishing or removing anything, it
verifies the requested revision, source type and identifier, mailbox
UIDVALIDITY, exact UID, and current draft flags. If an edit is pending without a
known replacement, recovery returns `unknown_replacement` without opening an
IMAP connection.

```bash
msgvault draft-recover <draft-id> --revision 1 --json
```

If another client already removed the exact original UID, recovery records that
absence and finishes the Store transition locally. When a replacement is known
but not yet published, recovery publishes it from its saved bytes and removes
the original using the same conditional `UID STORE` and `UID EXPUNGE` sequence.
Recovery never searches a mailbox, adopts a moved copy, calls APPEND, or rolls
back a publication. Once a replacement is published locally, cleanup of the
original can finish even if another client has already sent or deleted the
replacement; only an unpublished replacement must still be a live draft.

`draft.edit` authorizes active and pending edits, and `draft.delete` authorizes
discarded and pending deletes. A delegated token must also name the exact source
type and identifier. Wrong actions, sources, or revisions fail before msgvault
discloses draft content, policy errors, locks, or provider work.

If the original already has `\Deleted`, recovery returns `already_deleted`
without writing to the provider. This includes removals interrupted after
`UID STORE` succeeded. Repeating recovery cannot clear that state: use an IMAP
client to expunge the exact original UID in the recorded mailbox and
UIDVALIDITY, then run `draft-recover` with the revision from the recovery output
or `draft-get`. Recovery may have published a replacement locally and advanced
the revision before cleanup was refused. For a published edit, remove the
original copy rather than the replacement. Check the receipt before expunging,
and do not purge all deleted messages in the mailbox. Because saved cancellation
evidence does not establish who set the flag, automatic removal of an
already-deleted UID is outside this command's scope.

If cleanup fails after publication, the new revision and pending evidence stay
saved. Recovery can retry when the original is still an undeleted draft or is
confirmed absent, while an already-deleted original needs the manual step above.
A generation or SELECT refusal before `UID STORE` leaves the pending code,
receipts, timestamp, and revision unchanged. Output keeps the saved
`pending_code`; refused results use `refusal_code`, and pending cleanup results
use `observation.code` for the current provider result.

### If an edit or delete does not finish

- If a delete fails before any remote write, msgvault clears the pending claim.
  Fix the reported problem and retry with the same revision.
- An uncertain APPEND or removal returns a nonzero result along with the saved
  candidate and available receipt evidence. The operation stays pending and
  blocks further changes. Inspect the provider state before doing any manual
  reconciliation; general recovery for uncertain writes is not available.
- If `draft-get <draft-id> --json` reports `pending_code: "removed"`, removal
  was confirmed and saved, while local completion is still pending.
  `draft-recover` is the preferred way to complete it locally: run it with the
  revision from that read, and it finishes locally without another remote write.
  You can also repeat the matching `draft-edit` or `draft-delete` command with
  that revision, and the source policy still applies. For an edit, `--body` must
  match the already published replacement after MIME normalization. A `removed`
  observation in an error response alone is not enough; `draft-get` must report
  the saved pending code.

## Keep edited outgoing mail current

After you edit or send a draft in your mail app, IMAP sync can update its
archived body, recipients, attachments, and search text while keeping the local
message ID. A newer Sent copy takes precedence over a stale Drafts copy.

This replacement applies only to trusted outgoing folders. msgvault trusts
unambiguous server-advertised `\Sent` and `\Drafts` roles. If your server does
not advertise Sent accurately, configure the exact account and folder under
[`sync.trusted_imap_sent_mailboxes`](../configuration.md#sync). Name only
folders used for sent mail, and never include folders that receive incoming mail
through filters or filing rules. Ordinary received-mail and All Mail copies keep
their archived content instead of replacing it.

These rules apply during later syncs and do not automatically repair older
archive rows that already lost the location information needed to identify the
outgoing copy.
