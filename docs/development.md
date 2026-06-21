---
title: Development and Roadmap
description: Build, test, lint, and code conventions.
---

## Build

```bash
# Debug build
make build

# Release build (optimized, stripped)
make build-release

# Install to ~/.local/bin or GOPATH
make install
```

## Test

```bash
# Run all tests
make test

# Verbose output
make test-v
```

## Lint & Format

```bash
# Format code
make fmt

# Run linter (requires golangci-lint)
make lint

# Check for issues
go vet ./...
```

## Code Conventions

- **TUI**: Bubble Tea for model/update, lipgloss for styling
- **Database**: All DB operations through the `Store` struct (`internal/store`)
- **Error handling**: Return `error`, wrap with context via `fmt.Errorf`
- **Tests**: Table-driven tests
- **Cancellation**: Context-based cancellation for long operations
- **Encoding**: Charset detection via `gogs/chardet`, conversion via `golang.org/x/text/encoding`

## SQL Guidelines

- **Never use `SELECT DISTINCT` with JOINs**: use `EXISTS` subqueries instead (semi-joins)
- `EXISTS` is faster (stops at first match) and avoids duplicates at the source

Instead of:

```sql
SELECT DISTINCT m.id FROM messages m
JOIN message_recipients mr ON mr.message_id = m.id
WHERE mr.recipient_type = 'from' AND ...
```

Use:

```sql
SELECT m.id FROM messages m
WHERE EXISTS (
    SELECT 1 FROM message_recipients mr
    WHERE mr.message_id = m.id
      AND mr.recipient_type = 'from' AND ...
)
```

## Dependencies

| Library | Purpose |
|---|---|
| `cobra` | CLI framework |
| `charmbracelet/bubbletea` | TUI framework |
| `charmbracelet/lipgloss` | TUI styling |
| `mattn/go-sqlite3` | SQLite with CGO (FTS5) |
| `duckdb/duckdb-go/v2` | DuckDB driver for Parquet |
| `gogs/chardet` | Character set detection |
| `golang.org/x/text` | Text encoding conversion |

## Community

Join the [msgvault Discord server](https://discord.gg/fDnmxB8Wkq) to discuss development, ask questions, or share feedback.
