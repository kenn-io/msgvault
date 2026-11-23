---
last_edited: "2026-11-22"
title: CardDAV Contacts
description: Bring address-book contacts into msgvault, publish selected profiles, and resolve competing edits.
---

Connect msgvault profiles to an external address book with CardDAV.
You can import contacts, keep subscribed books in sync, and publish selected
saved people so their contact details appear in your usual contacts app.

msgvault acts as a CardDAV **client**. Each connection uses one account and can
hold several address books, and you can configure multiple independent
connections. Publishing, unpublishing, and choosing the local side of a
conflict can modify that external address book.

## Connect an address book

To test and save a connection in the Web UI, open **Settings → Contacts**, then
select an existing connection or choose **Add connection** for another account.
You can also discover the same accounts from the CLI:

```bash
msgvault add-carddav https://contacts.example.com/dav/ you@example.com
msgvault add-carddav https://work-contacts.example.com/dav/ you@example.com --connection work
msgvault carddav connections
msgvault carddav books --connection work
```

Pass the base URL supplied by your CardDAV service. The command prompts for the
password, or you can pipe the password on standard input. The daemon stores the
credential separately from `config.toml` in its token directory: `carddav.json`
for `default`, or `carddav-connections/<name>/carddav.json` for a named
connection. Saving one connection preserves every other connection. See the
[configuration reference](../configuration.md#carddav) for name rules and
trusted private destinations.

To save a connection without enabling synchronization, add `--disabled`. To run
background sync every 30 minutes, add `--schedule "*/30 * * * *"`. You can also
change connection settings and scheduling in the Web UI.

msgvault supports servers that challenge with HTTP Digest. After the first
challenge, the client sends Digest authentication on later requests without
trying Basic again, while Basic authentication continues to work for servers
that use it. A 401 response with an unsupported or malformed challenge fails as
an authentication error.

### Private servers

If your CardDAV server is reachable only at a private address, the operator can
approve one exact HTTPS origin and pin its address in the daemon's local
`config.toml` before adding the account:

```toml
[carddav]
trusted_origin = "https://contacts.example:8443"
trusted_addresses = ["10.1.2.3"]
```

Use that same origin, including its port, in the account's base URL. Because
the pin replaces DNS for that origin, the container does not need to resolve
the hostname. TLS still checks the certificate against the hostname, and a
failed pin does not fall back to DNS. A trailing `/` in `trusted_origin` is
accepted. If the account uses a different hostname or port, msgvault ignores
the pin for that account and uses normal DNS and destination checks. That
mismatch does not stop daemon startup, and private destinations remain blocked
for that account.

msgvault accepts only private addresses from the
[approved ranges](../configuration.md#carddav); loopback and link-local
addresses remain blocked. The Web UI and account API cannot grant private
network access. After changing this local policy, restart the daemon or test
and save the account again. Keep the config file readable only by the daemon's
operator.

## iCloud Contacts

To connect an iCloud Contacts account, use your Apple Account email and an
app-specific password:

```bash
msgvault add-carddav --icloud you@example.com --schedule "*/30 * * * *"
```

The command prompts for the app-specific password. To connect in the Web UI
instead, enter `https://contacts.icloud.com/` as the server URL with the same
email and password. msgvault discovers Apple's contacts partition and saves
that partition as the connection's credential origin. This bootstrap selects
only HTTPS Apple contacts partitions on port 443; other servers keep
same-origin discovery.

Before syncing an iCloud connection, msgvault checks the saved partition. If
that host is unavailable, it rediscovers through Apple's published URL. If
Apple has moved the account, msgvault keeps the saved connection and asks you
to reconnect with `add-carddav --icloud`. Review your address-book choices,
published contacts, and conflicts first; reconnecting replaces the previous
connection and can retire imported contact identities. A failed preflight
returns an error without starting a sync run, so the latest sync-history row
can remain an earlier successful run.

If you previously configured a connection with a partition URL, reconnect it
using `--icloud` to enable partition rediscovery.

## Google Contacts

To connect Google Contacts in the Web UI, select **Google Contacts** under
**Settings → Contacts → Provider**, then enter your Google account email and,
if needed, the name of an OAuth app from `[oauth.apps]`. Google supplies the
discovery URL, and no password is needed. Your contacts account and OAuth app
are independent of your mail accounts. When a stored Google authorization
matches both the account and the selected app, setup reuses it and requests the
combined permissions; otherwise, setup stores separate CardDAV credentials. If
you want Google consent and revocation to stay independent as well, use a
different OAuth client.

1. Configure a Google OAuth client following the
   [OAuth setup guide](../guides/oauth-setup.md), and enable the
   **Google Contacts CardDAV API** in the same Google Cloud project. The Gmail
   and Calendar APIs do not cover contacts; without it,
   **Save CardDAV account** fails during discovery. For Web UI sign-in,
   register the Web UI's root URL, including its trailing slash, as an
   authorized redirect URI in a **Web application** OAuth client (the settings
   form shows the exact URL). Remote Web UIs need HTTPS, while loopback HTTP is
   accepted for local use.
2. Click **Connect Google**. A new window opens for Google sign-in. Choose the
   requested account and keep existing permissions checked while granting
   contacts access, then return to settings when the window closes.
3. Click **Test CardDAV connection**, then click **Save CardDAV account**.
   Review the discovered book's roles before the first sync.

The daemon requests Google's `https://www.googleapis.com/auth/carddav` scope
and `userinfo.email` for account verification, while preserving Google
permissions already recorded in the account's token, including Gmail and
Calendar access. A mail token issued by another app, or with no recorded client
ID, does not block setup and stays untouched. Once a separate CardDAV token
exists, setup keeps using it even if a matching mail token is added later. An
account mismatch, missing permission, or expired callback leaves the saved
token intact. Sign-in expires after ten minutes; to stop waiting sooner, click
**Cancel sign-in**.

For terminal setup, use a Desktop application OAuth client, or register the
[terminal callback](../guides/oauth-setup.md#step-4-create-oauth-client-credentials)
in your Web application client, then run:

```bash
msgvault carddav authorize-google you@example.com
msgvault add-carddav --google you@example.com --schedule "*/30 * * * *"
msgvault sync-carddav
```

`authorize-google` opens the system browser and prints the saved token path. To
print the sign-in URL instead, pass `--no-browser`; the callback still returns
to the machine running the command. Both commands accept `--oauth-app <name>`,
and you should use the same app for authorization and the CardDAV connection.

Web UI authorization stores the token on the daemon, whereas terminal
authorization stores it on the machine running the command. For a remote
daemon, authorize on a browser-equipped machine with the same OAuth client,
then copy the account's JSON token file to the same relative location in the
daemon's token directory while preserving private file permissions. Shared
authorizations use the existing `tokens/<email>.json` file. Separate
authorizations use
`tokens/carddav-google/<SHA-256 of the OAuth app name>/<email>.json`, where the
default app uses the hash of an empty name. The `carddav.json` connection
record references that account and app and does not contain a Google password.
Each named connection has its own binding record, and an account can belong to
only one CardDAV connection even when different OAuth apps authorize it. The
daemon refreshes expired access tokens during requests and saves the refreshed
tokens. If you revoke access, connect Google again to reauthorize.

Google's canonical entry point is
`https://www.googleapis.com/.well-known/carddav`. msgvault discovers the
contact collection from Google's response and uses vCard 3.0 and incremental
sync. To rediscover its URLs, test and save the account again; Google
recommends rediscovery every two to four weeks. See
[Google's CardDAV reference](https://developers.google.com/people/carddav).

## Microsoft contacts

Microsoft 365 and Outlook.com do not support CardDAV. msgvault reads and writes
their contacts through Microsoft Graph instead, with the same roles,
publishing, and conflict review as a CardDAV server. The default **Contacts**
folder is one address book, and each folder inside it is its own address book;
Microsoft Graph does not list contact folders outside **Contacts**. Because
Graph cannot make a delete conditional, an Outlook edit made right before an
unpublish is deleted without a conflict, and the contact stays in Outlook's
Deleted Items.

1. Set up the Microsoft app registration from the
   [Microsoft Graph mail setup](../guides/oauth-setup.md#microsoft-graph-mail-sync),
   and add the delegated Microsoft Graph permission `Contacts.ReadWrite`.
2. Sign in and save the connection:

   ```bash
   msgvault add-carddav --microsoft you@example.com --schedule "*/30 * * * *"
   msgvault carddav books
   ```

   `add-carddav --microsoft` opens a browser for Microsoft sign-in, or you can
   pass `--headless` to sign in with a device code. The token is saved as
   `tokens/mscontacts_<email>.json`, separate from mail and Teams tokens. For a
   remote daemon, copy that file to the daemon's token directory, preserving
   private file permissions, before saving the connection.
3. Review the roles of each folder, then run `msgvault sync-carddav`.

To sign in without saving a connection, run
`msgvault carddav authorize-microsoft you@example.com`, then save the
connection with the **Microsoft 365 or Outlook.com** provider in
**Settings → Contacts**. Saving an existing connection keeps its schedule.

Graph stores fewer fields than vCard. msgvault maps names, nickname, email
addresses, phone numbers, organization, job title, postal addresses, a birthday
with a year, notes, and categories, and saves the full vCard of any card it
writes in a hidden property of the contact. After an edit in Outlook, Outlook's
fields win, and the saved vCard restores what Outlook never held: every
property msgvault does not map (such as a website, a photo, an anniversary, a
birthday without a year, or a phonetic name) along with emails, phones, and
addresses past Outlook's limits of three email addresses, two business phones,
two home phones, and three addresses. Fax and pager numbers count toward those
phone limits. A value that stays in its Outlook field keeps its saved form and
labels, while a moved value takes the form Outlook gives it. Because Outlook
holds one display name, name, nickname, title, organization, note, and
birthday, msgvault logs a second one when it publishes the card.

msgvault refuses any card whose vCard exceeds Graph's 4 MB write limit, for
example one with a large photo. A publish, approval, or conflict resolution
names the person in its error. A sync reports a fixed message, while the daemon log names
the person with the warning `CardDAV contact is over Outlook's 4 MB limit` and
its `person_id`. The CLI and Web UI cannot remove stored media yet, so use the
profile API instead: read the profile, note its `ETag` and the `envelope.id` of
each entry in `media` (`byte_size` shows its size), and supersede the entries
you want to drop:

```bash
curl -i -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  http://localhost:8080/api/v1/people/42/profile

curl -X PATCH -H "Authorization: Bearer $MSGVAULT_API_KEY" \
  -H 'If-Match: "person-42-r7"' -H "Content-Type: application/json" \
  -d '{"media": {"supersede": [123]}}' \
  http://localhost:8080/api/v1/people/42/profile
```

The next sync publishes the smaller card. Outlook's contact photo is not
synced.

After any change in a folder, the next sync lists the whole folder again, at
one request per 100 contacts, including each card's saved vCard and its photo.
Graph can omit a large saved vCard from a listing, and reading that contact on
its own costs one more request. Sync stops at 256 MB, so a folder with many
large photos may need fewer published photos. If Outlook returns a published
contact without its saved vCard even when read alone, the sync fails instead of
overwriting the person.

## Choose what each book does

When you discover the archive's first connection, msgvault selects the first
address book that supports creation, updates, and deletion and subscribes to
it. Additional connections start with identity lookup only and leave the global
write target unchanged. All discovered books allow identity lookup initially.
Review these roles before your first sync:

| Role | Effect |
|---|---|
| **Write target** / “Publish here” | The one book that receives explicitly published profiles. It must also be subscribed. |
| **Subscribed** / “Sync contacts” | Import unbound cards as profiles and synchronize later contact changes. |
| **Lookup source** | Retain cards for identity lookup without importing every unbound card as a profile. |

You can set roles in the Web UI or from the CLI:

```bash
msgvault carddav books set-role 3 --write-target --subscribed --lookup-source
msgvault carddav books set-role 4 --lookup-source
```

`set-role` replaces **all three roles** for the book, setting omitted flags to
false. Only one book across all connections can be the write target. If
publications, pending writes, or conflicts still depend on a book, msgvault
rejects incompatible role changes until you resolve those dependencies.

## Connect cards to archived identities

After upgrading to a version with participant binding, the next CardDAV sync
binds subscribed contacts to matching archived participants. Email matches ignore
case. Phone matches require `+`, `00`, or a numeric `tel:` phone context before
using the archive's phone normalization. Names never establish
a match. Phone extensions and service-scoped handles are excluded.

Each match includes that participant's existing identity links. A saved person
can cover several identity clusters; CardDAV does not create new links between
them. Unchanged cards are checked on each sync, so participants arriving later
are picked up too. Lookup-only books do not create bindings.

Preview or apply the same reconciliation using the retained cards, without
contacting the CardDAV server:

```bash
msgvault carddav bind-participants --dry-run
msgvault carddav bind-participants
msgvault carddav bind-participants --dry-run --json
msgvault person identities 7
```

The report gives `cards_scanned`, `participants_bound`, `participants_unbound`,
and `conflicts`. Counts in a dry run describe its read snapshot; an apply
recomputes them. Sync output and run history report binding conflicts archive-wide
at the time of that run, including conflicts from other connections. Conflict details contain participant and person IDs, without
addresses. A shared address claimed by different saved people, or a participant
already bound to another person, is reported without moving it or merging
profiles. Correct the cards or inspect the people and explicitly merge them
with [person merge](people.md#merge-duplicate-profiles-and-reverse-a-merge).
People with active merge history can receive new matching archived identities.
Those bindings stay with the surviving person after a split.

An unresolved CardDAV edit conflict also suspends that card's automatic claims
until you resolve it. Other subscribed cards can still support the same person.

Removing an address or deleting its card releases memberships created by this
feature. Role changes, discovery removal, conflict capture and publication
completion also release unsupported automatic memberships in their transaction.
New bindings after those operations appear on the next sync or explicit
reconciliation. A stale automatic owner is released before another person can
acquire the component on a later pass.

Existing memberships stay manual on upgrade. Promoting an already bound
participant or explicitly linking its identities confirms those memberships as
manual. User-approved identity matches and person merges/splits do the same.
Manual memberships survive card edits and automatic cleanup. System identity
matches and importer deduplication preserve automatic ownership.

To remove only the automatic memberships:

```bash
msgvault carddav bind-participants --undo --dry-run
msgvault carddav bind-participants --undo
```

Undo does not change cards or manual memberships. The next sync can recreate
supported automatic bindings. To keep a binding removed, correct the address in
the source card or remove that card from the subscribed book.

## Sync and publish selected people

To pull changes and reconcile existing publications, run:

```bash
msgvault sync-carddav
```

An unqualified sync runs every configured, enabled connection. To sync one
connection, including a disabled connection for manual repair, run
`msgvault sync-carddav --connection work`. Add `--full` for a full address-book
reconciliation. A failing connection does not stop the others, and partial or
failed aggregate results make the CLI exit nonzero. If no connections are
enabled, an unqualified sync fails as unavailable.

In the Web UI, Settings shows the selected connection's status, books, and
history, and its sync buttons run that connection. Switching connections clears
unsaved account fields, passwords, and pending test or sign-in results. To see
attributed runs across all connections, open the sidebar status dot's
**Past runs…** menu and filter Kind to **CardDAV sync**. Clicking
**Settings → Overview → Contacts → Sync now** runs an aggregate sync and is
disabled while any CardDAV run is active. Schedules, retry gates, and run
leases are independent per connection.

To publish a person in the Web UI, open their saved profile in **People**,
choose **More actions → Publish to address book**, and turn on
**Publish person to CardDAV**. A contact UID tracks that published person
across syncs, and later profile changes reconcile to the write target. Turning
publication off removes the remote card while keeping the local profile.

When a card uses the person's display name, changing that display name updates
the card on the next publish or sync. A formatted or structured profile name
takes priority over the display label, including a formatted name imported from
an address book. Renaming the display label retires only the inferred profile
name behind that label. Separately saved names remain and keep priority on the
card. Edit that profile name to change the card's name. If another device
removes or blanks the card's name and msgvault would otherwise upload the
display name in its place, sync records a conflict instead.

Cards published by earlier msgvault versions keep the name from their first
upload as a separate formatted-name (`FN`) line. On the next publish or sync,
msgvault replaces that line when it is the card's first plain `FN` line, still
matches the first upload, and was never imported from the address book. Names
with parameters, grouped names, repeated names, and names changed since the
first upload stay on the card.

You can run the same publish and unpublish actions from the CLI:

```bash
msgvault person publish 7
msgvault person unpublish 7
```

You can create and publish someone you have never messaged in a single command:

```bash
msgvault person create --name "Alex Example" --email alex@example.com --publish
```

This command saves the person locally first, then follows the same publication
path as `person publish`. If publication fails, the local person stays saved
and the command reports their ID. Retry with
`msgvault person publish <person-id>`, or use the preview and approval steps
below when review is required. Omitting `--publish` keeps creation local. See
[Create a person](people.md#create-a-person) for contact types, duplicate
refusal, and the API and MCP contracts.

For integrations, the publication API exposes these endpoints:

| Request | Effect |
|---|---|
| `GET /api/v1/carddav/publications/{person_id}` | Read publication state |
| `POST /api/v1/carddav/publications/{person_id}` | Publish the saved person |
| `GET /api/v1/carddav/publications/{person_id}/preview` | Return the exact vCard the next write would send and its approval token |
| `POST /api/v1/carddav/publications/{person_id}/approve` | Publish with a reviewed `approval_token` |
| `DELETE /api/v1/carddav/publications/{person_id}` | Remove their publication |

Publication state is `unpublished`, `published`, `pending`, or `conflict`. A
`pending` state means the operation has not settled yet; sync recovers pending
work before importing changes. Wait for the final state before assuming a write
or removal has finished.

The exported vCard includes supported contact and profile fields. Private Notes
map to `NOTE`, so review them before publishing. Generated person briefs stay
in the archive and are not included in CardDAV.

### Review inferred changes before they are exported

Profile facts that msgvault inferred from messages or enrichment, rather than
facts you declared, are never exported to the address book until you review the
exact card. Both the first publication and every later reconciliation compare
the inferred facts against the last approved export. When they differ, sync
skips that person, the publication response reports
`inference_review_required: true`, and a plain publish fails with HTTP 409
`carddav_inference_review_required`.

Inferred facts include attributes such as Notes, current employment, names,
contact points, and postal addresses. Adding, replacing, or removing any of
them, including through profile edits, requires a new review. A merge or split
that moves an inferred fact to another person requires a review of that
person's card too.

To review and approve changes, preview the card and then publish with its
token:

```bash
msgvault person publish 7 --preview
msgvault person publish 7 --approve <approval_token>
```

`--preview` prints JSON containing the full `vcard`, an `approval_token`, and
`review_required`. The token binds your approval to that exact card, the
address book, and the current inferred facts. If any of those change before you
approve, the approve request fails with HTTP 409 `carddav_review_stale`; run
the preview again and review the new card. Preview also covers a pending create
that has not settled and a conflict whose `keep_local` side would export
inferred changes. When `kind` is `conflict`, approval records the review
without publishing or resolving the conflict. After approving, run
`msgvault carddav conflicts resolve <conflict_id> keep_local` with the
preview's `conflict_id` to publish the reviewed card. API clients follow
approval with `POST /api/v1/carddav/conflicts/{id}/resolve` and
`{"choice":"keep_local"}`.

The preview route is the only CardDAV response that returns a raw vCard, and it
can include Private Notes. In the Web UI, the person's
**Publish to address book** action directs you to the CLI review commands when
approval is required.

### Recover a connection removed from config

If you remove a connection's config table, its account, books, contacts, and
publications stay in the archive. `msgvault carddav connections` lists the
connection as **orphaned**, and Settings marks the connection as
**configuration missing**. Publishing and unpublishing through that connection
stay unavailable until you restore it, and existing publications can also block
moving the write target to another book.

To recover the connection, restore the original config table, or select the
orphaned connection in Settings and save it with the same server URL and
username. You can also restore it from the CLI:

```bash
msgvault add-carddav https://work-contacts.example.com/dav/ you@example.com --connection work
```

Pass the original connection name (`default` for `[carddav]`). For Google, run
`msgvault add-carddav --google you@example.com --connection work` along with
the original `--oauth-app` if one was selected. Restoring the same identity
reconnects the existing account and books. Settle any pending publications and
conflicts before moving the write target. Adding the account under a different
name is rejected.

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
HTTP error behavior.

An aggregate sync with at least one enabled connection returns HTTP 200 with
`status` (`succeeded`, `partial`, or `failed`) and `connections` outcomes, even
when every connection fails. Consumers must inspect those fields, including
callers using `pkg/client`. This is a breaking change from the single-account
API: omitted-selector sync no longer reports a provider throttle as HTTP 503 or
missing Google authorization as HTTP 502. To keep explicit single-connection
error behavior and to sync a disabled default account manually, send
`{"connection":"default"}`. Each outcome includes safe errors, counts,
connection identity, and the run ID when the request started a run; a busy or
unavailable connection does not create a run.

Publication and conflict routes use global IDs and route network operations to
the persisted owner's connection. Store-only views stay available even when a
connection's credential needs repair.

## Resolve competing edits

When a contact is published back into its source address book, remote edits
update its imported values while preserving values added in msgvault or
imported from other books.

If both msgvault and the address book changed the same card, or if a remote
edit would overwrite another owner's value, msgvault records a conflict for
review. It also records edit/delete conflicts instead of silently choosing a
side.

```bash
msgvault carddav conflicts list
msgvault carddav conflicts show 18
```

The detail view compares the common base, local profile, and remote card. These
are bounded summaries and do not show every vCard field. To resolve a conflict
in the Web UI, open it in Settings and review the decision. From the CLI,
choose a side explicitly:

```bash
msgvault carddav conflicts resolve 18 keep_local
msgvault carddav conflicts resolve 18 keep_remote
```

`keep_local` applies the local choice to the remote card, and `keep_remote`
accepts the remote choice locally; either choice may represent a deletion. If
the remote state changed again, reload the conflict and review the new
comparison. For contacts with an active publication, a remote choice that would
restore a protected value or cannot be rendered safely leaves the conflict
unresolved, and an unresolved conflict prevents conflicting publication work
from proceeding.

When a card's name comes from the display name, `keep_remote` handles it as
follows:

- If neither side renamed the card, the display name keeps controlling the
  card's name, and later renames still update the card.
- If either side renamed it, `keep_remote` keeps the remote name and rejects a
  local rename. msgvault imports the card's formatted name as a profile name so
  the next sync keeps it, and that imported name takes priority over later
  display-name changes.
- If the remote card has no formatted name and no other profile name would take
  its place, `keep_remote` clears the display name. This applies to any
  published card, whatever supplied its earlier name.

Typed local profile names remain protected.

You cannot merge a saved person who has an active CardDAV publication with
another profile. Remove the publication and settle pending work first, then
follow the [profile merge workflow](/docs/usage/people/#merge-duplicate-profiles-and-reverse-a-merge).
