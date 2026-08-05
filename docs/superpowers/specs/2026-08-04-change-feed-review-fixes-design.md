# Change Feed Review Fixes Design

## Goal

Make the new message content-change feed safe for nullable legacy data, portable
across CI platforms, simpler to maintain, and easier for API consumers to use.
The PR description will be replaced with a concise explanation of the feature,
its intended use, and its important limitations.

## Scope

This change addresses the four maintainer-review findings plus the current
Windows CI failure:

- tolerate a nullable `messages.has_attachments` value without blocking feed
  progress;
- stop turning malformed stored watermarks into synthetic response timestamps;
- install watermark triggers only when their version changes;
- make archive identity lookup context-aware without a background goroutine;
- make the noncanonical-default test fixture valid on Windows SQLite; and
- update generated API artifacts, user documentation, the PR body, and the
  roborev review state to match the resulting behavior.

The feed remains a latest-state invalidation feed over its existing tracked
column set. Expanding it to labels, recipients, attachments, hard deletions, or
other child-table changes is out of scope.

## Storage behavior

`ListChangedMessages` will select
`COALESCE(has_attachments, FALSE)`. A regression test will insert a real legacy
row with `NULL` and exercise the production feed query. The shared store test
will run on SQLite by default and on PostgreSQL when `MSGVAULT_TEST_DB` is set.

The watermark scanner will distinguish an absent value from an invalid value.
A selected row with a `NULL` or unparseable `content_changed_at` will make the
query fail with an error that identifies the invalid watermark. It will no
longer substitute the request cursor or return a page that can repeat forever.
Ordinary nullable message timestamps remain optional and tolerant.

Watermark trigger installation will be guarded by a versioned migration-ledger
entry. Fresh and upgraded archives install the current definitions before the
content watermark backfill. Later opens skip the trigger DDL. Any future change
to the trigger definitions or tracked-column list must use a new migration
version. Failed or cancelled installations remain unmarked and retry on the
next open.

## API behavior

`ArchiveIdentifier` will accept `ArchiveUIDContext(context.Context)`. The store
implementation will query with `QueryRowContext`, while the existing
contextless `ArchiveUID` method remains as a compatibility wrapper. The daemon
adapter will expose the context-aware method. The API server will remove the
archive-identity cache, mutex, and background lookup goroutine and call the
context-aware method directly for each change-feed request.

All successfully returned `content_changed_at` values will be real timestamps,
so the OpenAPI field can use `format: date-time`. `complete_through` will be
nullable: `null` means that SQLite has not established a commit bound yet. This
replaces the year-one string sentinel and lets generated clients expose a typed
optional timestamp. `server_time` remains a required timestamp.

Malformed or missing stored watermarks are internal archive errors and return
the endpoint's existing sanitized `500 internal_error` response. The detailed
cause remains in server logs.

## Windows CI portability

The failing test currently rewrites `sqlite_schema` with a parenthesized
noncanonical default. The Windows SQLite build rejects that schema text while
opening the database, before msgvault can perform the intended validation. The
fixture will use a noncanonical default syntax that SQLite accepts on every
supported platform, while preserving the test's real assertion: msgvault must
reject a usable archive whose watermark default has drifted from the canonical
millisecond format.

## Tests and documentation

Each behavior change will follow a red-green cycle:

- nullable legacy boolean reaches the end of the feed;
- invalid watermarks return an error rather than a synthetic timestamp;
- an absent commit bound is encoded as JSON `null` and decoded by the generated
  client as an optional typed time;
- a cancelled archive UID lookup cancels the database query without leaving a
  background goroutine;
- a second schema initialization does not reinstall versioned triggers; and
- the noncanonical-default fixture opens far enough to be rejected by
  `InitSchema` on all platforms.

Obsolete tests and documentation for year-one sentinels, fabricated watermark
times, and repeating malformed-watermark pages will be removed. Repeated setup
helpers encountered in the touched tests will be consolidated where that makes
the contract clearer, without rewriting unrelated test suites.

The PR description will use short bullets covering:

- what the endpoint does and why;
- how to page with `next_cursor` and `has_more`;
- which changes are and are not tracked;
- the backward-clock and snapshot-restore limitations; and
- the one-time migration and SQLite polling costs.

It will not include a test-plan section or low-level implementation rationale.

## Verification and publication

Run focused tests for each red-green cycle, then `go fmt ./...`, `go vet ./...`,
`make test`, and `make lint-ci` with the repository's required build tags and an
isolated scratch environment. Regenerate and verify the OpenAPI, Go client, and
web schema artifacts. PostgreSQL-specific coverage will run when an isolated
test database is available; otherwise that gap will be reported explicitly and
left to CI.

Before committing or editing the public PR, scan the diff, commit message, PR
body, and roborev response for private data. Commit the implementation, update
PR #545's body through GitHub, and add a roborev response explaining that the
nullable scan was fixed and covered on both database backends.
