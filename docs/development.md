---
title: Development
description: Build, test, lint, and code conventions.
---

## Build

The macOS and Linux builds require Go 1.27+, Bun 1.3.14+, Node.js (20.19+ on
20.x, 22.13+ on 22.x, or 24+), and a C/C++ compiler. The Make targets install
the pinned browser dependencies when `web/package.json` or `web/bun.lock`
changes, then embed the production UI in the Go binary; Node runs the embed
validator in that path. On Debian/Ubuntu also install `libsqlite3-dev`, which
provides the `sqlite3.h` header needed to compile the default `sqlite_vec`
extension.

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

Use the PowerShell helper from the repository root to compile the Go binary.
It selects the host architecture automatically and embeds assets already in
`internal/web/dist`; it does not build the browser application.

For a binary with the Web UI, first run `make web-embed` in an MSYS2 shell
with GNU Make, Bun, and Node.js available. This builds and validates the browser
assets. Then run the PowerShell helper:

```powershell
# Debug Go build
.\scripts\build.ps1

# Optimized, stripped Go build
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

### Container builds

The repository's `Dockerfile` builds the embedded Web UI and the msgvault
binary. To build a local image and run its runtime checks:

```bash
docker buildx build --load --tag msgvault:dev .
scripts/smoke-container.sh msgvault:dev
```

For portable image archives, Docker Bake provides an `oci` target:

```bash
docker buildx bake oci
```

It exports `dist/amd64.oci.tar` and `dist/arm64.oci.tar` for Linux AMD64 and
ARM64. The builder needs support for both target architectures, either through
native workers or emulation. Override `OCI_OUTPUT_DIR` to change the output
directory. `VERSION`, `COMMIT`, and `BUILD_DATE` set the binary metadata;
`OCI_VERSION` and `REVISION` set the image's version and source-revision labels.
Defaults identify a development build.

Image builds run checks against a temporary empty archive: database
initialization, a DuckDB query, and the embedded Web UI and its JavaScript
asset. `scripts/smoke-container.sh` repeats these checks in the loaded image
with networking disabled. These commands build and check local artifacts; they
do not publish them.

Repository-owned release-publishing workflows and the old release/tagging
scripts have been removed. Pushing a tag no longer invokes those publishers. The
Docker inputs, local build commands, installers, and ordinary CI remain
available.

## Test

```bash
# Run all tests
make test

# Verbose output
make test-v
```

### Build tags and assertions

All Go test runs need `-tags "fts5 sqlite_vec"`; the Make targets supply these
automatically. Use `assert` and `require` from testify, with expected values
first. See [AGENTS.md](https://github.com/kenn-io/msgvault/blob/main/AGENTS.md)
for repository testing rules.

### Timing waits

Use `testing/synctest` bubbles for work owned by the test process, including
goroutines, channels, timers, tickers, and fakes. Advance virtual time with
`synctest.Sleep` and wait for durable state with `synctest.Wait`.

Keep real budgets for PostgreSQL and SQLite locks, database clocks, network
requests, subprocesses, DuckDB, and operating-system events. Name retained
sub-second testify budgets so their event and owner are clear.

The helper check rejects bare totals below one second in `Eventually`,
`Eventuallyf`, `EventuallyWithT`, `EventuallyWithTf`, `Never`, and `Neverf`.
Named budgets and variables stay outside this rule. Virtual sleeps are valid
inside a bubble. CI runs this check on Ubuntu, so Windows-only test files still
need Windows validation.

`make lint` and `make lint-ci` build a pinned golangci-lint with Kit's
`kennlint` plugin and run its `sleeptest` check. The check rejects `time.Sleep`
in tests outside a `synctest.Test` bubble. A kept real wait carries
`//nolint:kennlint // <what it waits for>` on the sleep line.

### PostgreSQL tests

`MSGVAULT_TEST_DB=postgres://...` runs PostgreSQL-backed
tests. pgvector tests require a PostgreSQL instance with the `vector`
extension and the `pgvector` build tag.

The PostgreSQL deadlock tests also require permission to set
`deadlock_timeout`. They defer the blocker transaction's deadlock detector
so the write under test is the deadlock victim. For a non-superuser test
role, have the database administrator run:

```sql
GRANT SET ON PARAMETER deadlock_timeout TO test_role;
```

