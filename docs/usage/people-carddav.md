---
last_edited: "2026-10-06"
title: CardDAV Contacts
description: Bring address-book contacts into msgvault, publish selected profiles, and resolve competing edits.
---

CardDAV connects msgvault profiles to an external address book. Import contacts,
keep subscribed books in sync, and publish selected saved people so their
contact details are available in your usual contacts app.

Msgvault is a CardDAV **client**. Each connection uses one account and can
contain several address books. You can configure multiple independent
connections. Publishing, unpublishing, and choosing the local side of a conflict can change that external address book.

## Connect an address book

In the Web UI, open **Settings → CardDAV account** to test and save a connection.
Select an existing connection or choose **Add connection** for another account.
The CLI can discover the same accounts:

```bash
msgvault add-carddav https://contacts.example.com/dav/ you@example.com
msgvault add-carddav https://work-contacts.example.com/dav/ you@example.com --connection work
msgvault carddav connections
msgvault carddav books --connection work
```

Use the base URL supplied by your CardDAV service. The command prompts for the
password; it also accepts a piped password on standard input. The daemon stores
the credential separately from `config.toml`, in its token directory's
`carddav.json` file for `default`, or
`carddav-connections/<name>/carddav.json` for a named connection. Saving one
connection preserves every other connection. See the [configuration reference](../configuration.md#carddav)
for name rules and trusted private destinations.

`--disabled` saves the connection without enabling synchronization. Add
`--schedule "*/30 * * * *"` for background sync every 30 minutes. You can also
change connection settings and scheduling in the Web UI.

Servers that challenge with HTTP Digest are supported. After the first
challenge, the client sends Digest authentication on later requests without
trying Basic again. Basic authentication continues to work for servers that
use it. A 401 with an unsupported or malformed challenge fails as an
authentication error.

### Private servers

For a CardDAV server reachable only at a private address, the operator can
approve one exact HTTPS origin and pin its address in the daemon's local
`config.toml` before adding the account:

```toml
[carddav]
trusted_origin = "https://contacts.example:8443"
trusted_addresses = ["10.1.2.3"]
```

Use the same origin in the account's base URL, including its port. The pin
replaces DNS for that origin, so the container does not need to resolve the
hostname. TLS still checks the certificate against the hostname. A failed pin
does not fall back to DNS. A trailing `/` in `trusted_origin` is accepted.
If the account uses a different hostname or port, msgvault ignores the pin for
that account and uses normal DNS and destination checks. The mismatch does not
stop daemon startup, and private destinations remain blocked for that account.

Only private addresses from the [approved ranges](../configuration.md#carddav)
are accepted; loopback and link-local addresses remain blocked. The Web UI and
account API cannot grant private network access. After changing this local
policy, restart the daemon or test and save the account again. Keep the config
file readable only by the daemon's operator.

## Google Contacts

Select **Google Contacts** under **Settings → CardDAV account → Provider**.
Enter your Google account email and, if needed, the name of an OAuth app from
`[oauth.apps]`. Google supplies the discovery URL; no password is needed.
The contacts account and OAuth app are independent of your mail accounts.
When a stored Google authorization matches both the account and selected app,
setup reuses it and requests the combined permissions. Otherwise, setup stores
separate CardDAV credentials. Use a different OAuth client when you want Google
consent and revocation to be independent as well.

1. Configure a Google OAuth client following the [OAuth setup guide](../guides/oauth-setup.md),
   and enable the **Google Contacts CardDAV API** in the same Google Cloud
   project. The Gmail and Calendar APIs do not cover contacts; without it,
   **Save CardDAV account** fails during discovery.
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
Each named connection has its own binding record. An account can belong to
only one CardDAV connection, even when different OAuth apps authorize it.
The daemon refreshes expired access tokens during requests and saves refreshed
tokens. After revoking access, connect Google again to reauthorize.

Google's canonical entry point is
`https://www.googleapis.com/.well-known/carddav`. Msgvault discovers the contact
collection from Google's response and uses vCard 3.0 and incremental sync.
Test and save the account again to rediscover its URLs; Google recommends
rediscovery every two to four weeks. See [Google's CardDAV reference](https://developers.google.com/people/carddav).

## Microsoft contacts

Microsoft 365 and Outlook.com do not support CardDAV. msgvault reads and
writes their contacts through Microsoft Graph instead, with the same roles,
publishing and conflict review as a CardDAV server. The default **Contacts**
folder and each folder inside it is one address book. Microsoft Graph does
not list contact folders outside **Contacts**. Graph can't make a delete
conditional, so an Outlook edit made in the moment before an unpublish is
deleted without a conflict; the contact stays in Outlook's Deleted Items.

1. Set up the Microsoft app registration from the
   [Microsoft Graph mail setup](../guides/oauth-setup.md#microsoft-graph-mail-sync), and add the delegated
   Microsoft Graph permission `Contacts.ReadWrite`.
2. Sign in and save the connection:

   ```bash
   msgvault add-carddav --microsoft you@example.com --schedule "*/30 * * * *"
   msgvault carddav books
   ```

   `add-carddav --microsoft` opens a browser for Microsoft sign-in. Use
   `--headless` to sign in with a device code instead. The token is saved as
   `tokens/mscontacts_<email>.json`, separate from mail and Teams tokens.
   For a remote daemon, copy that file to the daemon's token directory,
   preserving private file permissions, before the connection is saved.
3. Review the roles of each folder, then run `msgvault sync-carddav`.

To sign in without saving a connection, run
`msgvault carddav authorize-microsoft you@example.com`. Then save the
connection with the **Microsoft 365 or Outlook.com** provider in
**Settings → CardDAV account**. An existing connection keeps its schedule.

Graph stores fewer fields than vCard. msgvault maps names, nickname, email
addresses, phone numbers, organization, job title, postal addresses, a
birthday with a year, notes, and categories. msgvault saves the full vCard of a card it
writes in a hidden property of the contact. After an edit in Outlook,
Outlook's fields win, and the saved vCard adds back what Outlook never held:
every property msgvault doesn't map, such as a website, a photo, an
anniversary, a birthday without a year or a phonetic name, and emails, phones
and addresses past Outlook's limits of three email addresses, two business
phones, two home phones and three addresses. Fax and pager numbers count as
such phones. A value that stays in its Outlook field keeps its saved form and
labels; a moved value takes the form Outlook gives it. Outlook holds one
display name, name, nickname, title, organization, note and birthday, so
msgvault logs a second one when it publishes the card. A card whose vCard
exceeds Graph's 4 MB write limit, for example with a large photo, is refused.
A publish, approval or conflict resolution names the person in its error. A
sync reports a fixed message, and the daemon log names the person with a
warning, `CardDAV contact is over Outlook's 4 MB limit`, and its `person_id`.
The CLI and Web UI can't remove
stored media yet, so use the profile API. Read the profile, note its `ETag`
and the `envelope.id` of each entry in `media` (`byte_size` shows its size),
then supersede the ones to drop:

```bash
curl -i -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  http://localhost:8080/api/v1/people/42/profile

curl -X PATCH -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  -H 'If-Match: "person-42-r7"' -H "Content-Type: application/json" \
  -d '{"media": {"supersede": [123]}}' \
  http://localhost:8080/api/v1/people/42/profile
```

The next sync publishes the smaller card.
Outlook's contact photo is not synced.
After any change in a folder, the next sync lists the whole folder again, one
request for each 100 contacts, with each card's saved vCard and its photo.
Graph can leave a large saved vCard out of a listing; each such contact costs
one more request to read it alone. A
sync stops at 256 MB, so a folder with many large photos may need fewer
published photos.
If Outlook returns a published contact without its saved vCard even when read
alone, the sync fails instead of overwriting the person.

## Choose what each book does

On first discovery of the archive's first connection, msgvault selects the
first address book that supports creation, updates, and deletion and subscribes
to it. Additional connections start with identity lookup only and preserve the
global write target. All discovered books initially allow identity lookup. Review these roles before
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
false. Only one book across all connections can be the write target. Msgvault
rejects incompatible role changes when publications, pending writes, or conflicts still depend on
the book; resolve those dependencies first.

## Sync and publish selected people

Pull changes and reconcile existing publications with:

```bash
msgvault sync-carddav
```

Unqualified sync runs every configured, enabled connection. Use
`msgvault sync-carddav --connection work` to sync one connection, including a
disabled connection for manual repair. Add `--full` for a full address-book
reconciliation. A failing connection does not stop the others; partial or failed
aggregate results make the CLI exit nonzero. With no enabled connections,
unqualified sync fails as unavailable.

Settings shows the selected connection's status, books and history, and its
sync buttons run that connection. Switching connections clears unsaved account
fields, passwords and pending test or sign-in results. Operations shows all
connections' attributed runs and offers aggregate sync. Its action is disabled
while any CardDAV run is active. Schedules, retry gates and run leases are
independent per connection.

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

### Recover a connection removed from config

Removing a connection's config table leaves its account, books, contacts and
publications in the archive. `msgvault carddav connections` lists it as
**orphaned**, and Settings shows **Configuration missing**. Publishing and
unpublishing through that connection are unavailable until it is restored.
Existing publications can also block moving the write target to another book.

Restore the original config table, or select the orphaned connection in Settings
and save it with the same server URL and username. The CLI can also restore it:

```bash
msgvault add-carddav https://work-contacts.example.com/dav/ you@example.com --connection work
```

Use the original connection name (`default` for `[carddav]`). For Google, use
`msgvault add-carddav --google you@example.com --connection work` and the original
`--oauth-app` when one was selected. Restoring the same identity reconnects the
existing account and books. Settle pending publications and conflicts before
moving the write target. Adding the account under a different name is rejected.

### Select connections through the API

| Request | Without `connection` | With `connection` |
|---|---|---|
| `GET /api/v1/carddav/connections` | Summaries of configured and orphaned connections | No selector |
| Account test/save request body | Save or test `default` | Save or test the named connection |
| Status, runs and books query | Aggregate status or all rows | Selected connection |
| Sync request body | All configured enabled connections | One saved connection, including disabled connections |

For example, `POST /api/v1/carddav/sync` with `{"connection":"work"}` selects
one connection. Invalid or unknown selectors return HTTP 400 before changing
state; test and save allow a new valid name. Explicit sync keeps the existing
HTTP error behavior. An aggregate sync with at least one enabled connection
returns HTTP 200 with `status` (`succeeded`, `partial` or `failed`) and
`connections` outcomes, even when every connection fails. Consumers must inspect
those fields, including callers using `pkg/client`. This is a breaking change
from the single-account API: omitted-selector sync no longer reports a provider
throttle as HTTP 503 or missing Google authorization as HTTP 502. Send
`{"connection":"default"}` to retain explicit single-connection error behavior
and to sync a disabled default account manually. Outcomes include safe errors,
counts, connection identity and the run ID when this request started a run; a busy or unavailable connection does
not create a run.

Publication and conflict routes use global IDs and route network operations to
the persisted owner's connection. Store-only views stay available even when a
connection's credential needs repair.

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
