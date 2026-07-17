---
title: msgvault
description: Offline email, chat, and meeting archive with full-text search, an interactive TUI, backup snapshots, and multi-account support. Sync Gmail, IMAP, Google Calendar, Microsoft Teams, Beeper, Granola, and Circleback.
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

Supports Gmail, Google Calendar, Microsoft Teams, Granola, Circleback, Beeper
Desktop, IMAP, and Microsoft 365 mail sync; verifiable backup snapshots; PST,
MBOX, and Apple Mail import; and chat/text import from WhatsApp, iMessage,
Google Voice, Facebook Messenger, and SMS Backup & Restore.

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

!!! note "New in 0.18.0"
    Archive Beeper Desktop chats and Granola or Circleback meeting notes;
    browse meetings in the TUI; install bundled agent skills; retrieve
    attachments by hash through the HTTP API; and use native Windows ARM64
    releases. See the [Changelog](/changelog/) for the full release notes.

## Why msgvault?

Your email and message data is yours. msgvault downloads a complete local copy
of your email (from Gmail, IMAP, or local archives) and imports chats and texts
from WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup &
Restore, and can sync Beeper chats plus Granola and Circleback meeting notes.
Keyword search, analytics, the TUI, and the MCP server query your archive.
**Source services are contacted only by authorization/registration, sync,
media-backfill, and deletion workflows that you run or schedule explicitly.**
Optional vector search calls only the embedding endpoint you configure; use a
local or self-hosted endpoint if message text must never leave your machine or
network.

Years of PDFs, photos, documents, and spreadsheets buried in your inbox become
deduplicated content-addressed objects that can be searched and exported by
hash. Your data is no longer locked behind a provider's web interface or API;
it lives in an archive on disk that you own and control.

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
    <h3>Meetings &amp; Beeper</h3>
    <p>Sync Granola and Circleback notes and transcripts into a dedicated TUI browser, and archive chats and media from networks bridged through Beeper Desktop. Search each source separately by message type.</p>
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
    <p>Expose your archive to AI assistants through the Model Context Protocol. Search metadata or bodies lexically, run semantic search, and find matches within one message.</p>
  </section>
  <section>
    <h3>Agent Skills</h3>
    <p>Install bundled read-only skills that teach Claude Code and Codex how to search the archive, retrieve attachments, and run analytics from the terminal.</p>
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
