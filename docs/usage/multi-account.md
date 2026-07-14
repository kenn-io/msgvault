---
title: Accounts, Identities, and Collections
description: How msgvault organizes every source into accounts, tracks which identifiers are "you," and groups accounts into collections for scoped search, stats, and deduplication.
---

msgvault stores every source in one archive database: Gmail accounts, IMAP accounts, Microsoft 365 accounts, local imports, and chat/text imports. A source has an account identifier, optional display name, source type, labels, messages, attachments, and sync/import state.

As an archive grows it accumulates overlapping sources: a current Gmail sync, an old mbox export, Apple Mail from a retired laptop, IMAP backups, chat exports, SMS history. Three concepts keep that collection organized without losing any source's provenance.

## The Data Model

msgvault introduces three concepts, always in the same order: account, then identity, then collection. Each one builds on the previous.

**An account is one ingest source.** A Gmail sync is one account. An mbox import is another. It is the smallest durable unit of provenance in the archive. If you import the same real-world mailbox twice, once through Gmail sync and once from an old mbox export, you get two accounts. msgvault never silently merges them, and it never infers that two imports belong together just because an address, display name, or message content overlaps.

**An identity is the set of identifiers that mean "you" inside one account.** These are the email addresses, phone numbers, chat handles, or synthetic identifiers for that source. Identity is per-account because the same address can mean different things in different imports: an address that is unambiguously you in one source may be misleading in another. A confirmed identity lets msgvault treat a message as "from you" within that account's context, which is what makes sent-copy detection work during deduplication.

**A collection is a named group of accounts.** The `All` collection exists by default and contains every account. You create others (`work`, `personal`, or any grouping you like) to search, report, and deduplicate a logical group without changing the underlying sources. A collection is the boundary for every cross-account operation. A collection's identity is the union of its member accounts' identities, computed at read time, so you never manage it directly. Collections contain accounts only, never other collections.

<figure data-lightbox style="margin: 1.5rem 0; text-align: center;">
  <img src="/assets/generated/concepts/account-collection-concept.png" alt="Accounts on the left are individual ingest sources, each carrying the identifiers that mean you inside that source. Collections on the right are named groups of accounts: All contains every account, with Personal and Work as deliberate subsets." loading="lazy" style="width: 100%; display: block;" />
</figure>

Deduplication operates over all three concepts and has its own [Deduplication](/usage/deduplication/) page.

## OAuth Apps and Tokens

For personal Gmail accounts, a single `client_secret.json` supports all of them. Each `add-account` call authorizes one account and stores a separate token file.

Google Workspace organizations often restrict OAuth to apps created within their own org. If a Workspace account fails to authorize with your default app, create a separate OAuth app inside that org and add it as a named app in `config.toml`. See the [OAuth Setup Guide](/guides/oauth-setup/#google-workspace-accounts) for the full walkthrough.

Workspace admins can also use a Google service account with domain-wide delegation. Configure `service_account_key` under `[oauth]` or `[oauth.apps.<name>]`, authorize the service account client in the Google Admin Console, then run `msgvault add-account user@domain.com`. Service-account accounts do not store per-user refresh tokens; msgvault mints delegated tokens on demand.

<figure data-lightbox style="margin: 1.5rem 0; text-align: center;">
  <img src="/assets/generated/concepts/oauth-multi-account-concept.png" alt="Two OAuth apps and the token files they create. A default app (config block [oauth]) authorizes personal Gmail accounts personal@gmail.com and other@gmail.com; a named app ([oauth.apps.acme]) authorizes the Workspace account you@acme.com. Each add-account run writes its own token file under ~/.msgvault/tokens/, color-matched to its account." loading="lazy" style="width: 100%; display: block;" />
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

Each account has a confirmed "me" identity: the email addresses, phone numbers, chat handles, or synthetic identifiers that mean you inside that source. Deduplication uses this set for sent-copy detection, so for "sent" versus "received" to mean anything in older imports, msgvault needs to know which identifiers are you in each account.

New Gmail, IMAP, Microsoft 365, MBOX, EMLX, WhatsApp, and Google Voice sources auto-confirm the source identifier by default. Use `--no-default-identity` on supported add/import commands when that is not correct. (iMessage imports are exempt, because iMessage contacts are not self-identifying.)

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

Once several accounts hold overlapping copies of the same message, [deduplication](/usage/deduplication/) collapses each set to one visible survivor while keeping every source's provenance intact. It hides redundant copies rather than deleting them, and every step beyond hiding is a separate, opt-in action. See the [Deduplication](/usage/deduplication/) page for the detection rules, survivor selection, and the reversible safety ladder.

## TUI Filtering

Press `A` (uppercase) inside the TUI to open the account selector modal. Pick a single account to scope every view, or pick "All Accounts" to clear the filter. The currently selected account is shown in the title bar.

Meetings mode uses the same key for a separate source selector. It lists only
configured Granola and Circleback sources, and changing it does not replace the
Email account filter.

## Command Reference

See the [CLI Reference](/cli-reference/#add-account) for the complete flag list on `add-account`, `add-imap`, `add-o365`, `identity`, and `collection`.
