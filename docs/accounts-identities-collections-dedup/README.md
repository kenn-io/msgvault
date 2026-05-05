# Accounts, Identities, Collections, and Deduplication

**Shipped May 3, 2026.** Features for managing the growing size and
complexity of accounts in msgvault. The first deduplication command
landed in [commit `9ace189`](https://github.com/jesserobbins/msgvault/commit/9ace189d86949ccd595a012bf29864abcffa4dda)
on April 8, alongside the [implementation plan](https://github.com/jesserobbins/msgvault/commit/8d8cedeba20920a74dc5b1c0acb97f7548b64ff5)
and [test plan](https://github.com/jesserobbins/msgvault/commit/df61cee)
committed earlier the same day. The unified model that emerged over
the next four weeks is proposed in upstream issue
[wesm/msgvault#278](https://github.com/wesm/msgvault/issues/278) and
ships via [PR #304](https://github.com/wesm/msgvault/pull/304).

## What you need to know

A long-running msgvault archive accumulates overlapping sources: a
current Gmail sync, an old mbox export, Apple Mail from a retired
laptop, IMAP backups, chat exports, SMS history. Each source is
valuable. Together they create three problems. The same message
appears multiple times. Old imports no longer remember which
addresses are "you." Duplicates dominate search results.

These features fix all three and keep every source's provenance
intact.

The whole release rests on two halves: a data model (Account,
Identity, Collection) and a safety story (hide is the default; delete
is always opt-in). Read these if you read nothing else.

### The data model: Account, Identity, Collection (AIC)

1. **An *account* is one ingest source.** A Gmail sync is one
   account. An mbox import is another. Two imports of the same
   real-world mailbox produce two accounts. msgvault never silently
   merges them.
2. **An *identity* is the addresses, phone numbers, and identifiers
   that mean "me" inside one account.** Identity is per-account
   because the same address can mean different things in different
   imports.
3. **A *collection* is a named group of accounts.** `All` exists by
   default and contains every account. Create `work`, `personal`, or
   any other named group. Collections are the boundary for
   cross-account features: search, stats, dedup. A collection's
   identity is the union of its members'.

![Accounts and collections — left side shows six per-import accounts (Personal Gmail, Old mbox, Apple Mail archive, iMessage, Old work account, College email), each with the addresses or phone numbers that identify the owner inside that source. Right side shows three collections — All (every account), Personal (a deliberate subset), Work (another named view) — each composed of accounts.](./assets/account-collection-concept.png)

Each account on the left is one ingest source with its own owner
identity. Collections on the right are user-named groups of those
accounts. `All` is built automatically and contains everything.
`Personal` and `Work` are named subsets. One account can belong to
multiple collections. Collections contain accounts only, not other
collections.

### The safety story: hide, don't delete

1. **`deduplicate` hides redundant copies. It does not delete them.**
   One survivor stays visible. The other copies stay on disk and drop
   out of normal reads. `--undo <batch-id>` restores them.
2. **Deletion is never required.** msgvault never escalates from
   "hide" to "delete locally" to "delete from the source server" on
   its own. Each step is a separate, opt-in command. Stay on "hide"
   forever if you want. We call this design the **dedup safety
   ladder**: four explicit rungs the user climbs deliberately, with
   no automatic escalation between them.

![Data safety ladder — five rungs. Rung 00 (Backup, default-on) writes a point-in-time database copy before any rung that modifies data. Rung 01 (Scan, deduplicate --dry-run) reports what would change with no data touched. Rung 02 (Hide, deduplicate) soft-deletes redundant copies; reversible via --undo. Rung 03 (Local hard delete, delete-deduped --batch) permanently removes hidden rows from the local archive. Rung 04 (Remote delete, delete-staged) deletes from the source server, source-scoped, moves to source trash by default. A banner reads: deletion is never required — you can run deduplicate as many times as you want and stay on rung 02 forever.](./assets/safety-ladder-concept.png)

The diagram is the mental model for every dedup-related command.
**Each rung is a separate, explicit user action.** Rung 00 (backup)
and rung 02 (hide) happen by default when you run `deduplicate`.
Rungs 03 and 04 happen only when you invoke a different command:
`delete-deduped` or `delete-staged`. msgvault never escalates between
rungs on its own. "Apply dedup" never implies "hard-delete locally."
"Hard-delete locally" never implies "delete from the source server."
"Delete from the source server" never implies "permanently delete
from the source server."

The rest of this document is a HOWTO with worked examples.

## HOWTO

The examples below use a worked scenario with real command shapes.
Substitute your own account and collection names.

### List your accounts

Start by seeing what msgvault knows about:

```sh
msgvault list-accounts
```

Each row is one account. Note the identifier (typically the email
address or source name); every command in this guide takes it.

### Confirm your identities

For sent vs. received to mean anything in old imports, msgvault needs
to know which addresses are "you" inside each account.

List what's confirmed across all accounts:

```sh
msgvault identity list
```

Show one account's identity in detail:

```sh
msgvault identity show me@example.com
```

Add a confirmed identifier to an account's identity:

```sh
msgvault identity add me@example.com me@oldco.example
```

Remove one:

```sh
msgvault identity remove me@example.com me@oldco.example
```

Identity is per-account because an address safe to treat as "you" in
one source can be misleading in another. A collection's identity is
the union of its member accounts' identities, computed at read time.
You don't manage it directly.

### Group accounts into a collection

For a unified view across several accounts (for search, stats, or
dedup), group them into a collection. `All` already exists. Custom
collections are explicit.

Create a `work` collection from two accounts:

```sh
msgvault collection create work --accounts me@oldco.example,me@newco.example
```

List all collections:

```sh
msgvault collection list
```

Show one in detail:

```sh
msgvault collection show work
```

Add or remove members later:

```sh
msgvault collection add work --accounts contractor@example.org
msgvault collection remove work --accounts contractor@example.org
```

Delete a collection when you're done with it (the underlying accounts
and their messages are untouched):

```sh
msgvault collection delete work
```

`All` is auto-managed and immutable — msgvault rejects
`collection delete All` and explicit membership edits on `All`.

### Search or count across a collection

Wherever `--account` works, `--collection` works too. Cross-account
operations always go through a collection boundary.

Search inside one account:

```sh
msgvault search --account me@example.com "dinner friday"
```

Search across the `Personal` collection:

```sh
msgvault search --collection Personal "dinner friday"
```

Get stats for one collection:

```sh
msgvault stats --collection work
```

`--account` and `--collection` are mutually exclusive. msgvault
rejects a collection name passed to `--account` (or an account
identifier passed to `--collection`) with a hint to use the right
flag.

### Run dedup safely

Three flavors of dedup, ordered by risk:

```sh
msgvault deduplicate                       # per-account, each in isolation
msgvault deduplicate --account <name>      # one account
msgvault deduplicate --collection <name>   # cross-account, inside one collection
```

The unscoped form is the safest default. It processes each account
independently and never crosses source boundaries. Cross-account
dedup is higher-risk: it can collapse duplicates between independent
archives whose provenance you may want to preserve. So it requires
an explicit `--collection`. To dedup across every account, write
`--collection All`.

### Walking the safety ladder

The recommended sequence:

**1. Scan first (rung 01).** See what dedup would do before it does
it:

```sh
msgvault deduplicate --collection Personal --dry-run
```

The output shows duplicate groups, which copy is the proposed
survivor, and why it was chosen. Nothing is modified.

**2. Apply dedup (rung 02).** When the dry-run output looks right:

```sh
msgvault deduplicate --collection Personal
```

Before modifying any row, msgvault writes a point-in-time database
backup alongside your DB file (e.g.
`msgvault.db.dedup-backup-20260503-091500`). Opt out only with
`--no-backup`. The command then hides redundant copies and prints a
batch ID like `dedup-2026-05-03-1`.

The hidden copies are excluded from search, the TUI, vector and
hybrid retrieval, the API, MCP responses, exports, and stats — but
they remain on disk.

**3. Undo if you change your mind (still rung 02).** Use the batch ID
from step 2:

```sh
msgvault deduplicate --undo dedup-2026-05-03-1
```

Undo restores the rows hidden by that batch and cancels any pending
remote-deletion manifest the batch staged. It does not reverse every
side effect — for the precise guarantees, see the spec.

**4. Hard-delete locally (rung 03 — opt-in, irreversible).** Only run
this when you're confident you'll never want the hidden rows back:

```sh
msgvault delete-deduped --batch dedup-2026-05-03-1
```

This permanently removes rows the named batch hid. It refuses to
operate on rows it didn't hide. The `--all-hidden` form purges every
hidden row from every batch and requires interactive confirmation.
Backup runs again before the purge unless you pass `--no-backup`.
Undo cannot recover purged rows.

**5. Delete from the source server (rung 04 — opt-in, does not touch
the local archive).** Cross-source duplicate groups produce zero
remote-deletion entries — only same-source pairs stage. List, inspect,
and execute pending deletion manifests:

```sh
msgvault list-deletions
msgvault show-deletion <batch-id>
msgvault delete-staged <batch-id>
```

By default, `delete-staged` moves messages to the source's trash
(e.g. Gmail's ~30-day Gmail/Trash). Permanent removal requires an
explicit `--permanent` flag and interactive confirmation. As a v1
guardrail, the whole remote-delete command is gated behind an
environment variable (`MSGVAULT_ENABLE_REMOTE_DELETE=1`); read-only
inspection (`list-deletions`, `show-deletion`, `--list`, `--dry-run`)
is always permitted.

To cancel a staged batch before it executes:

```sh
msgvault cancel-deletion <batch-id>
```

### Common scenarios

#### "I just want to clean up duplicates inside my Gmail account."

```sh
msgvault deduplicate --account me@example.com --dry-run
msgvault deduplicate --account me@example.com
```

Stay on rung 02. You're done.

#### "I imported the same mailbox twice from two sources and want one clean view."

Create a collection containing both, scan it, apply dedup. msgvault
leaves the originals on each source server untouched.

```sh
msgvault collection create gmail-plus-mbox \
  --accounts me@example.com,me@example.org
msgvault deduplicate --collection gmail-plus-mbox --dry-run
msgvault deduplicate --collection gmail-plus-mbox
```

#### "I want to hard-delete the duplicates I hid last week to reclaim disk."

```sh
msgvault list-deletions          # find the batch ID
msgvault delete-deduped --batch dedup-2026-04-26-1
```

#### "I want to actually remove the duplicates from Gmail."

This is rung 04. Same-source only — only duplicate pairs that lived
on the same Gmail account stage remote-delete entries. Cross-source
groups (e.g. one copy on Gmail, another in mbox) stage nothing.

```sh
msgvault list-deletions
msgvault show-deletion <batch-id>
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged <batch-id>
```

The default moves messages to Gmail/Trash, where they're recoverable
for ~30 days. If you want them gone for good, add `--permanent` and
confirm interactively.
