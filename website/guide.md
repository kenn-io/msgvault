# How msgvault works

Sync or import your messages, link them to people, search your history, and
back up your archive. What gets saved depends on source access and your media
settings. Optional hosted processing sends selected data to the providers you
configure.

See the [0.20.0 changelog](/docs/changelog/#0200) for new features and upgrade
steps.

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

Choose a connected source for recurring sync or import a local export.
Supported sources include email, chat, calendars, meeting notes, and contacts.
Each source guide explains setup, captured history, media limits, and how
interrupted work resumes.

[Choose a source](/docs/guides/sources/)

## Preserve

Keep original message data alongside the records used for browsing. Downloaded
attachments share storage when their contents match. They start as individual
files and can be grouped into packs. Preview duplicate messages before hiding
extra copies; hidden copies remain available to restore.

[Data storage](/docs/architecture/storage/)

## Resolve

Connect the addresses and handles that belong to the same person. msgvault
groups identities using explicit links in the archive; matching display names
alone do not merge people. Review suggested identity matches before accepting
them.

[People, profiles, and identities](/docs/usage/people/)

## Curate

Save a profile to keep contact details, notes, employment, and relationships
together. Inspect the evidence behind profile facts and correct them when
needed. Activity calendars show when you were in contact; profile history
records supported merges and reversals.

[Curating people](/docs/usage/people/)

## Understand

Enable search by meaning with a local or hosted embedding service. Separately
configure document and image processing, and approve uploads before sending
attachment content to a provider. You can rebuild search indexes from the
archive; stored evidence and saved profiles remain part of the record.

[Vector search](/docs/usage/vector-search/)

## Search

Find messages offline with keywords and filters such as sender, subject, and
date. Semantic search finds related meanings using your configured service.
Hybrid search combines both. Coverage and ranking details show which content
the selected mode can find.

[Searching](/docs/usage/searching/)

## Analyze

See which people, domains, labels, and periods account for your messages and
storage. Drill down from a group to individual messages in the terminal or
browser. SQLite archives use a separate analytics cache so these summaries do
not scan message bodies.

[Analytics and stats](/docs/usage/analytics/)

## Act

Select messages and create a deletion manifest: a saved list you can review
before removing mail from a provider. A separate CLI command requires your
consent to execute it. Gmail and IMAP move messages to Trash by default;
permanent deletion needs an explicit option. Archived content remains
available unless you separately purge it locally.

[Deleting email](/docs/usage/deletion/)

## Own

Run msgvault on your laptop or your own server. One binary provides the browser
interface, API, scheduled work, and tools for assistants. For SQLite archives,
backup snapshots include the database and attachments and restore without
contacting the original providers. PostgreSQL archives require separate
database backups.

[Backup and restore](/docs/usage/backup/)

## Next

[Install msgvault and connect your first account](/docs/setup/). The
[docs](/docs/) cover commands, configuration, and how msgvault stores your data.
