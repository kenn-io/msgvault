# Agent guide

These instructions apply to all coding agents in this repository. Read
[CONSTITUTION.md](CONSTITUTION.md) first. `CLAUDE.md` imports this file;
keep shared project guidance here.

## Find the right reference

msgvault archives communications and relationships on the user's own hardware.
The daemon owns archive access; the CLI, Web UI, TUI, HTTP API, and MCP server
provide different ways to use the same archive.

- [Architecture](docs/architecture/overview.md): responsibilities and data flow.
- [Development](docs/development.md): build prerequisites, test scheduling,
  PostgreSQL test setup, and documentation checks.
- [CLI reference](docs/cli-reference.md) and [configuration](docs/configuration.md):
  commands, flags, defaults, and runtime paths.
- [Documentation contributor guide](docs/README.md): page ownership and maintenance.
- [Historical designs](docs/internal/README.md): implementation rationale, not
  a substitute for current source or architecture.

## Workflow

- Complete all requested steps. If you changed files, commit the work before
  ending the turn; do not ask for permission to commit. Preserve unrelated work.
- Before committing, inspect `git diff` and `git status`. Include all changes
  from the task, including formatting and generated output.
- Keep PR descriptions focused on the problem, resulting behavior, and review
  boundaries. Do not add `Validation`, `Test plan`, or equivalent command-run
  sections; CI carries routine check results.
- Never run `roborev review` in any form, or invoke a roborev skill, unless the
  user explicitly asks for it. Other roborev CLI commands may be used when
  appropriate.
- Never name private downstream projects or codebases in public artifacts.
  Describe reusable requirements generically and run the private-data scrub
  before publishing.

## Documentation

- Write for the person trying to use or maintain msgvault. Lead with the
  outcome, name who does what, use short sentences, and explain unfamiliar terms.
- Organize around reader questions. Put purpose and current capabilities first;
  separate limitations and future work. Use only the sections the topic needs.
- Give each bullet one main idea. Use numbered steps for sequences, paragraphs
  for rationale, and tables or diagrams when they clarify a comparison or flow.
- State rules directly. Preserve exact commands, field names, authorization
  checks, limits, and failure behavior when simplifying the wording.
- Give each fact an owning guide or reference and link to it elsewhere. Update
  that section instead of appending a narrative of the latest change. Indexes
  should route readers, not repeat implementation status.
- Describe current architecture separately from approved but unbuilt work,
  proposals, and historical decisions. Preserve rationale, approvals, and active
  exceptions with their removal conditions; label superseded designs and keep
  them outside normal navigation.
- Keep the website, its Markdown companions, README, and documentation on
  message. Distinguish the latest release from newer `main` functionality.
  Follow [docs/README.md](docs/README.md) for the publishing layout and checks.

## Feature discovery

Use these as reasoning checkpoints, not a requirement to create a design doc.

- Start from the concrete user outcome and inspect the nearest production path
  or real artifact with safe, reversible checks.
- For archives and importers, inspect schemas and aggregate metadata before
  recommending external services. Do not display personal content or identifiers
  unless needed and authorized.
- Separate verified source capabilities, current msgvault behavior, workaround
  dependencies and trust costs, and unverified assumptions.
- Prefer the smallest useful end-to-end change. Only conditions that block the
  next reversible step are prerequisites.
- For uncertain upstream contributions, prefer a focused draft PR linked to the
  motivating issue. Defer unrelated hardening until evidence warrants it.

## Build and test

- Use `make build` for a worktree binary. `make install` changes the user's
  installed binary and needs intentional authorization.
- All `go test` invocations need `-tags "fts5 sqlite_vec"`; prefer `make test`.
  PostgreSQL tests use the targets documented in [Development](docs/development.md).
- After Go changes, run `go fmt ./...` and `go vet ./...` before committing.
  Include resulting formatting changes. Use `make lint-ci` for lint checks.
- All new or modified Go assertions must use testify: `require.X` for setup or
  fatal preconditions, `assert.X` for independent checks. Never add `t.Errorf`,
  `t.Fatalf`, `t.Fatal`, or `t.Error`. Equality is `(want, got)`.
- Call testify directly. `internal/testutil` is for non-assertion helpers such
  as `MakeSet`, `NewTestStore`, and fixture builders.
- Exercise production behavior, a real parser/validator, or a built artifact.
  Do not copy scripts into synthetic trees, stub their primary commands, and
  assert the stub arguments. A fake command is justified only to prove a stable
  external contract or regression the real path cannot cover; explain why in
  the commit message.
- Do not write shell tests that grep scripts, workflows, config, or docs for
  expected implementation text. Use real execution or tool-native validation.
- A timeout may bound a wait for an observable transition. Do not make runner
  throughput a correctness requirement, such as ten polls within one second.

## Test data

Use synthetic names and reserved example addresses in ordinary fixtures. Never
copy real people's names, email addresses, or identifiers into them.

The provenance-documented Enron Web UI fixture on the `docs-fixtures` orphan
branch and its derived documentation screenshots are the sole exception. Its
manifest and README must retain provenance, attribution, the completed
message-by-message sensitive-content review, and the reviewed artifact digest
before publication. This exception does not permit reuse in ordinary tests.

## Code and SQL conventions

- Use Bubble Tea and lipgloss for the TUI; Svelte and the shared UI toolkit for
  the Web UI. See [Development](docs/development.md) for the dependency map.
- Route database operations through `Store`. Use DuckDB for Parquet queries,
  `mattn/go-sqlite3` for SQLite, context cancellation for long operations, and
  wrapped errors with useful context. Prefer table-driven tests.
- Use `gogs/chardet` for charset detection and `golang.org/x/text/encoding` for
  conversion.
- Never use `SELECT DISTINCT` with JOINs; use `EXISTS` subqueries to avoid
  duplicates at the source. See the [SQL example](docs/development.md#sql-guidelines).
- Never JOIN or scan `message_bodies` in list, aggregate, or search queries.
  Access it by direct primary-key lookup (`WHERE message_id = ?`) for one
  message's details. Use FTS5 (`messages_fts`) for text search; if unavailable,
  search `subject` and `snippet` only.
