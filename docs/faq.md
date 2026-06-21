---
title: Frequently Asked Questions
description: Common questions about msgvault, Gmail API safety, and what the tool can and cannot do.
---

<p class="faq-question">Can msgvault send email?</p>

**No.** msgvault cannot send, forward, or reply to email. While msgvault requests full Gmail account access (required for features like sync, search, and deletion), we have intentionally not built any send functionality into the tool. This is both a scope discipline decision and a security one: msgvault is an archival and analysis tool, not an email client. Whether you are using the CLI directly, running it from a script, or letting an AI agent operate through the MCP server, there is no code path in msgvault that composes or sends mail.

<p class="faq-question">Is it safe to use with AI agents (MCP, Claude, etc.)?</p>

For normal use, yes. The MCP server exposes read, search, and deletion-staging operations (no sync, no sending, no direct deletion). An AI agent operating through the MCP server can read and search your local archive, and can stage messages for deletion by creating a local manifest file. Staged deletions are not executed until you explicitly run `msgvault delete-staged` from the CLI.

However, you should be aware of prompt injection risks. If an adversary can influence the prompts your LLM processes (through malicious email content, for example), the agent could be manipulated into reading sensitive messages such as password reset links or two-factor codes. In a worst case scenario, this could allow an attacker to compromise accounts by combining prompt injection with the ability to read your inbox.

The MCP server's lack of sync capability offers some protection here, since an agent cannot pull new messages on demand. But this project does not claim to be immune to LLM security issues. Users are ultimately responsible for their own security setup and for understanding the risks of giving LLMs access to sensitive data.

<p class="faq-question">What is the web server for?</p>

`msgvault serve` starts a web server that lets you query your archive programmatically via a REST API. It is useful for building dashboards, running automations, or integrating with other tools. The server can also run background sync on a cron schedule to keep your archive up to date. See [Web Server](/api-server/) for details.

<p class="faq-question">Where is my email data stored?</p>

Everything stays on your local machine. msgvault stores messages in a SQLite database and Parquet analytics files inside your `MSGVAULT_HOME` directory (defaults to `~/.msgvault`). No data is sent to any remote server. See [Data Storage](/architecture/storage/) for details.

<p class="faq-question">Can I use msgvault with non-Gmail accounts?</p>

Yes. You can sync from any standard IMAP server using `msgvault add-imap`, use Microsoft 365/Outlook.com OAuth via `add-o365`, import email from local files using `import-pst`, `import-mbox`, or `import-emlx`, and import chats/texts from WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup & Restore. All messages are stored in the same database and work with the same tools: search, TUI, MCP server, web server, and export. See [Setup Guide](/setup/#add-an-imap-account) for IMAP setup, [Importing Local Email](/usage/importing/) for file imports, and [Text Messages](/usage/text-messages/) for chat imports.

<p class="faq-question">Does deleting email in msgvault delete it from Gmail?</p>

Only if you explicitly run the full deletion workflow. Staging messages for deletion in the TUI does not touch Gmail or your IMAP provider. You must run `MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged` to execute staged deletions. Gmail messages move to trash by default; `--permanent` opts into permanent Gmail deletion. IMAP deletion removes messages from the provider. Your local archive is always preserved. See [Deleting Email](/usage/deletion/) for the complete process.

---

Have a question not covered here? Join the [msgvault Discord server](https://discord.gg/fDnmxB8Wkq) or [open an issue on GitHub](https://github.com/kenn-io/msgvault/issues).
