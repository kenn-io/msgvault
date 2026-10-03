---
title: "Query Package Design"
description: "Query package architecture, backend responsibilities, and analytical data flow."
last_edited: "2026-03-18"
---

# Query Package Design

## Overview

The `internal/query` package provides a backend-agnostic query layer for msgvault.
It supports two use cases:

1. **TUI aggregate views** - Fast grouping by sender, domain, label, time
2. **CLI/API message retrieval** - List and detail queries

## Backend Strategy

The package defines an `Engine` interface that can be implemented by:

### SQLiteEngine

- Uses direct SQLite queries with JOINs
- Flexible: supports all filters and sorting options
- Performance: adequate for small-medium databases (<100k messages)
- Always available as fallback

### DuckDBEngine (Preferred)

- Uses DuckDB to query Parquet files for denormalized analytics
- Much faster for aggregates (~3000x vs SQLite JOINs)
- Automatically falls back to SQLite for message detail queries
- Best for large databases (100k+ messages)
- Built using `msgvault build-cache` command

### RemoteEngine

- Implements Engine interface over HTTP API
- Used by TUI when `[remote].url` is configured
- Connects to a remote msgvault daemon (`msgvault serve`)
- Disables deletion and attachment export for safety

## Parquet Schema

```
messages.parquet (partitioned by year):
- id: int64
- source_message_id: string
- subject: string
- sent_at: timestamp
- size_estimate: int64
- from_email: string
- from_domain: string
- to_emails: list<string>  # denormalized
- labels: list<string>     # denormalized
- attachment_count: int32
- attachment_size: int64
```

This denormalized schema avoids JOINs, enabling fast scans.

## DuckDB Hybrid Approach

The DuckDBEngine handles this automatically:

```go
// Create DuckDB engine with SQLite fallback for message details
engine, err := query.NewDuckDBEngine(analyticsDir, dbPath, sqliteDB, opts)
```

The engine routes queries to the appropriate backend:
- Aggregate queries → DuckDB over Parquet (fast scans)
- Message detail queries → SQLite (has body, raw MIME)
- Full-text search → SQLite FTS5 virtual table

## Build Process

```bash
# Build/rebuild Parquet files from SQLite
msgvault build-cache [--full-rebuild]

# Files stored in ~/.msgvault/analytics/
# - messages/year=2024/*.parquet
# - participants/
# - message_recipients/
# - labels/
# - message_labels/
# - attachments/
# - _last_sync.json (incremental state)
```

## Go Libraries

The implementation uses:
- `github.com/marcboeker/go-duckdb` - DuckDB via CGO (SQL interface over Parquet)
- SQLite FTS5 for full-text search (body content not in Parquet)
