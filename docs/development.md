---
title: Development and Roadmap
description: Build, test, lint, and code conventions.
---

## Build

### macOS and Linux

```bash
# Debug build
make build

# Release build (optimized, stripped)
make build-release

# Install to ~/.local/bin or GOPATH
make install
```

### Windows

Use the PowerShell build helper from the repository root. It provides the same
debug and release builds as the Make targets and selects the host architecture
automatically:

```powershell
# Debug build (equivalent to make build)
.\scripts\build.ps1

# Optimized, stripped build (equivalent to make build-release)
.\scripts\build.ps1 -Release
```

Go and [MSYS2](https://www.msys2.org/) are required because msgvault uses CGO.
Install the compiler for your Windows architecture:

```powershell
# Windows AMD64 (run from PowerShell)
C:\msys64\usr\bin\pacman.exe -S --needed mingw-w64-x86_64-toolchain
```

For Windows ARM64, install CMake and Ninja from an MSYS2 CLANGARM64 shell:

```bash
pacman -S --needed mingw-w64-clang-aarch64-cmake \
  mingw-w64-clang-aarch64-ninja
```

The first ARM64 build downloads a checksum-verified LLVM-MinGW toolchain,
compiles the pinned DuckDB native library, and caches both under
`%LOCALAPPDATA%\msgvault\build-cache`; subsequent builds reuse them. Set
`MSGVAULT_BUILD_CACHE` to use another cache location, or pass `-RebuildDuckDB`
to rebuild the cached library.

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