Replace `test_role` with the role in `MSGVAULT_TEST_DB`. This parameter grant
is sufficient; the test role does not need superuser access.

There are two PostgreSQL configurations to cover: the pgvector build
(`make test-pg`) and the shipped build, which has no pgvector tag
(`make test-pg-shipped`). Run `make test-pg-both` rather than both of those —
the tag changes the test binary of only the packages listed in
`PG_SHIPPED_ONLY_PKGS` in the Makefile, so the second full run would reprove
the first. Each test binary builds the schema once into a template — a
SQLite file copied per test, or a PostgreSQL template database cloned per
test with `CREATE DATABASE ... TEMPLATE` (`internal/testutil/sqlite_template.go`,
`internal/testutil/pg_template.go`) — instead of replaying `InitSchema()` for
every fixture. Nothing runs in the background, so no fixture ever issues a
schema statement while a test body is running. Each database is still private
to its test, still produced by the same `InitSchema()` path, and still dropped
on cleanup. A PostgreSQL template is owned through a session advisory lock the
server releases when the binary exits, so the next binary reclaims whatever an
earlier one left behind; a role without `CREATEDB` falls back to a private
schema in the configured database.


### Local test scheduling

`make test` automatically overlaps the CLI, store, API, and query package shards
with the remaining SQLite packages when at least 32 CPUs and 64 GiB of available
memory are detected. The planner reserves four test-process slots for the
unsharded remainder, then divides the remaining slots across the sharded
packages, up to 16 shards each. Each slot budgets two Go execution threads
(`GOMAXPROCS=2`) and a 2 GiB memory allowance. For four sharded packages, a
32-CPU budget permits three shards per package; 128 CPUs permits fifteen,
provided memory also permits them. This changes scheduling, not test coverage.

The planner emits the per-process and remainder settings with its shard count,
so execution uses the same aggregate budget. Shard builds use `go -p=1`; the
remainder uses `go test -p=4`. Memory allowances guide scheduling and do not
enforce per-process limits, including native allocations.

Detection accounts for CPU affinity and `GOMAXPROCS`. On Linux it also accounts
for visible cgroup v2 ancestor CPU quotas and remaining memory under both hard
and soft limits. macOS uses available system memory. Smaller budgets, cgroup v1,
other platforms, and unreadable limits retain the standard schedule. Detection
is a snapshot, not a reservation of resources against other workloads.

Use `TEST_PROFILE=standard` to disable automatic scaling. Setting `TEST_SHARDS`
explicitly also retains sequential package jobs with the requested shard count.
The PostgreSQL targets keep their existing connection-oriented concurrency
limits; setting `MSGVAULT_TEST_DB` disables automatic scaling in `make test` too.
CI's explicit `test-unsharded` and package-shard jobs keep their existing layout.

## Lint & Format

```bash
# Format code
make fmt

# Run linter (builds the pinned golangci-lint with Kit's plugin; needs git)
make lint

# Check for issues
go vet ./...
```

## Profile Web UI search

Inspect the `Server-Timing` headers on successful `/api/v1/explore` responses in
browser developer tools. Failed requests may contain only the phases completed
before the error. Durations are milliseconds:

| Metric | Work measured |
| --- | --- |
| `candidates` | Resolve the search candidate pool, including the search phases below |
| `lexical` | Count full-text matches and fetch bounded, ranked message IDs |
| `embedding` | Embed the semantic or hybrid query |
| `retrieval` | Retrieve vector or fused results; `desc` names the accelerator path |
| `projection` | Build result rows from the analytical cache |
| `identities` | Hydrate identity matches for the returned rows |

The `candidates` duration includes `lexical`, `embedding`, and `retrieval` work;
do not add those subphases to it. A reused semantic candidate snapshot skips
embedding and retrieval, so those metrics are absent. Group requests to
`/api/v1/explore/groups` report `grouping` for the analytical aggregation.

Run the synthetic HTTP benchmark to measure candidate resolution through the
real SQLite full-text index and DuckDB result projection:

```bash
go test -tags 'fts5 sqlite_vec' ./internal/api -run '^$' \
  -bench '^BenchmarkExploreFullText$' -benchtime=3x -count=3 -benchmem
```

