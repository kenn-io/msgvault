# How msgvault works

Sync or import your messages, link them to people, search your history, and
back up your archive. What msgvault saves depends on each source's access rules
and your media settings, and optional hosted processing sends selected data to
the providers you configure.

Check the [0.25.0 changelog](/docs/changelog/#0250) for new features and
upgrade steps.

1. [Capture](#capture)
2. [Preserve](#preserve)
3. [Resolve](#resolve)
4. [Curate](#curate)
5. [Understand](#understand)
6. [Search](#search)
7. [Analyze](#analyze)
8. [Act](#act)
9. [Own](#own)

## Capture

Connect a live source for recurring sync or import a local export you already
have. msgvault supports email, chat, calendars, meeting notes, and contacts.
Each source guide walks through setup, what history it captures, media limits,
and how interrupted runs resume.

[Choose a source](/docs/guides/sources/)

## Preserve

msgvault keeps original message data alongside the records used for browsing.
Downloaded attachments share storage when their contents match; they start as
individual files and can be grouped into packs. You can preview duplicate
messages before hiding extra copies, and hidden copies stay available to
restore.

[Data storage](/docs/architecture/storage/)

## Resolve

Connect the addresses and handles that belong to the same person.
msgvault groups identities using explicit links in the archive, and matching
display names alone never merge people. You review suggested identity matches
before accepting them.

[People, profiles, and identities](/docs/usage/people/)

## Curate

Save a profile to keep contact details, notes, employment, and relationships in
one place. You can inspect the evidence behind each profile fact and correct the
fact when needed. Activity calendars show when you were in contact, and profile
history tracks supported merges and reversals.

[Curating people](/docs/usage/people/)

## Understand

Turn on search by meaning with a local or hosted embedding service. You
configure document and image processing separately and approve uploads before
sending attachment content to a provider. You can rebuild search indexes from
your archive, and stored evidence and saved profiles remain part of the record.

[Vector search](/docs/usage/vector-search/)

## Search

Find messages offline with keywords and filters such as sender, subject, and
date. Semantic search finds related meanings through your configured service,
and hybrid search combines both. Coverage and ranking details show what content
the active mode can find.

[Searching](/docs/usage/searching/)

## Analyze

See which people, domains, labels, and time periods account for your messages
and storage, then drill down from any group to individual messages in the
terminal or browser. SQLite archives use a separate analytics cache so these
summaries never scan message bodies.

[Analytics and stats](/docs/usage/analytics/)

## Act

Select messages to create a deletion manifest, which is a saved list you can review
before removing mail from a provider. Running it requires a separate CLI command
and your consent. Gmail and IMAP move messages to Trash by default, while
Microsoft Graph mail moves them to Deleted Items; permanent deletion requires
an explicit option. Your archived copies remain available unless you purge them
locally in a separate step.

[Deleting email](/docs/usage/deletion/)

## Own

Run msgvault on your laptop or your own server. One binary gives you the
browser interface, API, scheduled jobs, and assistant tools. For SQLite
archives, backup snapshots include both the database and attachments and restore
without contacting the original providers. PostgreSQL archives require separate
database backups.

[Backup and restore](/docs/usage/backup/)

## Next

[Install msgvault and connect your first account](/docs/setup/). The
[docs](/docs/) cover commands, configuration, and how msgvault stores your data.
