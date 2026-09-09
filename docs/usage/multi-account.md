---
last_edited: "2026-09-08"
title: Accounts, Identities, and Collections
description: How msgvault organizes every source into accounts, tracks which identifiers are "you," and groups accounts into collections for scoped search, stats, and deduplication.
---

Keep live accounts, old imports, and chat histories in one archive while
retaining where each message came from. Use account identities to tell
msgvault which messages you sent, and collections to work with a chosen group
of accounts.

## Accounts, identities, and collections

| Concept | Meaning | Example |
|---|---|---|
| Account (also called a source) | One live connection or imported dataset | A Gmail sync and an MBOX import are separate accounts |
| Account identity | Confirmed addresses, numbers, or handles that mean “you” in that source | Your primary email and a sending alias |
| Collection | A named group of accounts | `Work` groups a current mailbox and older work exports |

Each account keeps its own source type, messages, labels, and sync or import
state. Importing the same mailbox through two paths does not silently merge
them. An overlapping address or message alone does not prove that two sources
belong together.

Identity is per account. This matters when an export contains several people's
mail or an old address means something different in another source. Confirmed
identities let msgvault attribute messages to you and detect sent copies during
[deduplication](/docs/usage/deduplication/).

The built-in `All` collection contains every account. Other collections have
membership you choose. Their combined identity is the union of their member
accounts' identities, calculated when used. Collections contain accounts only;
they cannot contain other collections.

<figure data-lightbox style="margin: 1.5rem 0; text-align: center;">
  <img src="/docs/assets/generated/concepts/account-collection-concept.png" alt="Accounts on the left are individual ingest sources, each carrying the identifiers that mean you inside that source. Collections on the right are named groups of accounts: All contains every account, with Personal and Work as deliberate subsets." loading="lazy" style="width: 100%; display: block;" />
</figure>

Deduplication operates over all three concepts and has its own [Deduplication](/docs/usage/deduplication/) page.

## OAuth Apps and Tokens

For personal Gmail accounts, a single `client_secret.json` supports all of them. Each `add-account` call authorizes one account and stores a separate token file.

Google Workspace organizations often restrict OAuth to apps created within their own org. If a Workspace account fails to authorize with your default app, create a separate OAuth app inside that org and add it as a named app in `config.toml`. See the [OAuth Setup Guide](/docs/guides/oauth-setup/#google-workspace-accounts) for the full walkthrough.

Workspace admins can also use a Google service account with domain-wide delegation. Configure `service_account_key` under `[oauth]` or `[oauth.apps.<name>]`, authorize the service account client in the Google Admin Console, then run `msgvault add-account user@domain.com`. Service-account accounts do not store per-user refresh tokens; msgvault mints delegated tokens on demand.

<figure data-lightbox style="margin: 1.5rem 0; text-align: center;">
  <img src="/docs/assets/generated/concepts/oauth-multi-account-concept.png" alt="Two OAuth apps and the token files they create. A default app (config block [oauth]) authorizes personal Gmail accounts personal@gmail.com and other@gmail.com; a named app ([oauth.apps.acme]) authorizes the Workspace account you@acme.com. Each add-account run writes its own token file under ~/.msgvault/tokens/, color-matched to its account." loading="lazy" style="width: 100%; display: block;" />
</figure>

## Adding Accounts

```bash
# Gmail accounts (OAuth)
msgvault add-account personal@gmail.com
msgvault add-account you@acme.com --oauth-app acme   # Workspace org

# IMAP accounts (password)
msgvault add-imap --host imap.fastmail.com --username you@fastmail.com

# Microsoft 365 / Outlook.com (OAuth2 over IMAP)
msgvault add-o365 you@outlook.com
```

Gmail accounts open a browser for OAuth authorization. IMAP accounts prompt for a password and test the connection. All accounts share the same archive database (SQLite by default, or PostgreSQL) and attachment storage.

Start by listing what msgvault already knows about. Every command in this guide takes the identifier from this list (typically the email address or source name):

```bash
msgvault list-accounts
```

!!! tip "Add every account as a Test user"
    Each Gmail account must be listed as a **Test user** in the OAuth consent screen of the app that authorizes it. For Workspace accounts using a named OAuth app, add test users in that org's Google Cloud project. This is the most common reason a second account fails to authorize.

### Gmail addresses and display names

Pass your Gmail account's email address to `add-account`, then select that
account in Google's consent screen. The argument is required and must be a bare
email address. A label such as `Work` belongs in `--display-name`:

```bash
msgvault add-account user@example.com --display-name "Work"
msgvault update-account user@example.com --display-name "Personal"
```

Msgvault checks the authenticated mailbox against the requested address, including
when reusing a stored token. A mismatch or failed verification stops registration
before account settings change. Gmail's equivalent spellings (dots,
plus-addressing, and `googlemail.com`) are accepted. For Google Workspace, use the
primary address returned by Google; a different local part is not assumed to be
an alias of the same account.

