---
title: IMAP Folder Sync
description: List IMAP folders and choose which folders msgvault scans during a sync.
---

# IMAP Folder Sync

By default, msgvault scans every selectable folder in an IMAP account. You can
limit a sync to the folders you need or skip folders that are large or
unimportant. This is useful when you want to try msgvault with a small part of
an account before starting a complete archive.

Folder filters work with both `sync-full` and `sync`. They affect IMAP accounts
only.

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

Use `--skip-folders` to scan every folder except the ones you name:

```bash
msgvault sync-full you@example.com \
  --skip-folders Trash \
  --skip-folders Spam
```

You can combine include and exclude filters. msgvault first keeps the folders
named by `--folders`, then removes any named by `--skip-folders`:

```bash
msgvault sync-full you@example.com \
  --folders INBOX \
  --folders Archive \
  --folders "Archive/Newsletters" \
  --skip-folders "Archive/Newsletters"
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
