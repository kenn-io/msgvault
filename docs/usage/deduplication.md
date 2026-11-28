---
last_edited: 2026-08-30
title: Deduplication
description: Find and merge duplicate messages across accounts and collections with a reversible, five-rung safety ladder that never deletes anything by default.
---

Over time, an archive gathers overlapping sources such as a current Gmail sync, an old mbox export, IMAP backups, and chat history. The same message often appears more than once, and duplicates start crowding your search results. Use `msgvault deduplicate` to collapse each set of copies into a single visible survivor while keeping every source's provenance intact.

**Deduplication hides redundant copies rather than deleting them.** One survivor stays visible. The other copies drop out of normal reads and stay on disk, and `--undo` restores them. Removing data is always a separate, explicit step that you opt into.

## How duplicates are detected

Detection runs in two passes:

1. **Message-ID pass.** msgvault groups messages by their RFC 822 `Message-ID` header, so each distinct ID forms one duplicate group. This pass is the default and works reliably for email.
2. **Content-hash pass.** When you pass `--content-hash`, msgvault also groups messages by a normalized hash of their raw MIME content. This catches duplicates whose `Message-ID` headers were stripped or rewritten in transit, as well as messages that never had a `Message-ID`.

The two passes run in sequence and do not merge into one transitive set. If a content-hash group contains two distinct Message-ID survivors, msgvault keeps both. If a group mixes a Message-ID survivor with a sent copy that has no Message-ID, msgvault skips it so the sent-copy protection below is never bypassed.

## Which copy survives

Survivor selection is deterministic and explainable, and dry-run output prints the reasoning. Selection runs in two stages.

