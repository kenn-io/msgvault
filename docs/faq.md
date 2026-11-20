---
last_edited: "2026-11-19"
title: Frequently asked questions
description: Common questions about msgvault, Gmail API safety, and what the tool can and cannot do.
---

<p class="faq-question">Can msgvault send email or prepare a reply?</p>

msgvault never sends email. For an IMAP source, an operator can grant access to
one exact Drafts folder, and `draft-reply` can then create a plain-text reply
draft for you to review and send in your usual mail app. That grant is off by
default. See [IMAP drafts](usage/imap.md#drafts).

Gmail authorization requests `gmail.modify` by default for archive and deletion
workflows. If you want read-only access instead, run `add-account --readonly`.
For restrictions on existing accounts, see
[read-only Gmail access](guides/oauth-setup.md#read-only-access).

<p class="faq-question">What can an AI assistant do through MCP?</p>

MCP gives an assistant access to archive search, messages, files, analytics, and
people tools. Assistants can read private message content and can explicitly
request private person notes. General profile reads omit Notes and sensitive
attributes, which is not a promise that the assistant cannot access private
archive content.

MCP can stage a deletion manifest, but it cannot execute remote deletion or send
mail. Executing a staged mail deletion remains a separate CLI step. Source sync
requires `--allow-source-writes`, and bounded historical Gmail or IMAP imports
require `--allow-import-writes`; each of those calls requires client
confirmation. Optional profile writes require
`--allow-profile-writes`, calendar event mutations require
`--allow-calendar-writes` along with explicit per-operation approval in an MCP
client that supports elicitation, and Kata issues that quote archive text
require `--allow-kata-writes`. HTTP writes also require `--http-allow-writes`.
For the full list, see the [MCP tool and access reference](usage/chat.md).

Treat imported messages, attachments, and generated briefs as untrusted input to
an assistant. Pick an assistant and model provider you are comfortable sharing
that data with, and review any actions it requests. MCP is an access interface,
not a guarantee against prompt injection.

<p class="faq-question">Does everything work offline?</p>

Keyword search, regular archive reads, and analytics run against your stored
data offline. Syncing contacts the source service. Optional features such as
semantic search, document extraction, profile automation, and external
enrichment can send selected data to the providers you configure. Using a
supported local embedding endpoint keeps embedding work local, though it does
not automatically change the providers for other features. See
[recommended configuration](usage/recommended-configuration.md). If you opt in,
msgvault also sends anonymous
[usage telemetry](configuration.md#usage-reporting).

<p class="faq-question">Why is a documented feature missing from my binary?</p>

The documentation follows current `main`, and your installed binary may be
older. Check `msgvault version` and the installed command's `--help` output, then compare
them with the
[latest release notes](https://github.com/kenn-io/msgvault/releases/latest). If
you run a remote daemon, upgrade the CLI and the daemon together.

<p class="faq-question">What is the web server for?</p>

`msgvault serve` starts the first-party analytical Web UI and the REST API
behind it. Use it to search and group email, chat, calendar, and meeting data,
inspect people and files, monitor sources, and review staged deletions. The same
server handles automations, integrations, and scheduled background sync. See
[Web UI](/docs/web-ui/) and [Web UI & API Server](/docs/api-server/) for
details.

<p class="faq-question">Where is my email data stored?</p>

By default, everything stays on your own machine. msgvault stores messages in a
SQLite database and Parquet analytics files inside your `MSGVAULT_HOME`
directory, which defaults to `~/.msgvault`. If you set up a remote deployment,
that archive lives on your own server instead. See
[Data Storage](/docs/architecture/storage/) for details.

<p class="faq-question">Can I use msgvault with non-Gmail accounts?</p>

Yes. You can sync any standard IMAP server, Microsoft 365 mail and Teams,
Discord guilds, Slack workspaces, Beeper Desktop chats, Google Calendar, and
supported meeting note services. You can also import email from PST, MBOX,
MailMate-style EML directories, or Apple Mail, and import chats or texts from
WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup & Restore.
Everything shares the same Web UI, search, TUI, MCP, REST API, and export tools.
See [Setup Guide](/docs/setup/#add-an-imap-account),
[Importing Local Email](/docs/usage/importing/),
[Text Messages](/docs/usage/text-messages/), and
[Discord](/docs/usage/discord/) or [Slack](/docs/usage/slack/).

<p class="faq-question">Can msgvault archive Discord direct messages?</p>

Discord bot sync cannot archive direct messages, because bot tokens expose
guilds the bot has joined rather than a person's direct messages, and msgvault
does not accept user tokens or implement selfbots. Bot sync can archive
accessible guild channels, threads, forum posts, and attachments.

Separately, `msgvault import-discord-package` imports the exporting account's
own sent messages, including DMs, from an official Discord data package.
Messages from other people are not in the package. See
[Discord](/docs/usage/discord/) for both workflows.

<p class="faq-question">Does deleting email in msgvault delete it from Gmail?</p>

Only if you explicitly run the full deletion workflow. Staging messages for
deletion in the Web UI or TUI does not touch Gmail or your IMAP provider.
Starting in v0.20.0, remote deletion stays permanently opt-in: you can enable it
durably in the invoking CLI with `[deletion] remote_enabled = true` or for a
single command with `MSGVAULT_ENABLE_REMOTE_DELETE=1`. Both opt-in mechanisms
are permanent, and there is no planned automatic removal of the guardrail. A
remote daemon's own `[deletion]`
section does not set policy for a command invoked elsewhere.

Gmail and IMAP move messages to Trash by default, and `--permanent` requests
permanent deletion. Whether you can recover messages from Trash depends on the
provider. Remote deletion keeps your archived content and records the
source-deletion state. To remove data from your local archive, run a separate
purge with `gc` or `delete-deduped`. See
[Deleting Email](/docs/usage/deletion/) for the full process.

---

Have a question not covered here? Join the
[msgvault Discord server](https://discord.gg/fDnmxB8Wkq) or
[open an issue on GitHub](https://github.com/kenn-io/msgvault/issues).
