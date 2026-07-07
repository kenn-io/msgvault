---
title: msgvault
description: Offline email and message archive with full-text search, interactive TUI, backup snapshots, and multi-account support. Sync Gmail, IMAP, Google Calendar, and Microsoft Teams; import PST, MBOX, Apple Mail, WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup & Restore.
---

# msgvault

Archive a lifetime of email and chat. Fast keyword search, opt-in semantic
search, and local AI workflows.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="/setup/">Quick Start</a>
  <a class="md-button" href="https://github.com/kenn-io/msgvault">GitHub</a>
  <a class="md-button" href="https://discord.gg/fDnmxB8Wkq">Discord</a>
</p>

<figure class="hero-shot" data-lightbox>
  <img src="/assets/generated/tui-senders.svg" alt="msgvault TUI showing the Senders view" loading="eager">
</figure>

Supports Gmail sync, Google Calendar sync, Microsoft Teams sync, IMAP,
Microsoft 365 mail, verifiable backup snapshots, PST import, MBOX import,
Apple Mail import, and chat/text import (WhatsApp, iMessage, Google Voice,
Facebook Messenger, and SMS Backup & Restore).

Read the [Introduction](/introduction/) to learn more about why this project
was created.

## Install

```bash
curl -fsSL https://msgvault.io/install.sh | bash
```

**Windows (PowerShell):**

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://msgvault.io/install.ps1 | iex"
```

Then [set up OAuth credentials](/guides/oauth-setup/) and [start
syncing](/setup/). You can also [build from source](/setup/#build-from-source).

!!! note "New in 0.17.1"
    HTTP clients can now stage, list, and cancel deletion manifests through the
    web API; CLI search reports when the full-text index is checking or
    rebuilding instead of blocking on that work; IMAP sync is more robust on
    servers with unusual UID or raw-fetch behavior. See the
    [Changelog](/changelog/) for the full release notes.

## Why msgvault?

Your email and message data is yours. msgvault downloads a complete local copy
of your email (from Gmail, IMAP, or local archives) and imports chats and texts
from WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup &
Restore. Keyword search, analytics, the TUI, and the MCP server query local
SQLite and Parquet files. **Nothing contacts your live mailbox outside sync and
deletion commands that you run explicitly.** Optional vector search calls only
the embedding endpoint you configure; use a local or self-hosted endpoint if
message text must never leave your machine or network.

Years of PDFs, photos, documents, and spreadsheets buried in your inbox become
ordinary files on your filesystem, deduplicated and instantly searchable. Your
data is no longer locked behind a web interface or an API. It's just files on
disk that you own and control.

## Features

<div class="feature-grid">
  <section>
    <h3>Full Email Backup</h3>
    <p>Downloads complete messages from Gmail or any IMAP server, including raw MIME, labels, metadata, and every attachment. Every PDF, photo, spreadsheet, and document you've ever received or sent is extracted and stored locally, deduplicated by content hash.</p>
  </section>
  <section>
    <h3>Calendar Sync</h3>
    <p>Archive Google Calendar alongside email. Events — including organizers, attendees, recurring series, and cancellations — become searchable by keyword and by meaning, and their participants dedupe with your email contacts. Read-only and incremental.</p>
  </section>
  <section>
    <h3>Teams Sync</h3>
    <p>Archive Microsoft Teams chats, channels, replies, link attachments, and inline media through delegated Microsoft Graph. Teams records use <code>message_type = teams</code> so they can be searched and queried separately from email.</p>
  </section>
  <section>
    <h3>Backup Snapshots</h3>
    <p>Create incremental, append-only backup repositories for the SQLite archive and attachments. Verify snapshots byte-for-byte, restore into a fresh archive home, and sync the repository off-site with ordinary file tools.</p>
  </section>
  <section>
    <h3>Lightning-Fast TUI</h3>
    <p>Explore hundreds of thousands of messages with instant aggregation and drill-down. Powered by DuckDB over Parquet, hundreds of times faster than SQL JOINs, in a small footprint.</p>
  </section>
  <section>
    <h3>Full-Text Search</h3>
    <p>SQLite FTS5-powered search with Gmail-like query syntax. Search by sender, date, label, size, attachments, and more.</p>
  </section>
  <section>
    <h3>Semantic &amp; Hybrid Search</h3>
    <p>Opt-in semantic search with vectors stored locally, plus hybrid ranking that fuses BM25 and vector similarity via Reciprocal Rank Fusion. Point msgvault at a local or self-hosted OpenAI-compatible embedding endpoint and query by meaning, not just keywords. Exposed through local CLI search, the HTTP API, and MCP server.</p>
  </section>
  <section>
    <h3>Multi-Account</h3>
    <p>Archive multiple sources in a single database, group accounts into collections, manage per-account identities, and deduplicate safely.</p>
  </section>
  <section>
    <h3>Incremental Sync</h3>
    <p>Uses Gmail History API for efficient updates after initial full sync. Resumable checkpoints for interrupted syncs.</p>
  </section>
  <section>
    <h3>MCP Server</h3>
    <p>Expose your archive to AI assistants like Claude Desktop via the Model Context Protocol. Search, read, and analyze your messages from any MCP-compatible LLM.</p>
  </section>
  <section>
    <h3>Web Server</h3>
    <p>REST API for programmatic access to your archive. Optional cron-based background sync scheduling. Build dashboards, automations, and integrations.</p>
  </section>
  <section>
    <h3>Local Import</h3>
    <p>Import PST archives, MBOX archives, Apple Mail <code>.emlx</code> exports, and chats/texts from WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup &amp; Restore. Messages are indexed and searchable alongside your email data.</p>
  </section>
  <section>
    <h3>Safe Deletion</h3>
    <p>Stage messages for deletion in the TUI or via AI assistant, review manifests, then permanently delete from Gmail or IMAP provider.</p>
  </section>
</div>

## How It Works

<img class="diagram-center" src="/assets/static/how-it-works.svg" alt="msgvault architecture: Gmail API syncs to SQLite, then offline Parquet analytics, FTS5 search, TUI, and MCP Server">