Changing a display name keeps the account identifier and archived mail intact.
Commands such as `sync-full` continue to use the identifier from `list-accounts`.

### Recovering an older mislabeled Gmail account

Older versions could store credentials under a label that did not match the
authenticated mailbox. Account identifiers cannot currently be renamed in place.
If you only need a readable name, use `update-account --display-name` above.

To replace an incorrect identifier, remove the old Gmail source and add the
primary address. **Removal deletes that source's local messages and sync state.**
Only use this recovery path if the mail is still available in Gmail. Take a
verified backup first so you can roll back; restoring it preserves the old
identifier rather than moving mail to the new one. Removal does not delete mail
from Google.

```bash
msgvault list-accounts
msgvault remove-account old-label --type gmail
msgvault add-account user@example.com
msgvault sync-full user@example.com
```

Reuse the `--oauth-app` value originally used for this account, if any. Named
apps are defined under `[oauth.apps.<name>]` in `config.toml`; identify the
applicable one before removal. Use `--readonly` if you want the replacement Gmail
grant to be read-only. Update any `[[accounts]]` schedule in `config.toml` to use the correct
address, and recreate custom collection memberships and source identities as
needed.

Gmail removal also removes its local Google token and attempts to revoke the
grant. Calendar sources registered under the old address remain in the archive,
but can no longer rely on that credential. If you also sync Calendar or Drive,
record their settings and re-authorize those integrations under the primary
address; the Gmail commands above do not migrate them.

## Syncing

Sync all accounts at once by omitting the email argument:

```bash
# Full sync all accounts
msgvault sync-full

# Incremental sync all accounts
msgvault sync
```

Or sync a specific account:

```bash
msgvault sync-full personal@gmail.com
msgvault sync work@company.com
```

If a token expires during sync, msgvault prints the re-authorization URL with the account name so you can select the correct Google account. It will not auto-launch a browser during re-auth to prevent accidentally authorizing the wrong account.

## Identities

An account can have a confirmed "me" identity: the email addresses, phone numbers, chat handles, or synthetic identifiers that mean you inside that source. Deduplication uses this set for sent-copy detection, so for "sent" versus "received" to mean anything in older imports, msgvault needs to know which identifiers are you in each account.

Source identities are different from the observed people and durable profiles
used by relationship exploration. See [People, Profiles, and Source
Identities](/docs/usage/people/) for evidence discovery, bulk import, optional
Fastmail alias inventory, person promotion, and typed attributes.

New Gmail, IMAP, Microsoft 365, MBOX, EML, EMLX, WhatsApp, and Google Voice sources auto-confirm the source identifier by default. Use `--no-default-identity` on supported add/import commands when that is not correct. (iMessage imports are exempt, because iMessage contacts are not self-identifying.)

```bash
# List confirmed identifiers across all accounts
msgvault identity list

# Show one account's identity in detail, including which signals confirmed it
msgvault identity show work@company.com

# Add or remove identifiers manually
msgvault identity add work@company.com alias@company.com
msgvault identity remove work@company.com old-alias@company.com
```

