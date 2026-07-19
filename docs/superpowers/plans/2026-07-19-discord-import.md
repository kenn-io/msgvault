# Discord Guild Import Implementation Plan

> **For Codex:** Execute this plan with the `superpowers:subagent-driven-development` workflow. Follow test-driven development for every behavior change, use testify in all Go tests, and run Go tests with `-tags "fts5 sqlite_vec"`.

**Goal:** Add a native, resumable Discord guild-history importer with bounded edit/deletion repair, durable attachment storage, CLI and scheduled sync integration, and no schema migration.

**Architecture:** Model each Discord guild as one `discord` source and each channel or thread as an independent message container. A small REST client supplies catalog and message pages to an importer that persists through the existing store APIs, checkpoints per-container state in sync runs, and only marks deletions after a complete comparison over an identical pinned snowflake interval. Credentials are bot-token records resolved by `sources.oauth_app` labels without involving the generic OAuth app loader.

**Technology:** Go, Cobra, SQLite/PostgreSQL stores, `golang.org/x/time/rate`, testify, `httptest` fake Discord API servers.

The authoritative behavior is in `docs/internal/discord-import-design.md`. If this plan and the design differ, preserve the design and update this plan before coding.

---

## Task 1: Configuration, filters, snowflakes, and checkpoint types

