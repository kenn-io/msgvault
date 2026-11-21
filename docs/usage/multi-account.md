---
last_edited: "2026-10-08"
title: Accounts, identities, and collections
description: How msgvault organizes every source into accounts, tracks which identifiers are "you," and groups accounts into collections for scoped search, stats, and deduplication.
---

You can keep live accounts, old imports, and chat histories in one archive
while preserving where each message came from. Account identities tell msgvault
which messages you sent, and collections let you work with a chosen group of
accounts.

## Accounts, identities, and collections

| Concept | Meaning | Example |
|---|---|---|
| Account (also called a source) | One live connection or imported dataset | A Gmail sync and an MBOX import are separate accounts |
| Account identity | Confirmed addresses, numbers, or handles that mean "you" in that source | Your primary email and a sending alias |
| Collection | A named group of accounts | `Work` groups a current mailbox and older work exports |

Each account keeps its own source type, messages, labels, and sync or import
state. Importing the same mailbox through two paths does not silently merge
them; an overlapping address or message alone does not prove that two sources
belong together.

Identities are tracked per account, which matters when an export contains
several people's mail or when an old address means something different in
another source. Confirmed identities let msgvault attribute messages to you and
detect sent copies during [deduplication](/docs/usage/deduplication/).

The built-in `All` collection includes every account, while other collections
hold the accounts you choose. A collection's combined identity is the union of
its member accounts' identities, calculated when used. Collections can contain
accounts only, not other collections.

<figure data-lightbox style="margin: 1.5rem 0; text-align: center;">
  <img src="/docs/assets/generated/concepts/account-collection-concept.png" alt="Accounts on the left are individual ingest sources, each carrying the identifiers that mean you inside that source. Collections on the right are named groups of accounts: All contains every account, with Personal and Work as deliberate subsets." loading="lazy" style="width: 100%; display: block;" />
</figure>

For how deduplication works across accounts, identities, and collections, see
[Deduplication](/docs/usage/deduplication/).

## OAuth apps and tokens

A single `client_secret.json` works for all of your personal Gmail accounts.
Each `add-account` call authorizes one account and stores a separate token file
for it.