**Stage 1, sent-copy eligibility.** If any message in a group looks like a copy you sent, only sent copies are eligible to survive, and received copies drop out before tie-breaking. A message looks sent when it carries a Gmail `SENT` label, when ingest metadata flagged it as from you, or when its `From` address matches a confirmed [identity](/docs/usage/multi-account/#identities) for that account. Because "I sent this" is harder to recover from data than "I received this," a richer received copy is never allowed to silently win.

**Stage 2, priority list.** Among the eligible copies, msgvault prefers, in order:

1. Source type, following `--prefer` or the default order `gmail,imap,msmail,mbox,emlx,hey`.
2. Presence of the complete raw MIME payload.
3. Fewer Apple Mail attachment placeholders.
4. More attachments.
5. An attachment-presence signal when attachment counts tie.
6. A larger payload.
7. Higher source metadata quality.
8. Richer label or folder metadata.
9. Earlier archive timestamp.
10. A stable row ID, as the final tie-breaker.

Earlier rules win outright, and later rules apply only when all earlier ones tie.
The attachment-count, attachment-presence, and payload-size rules apply only
when every eligible copy has raw MIME and all their normalized MIME hashes
match. A shared `Message-ID` alone cannot make those payload-completeness
signals authoritative.

The placeholder rule handles Apple Mail `.partial.emlx` copies, where an
attachment part carries an `X-Apple-Content-Length` header instead of its
bytes, next to a copy with some of those attachments restored. It applies only
in `Message-ID` groups where every eligible copy from the preferred source type
has raw MIME that parses into the same parts with the same headers and content,
except at placeholder parts. Copies that restore the same part must restore the
same bytes; otherwise the group skips this rule.

Source metadata quality counts three independent facts, one point each: a
native Gmail, IMAP, or Microsoft Mail message ID, threading evidence, and an
RFC822 `Message-ID`.
Threading evidence means a Gmail provider conversation ID, preserved Google
Groups grouping derived from a valid exported `X-GM-THRID`, an `In-Reply-To`
header in archived metadata, or a resolved reply parent. Gmail conversation
IDs count for both historical and newly synced copies. A conversation ID equal
to its message ID earns no point, because it may be a generated fallback and
archived rows cannot distinguish that fallback from a genuine single-message
thread. Generic import conversation IDs do not count. This comparison applies
even when normalized MIME hashes differ.

The survivor inherits the union of labels from the copies it replaces, and
backfills raw MIME from a non-survivor if it was missing the original payload.

```mermaid
flowchart TD
    A[Duplicate group] --> B{Any sent copy?}
    B -- Yes --> C[Keep sent copies eligible]
    B -- No --> D[Keep all copies eligible]
    C --> E[1. Source preference]
    D --> E
    E --> F[2. Raw MIME present]
    F --> P{Apple Mail copies match outside placeholders?}
    P -- Yes --> Q[3. Fewer attachment placeholders]
    P -- No --> G
    Q --> G{All eligible normalized MIME hashes match?}
    G -- Yes --> H[4. Attachment count]
    H --> I[5. Attachment presence]
    I --> J[6. Payload size]
    J --> K[7. Source metadata quality]
    G -- No --> K
    K --> L[8. Label or folder richness]
    L --> M[9. Earlier archive timestamp]
    M --> N[10. Stable row ID]
```

## Choose a scope

You can run deduplication at three levels, ordered by how much they compare:

```bash
msgvault deduplicate                       # per-account, each source in isolation
msgvault deduplicate --account <name>      # one account
msgvault deduplicate --collection <name>   # cross-account, inside one collection
```

The unscoped form is the safest default. It processes each account independently and never crosses source boundaries. Cross-account deduplication carries more risk, because it can collapse copies that live in independent archives whose separate provenance you may want to keep, so it requires an explicit `--collection`. To deduplicate across every account, pass the built-in collection: `--collection All`.

This boundary protects sent-message provenance. If Alice's Sent copy and Bob's Inbox copy share one `Message-ID`, both must survive, and per-account scope guarantees they do.

## The safety ladder

Every dedup-related command sits on one of five rungs, from 00 through 04. Rung 00 is an automatic backup, and you climb the remaining rungs deliberately, one explicit action at a time. msgvault never escalates from one rung to the next on its own: applying deduplication never triggers a local hard delete, and a local hard delete never triggers a remote delete.

| Rung | Action | Command | Reversibility |
|---|---|---|---|
| 00 | Backup (automatic, SQLite-only) | runs before rungs 02 and 03 | point-in-time backup; PostgreSQL uses `pg_dump` |
| 01 | Scan | `deduplicate --dry-run` | no data touched |
| 02 | Hide | `deduplicate` | reversible with `--undo <batch-id>` |
| 03 | Local hard delete | `delete-deduped --batch <batch-id>` | irreversible locally |
| 04 | Remote delete | `delete-staged` | archived content retained; Gmail/IMAP default to Trash, `--permanent` is irreversible |

!!! tip "Deletion is never required"
    You can run `deduplicate` as many times as you like and stay on rung 02 forever. Rungs 03 and 04 only ever run when you invoke a different command.

- **Rung 00, backup.** Before `deduplicate` or `delete-deduped` modifies any row, msgvault writes a point-in-time copy of the database alongside the live file (for example `msgvault.db.dedup-backup-20260503-091500`) using SQLite `VACUUM INTO`. msgvault builds the copy under a private temporary path and moves it to the advertised backup filename only after it succeeds, so cancellation cannot leave an incomplete file looking like a valid backup. Pass `--no-backup` to opt out. Scanning modifies nothing and triggers no backup. This automatic backup is SQLite-only; on a PostgreSQL archive, `deduplicate` refuses the built-in backup (there is no `VACUUM INTO` equivalent). On PostgreSQL, snapshot the database out-of-band with `pg_dump` first, then rerun with `--no-backup`.
- **Rung 01, scan.** `deduplicate --dry-run` reports the duplicate groups it found, the proposed survivor for each, and the reason. It modifies nothing.
- **Rung 02, hide.** `deduplicate` applies the scan, hides pruned copies from normal reads while keeping them on disk, and prints a batch ID. Run `--undo <batch-id>` to restore them.
- **Rung 03, local hard delete.** `delete-deduped` permanently removes hidden rows from your local archive to reclaim disk space. It acts on named batches with `--batch` and refuses to touch rows it did not hide. All selected batches commit as one transaction, so cancellation rolls the entire selection back. `--all-hidden` purges every hidden row and always prompts for confirmation. Undo cannot recover purged rows.
- **Rung 04, remote delete.** This rung has two parts, stage and execute, and only staging is dedup-specific. To stage, run `deduplicate --delete-dups-from-source-server`; it writes pending deletion manifests only for Gmail duplicates whose loser and survivor share a source and have matching normalized raw MIME. A group spanning two sources or lacking content equivalence stages nothing. To execute, run `delete-staged`, the generic executor for any staged deletion manifest (not only dedup). It acts on the source server, retains archived content, and records source-deletion state. Inspect pending batches first with `delete-staged --list`, then target one batch with `delete-staged <batch-id>`. Execution requires durable `[deletion] remote_enabled = true` consent in the invoking CLI config, or `MSGVAULT_ENABLE_REMOTE_DELETE=1` for one command. The same-source and content-equivalence restrictions live in the staging step, not in `delete-staged`. See [Deleting Email](/docs/usage/deletion/) for how remote deletion works.

!!! note "What \"hidden\" means"
    A hidden copy is excluded from search, the Web UI, the TUI, vector and hybrid retrieval, the API, MCP responses, exports, and stats, while still living on disk. Every read path applies the same visibility rule, so a hidden duplicate cannot leak back into results through one backend.

## A worked walkthrough

Follow this sequence to scan, apply, and optionally undo or hard-delete duplicates:

1. **Scan first.** Check what deduplication would change before modifying anything:

    ```bash
    msgvault deduplicate --collection Personal --dry-run
    ```

2. **Apply the scan.** When the dry run looks right, run `deduplicate` to write a backup, hide redundant copies, and print a batch ID:

    ```bash
    msgvault deduplicate --collection Personal
    ```

    Batch IDs look like `dedup-20260503-091500-run-0d4cb6f1`. Save the ID printed by your run for the steps below.

3. **Undo if you change your mind.** While you are still on rung 02, you can reverse the batch completely:

    ```bash
    msgvault deduplicate --undo <batch-id>
    ```

4. **Hard-delete locally only when you are sure.** This step is rung 03 and cannot be undone:

    ```bash
    msgvault delete-deduped --batch <batch-id>
    ```

## Common scenarios

**Clean up duplicates inside one Gmail account.** Scan, apply, and stop on rung 02:

```bash
msgvault deduplicate --account me@example.com --dry-run
msgvault deduplicate --account me@example.com
```

**Merge a mailbox you imported twice into one clean view.** Put both sources in a collection, scan it, and apply. The originals on each source server remain untouched:

```bash
msgvault collection create gmail-plus-mbox --accounts me@example.com,me@example.org
msgvault deduplicate --collection gmail-plus-mbox --dry-run
msgvault deduplicate --collection gmail-plus-mbox
```

**Reclaim disk from duplicates you hid earlier.** Find the batch ID and hard-delete it (rung 03):

```bash
msgvault delete-deduped --batch <batch-id>
```

**Remove duplicates from the source server.** Stage during a dedup run, review the pending manifests, and execute the specific batch. Staging only covers same-source Gmail pairs with matching normalized raw MIME; cross-source or non-equivalent pairs stage nothing. `delete-staged` itself is the generic deletion executor, and execution is gated:

```bash
# Stage same-source Gmail duplicates for remote deletion during a dedup run
msgvault deduplicate --collection Personal --delete-dups-from-source-server

# Review the pending deletion manifests
msgvault delete-staged --list

# Execute one batch against the source server after durable config consent
msgvault delete-staged <batch-id>

# Or grant consent for this command only
MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged <batch-id>
```

Starting in v0.20.0, remote deletion remains permanently opt-in. The invoking
CLI can enable it durably with `[deletion] remote_enabled = true` or for one
command with `MSGVAULT_ENABLE_REMOTE_DELETE=1`. Both mechanisms are permanent,
with no planned automatic removal of the guardrail. A remote daemon's own
`[deletion]` section is not policy for a command invoked elsewhere. Staging,
listing, inspecting, and dry-running deletion batches remain ungated.

See [Deleting Email](/docs/usage/deletion/) for the full workflow.

## What undo restores

`deduplicate --undo <batch-id>` restores the rows that batch hid and cancels any pending remote-deletion manifest the batch staged that has not yet executed. If you pass several `--undo` flags, msgvault undoes those batches in order. You cannot combine `--undo` with `--account`, `--collection`, or `--dry-run`.

Undo is not full time travel. It does not:

- Reverse the label union applied to the survivor.
- Reverse raw MIME backfilled onto the survivor from a non-survivor.
- Recover rows already purged with `delete-deduped`.
- Reverse remote deletions already executed against a source.

Derived indexes catch up on their next rebuild rather than right away.

## After a large local delete

A `delete-deduped` purge updates the canonical archive, while derived analytics and vector caches can still hold stale entries. If you use those caches, rebuild them:

```bash
msgvault build-cache --full-rebuild
msgvault embeddings build --full-rebuild
```

## Command reference

See the [CLI Reference](/docs/cli-reference/#deduplicate) for the complete flag list on `deduplicate`, `delete-deduped`, `identity`, and `collection`.
