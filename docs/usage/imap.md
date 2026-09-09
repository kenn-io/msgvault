---
last_edited: "2026-09-08"
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
