# Backlog Prompt: Account-Scoped Embedding Builds

- **Slug:** `msgvault_embed_account_scope`
- **Created:** 2026-08-10 22:25
- **Status:** implemented
- **Area:** `internal/vector`, `internal/store`, `cmd/msgvault/cmd` (embeddings), daemon embed job, API/MCP coverage

## Task

Add account scoping to vector embedding builds so an operator can restrict which
accounts' messages are sent to the embedding endpoint, via both:

1. CLI flags on `msgvault embeddings build` / `msgvault embeddings resume`
   (`--account`, repeatable; `--collection` for collection expansion), and
2. durable config (`[vector.embed.scope] accounts = [...]`) so the daemon's
   scheduled embed job honors the same restriction without operator flags.

## Motivation

- **Cost control.** Embedding a multi-decade, multi-account archive is the most
  expensive operation msgvault performs. Operators want to embed one account
  first (e.g. a personal mailbox) before paying to embed the rest.
- **Privacy.** Content from specific accounts must never leave for the
  embedding endpoint. A CLI-only flag is insufficient here: the daemon's
  scheduled embed job (`[vector.embed.schedule]`, `run_after_sync`) would still
  embed those accounts, so the restriction must be expressible in config.
- **Incremental rollout.** Build a scoped generation, validate search quality,
  then widen the scope with a full rebuild.

## Current state map (verified against the tree)

- Build scope today is **message-types only**:
  `internal/vector/build_scope.go` — `BuildScope{MessageTypes []string}` with
  normalization (lowercase/trim/dedupe/sort) and `Fingerprint()` → `"mt-..."`.
- Config: `internal/vector/config.go` — `EmbedScopeConfig{MessageTypes}` under
  `[vector.embed.scope]`; `Config.GenerationFingerprint()` (config.go:269)
  appends `:s<scopeFP>` when the scope fingerprint is non-empty. Scope is
  already part of generation identity — extend, don't reinvent.
- Scan/coverage SQL: `internal/store/embed_gen.go` —
  `ScanForEmbeddingScoped`, `CoverageCountsScoped`, `MissingCountScoped`,
  `countLiveMessagesScoped` all take `messageTypes []string` and build their
  predicate via `liveMessagesWhereWithMessageTypes` (embed_gen.go:317). The
  `messages` table has `source_id`; filtering by account means adding
  `AND source_id IN (...)`.
- Worker threading: `internal/vector/embed/worker.go:595` (`scanForEmbedding`)
  re-derives the scope from `deps.BuildScope.MessageTypes` and
  interface-asserts the scoped store method. Extend to carry source IDs.
- CLI entry: `cmd/msgvault/cmd/embed.go` (`runEmbeddingsBuild` →
  `runEmbeddingsBuildLocal` → `runEmbed`) and
  `cmd/msgvault/cmd/embed_vector.go:127` (`scope := cfg.Vector.Embed.Scope.BuildScope()`).
  Activation gate at embed_vector.go:173-188 activates a rebuild only when
  `CoverageCountsScoped` reports `remaining == 0` **within scope** — scoped
  generations activate correctly once scope carries source IDs.
- Account resolution already exists:
  `cmd/msgvault/cmd/account_scope.go` — `ResolveAccountFlag`,
  `ResolveCollectionFlag`, `Scope.SourceIDs()` (note `AdditionalSourceIDs`: one
  identifier can map to several source rows). Use these; do not write a new
  resolver.
- Daemon forwarding is automatic: `daemonCLIArgsFromCobra`
  (`cmd/msgvault/cmd/daemon_cli_http.go:96`) visits changed local flags, and
  `appendDaemonCLIFlag` (daemon_cli_http.go:149) handles `pflag.SliceValue`, so
  a repeatable `--account` string flag forwards to the daemon-spawned
  subprocess with no extra plumbing. Verify with a test.
- Search side needs no changes for correctness: vector search already filters
  hits by account at query time (`internal/vector/sqlitevec/backend.go:1128`
  `inClause("m.source_id", f.SourceIDs)`; `internal/vector/hybrid/filter.go:99`
  maps `AccountIDs` → `SourceIDs`). Out-of-scope accounts simply have no
  vectors; BM25/hybrid degrade gracefully.
- Consumers of the configured scope to keep consistent:
  `cmd/msgvault/cmd/embeddings_manage.go:74,98,639,678` (list/coverage/job
  wiring), `cmd/msgvault/cmd/serve_vector_init.go` (~line 178, daemon
  `EmbedJob`), `internal/api/search_coverage.go:78`
  (`semanticCoverageContext(ctx, cfg.Embed.Scope.BuildScope())`) and
  `semanticCoverageContext` itself (search_coverage.go:317),
  `internal/api/handlers.go:1114`, `internal/mcp/handlers.go:914`.

