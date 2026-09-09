---
last_edited: "2026-09-08"
title: CardDAV Contacts
description: Bring address-book contacts into msgvault, publish selected profiles, and resolve competing edits.
---

CardDAV connects msgvault profiles to an external address book. Import contacts,
keep subscribed books in sync, and publish selected saved people so their
contact details are available in your usual contacts app.

Msgvault is a CardDAV **client**. It connects to one configured account, which
can contain several address books. Publishing, unpublishing, and choosing the
local side of a conflict can change that external address book.

## Connect an address book

In the Web UI, open **Settings → CardDAV account** to test and save a connection.
The CLI can discover the same account:

```bash
msgvault add-carddav https://contacts.example.com/dav/ you@example.com
msgvault carddav books
```

Use the base URL supplied by your CardDAV service. The command prompts for the
password; it also accepts a piped password on standard input. The daemon stores
the credential separately from `config.toml`, in its token directory's
`carddav.json` file.

`--disabled` saves the connection without enabling synchronization. Add
`--schedule "*/30 * * * *"` for background sync every 30 minutes. You can also
change connection settings and scheduling in the Web UI.

## Choose what each book does

On first discovery, msgvault selects the first address book that supports
creation, updates, and deletion as the write target and subscribes to it. All
discovered books initially allow identity lookup. Review these roles before
your first sync:

| Role | Effect |
|---|---|
| **Write target** / “Publish here” | The one book that receives explicitly published profiles. It must also be subscribed. |
| **Subscribed** / “Sync contacts” | Import unbound cards as profiles and synchronize later contact changes. |
| **Lookup source** | Retain cards for identity lookup without importing every unbound card as a profile. |

Set roles with the Web UI or CLI:

```bash
msgvault carddav books set-role 3 --write-target --subscribed --lookup-source
msgvault carddav books set-role 4 --lookup-source
```

`set-role` replaces **all three roles** for the book. Omitted flags become
false. Only one book can be the write target. Msgvault rejects incompatible
role changes when publications, pending writes, or conflicts still depend on
the book; resolve those dependencies first.

## Sync and publish selected people

Pull changes and reconcile existing publications with:

```bash
msgvault sync-carddav
```

Use `msgvault sync-carddav --full` for a full address-book reconciliation.
Settings shows active and recent runs, counts, and errors; Operations also
provides CardDAV status and advertised sync actions.

To publish a person, open their saved profile in **Directory** and turn on
**Publish person to CardDAV**. A contact UID identifies that published person
across syncs. Later profile changes are reconciled to the write target.
Turning publication off removes the remote card while retaining the local
profile.

The same actions are available from the CLI:

```bash
msgvault person publish 7
msgvault person unpublish 7
```

For integrations, the publication API is:

| Request | Effect |
|---|---|
| `GET /api/v1/carddav/publications/{person_id}` | Read publication state |
| `POST /api/v1/carddav/publications/{person_id}` | Publish the saved person |
| `DELETE /api/v1/carddav/publications/{person_id}` | Remove their publication |

Publication state is `unpublished`, `published`, `pending`, or `conflict`.
`pending` means the operation has not been settled; sync recovers pending work
before importing changes. Wait for the resulting state before assuming a write
or removal has finished.

The vCard includes supported contact/profile fields. Private Notes map to
`NOTE`, so review them before publishing. Generated person briefs stay in the
archive and are not included in CardDAV.

## Resolve competing edits

If both msgvault and the address book changed the same card, msgvault records a
conflict for review. It also records edit/delete conflicts instead of silently
choosing a side.

```bash
msgvault carddav conflicts list
msgvault carddav conflicts show 18
```

The detail compares the common base, local profile, and remote card. These are
bounded summaries; they do not show every vCard field. In the Web UI, open the
conflict in Settings and review the decision. From the CLI, choose explicitly:

```bash
msgvault carddav conflicts resolve 18 keep_local
msgvault carddav conflicts resolve 18 keep_remote
```

`keep_local` applies the local choice to the remote card. `keep_remote` accepts
the remote choice locally. Either choice may represent a deletion. If the
remote state changed again, reload the conflict and review the new comparison.
An unresolved conflict prevents conflicting publication work from proceeding.

A saved person with an active CardDAV publication cannot be merged with another
profile. Remove the publication and settle pending work before following the
[profile merge workflow](/docs/usage/people/#merge-duplicate-profiles-and-reverse-a-merge).
