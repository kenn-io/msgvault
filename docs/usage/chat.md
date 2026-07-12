---
title: MCP Server
description: Expose your email archive to AI assistants via MCP.
---

The MCP server operates on your msgvault archive through the selected daemon, not your live Gmail account. Without `[remote].url`, `msgvault mcp` starts or reuses the local background daemon; with `[remote].url`, it uses that remote server. The AI cannot send emails, modify labels, or access your Google credentials. Standard read and search operations go through the daemon. If [vector search](/usage/vector-search/) is enabled, semantic and hybrid searches also call the embedding endpoint configured in `[vector.embeddings]`; use a local or self-hosted endpoint if message text must stay on your machine or network. The `stage_deletion` tool asks the selected daemon to save a deletion manifest, and `export_attachment` saves an attachment to a requested path on the MCP server's filesystem. Neither modifies the database, and actual deletion still requires you to run `msgvault delete-staged` from the CLI. You control when data enters the archive (via sync and import commands) and when anything is deleted (via the explicit [deletion workflow](/usage/deletion/)). Compared to giving an AI assistant direct OAuth access to your mailbox, this is a fundamentally smaller attack surface.

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
| `search_messages` | Deprecated compatibility wrapper. Omitted mode dispatches to `search_metadata`; `vector`/`hybrid` dispatch to `semantic_search_messages`. | `query` (string, required), `mode` (string: `vector`/`hybrid`), `explain` (bool), `min_score` (number), `limit` (int), `offset` (int), `account` (string) |
| `search_metadata` | Search message metadata with a subset of Gmail query syntax (not full Gmail compatibility). Matches subject, snippet, and sender/recipient metadata, not message bodies. | `query` (string, required), `limit` (int), `offset` (int), `account` (string) |
| `search_message_bodies` | Keyword full-text search inside message bodies. Returns `matches` excerpts (up to 5 per message), ordered newest-first. Backend excerpts may omit `char_offset` and `line`; use `search_in_message` when exact locations are needed. | `query` (string, required), `limit` (int), `offset` (int), `account` (string) |
| `semantic_search_messages` | Semantic search over preprocessed message subjects and bodies when [vector search](/usage/vector-search/) is configured. Returns scored chunk excerpts; `min_score` filters excerpts, not ranked messages. | `query` (string, required), `mode` (string: `vector`/`hybrid`, default `hybrid`), `explain` (bool), `min_score` (number), `limit` (int), `offset` (int), `account` (string) |
| `find_similar_messages` | Nearest-neighbor search from a seed message's embedding. Requires vector search to be configured and an active index generation. | `message_id` (int, required), `limit` (int), `account` (string), `message_type` (string), `after` (string), `before` (string), `has_attachment` (bool) |
| `search_by_domains` | Find messages where any participant (`from`, `to`, or `cc`) belongs to one of several domains, regardless of direction. | `domains` (comma-separated string, required), `limit` (int), `offset` (int), `after` (string), `before` (string) |
| `get_message` | Get message details with windowed body paging | `id` (int, required), `offset` (int), `center_at` (int), `max_chars` (int), `body_format` (string: `auto`/`text`/`html`), `full_body` (bool) |
| `list_messages` | List messages with filters | `from` (string), `to` (string), `label` (string), `after` (string), `before` (string), `has_attachment` (bool), `conversation_id` (int), `limit` (int), `offset` (int), `account` (string) |
| `get_attachment` | Get attachment content by ID | `attachment_id` (int) |
| `export_attachment` | Save attachment to filesystem | `attachment_id` (int), `destination` (string) |
| `get_stats` | Archive overview statistics. Includes vector index state when configured. | — |
| `aggregate` | Grouped statistics (top senders, domains, labels, or message volume by calendar year) | `group_by` (string: sender/recipient/domain/label/time), `limit` (int), `after` (string), `before` (string), `account` (string) |
| `stage_deletion` | Stage messages for deletion (creates manifest only) | `query` (string) OR structured filters: `from` (string), `domain` (string), `label` (string), `after` (string), `before` (string), `has_attachment` (bool); optional: `account` (string) |

`search_metadata`, `search_message_bodies`, `semantic_search_messages`, and `list_messages` return paginated JSON. `search_metadata` reports an exact `total`; `search_message_bodies`, `semantic_search_messages`, and `list_messages` return `total = -1` because they do not run a separate count query:

```json
{
  "data": [],
  "total": -1,
  "returned": 20,
  "offset": 0,
  "has_more": true
}
```