Each confirmed identifier records the signals that confirmed it: `account-identifier` (the address matches the account's own identifier, such as the Gmail address itself), `phone-e164` (a phone number from an SMS or chat source), `manual` (an entry you added via `identity add`), or `config_migration` (carried over from a legacy `[identity]` config block). An identifier accumulates signals over time as new evidence appears; it is removed only by `identity remove`.

`identity list` can be scoped to one account or one collection. A collection's identity is the union of its member accounts' confirmed identifiers:

```bash
msgvault identity list --account work@company.com
msgvault identity list --collection Work
```

### Repair attribution in older archives

If your account's own address was never confirmed, older sent messages may
not be marked as yours. Confirm it and update the existing messages without a
provider resync:

```bash
msgvault repair-identity you@example.com
msgvault repair-identity --type imap
```

The default source type is Gmail. The command skips accounts whose own address
is already confirmed or is not a plain valid email address. Existing aliases
do not prevent it from adding the primary account address.

### Select an exact source

A live account and an import can share the same email address. Use the numeric
ID from `list-accounts` when a command needs one exact source:

```bash
msgvault sync --source-id 3
msgvault sync-full --source-id 3
msgvault identity show --source-id 3
```

On commands that offer `--source-id`, use it in place of an account name. This
avoids selecting a different source with the same identifier or display name.
See the [CLI reference](/docs/cli-reference/) for the commands that accept it.

## Collections

Collections are named groups of accounts. The default `All` collection is created automatically and includes every account. User-created collections let you search, report, and deduplicate a logical group without changing the underlying sources.

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

`--accounts` accepts account identifiers and numeric source IDs. One account can belong to multiple collections.

!!! note "`All` is auto-managed"
    `All` is auto-managed and immutable. msgvault rejects `collection delete All` and explicit membership edits on `All`. New accounts join `All` automatically when they are created.

## Scoped Search and Stats

Search queries run across all accounts by default:

```bash
msgvault search "quarterly report"
```

Use `--account` to limit results to a specific account, or `--collection` to limit results to all member accounts in a collection:

```bash
msgvault search "quarterly report" --account work@company.com
msgvault search "quarterly report" --collection Work
msgvault stats --collection Work
```

`--account` and `--collection` are mutually exclusive. If you pass a collection name to `--account` (or an account identifier to `--collection`), msgvault rejects it with a hint to use the other flag, so the two scopes never silently cross.

## Deduplication

Once several accounts hold overlapping copies of the same message, [deduplication](/docs/usage/deduplication/) collapses each set to one visible survivor while keeping every source's provenance intact. It hides redundant copies rather than deleting them, and every step beyond hiding is a separate, opt-in action. See the [Deduplication](/docs/usage/deduplication/) page for the detection rules, survivor selection, and the reversible safety ladder.

## TUI Filtering

Press `A` (uppercase) inside the TUI to open the Email scope selector. Pick a
single account, a named collection, or "All Accounts" to clear the filter. The
title bar shows the selected account or `Collection: <name>`. A collection
scope carries its exact member source IDs through Email aggregates, message
lists, fast search, statistics, and deletion-target inspection. An empty
collection matches nothing.

Named collections require API schema `2.17.0` or newer. Upgrade the daemon if
its source selector does not offer collections. Texts and Meetings keep
independent source selectors. Changing the Email scope returns
to the top-level view and clears the current search and selection.

Multi-source collections offer Fast search only. Deep search is available for
single-source collections. Collection scopes do not offer Semantic search.
Deletion staging accepts selections from one source, including within a larger
collection; selections spanning sources are rejected. Deduplication remains
available through the collection-scoped CLI commands, not through the TUI.

Meetings mode uses the same key for a separate source selector. It lists
Granola, Circleback, and Notion meeting sources. Changing it does not replace
the Email account filter.

## Command Reference

See the [CLI Reference](/docs/cli-reference/#add-account) for the complete flag list on `add-account`, `add-imap`, `add-o365`, `identity`, and `collection`.
