# msgvault

**Your communications. Your relationships. One archive.**

msgvault is a local-first, open-source archive. Bring email, chat, meetings, and
contacts together on your own hardware. Find what matters, connect the people
behind it, and use your history from the terminal, browser, or an AI assistant.

![People and communications flow into one msgvault archive, accessible through the TUI, Web UI, and MCP for AI assistants.](/assets/archive-flow.svg)

msgvault is usable through the CLI, browser application, terminal interface,
HTTP API, MCP server, and bundled agent skills. It is alpha software — back up
your data.
[See what changed in 0.20.0 and read the upgrade notes](/docs/changelog/#0200).

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

Bring history from several providers into one searchable archive. Sync connected
accounts or import local exports. Keep original message data and downloaded
attachments alongside the records you browse.

- **Mail** — Gmail, IMAP, and Microsoft 365 sync; MBOX, Maildir, Apple Mail, PST, and
  EML imports.
- **Chat** — Slack, Teams, Discord, and chats available through Beeper Desktop;
  WhatsApp, iMessage, Google Voice, Messenger, and SMS imports.
- **Meetings** — Granola, Circleback, and Notion AI Meeting Notes in the same
  searchable record.
- **Calendar** — Google Calendar events, organizers, and attendees, read-only.
- **Contacts** — bidirectional CardDAV: pull address books, publish curated
  people back.

## Messages come from addresses. Relationships come from people.

Connect the addresses, handles, and phone numbers that belong to one person.
Keep information found in the archive separate from the profile details you
choose to save.

### Identity evidence

msgvault groups identities using explicit links in the archive. Matching display
names alone do not merge two people.

### Saved profiles

Save a profile to keep names, notes, and contact details when linked identities
change. Review profile history, merge duplicates, and reverse supported merges.

### Profile history

Maintain organizations, employment, relationships, and custom fields. Inspect
the evidence behind a fact and pin a correction so automated updates keep your
choice.

### Activity

See when you exchanged messages or shared events and meetings. Activity
calendars and relationship scores help you find frequent contacts and people you
have not heard from recently.

## Work the archive in the browser

Search messages, browse files, and maintain contacts in the browser. Save a
useful view or share its URL with someone who has access to your archive.
Browser Back and Forward restore your browsing context.

Maintain profiles, review identities and merges, publish contacts through
CardDAV, and catch up with a saved conversation brief in **Directory**.
**Operations** tracks syncs and background work; **Settings** manages provider
credentials and restart-pending changes. [Explore the workspaces](/docs/web-ui/).

## Semantic search and document understanding

Keyword search reads your archive offline. Optional search by meaning sends
message and query text to the embedding service you configure, which can run
locally. Document, image, and profile processing have separate settings and
consent steps.

- **Hybrid search:** search with familiar filters such as sender, subject, and
  date. Use semantic search to find related meanings, or hybrid search to
  combine words and meaning. Ranking details explain each result.
- **Local models:** an embedding service turns text into numbers used to compare
  meaning. Choose a supported local or hosted service and select which accounts
  to index.
- **Attachments:** find text inside supported attachments or search images by
  their content. The embedded [Docbank](https://github.com/kenn-io/docbank) engine
  manages this processing. Review provider access and approve uploads before
  sending attachment content.
- **Agents:** an MCP server exposes search, people, files, and analytics
  tools to Claude Desktop and other agents; bundled agent skills install into
  Claude Code and Codex. Profile writes stay behind explicit flags.

## One archive across every surface

One background service, the daemon, coordinates archive access and scheduled
work. Use the browser, terminal, scripts, or an assistant to work with the same
archive.

- **CLI:** scriptable sync, search, and repair.
- **Web:** analytical workspaces in the browser.
- **TUI:** keyboard drill-down analytics.
- **HTTP:** an authenticated, versioned API.
- **MCP:** archive tools for AI assistants.
- **Skills:** workflows for Claude Code and Codex.

[Connect an agent](/docs/usage/chat/) or [inspect the API](/docs/api-server/).

## Archive everything. Then delete upstream.

Back up your archive and check the messages you intend to remove before deleting
from a provider. Review the selection, then run a separate command to move mail
to Trash or permanently delete it.

- **Verify:** for Gmail, compare message counts and check a sample of stored
  messages. This does not prove every message or attachment was captured.
  Review the items you plan to delete and keep a backup.
- **Stage:** create a deletion manifest from the CLI, Web UI, TUI, or MCP,
  then inspect it. Staging does not remove provider messages; execution is a
  separate CLI command.
- **Execute:** the CLI requires explicit client consent. Gmail and IMAP default
  to moving messages to Trash; permanent deletion requires explicit opt-in.
  Archived messages and attachments remain available; msgvault records their
  source-deletion state.
- **Restore:** append-only, verifiable backup snapshots cover the database and
  attachments, with restore paths that need no provider at all.

## Keep a record beyond the provider.

Choose how you use your communications history. msgvault keeps a searchable
archive that you operate, query, and extend.

- **Mail client:** read, compose, and send mail. msgvault can prepare managed
  IMAP drafts for review there; it does not send mail.
- **Export archive:** keep a snapshot in a provider's format. Import supported
  exports into msgvault to browse and search them alongside other sources.
- **msgvault:** keep captured history available after it leaves the provider.
  Sync supported accounts, connect identities, search across sources, and use
  your own tools.

## Follow one archive through the system

The [lifecycle guide](/guide/) walks the archive from capture to ownership.
The [documentation](/docs/) carries setup, exact command behavior,
configuration, and architecture.
