# msgvault

**Keep and search your message history.**

msgvault is open source and runs on your hardware. Save email, chats, meetings,
calendars, and contacts on your own computer or server. Search across accounts,
look up a person's history, and work from a browser, terminal, or AI assistant.

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

Then [read how msgvault works](/guide/).

## Bring your accounts together

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
- **Contacts** — import contacts from a CardDAV address book and choose which
  saved profiles to sync back.

## Keep contact details and message history together

Connect the addresses, handles, and phone numbers that belong to one person.
Keep information found in the archive separate from the profile details you
choose to save.

### Link addresses to the same person

msgvault groups identities using explicit links in the archive. Matching display
names alone do not merge two people.

### Save names, notes, and contact details

Save a profile to keep names, notes, and contact details when linked identities
change. Review profile history, merge duplicates, and reverse supported merges.

### See where a profile fact came from

Maintain organizations, employment, relationships, and custom fields. Inspect
the evidence behind a fact and pin a correction so automated updates keep your
choice.

### See when you were last in contact

See when you exchanged messages or shared events and meetings. Activity
calendars and relationship scores help you find frequent contacts and people you
have not heard from recently.

## Browse your archive

Search messages, browse files, and maintain contacts in the browser. Save a
useful view or share its URL with someone who has access to your archive.
Use Back and Forward to return to earlier views.

Maintain profiles, review identities and merges, publish contacts through
CardDAV, and catch up with a saved conversation brief in **Directory**.
Check syncs and background jobs in **Operations**. Update provider credentials
in **Settings**, which shows changes that take effect after a restart.
[Explore the workspaces](/docs/web-ui/).

## Search messages and attachments

Keyword search reads your archive offline. Optional search by meaning sends
message and query text to the embedding service you configure, which can run
locally. Document, image, and profile processing have separate settings and
consent steps.

- **Hybrid search:** search with familiar filters such as sender, subject, and
  date. Use semantic search to find related meanings, or hybrid search to
  combine words and meaning. Ranking details explain each result.
- **Search models:** an embedding service turns text into numbers used to compare
  meaning. Choose a supported local or hosted service and select which accounts
  to index.
- **Attachments:** find text inside supported attachments or search images by
  their content. The embedded [Docbank](https://github.com/kenn-io/docbank) engine
  manages this processing. Review provider access and approve uploads before
  sending attachment content.
- **Agents:** an MCP server exposes search, people, files, and analytics
  tools to Claude Desktop and other agents; bundled agent skills install into
  Claude Code and Codex. Allowing an agent to edit profiles requires explicit
  flags.

## Use the browser, terminal, or an assistant

One background service, the daemon, coordinates archive access and scheduled
work. Use the browser, terminal, scripts, or an assistant to work with the same
archive.

- **CLI:** scriptable sync, search, and repair.
- **Web:** search, browse, and manage contacts.
- **TUI:** explore message counts and storage with the keyboard.
- **HTTP:** an authenticated, versioned API.
- **MCP:** archive tools for AI assistants.
- **Skills:** workflows for Claude Code and Codex.

[Connect an agent](/docs/usage/chat/) or [inspect the API](/docs/api-server/).

## Review mail before deleting provider copies

Back up your archive and check the messages you intend to remove before deleting
from a provider. Review the selection, then run a separate command to move mail
to Trash or permanently delete it.

- **Verify:** for Gmail, compare message counts and check a sample of stored
  messages. This does not prove every message or attachment was captured.
  Review the items you plan to delete and keep a backup.
- **Stage:** create a deletion manifest: a saved list of messages to review.
  You can create it from the CLI, Web UI, TUI, or MCP. Staging does not remove
  provider messages; execution is a separate CLI command.
- **Execute:** the CLI requires explicit client consent. Gmail and IMAP default
  to moving messages to Trash; permanent deletion requires explicit opt-in.
  Archived messages and attachments remain available; msgvault records their
  deletion from their source.
- **Restore:** back up SQLite archives with snapshots of the database and
  attachments. New snapshots leave earlier ones intact, and you can verify
  them before restoring. Restoring does not require the original providers.
  PostgreSQL databases need separate backups.

## Keep a record beyond the provider.

Use msgvault alongside your mail client and provider exports. Keep searching
your saved history after messages leave the provider.

- **Mail client:** read, compose, and send mail. msgvault can prepare managed
  IMAP drafts for review there; it does not send mail.
- **Export archive:** keep a snapshot in a provider's format. Import supported
  exports into msgvault to browse and search them alongside other sources.
- **msgvault:** keep captured history available after it leaves the provider.
  Sync supported accounts, connect identities, search across sources, and use
  your own tools.

## Set up your first archive

Read the [guide](/guide/) for an overview, or go straight to
[setup](/docs/setup/). The [docs](/docs/) cover commands, configuration, and how
msgvault stores your data.
