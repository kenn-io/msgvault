---
title: MCP Server
description: Expose your email archive to AI assistants via MCP.
---

The MCP server operates on your **local archive**, not your live Gmail account. The AI cannot send emails, modify labels, or access your Google credentials. The archive database is opened in read-only mode, so multiple AI sessions can query the archive concurrently without blocking each other. Standard read and FTS search operations work against your local database. If [vector search](/usage/vector-search/) is enabled, semantic and hybrid searches also call the embedding endpoint configured in `[vector.embeddings]`; use a local or self-hosted endpoint if message text must stay on your machine or network. Two tools write to the local filesystem: `stage_deletion` creates a deletion manifest file, and `export_attachment` saves an attachment to a requested path. Neither modifies the database, and actual deletion still requires you to run `msgvault delete-staged` from the CLI. You control when data enters the archive (via sync and import commands) and when anything is deleted (via the explicit [deletion workflow](/usage/deletion/)). Compared to giving an AI assistant direct OAuth access to your mailbox, this is a fundamentally smaller attack surface.

## Setup

The `mcp` command starts a [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server that exposes your email archive as a set of tools. This lets AI assistants like Claude Desktop search, read, and analyze your messages directly.

### Claude Desktop Configuration

Add the following to your Claude Desktop config file:

- **macOS**: `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Windows**: `%APPDATA%\Claude\claude_desktop_config.json`

```json
{
  "mcpServers": {
    "msgvault": {
      "command": "msgvault",
      "args": ["mcp"]
    }
  }
}
```

If `msgvault` is not on your PATH, use the full path to the binary. Restart Claude Desktop after saving the config.

### StreamableHTTP Transport

For MCP clients that connect over HTTP instead of stdio, run:

```bash
msgvault mcp --http 8080
```

Bare ports and `:port` forms bind to loopback only, so the command above listens on `127.0.0.1:8080`. Explicit loopback addresses such as `127.0.0.1:8080` and `[::1]:8080` are also allowed.

The MCP HTTP server has no built-in authentication. Non-loopback hosts are rejected unless you pass `--http-allow-insecure`; only use that behind a trusted network boundary or an authenticated reverse proxy.

## Available Tools

The MCP server exposes the following tools to connected AI clients:

| Tool | Description | Parameters |
|---|---|---|
| `search_messages` | Search with Gmail-like query syntax. When [vector search](/usage/vector-search/) is configured, supports semantic and hybrid modes. | `query` (string, required), `mode` (string: `fts`/`vector`/`hybrid`, default `fts`), `explain` (bool), `limit` (int), `offset` (int), `account` (string) |
| `find_similar_messages` | Nearest-neighbor search from a seed message's embedding. Requires vector search to be configured and an active index generation. | `message_id` (int, required), `limit` (int), `account` (string), `after` (string), `before` (string), `has_attachment` (bool) |
| `search_by_domains` | Find messages where any participant (`from`, `to`, or `cc`) belongs to one of several domains, regardless of direction. | `domains` (comma-separated string, required), `limit` (int), `offset` (int), `after` (string), `before` (string) |
| `get_message` | Get full message details by ID | `id` (int) |
| `list_messages` | List messages with filters | `from` (string), `to` (string), `label` (string), `after` (string), `before` (string), `has_attachment` (bool), `limit` (int), `offset` (int), `account` (string) |
| `get_attachment` | Get attachment content by ID | `attachment_id` (int) |
| `export_attachment` | Save attachment to filesystem | `attachment_id` (int), `destination` (string) |
| `get_stats` | Archive overview statistics. Includes vector index state when configured. | — |
| `aggregate` | Grouped statistics (top senders, domains, labels, time series) | `group_by` (string: sender/recipient/domain/label/time), `limit` (int), `after` (string), `before` (string), `account` (string) |
| `stage_deletion` | Stage messages for deletion (creates manifest only) | `query` (string) OR structured filters: `from` (string), `domain` (string), `label` (string), `after` (string), `before` (string), `has_attachment` (bool); optional: `account` (string) |

`search_messages` and `list_messages` return paginated JSON:

```json
{
  "data": [],
  "total": -1,
  "returned": 20,
  "offset": 0,
  "has_more": true
}
```

Use `offset` and `limit` to request subsequent pages. `search_messages`
and `list_messages` default to `limit = 20` and cap it at 50. When a
backend cannot report a full result count, `total` is `-1`; use
`has_more` as the pagination signal. `list_messages` uses this
`total = -1` shape because it does not run a separate count query.
`search_messages` accepts msgvault's local subset of Gmail-like syntax.
Gmail-only operators such as `list:` are rejected because msgvault does
not index `List-ID` locally; use Gmail-side validation for those checks.

`find_similar_messages` is only registered when the server starts with vector search configured. `search_messages` is always available, but `mode=vector` and `mode=hybrid` return `vector_not_enabled` when the server is not configured for vector search. Vector and hybrid queries require at least one free-text term (operator-only queries return `missing_free_text`). They support `offset`/`limit` pagination inside the configured hybrid ranking window; when `[vector.search].max_page_size_hybrid` is positive, an `offset` at or beyond that cap returns `pagination_limit`. Use `mode=fts` for deeper pagination or adjust that config cap.

In `mode=vector` and `mode=hybrid`, the paginated response also includes
top-level `mode`, `pool_saturated`, and `generation` fields. When
`explain = true`, each item in `data` may include a `score` object with
the fused ranking components.

## Example Usage with Claude

Once configured, you can ask Claude questions like:

- *"Search my email for messages from alice@example.com about the project proposal"*
- *"How many emails did I receive last month?"*
- *"Show me the top 10 senders in my archive"*
- *"Find all messages with attachments larger than 5MB"*
- *"Stage all messages from linkedin.com for deletion"*
- *"Stage promotional emails from before 2023 for deletion"*

Claude will automatically call the appropriate msgvault tools to retrieve and analyze your messages.

## Staged Deletion via MCP

The `stage_deletion` tool lets an AI assistant help you clean up your inbox. It accepts either a Gmail-style query string or structured filters (sender, domain, label, date range), but not both at once. Results are capped at 100,000 messages per call.

When called, `stage_deletion` creates a local deletion manifest, a JSON file listing the matched messages. It does **not** delete anything. To execute the deletion, you must run `msgvault delete-staged` from the CLI. See [Deleting Email](/usage/deletion/) for the full workflow.

The tool returns the batch ID, message count, and next steps:

```json
{
  "batch_id": "20260224-095132-from-linkedin",
  "message_count": 150,
  "status": "pending",
  "next_step": "Run 'msgvault delete-staged' to execute deletion"
}
```

## CLI Flags

```bash
# Start the MCP server (stdio transport)
msgvault mcp

# Force SQL retrieval for all searches
msgvault mcp --force-sql

# StreamableHTTP transport on loopback
msgvault mcp --http 8080
```

| Flag | Default | Description |
|---|---|---|
| `--force-sql` | `false` | Always use SQL retrieval instead of FTS5 |
| `--http` | — | Serve over MCP StreamableHTTP instead of stdio. Bare ports bind to `127.0.0.1`. |
| `--http-allow-insecure` | `false` | Allow non-loopback HTTP binding. Use only behind your own network/auth layer. |

## Claude Code Skill

msgvault ships a Claude Code skill for running SQL queries against your archive. The skill uses `msgvault query` with the views documented in [SQL Queries](/usage/querying/), so you can ask Claude Code natural-language questions and it will translate them into SQL.

To enable the skill, add msgvault's skill directory to your Claude Code configuration:

```json
{
  "permissions": {
    "allow": [
      "Skill(msgvault:*)"
    ]
  }
}
```

Once configured, Claude Code can query your archive directly during a conversation, for example: "How many emails did I get from linkedin.com last year?" or "Show my top 20 senders by message count."
