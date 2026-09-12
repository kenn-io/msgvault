---
last_edited: "2026-09-09"
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

## Google Contacts

Select **Google Contacts** under **Settings → CardDAV account → Provider**.
Enter your Google account email and, if needed, the name of an OAuth app from
`[oauth.apps]`. Google supplies the discovery URL; no password is needed.
The contacts account and OAuth app are independent of your mail accounts.
When a stored Google authorization matches both the account and selected app,
setup reuses it and requests the combined permissions. Otherwise, setup stores
separate CardDAV credentials. Use a different OAuth client when you want Google
consent and revocation to be independent as well.

1. Configure a Google OAuth client following the [OAuth setup guide](../guides/oauth-setup.md).
   For Web UI sign-in, register the Web UI's root URL, including its trailing
   slash, as an authorized redirect URI in a **Web application** OAuth client.
   The settings form shows the exact URL. Remote Web UIs need HTTPS; loopback
   HTTP is accepted for local use.
2. Click **Connect Google**. A new window opens for Google sign-in. Choose the
   requested account and keep existing permissions checked while granting
   contacts access. Return to settings when the window closes.
3. Click **Test CardDAV connection**, then **Save CardDAV account**. Review the
   discovered book's roles before the first sync.

The daemon requests Google's `https://www.googleapis.com/auth/carddav` scope
and `userinfo.email` for account verification. It preserves Google permissions
already recorded in the account's token, including Gmail and Calendar access.
A mail token issued by another app, or with no recorded client ID, does not
block setup and is left untouched. Once a separate CardDAV token exists, setup
continues to use it even if a matching mail token is added later.
An account mismatch, missing
permission, or expired callback leaves the saved token intact.
Sign-in expires after ten minutes; use **Cancel sign-in** to stop waiting sooner.

For terminal setup, use a Desktop application OAuth client, or register the
[terminal callback](../guides/oauth-setup.md#step-4-create-oauth-client-credentials)
in your Web application client, then run:

```bash
msgvault carddav authorize-google you@example.com
msgvault add-carddav --google you@example.com --schedule "*/30 * * * *"
msgvault sync-carddav
```

`authorize-google` opens the system browser and prints the saved token path. `--no-browser` prints the sign-in
URL instead; the callback still returns to the machine running the command.
Both commands accept `--oauth-app <name>`. Use the same app for authorization
and the CardDAV connection.

Web UI authorization stores the token on the daemon. Terminal authorization
stores it on the machine running the command. For a remote daemon, authorize
on a browser-equipped machine with the same OAuth client, then copy the account's
JSON token file to the same relative location in the daemon's token directory,
preserving private file permissions. Shared authorizations use the existing
`tokens/<email>.json` file. Separate authorizations use
`tokens/carddav-google/<SHA-256 of the OAuth app name>/<email>.json`; the default
app uses the hash of an empty name. The `carddav.json` connection
record references that account and app; it does not contain a Google password.
The daemon refreshes expired access tokens during requests and saves refreshed
tokens. After revoking access, connect Google again to reauthorize.

Google's canonical entry point is
`https://www.googleapis.com/.well-known/carddav`. Msgvault discovers the contact
collection from Google's response and uses vCard 3.0 and incremental sync.
Test and save the account again to rediscover its URLs; Google recommends
rediscovery every two to four weeks. See [Google's CardDAV reference](https://developers.google.com/people/carddav).

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
| `GET /api/v1/carddav/publications/{person_id}/preview` | Return the exact vCard the next write would send and its approval token |
| `POST /api/v1/carddav/publications/{person_id}/approve` | Publish with a reviewed `approval_token` |
| `DELETE /api/v1/carddav/publications/{person_id}` | Remove their publication |

Publication state is `unpublished`, `published`, `pending`, or `conflict`.
`pending` means the operation has not been settled; sync recovers pending work
before importing changes. Wait for the resulting state before assuming a write
or removal has finished.

The vCard includes supported contact/profile fields. Private Notes map to
`NOTE`, so review them before publishing. Generated person briefs stay in the
archive and are not included in CardDAV.

### Review inferred changes before they are exported

Profile facts that msgvault inferred from messages or enrichment, rather than
facts you declared, are never sent to the address book without a review of the
exact card. The first publication and every later reconciliation compare the
inferred facts against the last approved export. When they differ, sync skips
that person, the publication response reports
`inference_review_required: true`, and a plain publish fails with HTTP 409
`carddav_inference_review_required`.

To review and approve, preview the card and then publish with its token:

```bash
msgvault person publish 7 --preview
msgvault person publish 7 --approve <approval_token>
```

`--preview` prints JSON with the full `vcard`, an `approval_token`, and
`review_required`. The token binds the approval to that exact card, the
address book, and the current inferred facts. If any of them change before
approval, the approve request fails with HTTP 409 `carddav_review_stale`; run
the preview again and review the new card. The preview also covers a pending
create that has not settled and a conflict whose `keep_local` side would
export inferred changes. When `kind` is `conflict`, approval records the review
without publishing or resolving the conflict. Then run
`msgvault carddav conflicts resolve <conflict_id> keep_local`, using the preview's
`conflict_id`, to publish the reviewed card. API clients follow approval with
`POST /api/v1/carddav/conflicts/{id}/resolve` and `{"choice":"keep_local"}`.

The preview route is the one CardDAV response that returns a raw vCard, and
it can include Private Notes. The Directory UI directs you to the CLI review
commands when approval is required.

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
