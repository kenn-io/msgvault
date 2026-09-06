# msgvault

**The system of record for your communications and relationships.**

msgvault is a local-first, open-source archive for a lifetime of email, chat,
meetings, calendars, and contacts. It keeps everything in one database on your
own hardware, resolves the people behind decades of messages, and searches by
keyword or by meaning.

msgvault is usable through the CLI, browser application, terminal interface,
HTTP API, MCP server, and bundled agent skills. It is alpha software — back up
your data.

## Install

On macOS or Linux:

```sh
curl -fsSL https://msgvault.io/install.sh | bash
```

Or with Homebrew:

```sh
brew install msgvault
```

On Windows (PowerShell):

```powershell
irm https://msgvault.io/install.ps1 | iex
```

The installers fetch the latest GitHub release and verify its SHA-256
checksum. msgvault is also on
[conda-forge](https://prefix.dev/channels/conda-forge/packages/msgvault), and
the [setup documentation](/docs/setup/) covers building from source.

Then [follow the archive lifecycle](/guide/).

## Every channel. One archive.

Twenty years of correspondence should not be scattered across a dozen walled
gardens. msgvault syncs live sources and imports dead exports into one schema,
keeping raw payloads and content-addressed attachments intact.

- **Mail** — Gmail, IMAP, and Microsoft 365 sync; MBOX, Apple Mail, PST, and
  EML imports.
- **Chat** — Slack, Teams, Discord, and every network behind Beeper; WhatsApp,
  iMessage, Messenger, and SMS imports.
- **Meetings** — Granola and Circleback notes and transcripts in the same
  searchable record.
- **Calendar** — Google Calendar events, organizers, and attendees, read-only.
- **Contacts** — bidirectional CardDAV: pull address books, publish curated
  people back.

## Messages come from addresses. Relationships come from people.

The people layer resolves decades of addresses, handles, and phone numbers into
the people behind them — with archive evidence and user curation kept strictly
apart.

### Observed, not guessed

Observed people are assembled from explicit archive links across sources. Equal
display names alone never merge two people.

### Durable profiles

A promoted profile gets a stable ID and vCard UID, so names, notes, and typed
attributes survive later identity changes. Merges are atomic and reversible.

### Fact ledger

Organizations, employment history, typed relationships, and custom attributes
rest on immutable evidence, deterministic decisions, and per-person pins.

### Activity

An activity calendar tracks interaction with each person across email, chat,
calendar, and meetings, year by year, including current and peak relationship
temperature.

## Work the archive in the browser

The daemon serves a dense, keyboard-driven browser application: relationships,
a unified Everything table, files, saved views, source status, deletion
staging, and settings. Every analytical slice is URL-addressable, so Back and
Forward restore exact views.

**In development:** an open pull request adds Directory and Reviews workspaces —
durable-person search, profile maintenance and history, identity and merge
review queues, the privacy-gated fact ledger, CardDAV publication, and a
self-describing Settings surface with write-only credential management.

## Semantic search and document understanding

Keyword search works offline, always. Semantic search, document extraction,
and visual search are opt-in, with explicit consent recording exactly what
leaves your machine and where it goes.

- **Hybrid search:** FTS5 with Gmail-style operators, pure semantic search,
  or hybrid BM25-plus-vector fusion via reciprocal rank fusion, with an
  explain mode that shows why each result ranked.
- **Local models:** any OpenAI-compatible endpoint works: Ollama, llama.cpp,
  LM Studio, or Apple's on-device model. Embedding scope is a privacy
  boundary; out-of-scope accounts are never sent anywhere.
- **Attachments:** the embedded
  [Docbank](https://github.com/kenn-io/docbank) document engine handles OCR
  extraction, normalized chunks, lexical and semantic document search, and
  visual search over images. Consent-gated and fail-closed.
- **Agents:** an MCP server exposes search, people, files, and analytics
  tools to Claude Desktop and other agents; bundled agent skills install into
  Claude Code and Codex. Profile writes stay behind explicit flags.

## One archive across every surface

The daemon owns all writes and serializes every mutation. People, scripts, and
agents work through the interface suited to the task, against the same record.

- **CLI:** scriptable sync, search, and repair.
- **Web:** analytical workspaces in the browser.
- **TUI:** keyboard drill-down analytics.
- **HTTP:** an authenticated, versioned API.
- **MCP:** archive tools for AI assistants.
- **Skills:** workflows for Claude Code and Codex.

[Connect an agent](/docs/usage/chat/) or [inspect the API](/docs/api-server/).

## Archive everything. Then delete upstream.

Once the archive is complete and verified, you can start deleting from the
provider. Every step is explicit and reviewed, and nothing is irreversible
until the last one.

- **Verify:** integrity verification checks the archive against the mailbox
  before you trust it with anything irreversible.
- **Stage:** deletions are staged into manifests from the Web UI, TUI, or
  MCP — inspected, counted, and cancellable. No surface executes them.
- **Execute:** execution is a separate CLI step behind an explicit environment
  gate, defaulting to recoverable trash. The local archive is never modified.
- **Restore:** append-only, verifiable backup snapshots cover the database and
  attachments, with restore paths that need no provider at all.

## Not a mail client. Not a takeout file.

msgvault is a data warehouse for your communications: a system of record you
operate, query, and extend. Not a viewport, and not cold storage.

- **Mail client:** the provider is the record. A client renders whatever the
  server still holds; identity, search, and history live and die with the
  account.
- **Export archive:** the zip is a snapshot. A takeout captures one moment in
  one format; it does not sync, resolve people, answer questions, or talk to
  agents.
- **msgvault:** the archive is the record. Providers become replaceable feeds
  around a database you own — continuously synced, people-resolved, searchable
  by meaning, and open to your tools.

## Follow one archive through the system

The [lifecycle guide](/guide/) walks the archive from capture to ownership.
The [documentation](/docs/) carries setup, exact command behavior,
configuration, and architecture.