Use `offset` and `limit` to request subsequent pages. `search_metadata`,
`search_message_bodies`, `semantic_search_messages`, and `list_messages` default to `limit = 20` and
cap it at 50. `search_message_bodies`, `semantic_search_messages`, and `list_messages` use this
`total = -1` shape because they do not run a separate count query.
`search_metadata` accepts msgvault's local subset of Gmail-like syntax.
Gmail-only operators such as `list:` are rejected because msgvault does
not index `List-ID` locally; use Gmail-side validation for those checks.
To restrict mixed archives to values such as `email`, `calendar_event`,
`teams`, `sms`, or `mms`, include a `message_type:` operator in the query
(for example `message_type:teams incident review`). `find_similar_messages`
accepts a dedicated `message_type` parameter; `list_messages` does not
support message-type filtering.

`get_message` returns large bodies in windows: each response carries one
slice of the body plus `body_length`, `body_returned`, `offset`, and
`has_more`, so unusually large messages are paged across calls instead of
being returned in a single response.

### `search_metadata` and `search_message_bodies` / `semantic_search_messages` query syntax

Supported operators: `from:`, `to:`, `cc:`, `bcc:`, `subject:`, `label:` (or `l:`), `has:attachment`, `before:`/`after:` (YYYY-MM-DD), `older_than:`/`newer_than:` (e.g. `7d`, `2w`, `1m`, `1y`), `larger:`/`smaller:` (e.g. `5M`). Bare domains on `from:`/`to:` match any address at that domain. Multiple terms are ANDed.

Not supported: negation (`-has:attachment`), `OR`, or parentheses grouping.

Free text in `search_metadata` matches subject, snippet, and sender/recipient metadata only. Use `search_message_bodies` for keyword body search or `semantic_search_messages` for vector/hybrid search over preprocessed subject and body content; both require at least one free-text term. Keyword matches literal words; semantic returns ranked messages with scored chunk excerpts. Keyword backend excerpts omit `char_offset` and `line` when the search backend does not provide efficient locations; semantic excerpts also commonly omit them because preprocessing rewrites message text. Use distinctive snippet terms with keyword `search_in_message` when raw-body navigation is needed.

### `aggregate` response

`group_by=time` buckets messages by **calendar year** only. Each row's `Key` is a year string (e.g. `"2024"`). Month or day granularity is not available via MCP.

All `group_by` values return a JSON array of objects with these fields:

| Field | Description |
|---|---|
| `Key` | Grouping value (email, domain, label name, or year) |
| `Count` | Number of messages in the group |
| `TotalSize` | Sum of `size_estimate` in bytes |
| `AttachmentSize` | Sum of attachment sizes in bytes |
| `AttachmentCount` | Number of attachments |
| `TotalUnique` | Total number of distinct groups (same on every row) |

`semantic_search_messages` is always registered so callers receive actionable discovery guidance. Without vector search it exposes a reduced schema and calls return `vector_not_enabled`; with vector search it advertises the full vector parameters. `search_message_bodies` and the deprecated `search_messages` compatibility wrapper are always available. Vector and hybrid queries require at least one free-text term (operator-only queries return `missing_free_text`). They support `offset`/`limit` pagination inside the configured hybrid ranking window; when `[vector.search].max_page_size_hybrid` is positive, an `offset` at or beyond that cap returns `pagination_limit`. `min_score` filters returned chunk excerpts only and does not remove ranked messages. For deeper pagination, adjust `[vector.search].max_page_size_hybrid`.

In `semantic_search_messages` (vector/hybrid), the paginated response also includes
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

When called, `stage_deletion` creates a pending deletion manifest through the selected daemon. With a remote server configured, the manifest is saved on that remote host; otherwise it is saved by the local daemon. It does **not** delete anything. To execute the deletion, you must run `msgvault delete-staged` from the CLI. See [Deleting Email](/usage/deletion/) for the full workflow.

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

# StreamableHTTP transport on loopback
msgvault mcp --http 8080
```

| Flag | Default | Description |
|---|---|---|
| `--force-sql` | `false` | Deprecated in 0.17.0; use `[analytics].engine = "sql"` in `config.toml` instead. See [Configuration: analytics](/configuration/#analytics). |
| `--no-sqlite-scanner` | `false` | Deprecated in 0.17.0; cache engine selection is daemon-managed. Use `[analytics].engine = "sql"` for live SQL. |
| `--http` | — | Serve over MCP StreamableHTTP instead of stdio. Bare ports bind to `127.0.0.1`. |
| `--http-allow-insecure` | `false` | Allow non-loopback HTTP binding. Use only behind your own network/auth layer. |

Deprecated in 0.17.0: MCP analytics behavior moved from per-command flags to daemon configuration. Use `[analytics].engine` and `[analytics].auto_build_cache` in `config.toml` so local and remote daemon behavior stays consistent.

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
