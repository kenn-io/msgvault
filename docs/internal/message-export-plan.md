# Generic Message Export Implementation Plan

> **For agentic workers:** REQUIRED: Use
> `superpowers:subagent-driven-development` if subagents are available, or
> `superpowers:executing-plans` otherwise, to implement this plan. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a deterministic, provider-neutral JSONL export for bounded
message windows while retaining the existing compatibility command through its
consumer migration window.

**Architecture:** A store-level streaming read model resolves stable source,
conversation, author, and message identities without exposing database keys or
raw provider metadata. The CLI validates and resolves filters before writing a
manifest, streams the three ordered data phases through the existing daemon
transport, and writes a count-bearing completion record only after the store
export succeeds. Message bodies are fetched by direct primary-key lookup after
each bounded metadata page rather than joined into list queries.

**Tech Stack:** Go 1.26, Cobra, SQLite, PostgreSQL, Testify, JSON Lines.

---

## Working rules

- Work on `codex/discord-export`; do not implement on `main`.
- Use `@superpowers:test-driven-development` and `@kenn:commit` for each code
  task.
- Use `@kenn:scrub-private-data` before every public push or pull-request
  update. If the skill is unavailable, perform the equivalent repository,
  commit, and pull-request diff audit manually and report that fallback.
- Keep public artifacts application-neutral. Do not name, link, or encode
  private downstream repositories, paths, fixtures, requirements, or
  implementation details.
- Keep `export-discord` and `msgvault-discord-export/1` working during this
  change. Their removal is a separate coordinated follow-up after the consumer
  migration and live parity check.
- Use Testify. Run msgvault Go tests with `-tags "fts5 sqlite_vec"`.
- Keep stdout machine-readable: JSONL records only. Send diagnostics to stderr.
- One focused commit per task; never amend or squash unless asked.

## Public contract

```text
msgvault export-messages \
  --start <RFC3339> \
  --end <RFC3339> \
  [--message-type <type> ...] \
  [--source <source-type>:<identifier> ...] \
  [--format jsonl]
```

The stream schema is `msgvault-message-export/1` and has exactly five ordered
phases:

```text
manifest -> source* -> conversation* -> message* -> complete
```

The half-open interval is `[start, end)`. Filter arrays and every record phase
are deterministic. A missing `complete` record is an invalid partial export.

## Store model

Create a provider-neutral store interface along these lines; names may improve
during implementation while preserving the contract:

```go
type MessageExportFilter struct {
	Start        time.Time
	End          time.Time
	SourceIDs    []int64
	MessageTypes []string
}

type MessageExportSource struct {
	SourceType          string
	Identifier          string
	DisplayName         string
	LastSuccessfulSyncAt *time.Time
}

type MessageExportConversation struct {
	SourceType       string
	SourceIdentifier string
	ID               string
	Title            string
	ConversationType MessageExportConversationType
	ParentID         *string
}

type MessageExportAuthor struct {
	DisplayName string
	Address     string
}

type MessageExportMessage struct {
	SourceType       string
	SourceIdentifier string
	ID               string
	ConversationID   string
	MessageType      string
	Subject          string
	Text             string
	Author           *MessageExportAuthor
	OccurredAt       time.Time
	DeletedFromSource bool
}

type MessageExportCounts struct {
	Sources       int
	Conversations int
	Messages      int
}

type MessageExportSink interface {
	Source(MessageExportSource) error
	Conversation(MessageExportConversation) error
	Message(MessageExportMessage) error
}

func (s *Store) ExportMessages(
	ctx context.Context,
	filter MessageExportFilter,
	sink MessageExportSink,
) (MessageExportCounts, error)
```

`ConversationType` is a closed enum containing `email_thread`, `channel`,
`thread`, `direct_chat`, `group_chat`, `meeting`, `calendar`, and `other`.
Provider storage values are normalized internally; no raw metadata enters the
public record structs.

## Task 1: Add the bounded store export read model

**Files:** create `internal/store/message_export.go` and
`internal/store/message_export_test.go`.

- [ ] Write failing SQLite tests:

```go
func TestExportMessagesStreamsDeterministicPhases(t *testing.T)
func TestExportMessagesUsesHalfOpenEffectiveTimestampWindow(t *testing.T)
func TestExportMessagesFiltersSourcesAndMessageTypes(t *testing.T)
func TestExportMessagesEmitsExplicitEmptySources(t *testing.T)
func TestExportMessagesNormalizesConversationTypes(t *testing.T)
func TestExportMessagesNormalizesAuthorsAndDeletionState(t *testing.T)
func TestExportMessagesStopsWhenSinkFails(t *testing.T)
func TestExportMessagesRejectsInvalidFilter(t *testing.T)
```

- [ ] Cover the timestamp fallback order `sent_at`, `received_at`,
  `internal_date`; exclude messages with no effective timestamp.
- [ ] Cover all eight public conversation types. For Discord storage, prove
  channel types 10, 11, and 12 normalize to `thread`, other containers to
  `channel`, and a normalized parent identity is retained when present.
- [ ] Cover stable author display name/address, full text, subject, message
  type, and `deleted_from_source`. Prove locally hidden or deduplicated rows do
  not leak into the export.
- [ ] Confirm red:

```bash
go test -tags "fts5 sqlite_vec" ./internal/store \
  -run 'TestExportMessages' -count=1
```

- [ ] Implement deterministic source, conversation, and message phases.
  Explicit source IDs emit source records even for an empty window; otherwise
  sources and conversations are derived only from matching messages.