## Design

### D1 — Account scope is part of generation identity (chosen)

Fold the account scope into `vector.BuildScope` and therefore into
`Config.GenerationFingerprint()`, exactly like `message_types` today. A
scoped build creates/resumes a scoped generation; the existing
fingerprint-mismatch machinery (`pickEmbedGeneration`,
`vector.ResolveActiveForFingerprint`) prevents silently mixing scopes in one
index and forces an explicit `--full-rebuild` when the scope changes.

Rejected alternative: a run-time-only scan filter that leaves the generation
"full corpus". That breaks the activation gate (a scoped full rebuild would
never reach `remaining == 0`), misreports coverage, and cannot express the
privacy requirement (the daemon would still embed excluded accounts).

### D2 — Extend `vector.BuildScope`

```go
type BuildScope struct {
    MessageTypes []string
    SourceIDs    []int64
}
```

- `NewBuildScope(messageTypes []string, sourceIDs []int64)` normalizes both:
  source IDs are deduped, sorted ascending, non-positive IDs dropped.
- `Fingerprint()` gains a source segment, e.g. `"mt-email,teams:src-3,7,11"`,
  keeping `mt-` first for readability. Source IDs are DB-local integers;
  sorting makes the fingerprint deterministic per archive. Document that
  deleting and re-adding an account changes its source ID and therefore the
  fingerprint (forces a rebuild — acceptable under the existing "policy change
  stales the index" contract).
- Add `ContainsSource(sourceID int64)` for symmetry with `ContainsMessageType`.

### D3 — Config: `[vector.embed.scope] accounts`

`EmbedScopeConfig` gains `Accounts []string `toml:"accounts"`` — account
identifiers (same syntax `--account` accepts), not source IDs, because config
must survive archive rebuilds that renumber sources.

Resolution to source IDs happens once at startup against the open store, in
the `cmd` layer (both `runEmbed` and the daemon's vector wiring in
`serve_vector.go` / `serve_vector_init.go` live there), reusing
`ResolveAccountFlag` semantics. Unknown identifiers are a hard startup error,
not a silent skip.

Because `internal/api` and `internal/mcp` currently call
`cfg.Embed.Scope.BuildScope()` directly, inject the **resolved** scope into
the server (e.g. via `api.ServerOptions`, set during daemon wiring) so
coverage endpoints, the CLI, and the embed job all share one scope value.
Audit every `BuildScope()` call site listed in the map above.

### D4 — CLI flags

- `embeddings build` and `embeddings resume` gain:
  - `--account <id>` (repeatable `StringArrayVar`; resolve each with
    `ResolveAccountFlag`, unioning `Scope.SourceIDs()`),
  - `--collection <name>` (repeatable; expand via `ResolveCollectionFlag`).
- When any scope flag is present it **replaces** the configured scope for that
  run (explicit operator intent); the resulting fingerprint difference is
  surfaced by the existing mismatch errors — improve those messages to name
  the scope difference, not just the fingerprint strings.
- With no flags, behavior is unchanged: config scope applies.
- `embeddings list` / coverage output prints the source-ID segment of a
  generation's scope alongside the message types it already shows.
- The deprecated `build-embeddings` alias shares `newEmbeddingsBuildCmd`, so
  it inherits the flags for free.

### D5 — Store and worker plumbing

- `internal/store/embed_gen.go`: extend the scoped functions to carry source
  IDs — either a new `sourceIDs []int64` parameter on
  `ScanForEmbeddingScoped`, `CoverageCountsScoped`, `MissingCountScoped`,
  `countLiveMessagesScoped`, or a small scope struct; the repo owns all
  callers, so pick the form that reads best and update every call site.
  Replace `liveMessagesWhereWithMessageTypes` with a scope-aware builder that
  appends `AND source_id IN (?...)`.
- `internal/vector/embed/worker.go:595` `scanForEmbedding`: thread
  `BuildScope.SourceIDs` through the interface assertion (extend the asserted
  signature or assert a new method); keep the unscoped fast path when the
  scope is empty.
- Audit the embed_gen backfill paths (`internal/vector/embed/backfill_test.go`,
  `internal/vector/sqlitevec/backfill*.go`) for scope handling; extend or
  explicitly document them.
- The daemon `EmbedJob` (`serve_vector_init.go`, fingerprint near line 178)
  must receive the resolved scope so scheduled embeds and CLI embeds target
  the same generation.

## Edge cases and invariants

- **Scoped activation gate.** A scoped `--full-rebuild` activates when
  in-scope `remaining == 0`; out-of-scope messages keep `embed_gen` untouched
  and are never scanned. Add a test proving out-of-scope rows are neither
  embedded nor stamped.
- **Empty scope result.** `--account` resolving to a source with zero live
  messages yields live=0/missing=0 and immediate activation; print a clear
  "scope matched 0 messages" line instead of silently succeeding.
- **Fingerprint instability.** Account delete/re-add or a restored backup can
  renumber source IDs → new fingerprint → rebuild prompt. Note this in the
  flag/config help text.
- **Scope widening/narrowing is a rebuild.** Changing the account set produces
  a new fingerprint; there is no incremental "un-embed" — `embeddings retire`
  plus a rebuild is the escape hatch.
- **Message-types × accounts compose.** Both filters apply (AND semantics);
  the fingerprint carries both segments.
- **No cross-package leak.** `internal/vector` must not import
  `internal/store`; account-identifier → source-ID resolution stays in the
  `cmd` layer / server wiring.

## Non-goals

- Per-account *generations* searched selectively (one index, scoped at build
  time; query-time account filters already exist).
- Removing already-embedded vectors for an account (retire + rebuild covers it).
- TUI/web UI scope pickers (read-only coverage display updates only).
- Changing the search ranking path.

## Implementation plan

1. `internal/vector/build_scope.go`: add `SourceIDs`, normalization,
   fingerprint segment, `ContainsSource`; update `build_scope` tests.
2. `internal/vector/config.go`: `EmbedScopeConfig.Accounts`; extend
   `GenerationFingerprint` tests for the combined scope.
3. `internal/store/embed_gen.go`: scope-aware WHERE builder + extended scoped
   scan/coverage signatures; update all callers (store tests, cmd, api, mcp).
4. `internal/vector/embed/worker.go`: thread source IDs through
   `scanForEmbedding`; extend worker tests.
5. `cmd/msgvault/cmd/embed.go` / `embed_vector.go`: register `--account` /
   `--collection` on build+resume, resolve against the opened store, fold into
   the scope passed to generation selection, coverage, and the worker; improve
   fingerprint-mismatch error text to mention scope.
6. Daemon wiring (`serve_vector.go`, `serve_vector_init.go`,
   `embeddings_manage.go`): resolve configured accounts once, share the
   resolved scope with the embed job and the API/MCP servers.
7. `internal/api` / `internal/mcp`: consume the injected resolved scope in
   coverage/validation paths.
8. Docs: `docs/cli-reference.md` (new flags), the vector-search usage page
   (`docs/usage/`, verify exact file), and `docs/internal/` design note if the
   change grows beyond the above.

## Testing requirements (repo rules — AGENTS.md / CLAUDE.md)

- All Go tests use `github.com/stretchr/testify` (`assert.X` / `require.X`;
  equality is `(t, want, got)`). Never `t.Errorf`/`t.Fatal*`.
- Run tests with build tags: `make test` (adds `-tags "fts5 sqlite_vec"`), plus
  the pg-tagged path if PG-facing code changed (`PG_TEST_TAGS := fts5
  sqlite_vec pgvector`).
- No fake TDD: exercise the real store against a real SQLite DB
  (`internal/testutil` builders, e.g. `NewTestStore`) and assert on actual
  scan/coverage results — do not stub the store and assert on arguments.
- Minimum coverage:
  - `BuildScope` normalization + fingerprint with source IDs (unit).
  - `ScanForEmbeddingScoped` / `CoverageCountsScoped` with a source filter
    against a real DB with two sources (integration).
  - Worker skips out-of-scope messages and still stamps in-scope ones.
  - CLI: `embeddings build --account X --full-rebuild` on a two-account
    fixture activates with only X's messages embedded; a second run with a
    different account set fails with the scope-naming mismatch error.
  - Daemon forwarding: `--account` survives `daemonCLIArgsFromCobra` /
    `appendDaemonCLIFlag` (slice-value path).

## Acceptance criteria

- `msgvault embeddings build --account A [--account B] [--collection C]`
  builds/activates a generation covering exactly the resolved sources ×
  configured message types; out-of-scope messages are untouched.
- `[vector.embed.scope] accounts = [...]` makes daemon-scheduled embeds obey
  the same scope; unknown identifiers fail startup loudly.
- Scope appears in the generation fingerprint; changing scope requires
  `--full-rebuild` and the error message says why.
- Coverage reporting (`embeddings list`, API/MCP coverage) reflects the scope.
- `make test` and `make lint` pass; commit the change at the end of the turn
  (repo convention: commit without asking).
