---
last_edited: "2026-09-08"
title: Documentation
description: Set up your archive, find messages and files, maintain people, and operate msgvault.
---

# Use your communications archive

msgvault keeps email, chat, meetings, calendars, and contacts on your own
hardware. These guides help you bring data in, find what matters, and maintain
the archive through the browser, terminal, CLI, or an agent.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="/docs/setup/">Get started</a>
  <a class="md-button" href="/docs/changelog/#unreleased">Changelog</a>
</p>

!!! note "Returning after 0.19?"
    These docs follow current `main`, including unreleased work after 0.19.3.
    The [changelog](changelog.md#unreleased) lists new capabilities and
    [upgrade notes](changelog.md#upgrade-and-compatibility), with released and
    unreleased changes kept separate.

## Start an archive

1. [Install msgvault](setup.md).
2. [Choose a source](guides/sources.md) and follow its sync or import guide.
3. Open the [Web UI](web-ui.md) or [TUI](usage/tui.md) to browse your messages.
4. [Create a backup](usage/backup.md) before making substantial changes.

Google OAuth setup is needed for Google-backed sources. Local imports and other
providers have their own requirements. To understand the product first, read
the [archive lifecycle](/guide/) or the [project's origin](introduction.md).

## Find messages and files

| Your question | Guide |
|---|---|
| Who sent this, and when? | [Keyword search and filters](usage/searching.md) |
| How do I search by meaning? | [Semantic and hybrid search](usage/vector-search.md) |
| How do I search inside attachments? | [Document indexing](usage/document-indexing.md) |
| Which model settings should I use? | [Recommended configuration](usage/recommended-configuration.md) |
| What does my archive contain? | [Analytics](usage/analytics.md) and [SQL queries](usage/querying.md) |
| How can an AI assistant use it? | [MCP](usage/chat.md) and [agent skills](guides/agent-skills.md) |

## Keep track of people

[People and profiles](usage/people.md) explains observed identities, durable
profiles, contact activity, and relationship curation. Start there to connect
addresses and handles, maintain contacts, or set up optional profile automation.
Use [accounts and collections](usage/multi-account.md) to organize sources and
limit an archive view.

## Maintain and operate the archive

| Your task | Guide |
|---|---|
| Run on a NAS or server | [Remote deployment](guides/remote-deployment.md) |
| Understand the background daemon | [Daemon migration](guides/daemon-migration.md) |
| Verify stored mail | [Archive verification](guides/verification.md) |
| Hide duplicate copies | [Deduplication](usage/deduplication.md) |
| Remove mail from a provider | [Deletion staging and execution](usage/deletion.md) |
| Keep a recoverable copy | [Backup and restore](usage/backup.md) |
| Take data elsewhere | [Exporting](usage/exporting.md) |
| Diagnose a problem | [Troubleshooting](troubleshooting.md) and [FAQ](faq.md) |

## Reference and development

- [CLI reference](cli-reference.md): commands and flags.
- [Configuration](configuration.md): settings, defaults, and file locations.
- [HTTP API](api-server.md): integration contracts and authentication.
- [Architecture](architecture/overview.md): responsibilities, data flow, and limits.
- [Development](development.md): building, testing, and contributing.
