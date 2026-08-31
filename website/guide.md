# The archive lifecycle

One archive moves through nine deliberate stages. Your data stays local,
complete, and yours at every stop.

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
Google Calendar, CardDAV, meeting notes. Dead exports import once — MBOX,
Apple Mail, PST, WhatsApp, iMessage, Messenger, SMS backups. Interrupted syncs
resume from checkpoints.

[Importing local email](/docs/usage/importing/)

## Preserve

Raw provider payloads are retained compressed beside the parsed record.
Attachments are content-addressed by SHA-256, deduplicated, and sealed into
immutable packs. Cross-account duplicates hide behind a reversible safety
ladder — the surviving copy is always the complete one.

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
Every intelligence lane is disposable and rebuildable; the record is not.

[Vector search](/docs/usage/vector-search/)

## Search

Full-text search with Gmail-style operators answers instantly and offline.
Semantic and hybrid modes fuse BM25 with vectors through reciprocal rank
fusion, with explainable ranking and honest coverage states — msgvault never
quietly substitutes one mode for another.

[Searching](/docs/usage/searching/)

## Analyze

A DuckDB-over-Parquet analytics cache answers aggregate questions across
hundreds of thousands of messages in milliseconds: senders, domains, labels,
time. Drill down from a decade to a single message in the TUI or the browser.

[Analytics and stats](/docs/usage/analytics/)

## Act

Staging and execution never share a surface. Any interface can stage a
deletion manifest for review; only the CLI executes it, behind an explicit
environment gate, defaulting to recoverable trash. The local archive is never
modified — deleted mail remains searchable forever.

[Deleting email](/docs/usage/deletion/)

## Own

Run it on a laptop or serve it from your own NAS — the daemon carries the Web
UI, HTTP API, scheduler, and MCP server in one binary. Verifiable backup
snapshots restore the archive with no provider in the loop. Your
conversations, your hardware, your rules.

[Backup and restore](/docs/usage/backup/)

## Next

Move from the lifecycle model to [installation and setup](/docs/setup/) or
[all documentation](/docs/).
