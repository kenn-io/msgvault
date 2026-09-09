---
last_edited: 2026-09-08
title: Frequently Asked Questions
description: Common questions about msgvault, Gmail API safety, and what the tool can and cannot do.
---

<p class="faq-question">Can msgvault send email?</p>

No. msgvault archives and analyzes messages; it does not compose, send, forward,
or reply to mail. Gmail authorization requests `gmail.modify` by default for
archive and deletion workflows. `add-account --readonly` requests read-only
access instead. See [read-only Gmail access](guides/oauth-setup.md#read-only-access)
for existing-account restrictions.

<p class="faq-question">What can an AI assistant do through MCP?</p>

MCP exposes archive search, messages, files, analytics, and people tools.
Assistants can read private message content and can explicitly request private
person notes. General profile reads omit Notes and sensitive attributes; this
is not a promise that the assistant cannot access private archive content.

MCP can stage a deletion manifest but cannot execute remote deletion, send mail,
or sync new messages. Optional profile writes require `--allow-profile-writes`;
HTTP write tools also need `--http-allow-writes`. Execution of a staged mail
deletion remains a separate CLI step. See the [MCP tool and access reference](usage/chat.md).

Treat imported messages, attachments, and generated briefs as untrusted input to
an assistant. Choose an assistant and model provider you are willing to give
that data to, and review its requested actions. MCP is an access interface,
not a guarantee against prompt injection.

<p class="faq-question">Does everything work offline?</p>

Keyword search, ordinary archive reads, and analytics use stored data. Sync
contacts the source service. Optional semantic search, document extraction,
profile automation, and external enrichment can send selected data to your
configured providers. A supported local embedding endpoint keeps that embedding
work local; it does not automatically change the providers used by other
features. See [recommended configuration](usage/recommended-configuration.md).

<p class="faq-question">Why is a documented feature missing from my binary?</p>

The documentation follows current `main`, including work after 0.19.3 that is
not yet released. Check `msgvault version` and the installed command's `--help`,
then consult [the changelog](changelog.md#unreleased). A configured remote daemon also
needs a compatible version.

<p class="faq-question">What is the web server for?</p>

`msgvault serve` starts the first-party analytical Web UI and the REST API it
uses. Open it to search and group across email, chat, calendar, and meeting
data, inspect people and files, monitor sources, and review staged deletions.
The same server supports automations, integrations, and scheduled background
sync. See [Web UI](/docs/web-ui/) and [Web UI & API Server](/docs/api-server/) for details.

<p class="faq-question">Where is my email data stored?</p>

By default, everything stays on your local machine. msgvault stores messages in a SQLite database and Parquet analytics files inside your `MSGVAULT_HOME` directory (defaults to `~/.msgvault`). If you configure a remote deployment, that archive lives on your own server. See [Data Storage](/docs/architecture/storage/) for details.

<p class="faq-question">Can I use msgvault with non-Gmail accounts?</p>

Yes. You can sync any standard IMAP server, Microsoft 365 mail and Teams,
Discord guilds, Slack workspaces, Beeper Desktop chats, Google Calendar, and supported meeting
note services. You can also import email from PST, MBOX, MailMate-style EML directories, or Apple Mail and
chats/texts from WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS
Backup & Restore. All messages use the same Web UI, search, TUI, MCP, REST API,
and export surfaces. See [Setup Guide](/docs/setup/#add-an-imap-account),
[Importing Local Email](/docs/usage/importing/), [Text Messages](/docs/usage/text-messages/),
and [Discord](/docs/usage/discord/) or [Slack](/docs/usage/slack/).

<p class="faq-question">Can msgvault archive Discord direct messages?</p>

No. Discord bot tokens expose guilds the bot has joined, not a person's direct
messages. msgvault does not accept user tokens or implement selfbots. It can
archive accessible guild channels, threads, forum posts, and attachments; see
[Discord](/docs/usage/discord/).

<p class="faq-question">Does deleting email in msgvault delete it from Gmail?</p>

Only if you explicitly run the full deletion workflow. Staging messages for
deletion in the Web UI or TUI does not touch Gmail or your IMAP provider.
Starting in v0.20.0, remote deletion remains permanently opt-in. The invoking
CLI may enable it durably with `[deletion] remote_enabled = true` or for one
command with `MSGVAULT_ENABLE_REMOTE_DELETE=1`. Both mechanisms are permanent;
there is no planned automatic removal of the guardrail. A remote daemon's own
`[deletion]` section is not policy for a command invoked elsewhere. Gmail and
IMAP move messages to Trash by default; `--permanent` requests permanent
deletion. Recovery from Trash depends on the provider. Remote deletion retains
archived content and records source-deletion state. A separate local purge
with `gc` or `delete-deduped` can remove archive data. See
[Deleting Email](/docs/usage/deletion/) for the complete process.

---

Have a question not covered here? Join the [msgvault Discord server](https://discord.gg/fDnmxB8Wkq) or [open an issue on GitHub](https://github.com/kenn-io/msgvault/issues).
