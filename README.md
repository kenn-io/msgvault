---
last_edited: "2026-09-08"
---

<p align="center">
  <img src=".github/assets/msgvault-mark.svg" width="160" height="160" alt="msgvault logo">
</p>

<h1 align="center">msgvault</h1>

<p align="center">
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go" alt="Go 1.27+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
  <a href="https://msgvault.io"><img src="https://img.shields.io/badge/Docs-msgvault.io-blue" alt="Docs"></a>
  <a href="https://discord.gg/fDnmxB8Wkq"><img src="https://img.shields.io/badge/Discord-Join-5865F2?logo=discord&amp;logoColor=white" alt="Discord"></a>
</p>

<p align="center">
  <a href="https://msgvault.io/docs/">Documentation</a> ·
  <a href="https://msgvault.io/docs/guides/oauth-setup/">Setup Guide</a> ·
  <a href="https://msgvault.io/docs/usage/tui/">Interactive TUI</a>
</p>

**The system of record for your communications and relationships.**

msgvault is a local-first, open-source archive for email, chat, meetings,
calendars, and contacts. Keep your history on your own hardware, find messages
and files, and connect the addresses and handles that belong to the same person.
Use the browser, terminal, CLI, HTTP API, or an AI assistant through MCP.

> **Alpha software.** APIs, storage format, and CLI flags may change. Back up
> your data. This README describes current `main`; see
> [the changelog](docs/changelog.md#unreleased) for unreleased features
> and upgrade steps.

## What you can do

- **Bring your history together.** Sync mail, chat, calendars, meeting notes,
  and contacts, or import local exports. See the [source guide](docs/guides/sources.md).
- **Find the message or file you need.** Search by sender, date, mailing list,
  or words. Optionally enable search by meaning, document text extraction,
  and image search with a provider you choose.
- **Keep track of people.** Connect identities, curate profiles and relationships,
  browse contact activity, and sync contacts with CardDAV. Optional profile
  automation and conversation briefs require separate consent.
- **Explore the archive.** Group messages by people, domains, time, source, and
  type in the [Web UI](docs/web-ui.md) or [TUI](docs/usage/tui.md). Save useful
  views and monitor background work.
- **Use your own tools.** Query with SQL, export messages and attachments, or
  connect an agent to the [MCP server](docs/usage/chat.md).
- **Preserve and maintain it.** Deduplicate copies, back up the archive, and
  review staged mail deletions before explicitly removing messages upstream.
  Remote deletion preserves archived messages and attachments.

Keyword search and analytics read your archive without contacting its source
services. Sync needs access to those services. Optional model and enrichment
features send selected data to the endpoints you configure; local embedding
servers are also supported. See [recommended configuration](docs/usage/recommended-configuration.md)
for the choices and consent steps.

## Installation


**macOS / Linux:**
```bash
curl -fsSL https://msgvault.io/install.sh | bash
```

**macOS via Homebrew**
```bash
brew install msgvault
```

**Windows (PowerShell):**
```powershell
powershell -ExecutionPolicy ByPass -c "irm https://msgvault.io/install.ps1 | iex"
```

The installer detects your OS and architecture, downloads the latest release from [GitHub Releases](https://github.com/kenn-io/msgvault/releases), verifies the SHA-256 checksum, and installs the binary. You can review the script ([bash](https://msgvault.io/install.sh), [PowerShell](https://msgvault.io/install.ps1)) before running, or download a release binary directly from GitHub.

To build from source on macOS or Linux instead (requires **Go 1.27+**, **Bun
1.3.14+**, **Node.js 20.19+ on 20.x, 22.13+ on 22.x, or 24+**, and a C/C++
compiler for CGO and to statically link DuckDB; on Debian/Ubuntu also install
`libsqlite3-dev` for the `sqlite3.h` header used by the default `sqlite_vec`
build):

```bash
git clone https://github.com/kenn-io/msgvault.git
cd msgvault
make install
```

**Conda-Forge:**

You can install msgvault [from conda-forge](https://prefix.dev/channels/conda-forge/packages/msgvault) using Pixi or Conda:

```bash
pixi global install msgvault
conda install -c conda-forge msgvault
```

## Quick Start

For Gmail, first create an OAuth credential with the
[OAuth setup guide](docs/guides/oauth-setup.md). Then archive a small first batch:

```bash
msgvault init-db
msgvault add-account you@gmail.com
msgvault sync-full you@gmail.com --limit 100
msgvault serve
```

Open the `API server` URL printed by `msgvault serve`. The release binary
includes the browser application; it needs no separate Node or Bun installation
at runtime. Use `msgvault tui` for the terminal interface.

For another provider or a local export, start with
[choosing a source](docs/guides/sources.md). Google credentials are needed only
for Google-backed sources. The [setup guide](docs/setup.md) covers installation,
first sync, and running on your own server.

## Find your next step

| I want to… | Read |
|---|---|
| Understand the product | [Product overview](https://msgvault.io/) and [archive lifecycle](https://msgvault.io/guide/) |
| Catch up after 0.19 | [Changelog and upgrade notes](docs/changelog.md#unreleased) |
| Search messages and attachments | [Searching](docs/usage/searching.md) and [document indexing](docs/usage/document-indexing.md) |
| Maintain contacts and relationships | [People and profiles](docs/usage/people.md) |
| Configure optional AI features | [Recommended configuration](docs/usage/recommended-configuration.md) |
| Run msgvault on a server | [Remote deployment](docs/guides/remote-deployment.md) |
| Back up or free mailbox space | [Backup](docs/usage/backup.md) and [deleting email](docs/usage/deletion.md) |
| Look up a command or setting | [CLI reference](docs/cli-reference.md) and [configuration](docs/configuration.md) |
| Build or contribute | [Development](docs/development.md) and [agent guide](AGENTS.md) |

## Community

Join the [msgvault Discord](https://discord.gg/fDnmxB8Wkq),
[report an issue](https://github.com/kenn-io/msgvault/issues), or read the
[full documentation](https://msgvault.io/docs/).

## License

[MIT](LICENSE)
