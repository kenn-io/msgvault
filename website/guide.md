# The archive lifecycle

Follow your archive from first capture to long-term ownership. Source access
and media policies determine what is captured. Optional hosted processing
sends selected data to the providers you configure.

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

Live sources sync on a schedule — Gmail, IMAP, Slack, Teams, Discord, Beeper,
Google Calendar, CardDAV, meeting notes. Local exports import on demand — MBOX, Maildir,
Apple Mail, PST, EML, Slackdump, WhatsApp, iMessage, Messenger, and SMS backups.
Interrupted syncs resume from checkpoints.

[Importing local email](/docs/usage/importing/)

## Preserve

Raw provider payloads are retained compressed beside the parsed record.
Attachments are content-addressed by SHA-256, deduplicated, and sealed into
immutable packs. Cross-account duplicates hide behind a reversible safety
ladder — msgvault checks source preference, raw message evidence, and attachment
completeness under defined rules.

[Data storage](/docs/architecture/storage/)

## Resolve

Every source knows you and your contacts by different addresses and handles.
Identity discovery classifies the evidence; observed people cluster from
explicit archive links, never from matching display names. Nothing merges
without proof.

[People, profiles, and identities](/docs/usage/people/)

## Curate

Promote the people who matter into durable profiles with stable IDs and vCard
UIDs. Attach typed attributes, organizations, employment history, and
relationships over a fact ledger with evidence and reversible merges. Watch
each relationship's activity calendar and temperature across every channel.

[Curating people](/docs/usage/people/)

## Understand

Opt in to semantic search by pointing msgvault at an embedding server you
choose — local ones included. The embedded Docbank document engine extracts
and indexes attachment text and images behind explicit, fail-closed consent.
Search indexes can be rebuilt from the archive. Stored evidence and curated
profiles remain part of the record.

[Vector search](/docs/usage/vector-search/)

## Search

Full-text search with Gmail-style operators answers instantly and offline.
Semantic mode finds results by meaning. Hybrid mode combines keyword and
vector rankings, with explicit coverage and ranking details; msgvault never quietly
substitutes one mode for another.

[Searching](/docs/usage/searching/)

## Analyze

A DuckDB-over-Parquet analytics cache answers aggregate questions across
hundreds of thousands of messages in milliseconds: senders, domains, labels,
time. Drill down from a decade to a single message in the TUI or the browser.

[Analytics and stats](/docs/usage/analytics/)

## Act

Stage a deletion manifest from the CLI, browser, TUI, or MCP and review it
before execution. The separate CLI execution step requires client consent.
Gmail and IMAP default to moving messages to Trash; permanent deletion requires
explicit opt-in. Archived content remains searchable unless you separately
purge it locally.

[Deleting email](/docs/usage/deletion/)

## Own

Run it on a laptop or serve it from your own NAS: the daemon carries the Web
UI, HTTP API, scheduler, and MCP server in one binary. Verifiable backup
snapshots restore the archive with no provider in the loop.

[Backup and restore](/docs/usage/backup/)

## Next

Move from the lifecycle model to [installation and setup](/docs/setup/) or
[all documentation](/docs/).