- [ ] Page message metadata in fixed-size deterministic batches. Close each
  rows iterator before fetching bodies. Fetch each body only with
  `WHERE message_id = ?`; never join or scan `message_bodies` in the list
  query.
- [ ] Use bound parameters for all source and type filters. Include an internal
  primary-key tie-breaker for stable ordering but never expose that key.
- [ ] Reject missing stable identities, duplicate scoped identities, invalid
  bounds, and nil sinks. Return immediately on a sink error.
- [ ] Add PostgreSQL coverage through the existing dialect test harness for
  dynamic placeholders, effective timestamp ordering, and direct body lookup.
- [ ] Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/store -count=1
go test -race -tags "fts5 sqlite_vec" ./internal/store \
  -run 'TestExportMessages' -count=1
```

Expected: pass.

- [ ] Commit: `feat: add generic message export read model`.

## Task 2: Add the JSONL command and daemon route

**Files:** create `cmd/msgvault/cmd/export_messages.go` and
`cmd/msgvault/cmd/export_messages_test.go`; modify `cmd/msgvault/cmd/root.go`,
`internal/api/cli_handlers.go`, and their focused tests.

- [ ] Write failing command tests:

```go
func TestExportMessagesCommandWritesContractOrderAndCounts(t *testing.T)
func TestExportMessagesCommandNormalizesFilters(t *testing.T)
func TestExportMessagesCommandRejectsInvalidBoundsBeforeOutput(t *testing.T)
func TestExportMessagesCommandRejectsMissingSourceBeforeOutput(t *testing.T)
func TestExportMessagesCommandAllowsAnEmptyMessageTypeResult(t *testing.T)
func TestExportMessagesCommandOmitsCompletionAfterStreamFailure(t *testing.T)
func TestExportMessagesCommandRejectsUnsupportedFormat(t *testing.T)
func TestExportMessagesCommandRoutesThroughDaemon(t *testing.T)
```

- [ ] Confirm red:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api \
  -run 'TestExportMessages' -count=1
```

- [ ] Add `export-messages` with no positional arguments. Require valid RFC3339
  `--start` and `--end`, require `start < end`, and accept only `jsonl` for v1.
- [ ] Parse each source selector at its first colon. Require non-empty type and
  identifier, resolve the exact typed source before writing stdout, reject
  missing selectors, and deduplicate resolved IDs.
- [ ] Reject empty message-type values. Sort and deduplicate both public filter
  arrays before writing the manifest.
- [ ] Write one compact JSON object per line with HTML escaping disabled:
  manifest, store-emitted sources, store-emitted conversations, store-emitted
  messages, then complete. Timestamps are UTC RFC3339. Nullable fields encode
  as JSON null where the schema requires it.
- [ ] Keep all preflight validation before the manifest. If a store, encoding,
  or output error occurs after the manifest, return nonzero without writing a
  completion record.
- [ ] Register the command in the root tree and daemon CLI allowlist. Retain
  normal operation gating so the multi-phase export observes one coherent
  archive state.
- [ ] Prove the existing provider-specific compatibility command and schema
  remain unchanged:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run 'TestExportDiscord|TestExportMessages' -count=1
```

- [ ] Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api -count=1
go test -race -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run 'TestExportMessages' -count=1
```

Expected: pass.

- [ ] Commit: `feat: stream bounded message exports`.

## Task 3: Document the generic export

**Files:** modify `docs/usage/exporting.md`, `docs/usage/discord.md`,
`docs/configuration.md`, generated CLI reference artifacts, and the README
only where the command is already surfaced.

- [ ] Document required bounds, half-open window semantics, repeatable source
  and message-type filters, the five JSONL phases, deterministic order,
  completion-count validation, nullable sync provenance, and the fact that the
  command never contacts providers.
- [ ] Document the closed conversation-type vocabulary and the stable identity
  scopes. Do not document raw provider metadata layouts.
- [ ] Mark the provider-specific export as a temporary compatibility surface
  without naming or describing any downstream consumer.
- [ ] Regenerate the CLI reference using the repository's existing generator;
  do not hand-edit generated sections.
- [ ] Run:

```bash
make docs-check
make openapi-check
```

Expected: pass with no generated drift.

- [ ] Commit: `docs: document generic message exports`.

## Task 4: Verify and publish the public stage

**Files:** no new files expected.

- [ ] Run the full verification suite:

```bash
make test
make lint-ci
go vet ./...
make docs-check
make openapi-check
```

Expected: pass.

- [ ] If `MSGVAULT_TEST_DB` is configured, run `make test-pg`. Otherwise record
  that the PostgreSQL integration suite was unavailable; the dialect unit
  coverage from Task 1 remains required.
- [ ] Inspect `git status`, `git diff origin/main...HEAD`, and every commit
  added by this work. Confirm there are no secrets, local absolute paths,
  private downstream names, private fixtures, or unrelated changes.
- [ ] Run the private-data scrub skill or its documented manual equivalent over
  the complete branch diff and planned pull-request text.
- [ ] Push `codex/discord-export` and update its pull request with a
  rationale-first, provider-neutral description of the command, compatibility
  window, and verification evidence.
- [ ] Do not remove `export-discord` in this stage.

## Deferred coordinated follow-up

After the generic command is available, migrate the active consumer to
`msgvault-message-export/1` and verify a live bounded-window parity result. Only
then remove `export-discord`, its provider-specific schema, tests, and
documentation in a separate public commit and pull-request update. The public
follow-up must remain generic and refer only to completion of the compatibility
window.