It creates an isolated 20,000-message archive and measures rare, common, and
candidate-limited queries. Setup and a warm-up request are excluded. Compare
the same fixture and machine before and after a change; these timings do not
predict latency on a larger archive.

For analytical grouping and domain drill-downs, build the larger synthetic
cache once in an empty scratch directory, then reuse it:

```bash
explore_bench_root=$(mktemp -d)
go test -tags 'fts5 sqlite_vec' ./internal/query -run '^$' \
  -bench '^BenchmarkExploreScaleBuild$' -benchtime=1x \
  -args -relationship-bench-root="$explore_bench_root"
go test -tags 'fts5 sqlite_vec' ./internal/query -run '^$' \
  -bench '^BenchmarkExploreScaleQueries$' -benchtime=4x \
  -args -relationship-bench-root="$explore_bench_root"
```

This fixture has 2,562,000 messages and 71,486 people. The query benchmark
reports the first request and the median of later requests separately. It uses
the default interactive memory limit; DuckDB can spill work to temporary disk.
Add `-relationship-bench-memory=8GB` for diagnostic comparisons with older
queries that exceed that limit. Add
`-relationship-bench-profile` to write DuckDB operator profiles into the scratch
directory. Run timing comparisons without other heavy workloads, and retain
the same memory limit and thread settings. This fixture uses uniform senders
and one chat roster; it does not model real participant skew or attachments.

To separate local vector retrieval from embedding-provider latency, run:

```bash
go test -tags 'fts5 sqlite_vec' ./internal/vector/sqlitevec -run '^$' \
  -bench '^BenchmarkVectorRetrieval$' -benchtime=3x -count=3 -benchmem
```

This benchmark uses 100,000 synthetic messages with one 64-dimensional vector
each. It measures exact semantic retrieval and rare/common hybrid retrieval,
without an accelerator, embedding calls, or HTTP result projection. Use the
request timing headers to determine which phase needs attention on a real
archive before comparing it with this narrower benchmark.

## Evaluate search quality

Use [`msgvault eval`](cli-reference.md#eval) to compare keyword, semantic, and
hybrid results against queries and relevance ratings you supply. Keep the
archive, topics, and ratings the same when comparing runs. The command reports
ranking quality and query timings; it does not create the ratings for you.

## vCard registry maintenance

The lossless vCard 2.1/3.0/4.0 codec vendors the IANA vCard Elements registry
under `internal/vcard/registry/data`. Registry checks are deliberate networked
maintenance commands, not ordinary CI steps:

```bash
# Report whether the upstream registry differs from the vendored snapshot.
make vcard-registry-check

# Fetch, validate, and atomically update the snapshot for review.
make vcard-registry-update
```

The codec preserves ordered properties, source spelling, parameter quoting,
unknown extensions, and raw values. Keep new registry elements covered by an
explicit handling declaration so an upstream addition cannot be silently
ignored.

## Code Conventions

- **Web UI**: Svelte with TypeScript, generated OpenAPI types, and components
  from the shared UI toolkit
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
| `svelte` | Embedded analytical Web UI |
| `orval` | Typed browser API client generation |
| `cobra` | CLI framework |
| `charmbracelet/bubbletea` | TUI framework |
| `charmbracelet/lipgloss` | TUI styling |
| `mattn/go-sqlite3` | SQLite with CGO (FTS5) |
| `duckdb/duckdb-go/v2` | DuckDB driver for Parquet |
| `gogs/chardet` | Character set detection |
| `golang.org/x/text` | Text encoding conversion |

## Documentation

The site has three reading levels: the product overview at `/`, the archive
lifecycle at `/guide/`, and task guides and references at `/docs/`. Keep
technical setup and command details in the documentation tier.

From the repository root:

```bash
make docs-install
make docs-build
make docs-check
```

`make docs-check` validates Markdown, builds the real site with its asset
branches, and checks pages, links, metadata, and redirects. Use `make docs-serve`
to inspect the complete site at `http://127.0.0.1:8000`.

See the [documentation contributor guide](https://github.com/kenn-io/msgvault/blob/main/docs/README.md)
for content ownership, historical designs, and media maintenance.

## Community

Join the [msgvault Discord server](https://discord.gg/fDnmxB8Wkq) to discuss development, ask questions, or share feedback.