**Files:**
- Create: `internal/discord/config.go`
- Create: `internal/discord/config_test.go`
- Create: `internal/discord/snowflake.go`
- Create: `internal/discord/snowflake_test.go`
- Create: `internal/discord/syncstate.go`
- Create: `internal/discord/syncstate_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

1. Write failing table-driven tests for the 50 MiB media default, seven-day rescan default, TOML round trips, parent-filter inheritance, explicit thread overrides, exclude precedence, snowflake timestamp bounds, state round trips, monotonic state merges, and malformed-state rejection.
2. Run `go test -tags "fts5 sqlite_vec" ./internal/discord ./internal/config` and confirm the new tests fail for missing behavior.
3. Add `DiscordConfig` and `DiscordGuildConfig` to the root config, with defaults reapplied by both `NewDefaultConfig` and `Load`.
4. Implement `ContainerIncluded`, timestamp/snowflake conversion helpers, and versioned checkpoint types:
   - `ContainerState { HighWater, BackfillBefore, BackfillUpper string; BackfillComplete bool }`
   - `ThreadCatalogState { PublicArchiveWatermark, PrivateArchiveWatermark string }`
   - `SyncState { Version int; Containers map[string]ContainerState; ThreadCatalog map[string]ThreadCatalogState }`
5. Make merges monotonic for high-water and catalog fields, preserve incomplete backfill bounds, and return an error instead of resetting malformed state.
6. Re-run the focused tests, `gofmt` changed Go files, and commit as `feat(discord): add import configuration and sync state`.

## Task 2: Discord bot token storage and binding resolution

**Files:**
- Create: `internal/discord/token.go`
- Create: `internal/discord/token_test.go`
- Modify: `internal/clirun/env.go`
- Modify: `internal/clirun/env_test.go`

1. Write failing tests for token-file permissions, protected serialization, `discord_<bot-user-id>.json` naming, named binding lookup, sole unnamed-token fallback, unnamed-to-named promotion, ambiguous unnamed lookup, duplicate binding rejection, and environment handoff.
2. Define a token record containing bot user ID, bot username, access token, and optional binding. Keep binding resolution entirely inside `internal/discord`.
3. Implement a token manager that lists and validates records, writes atomically with mode `0600`, never logs tokens, and resolves explicit labels, sole/default records, promotions, and ambiguity exactly as the design specifies.
4. Add a protected CLI-run environment variable for Discord token handoff.
5. Run `go test -tags "fts5 sqlite_vec" ./internal/discord ./internal/clirun` and commit as `feat(discord): add bot credential bindings`.

## Task 3: Minimal REST client and rate-limit handling

**Files:**
- Create: `internal/discord/types.go`
- Create: `internal/discord/client.go`
- Create: `internal/discord/client_test.go`
- Create: `internal/discord/fake_server_test.go`

1. Build an `httptest` server that exercises real request parsing and response behavior for current bot, guilds, guild detail, channels, active threads, archived public/private threads, message pages, and one-message refresh.
2. Write failing tests for authorization and user agent headers, Discord API error decoding, pagination parameters, route-bucket serialization, global `429` handling, `Retry-After` in headers and JSON, cancellation during waits, and rejecting redirects or pagination URLs outside the configured API origin.
3. Define only the response fields required by the design, retaining `json.RawMessage` for archival payloads. Expose an importer-facing `API` interface with `Me`, `Guilds`, `Guild`, `GuildChannels`, `ActiveThreads`, `ArchivedThreads`, `Messages`, and `Message`; member data is consumed only when attached to observed message payloads, never through roster enumeration.
4. Implement explicit read-only routes with a shared limiter, per-route bucket state learned from response headers, global pause state, bounded retries, and context-aware waits.
5. Run `go test -tags "fts5 sqlite_vec" ./internal/discord` and commit as `feat(discord): add rate-limited REST client`.

## Task 4: Store primitives for Discord conversations, deletion repair, and attachments

**Files:**
- Modify: `internal/store/conversations.go`
- Modify: `internal/store/conversations_test.go`
- Modify: `internal/store/messages.go`
- Modify: `internal/store/messages_test.go`
- Modify: `internal/store/attachments.go`
- Modify: `internal/store/attachments_test.go`

1. Write failing SQLite-backed tests for conversation metadata get/set, source IDs selected by exact numeric snowflake interval `(lower, upper]` and conversation, clearing `deleted_from_source_at`, Discord attachment replacement/listing, and pending-attachment selection.
2. Add `SetConversationMetadata` and `GetConversationMetadata` without changing the schema.
3. Add `MessageSourceIDsInSnowflakeInterval(sourceID, conversationID, lower, upper)` using length-then-lexicographic decimal comparison so values larger than signed 64-bit remain correctly ordered in both supported databases.
4. Add `ClearMessageDeletedFromSource` for messages reappearing in a rescan.
5. Extract provider-neutral attachment query helpers from the existing Beeper implementation, preserve the Beeper API, and add `ReplaceMessageDiscordAttachments`, `MessageDiscordAttachments`, and `ListDiscordPendingAttachmentMessages`.
6. Run `go test -tags "fts5 sqlite_vec" ./internal/store` and commit as `feat(store): add Discord import persistence helpers`.

## Task 5: Participant and message mapping

**Files:**
- Create: `internal/discord/participants.go`
- Create: `internal/discord/participants_test.go`
- Create: `internal/discord/mapping.go`
- Create: `internal/discord/mapping_test.go`

1. Write failing table-driven tests covering regular users, bots, webhooks with per-message presentation overrides, guild nicknames as observations, mention recipients, replies, Unicode and custom reaction summaries, known system-message rendering, unknown message-type fallback, attachment metadata, embeds, and raw JSON.
2. Implement stable identities keyed by `discord_user_id` or `discord_webhook_id`. Only materialize observed authors and mentions; never import the whole guild roster as conversation participants.
3. Map channels and threads to `conversation_type='channel'`, messages to `message_type='discord'` and `raw_format='discord_json'`, author recipients to `from`, mentions to `mention`, and thread/reply fields to metadata.
4. Store reaction summaries as `[{emoji, emoji_id?, animated?, count}]` in message metadata without creating normalized reaction rows.
5. Render all known system types to readable text and retain unknown types with conservative text plus raw payload. Preserve unresolved referenced message/channel snowflakes in metadata.
6. Run `go test -tags "fts5 sqlite_vec" ./internal/discord` and commit as `feat(discord): map participants and messages`.

## Task 6: Attachment ingestion and media backfill

**Files:**
- Create: `internal/discord/media.go`
- Create: `internal/discord/media_test.go`

1. Write failing tests for successful downloads through `export.StoreAttachmentFile`, the size cap before and during streaming, signed URL refresh through the message endpoint, CDN HTTP failures, canceled downloads, pending-state preservation, HTTPS-only approved Discord CDN hosts and paths, rejected cross-origin redirects, and the absence of bot authorization on CDN requests.
2. Implement attachment persistence that records metadata first, downloads within the configured cap, uses temporary files safely, validates content length while streaming, validates every initial and redirected CDN URL, never sends the bot token to a CDN, and treats media failure as retryable attachment state rather than message failure.
3. Implement backfill that re-fetches the source message for fresh signed URLs. Return an explicit unrecoverable result when the source message is gone.
4. Run `go test -tags "fts5 sqlite_vec" ./internal/discord` and commit as `feat(discord): archive attachments and support backfill`.

## Task 7: Guild, channel, and thread catalog

**Files:**
- Create: `internal/discord/catalog.go`
- Create: `internal/discord/catalog_test.go`

1. Write fake-server integration tests for text/announcement/forum discovery, active threads, paginated public/private archived threads, parent filter inheritance, explicit thread overrides, advancing each archive watermark only after full pagination, and preserving watermarks on `403`, `404`, cancellation, or malformed pages.
2. Implement a catalog result that returns each channel/thread as an independent container with parent metadata.
3. Keep public and private archive watermarks per parent. Do not treat forum parents as message containers; import each forum post/thread independently.
4. Run `go test -tags "fts5 sqlite_vec" ./internal/discord` and commit as `feat(discord): discover message containers`.

## Task 8: Importer, forward pagination, and crash resumption

**Files:**
- Create: `internal/discord/importer.go`
- Create: `internal/discord/importer_test.go`

1. Write integration tests for an initial pinned `backfill_upper`, backward paging below that head, forward collection above it, independent container cursors, page-level `cursor_before` checkpoints, last-successful `cursor_after` plus newer failed/running checkpoint precedence, at-most-one-page repeat, `--after` lower bounds, and `--full` state reset.
2. Persist each message atomically through `store.PersistMessage`, then add recipients, reply metadata, metadata, and attachments idempotently. Upsert existing source IDs so repeated pages and repair scans update edits instead of duplicating rows.
3. After message persistence, run a deferred reply-linking pass so replies imported before older parents on later pages or syncs can populate `reply_to_message_id`; keep unresolved source snowflakes in metadata.
4. After every durable page, update the running sync's `cursor_before`. Only publish merged `cursor_after` and mark the run successful after all requested work completes.
5. On successful guild completion call `RecomputeConversationStats` for affected conversations.
6. Run `go test -tags "fts5 sqlite_vec" ./internal/discord` and commit as `feat(discord): import guild message history`.

## Task 9: Bounded edit, reaction, and deletion reconciliation

**Files:**
- Modify: `internal/discord/importer.go`
- Modify: `internal/discord/importer_test.go`
- Modify: `internal/discord/syncstate.go`
- Modify: `internal/discord/syncstate_test.go`

1. Write integration tests that pin `lower` from the window start and `upper` to the maximum of the remote container head and the highest archived/high-water snowflake at scan start, apply the identical `(lower, upper]` predicate remotely and locally, support an empty remote result, refresh edits and reaction summaries, clear false tombstones when messages reappear, detect deletion of the newest or every message, and never tombstone an archived message above `upper`.
2. Add complete-range ID comparison for seven-day repair scans and the whole selected range during `--full`. Mark missing IDs only after a complete successful enumeration.
3. Add suppression tests for `403`, `404`, cancellation, page failure, malformed response, and partial pagination. One incomplete condition disables deletion marking only for that container.
4. Record `container_inaccessible_since` for `403` and `container_missing_since` plus reason for `404` in conversation metadata. Preserve messages and cursors, clear the marker on recovery, and never mass-tombstone from container status alone.
5. Run `go test -tags "fts5 sqlite_vec" ./internal/discord` and commit as `feat(discord): reconcile recent message changes safely`.

## Task 10: CLI commands

**Files:**
- Create: `cmd/msgvault/cmd/add_discord.go`
- Create: `cmd/msgvault/cmd/add_discord_test.go`
- Create: `cmd/msgvault/cmd/sync_discord.go`
- Create: `cmd/msgvault/cmd/sync_discord_test.go`
- Create: `cmd/msgvault/cmd/backfill_discord_media.go`
- Create: `cmd/msgvault/cmd/backfill_discord_media_test.go`
- Modify: `cmd/msgvault/cmd/root.go`

1. Write command tests using a real test store and fake Discord server for `add-discord` token inputs and binding, sole-guild selection, guild ambiguity, source creation, permission diagnostics, `sync-discord` source selection and no-argument aggregate behavior, `--full`/`--after`, and media backfill reporting.
2. Register the commands and share importer construction so manual and scheduled runs use the same configuration, token resolution, sync-run lifecycle, and cache/stat finalization.
3. Keep command output free of bot tokens and signed URLs.
4. Run `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd` and commit as `feat(cli): add Discord import commands`.

## Task 11: Daemon scheduling, account removal, and query integration

**Files:**
- Modify: `cmd/msgvault/cmd/serve.go`
- Modify: `cmd/msgvault/cmd/serve_test.go`
- Modify: `cmd/msgvault/cmd/remove_account.go`
- Modify: `cmd/msgvault/cmd/remove_account_test.go`
- Modify: `internal/api/cli_handlers.go`
- Modify: `internal/api/cli_handlers_test.go`
- Modify: `internal/query/text_models.go`
- Modify: `internal/query/text_models_test.go`

1. Write failing tests for stable per-guild scheduling, shared-importer dispatch, one guild failure not blocking later guilds, CLI API allowlisting, and Discord inclusion in known/text message types.
2. Write removal tests that resolve the source token before cascade deletion, preserve a shared token while another source references it, and delete the file only after the final reference disappears.
3. Add `discord` to scheduled-source discovery and `runScheduledSync`, preserving deterministic ordering and existing providers.
4. Allow the three Discord commands in the CLI API and classify attachment-producing commands consistently.
5. Run focused command/API/query tests and commit as `feat(discord): integrate scheduling and account lifecycle`.

## Task 12: User documentation, full verification, and requested automated review

**Files:**
- Create: `docs/usage/discord.md`
- Modify: the repository's usage navigation file
- Modify: `README.md` only if provider support is enumerated there

1. Document bot creation and least-privilege permissions, guild-only scope, the selfbot prohibition, sole-guild selection, token bindings, filters and thread inheritance, manual and scheduled sync, `--full`/`--after`, seven-day consistency boundary, reaction-summary limitation, media cap and signed-URL recovery limits, deletion semantics, and removal behavior.
2. Use placeholders only and do not name or quote any private source project.
3. Run `gofmt` on all changed Go files, `go vet -tags "fts5 sqlite_vec" ./...`, `make test`, `make docs-check`, and `git diff --check`.
4. Run the repository private-data scrub over every unpushed commit, docs, tests, fixtures, command output, and commit/PR text. Resolve every finding.
5. Commit docs and cleanup as `docs: add Discord import guide`.
6. If the user explicitly requests `$roborev-fix` in the active implementation session, invoke that workflow. Triage every reported finding against the design, fix valid issues with focused regression tests, record justified non-fixes through the workflow, and repeat its required verification.
7. Run the full verification set again after review fixes. Ensure `git status --short` is clean and every code-producing change is committed.

## Completion criteria

- Every design behavior has a named test, especially fixed-interval deletion comparison, partial-scan suppression, independent thread checkpoints, rate-limit waits, shared-token deletion, unknown system messages, and expiring attachment URLs.
- SQLite tests pass, PostgreSQL-compatible SQL is used, and no schema migration is introduced.
- The public diff, commit history, documentation, fixtures, and review messages pass the private-data scrub.
- Manual sync, scheduled sync, media backfill, and account removal all exercise the same production provider wiring.