Google Workspace organizations often restrict OAuth to apps created inside
their own org. If a Workspace account fails to authorize with your default app,
create a separate OAuth app inside that org and add it as a named app in
`config.toml`. Follow the
[OAuth Setup Guide](/docs/guides/oauth-setup/#google-workspace-accounts) for
the full walkthrough.

Workspace admins can also use a Google service account with domain-wide
delegation. Configure `service_account_key` under `[oauth]` or
`[oauth.apps.<name>]`, authorize the service account client in the Google Admin
Console, and then run `msgvault add-account user@domain.com`. Service-account
accounts do not store per-user refresh tokens; msgvault mints delegated tokens
on demand.

<figure data-lightbox style="margin: 1.5rem 0; text-align: center;">
  <img src="/docs/assets/generated/concepts/oauth-multi-account-concept.png" alt="Two OAuth apps and the token files they create. A default app (config block [oauth]) authorizes personal Gmail accounts personal@gmail.com and other@gmail.com; a named app ([oauth.apps.acme]) authorizes the Workspace account you@acme.com. Each add-account run writes its own token file under ~/.msgvault/tokens/, color-matched to its account." loading="lazy" style="width: 100%; display: block;" />
</figure>

## Add accounts

Add each mail source with the command that matches its provider:

```bash
# Gmail accounts (OAuth)
msgvault add-account personal@gmail.com
msgvault add-account you@acme.com --oauth-app acme   # Workspace org

# IMAP accounts (password)
msgvault add-imap --host imap.fastmail.com --username you@fastmail.com

# Microsoft 365 / Outlook.com (OAuth2 over IMAP)
msgvault add-o365 you@outlook.com
```

For Gmail accounts, msgvault opens a browser for OAuth authorization; for IMAP
accounts, it prompts for a password and tests the connection. All accounts
share one archive database (SQLite by default, or PostgreSQL) and the same
attachment storage.

To see what msgvault already tracks, list your accounts. Every command in this
guide takes the identifier from this list (typically the email address or
source name):

```bash
msgvault list-accounts
```

!!! tip "Add every account as a Test user"
    List each Gmail account as a **Test user** in the OAuth consent screen of
    the app that authorizes it. For Workspace accounts using a named OAuth app,
    add test users in that org's Google Cloud project. Missing test users are
    the most common reason a second account fails to authorize.

### Gmail addresses and display names

Pass your Gmail account's bare email address as the required argument to
`add-account`, then select that account in Google's consent screen. Put a
readable label such as `Work` in `--display-name`:

```bash
msgvault add-account user@example.com --display-name "Work"
msgvault update-account user@example.com --display-name "Personal"
```

msgvault verifies that the authenticated mailbox matches the requested address,
including when reusing a stored token. A mismatch or failed verification stops
registration before any account settings change. Gmail's equivalent spellings
(dots, plus-addressing, and `googlemail.com`) are accepted. For Google
Workspace, use the primary address returned by Google; msgvault does not assume
a different local part is an alias of the same account.

Updating a display name keeps the account identifier and archived mail intact.
Commands such as `sync-full` continue to use the identifier from
`list-accounts`.

### Recovering an older mislabeled Gmail account

Older versions of msgvault could store credentials under a label that did not
match the authenticated mailbox. Account identifiers cannot currently be
renamed in place. If you only need a readable name, set it with
`update-account --display-name` instead.

To replace an incorrect identifier, remove the old Gmail source and add the
primary address. **Removal deletes that source's local messages and sync
state**, though it does not delete mail from Google. Use this recovery path
only if the mail is still available in Gmail, and take a verified backup first
so you can roll back. Restoring that backup preserves the old identifier rather
than moving mail to the new one.

1. Named OAuth apps are defined under `[oauth.apps.<name>]` in `config.toml`.
    Before removing the account, identify which named app applies to it, if any,
    and record any Calendar or Drive settings tied to the old address.
2. Remove the old source, add the primary address, and run a full sync:

    ```bash
    msgvault list-accounts
    msgvault remove-account old-label --type gmail
    msgvault add-account user@example.com
    msgvault sync-full user@example.com
    ```

    Reuse the account's original `--oauth-app` value if it had one, and pass
    `--readonly` if you want the replacement Gmail grant to be read-only.
3. Update any `[[accounts]]` schedule in `config.toml` to use the correct
    address, and recreate custom collection memberships and source identities as
    needed.

Removing a Gmail account also removes its local Google token and attempts to
revoke the grant. Calendar sources registered under the old address remain in
the archive, but they can no longer rely on that credential. If you also sync
Calendar or Drive, re-authorize those integrations under the primary address;
the Gmail commands above do not migrate them.

## Sync accounts

To sync all accounts at once, omit the email argument:

```bash
# Full sync all accounts
msgvault sync-full

# Incremental sync all accounts
msgvault sync
```

To sync one specific account, pass its email address:

```bash
msgvault sync-full personal@gmail.com
msgvault sync work@company.com
```

Before `sync` or `sync-full` starts, if a Gmail token stored on this machine
has expired or been revoked, msgvault prints
`Token for <account> is expired; opening browser to re-authorize...` and opens
a browser to re-authorize that account, with the account pre-selected in
Google's sign-in. If the browser does not open, msgvault prints the URL to
visit. Accounts that use a service account are not re-authorized this way.

## Identities

Each account can have a confirmed "me" identity made up of the email addresses,
phone numbers, chat handles, or synthetic identifiers that mean you inside that
source. Deduplication uses this set for sent-copy detection, so for "sent"
versus "received" to mean anything in older imports, msgvault needs to know
which identifiers are you in each account.

Source identities are separate from the observed people and durable profiles
used in relationship exploration. See [People, Profiles, and Source
Identities](/docs/usage/people/) for evidence discovery, bulk import, optional
Fastmail alias inventory, person promotion, and typed attributes.

New Gmail, IMAP, Microsoft 365, MBOX, EML, EMLX, WhatsApp, and Google Voice
sources confirm the source identifier automatically by default. When that is
not correct, pass `--no-default-identity` on supported add or import commands.
iMessage imports are exempt because iMessage contacts are not self-identifying.

For older accounts, run the add command with `--no-default-identity` to save
that choice. Removing the last confirmed identity also saves this choice, so
later syncs do not restore the source identifier. To re-enable automatic
confirmation, pass `--no-default-identity=false` on the source's add command.
See the CLI reference for
[saved identity choices](../cli-reference.md#saved-default-identity-choice),
including re-authorization and re-enabling defaults.

```bash
# List confirmed identifiers across all accounts
msgvault identity list

# Show one account's identity in detail, including which signals confirmed it
msgvault identity show work@company.com

# Add or remove identifiers manually
msgvault identity add work@company.com alias@company.com
msgvault identity remove work@company.com old-alias@company.com
```

Each confirmed identifier records the signals that confirmed it:
`account-identifier` (the account's own address), `phone-e164` (a phone number
from an SMS or chat source), `manual` (an entry added with `identity add`), or
`config_migration` (carried over from a legacy `[identity]` config block).
Observed sent mail can also add `is_from_me`, `sent-folder`, or `sent-label`
evidence to confirmed identities.

During Gmail sync, msgvault adds `oauth` when the authenticated profile matches
an already-confirmed address, including Gmail's equivalent address spellings.
These refreshes preserve identity removal and `--no-default-identity`; they do
not confirm new addresses. If the profile address differs from the source
address, sync logs a warning and continues without adding identity evidence. To
review and confirm new identities, including authenticated Gmail profile
evidence with `--provider`, use the [identity discovery workflow](people.md).

An identifier accumulates signals over time as new evidence appears, and only
`identity remove` removes it.

You can scope `identity list` to one account or one collection. A collection's
identity is the union of its member accounts' confirmed identifiers:

```bash
msgvault identity list --account work@company.com
msgvault identity list --collection Work
```

### Repair attribution in older archives

If your account's own address was never confirmed, older sent messages may not
be marked as yours. Confirm the address and update existing messages without a
provider resync:

```bash
msgvault repair-identity you@example.com
msgvault repair-identity --type imap
```

The default source type is Gmail. When other email account types have an
unconfirmed account address, msgvault lists them with a `--type` repair hint;
if you supplied an identifier, reuse it. The command skips accounts whose own
address is already confirmed or is not a plain valid email address, and
existing aliases do not prevent it from adding the primary account address.
Activity and contacts update at the daemon's next refresh, or immediately when
you run `msgvault activity build`.

### Select an exact source

A live account and an import can share the same email address. When a command
needs one exact source, pass the numeric ID from `list-accounts`:

```bash
msgvault sync --source-id 3
msgvault sync-full --source-id 3
msgvault identity show --source-id 3
```

On commands that offer `--source-id`, use it instead of an account name to
avoid selecting a different source with the same identifier or display name.
See the [CLI reference](/docs/cli-reference/) for the commands that accept it.

## Collections

Collections are named groups of accounts. msgvault creates the default `All`
collection automatically to include every account. User-created collections
let you search, report on, and deduplicate a logical group without changing
the underlying sources.

```bash
# Create a collection from two accounts
msgvault collection create Work --accounts you@company.com,archive@company.com

# Inspect and edit membership
msgvault collection list
msgvault collection show Work
msgvault collection add Work --accounts old-pst@company.com
msgvault collection remove Work --accounts archive@company.com

# Delete only the collection record; sources and messages are untouched
msgvault collection delete Work
```

`--accounts` accepts account identifiers and numeric source IDs, and one
account can belong to multiple collections.

!!! note "`All` is auto-managed"
    `All` is auto-managed and immutable. msgvault rejects
    `collection delete All` and explicit membership edits on `All`. New
    accounts join `All` automatically when created.

## Scope search and stats

By default, search queries run across all accounts:

```bash
msgvault search "quarterly report"
```

To limit results to a specific account, pass `--account`. To limit results to
all member accounts in a collection, pass `--collection`:

```bash
msgvault search "quarterly report" --account work@company.com
msgvault search "quarterly report" --collection Work
msgvault stats --collection Work
```

`--account` and `--collection` are mutually exclusive. If you pass a collection
name to `--account` or an account identifier to `--collection`, msgvault
rejects the command with a hint to use the other flag, so the two scopes never
silently cross.

## Deduplication

When several accounts hold overlapping copies of the same message,
[deduplication](/docs/usage/deduplication/) collapses each set to one visible
survivor while keeping every source's provenance intact. It hides redundant
copies instead of deleting them, and every step beyond hiding is a separate,
opt-in action. See [Deduplication](/docs/usage/deduplication/) for detection
rules, survivor selection, and the reversible safety ladder.

## Filter in the TUI

In the TUI, press `A` (uppercase) to open the Email scope selector and choose a
single account, a named collection, or **All Accounts** to clear the filter.
The title bar shows the selected account or `Collection: <name>`. A collection
scope carries its exact member source IDs through Email aggregates, message
lists, fast search, statistics, and deletion-target inspection, and an empty
collection matches nothing. Changing the Email scope returns you to the
top-level view and clears the current search and selection.

Named collections require API schema `2.17.0` or newer; upgrade the daemon if
its source selector does not offer collections. Texts and Meetings each keep an
independent source selector.

Collection scopes have a few limits in the TUI:

- **Search.** Multi-source collections offer Fast search only. Single-source
  collections also offer Deep search. Collection scopes do not offer Semantic
  search.
- **Deletion staging.** You can stage selections from one source, including
  within a larger collection, but msgvault rejects selections spanning multiple
  sources.
- **Deduplication.** Run deduplication through the collection-scoped CLI
  commands rather than in the TUI.

In Meetings mode, pressing `A` opens a separate source selector that does not
replace the Email account filter. It lists Omi, Granola, Plaud, Pocket,
Circleback, Notion, Muesli, Twilio, Bland, Twenty, and Chatwoot meeting
sources, plus sources created through the meeting import API.

## Command reference

See the [CLI Reference](/docs/cli-reference/#add-account) for the complete flag
list on `add-account`, `add-imap`, `add-o365`, `identity`, and `collection`.
