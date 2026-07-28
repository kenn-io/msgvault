# Relationships Analytical Index Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace archive-wide request-time people, domain, and relationship
aggregation with exact version-15 Parquet read models while bounding every
production DuckDB connection and derived-cache refresh.

**Architecture:** A new `internal/identityindex` package owns shared
classification rules, dataset names, and set-wise derivation SQL. The cache
builder writes scalar message facts and origin-aware participant edges, then
builds canonical directories and rollups from those narrow datasets. The
daemon reads rollups for unfiltered requests and recomputes filtered results
from the narrow facts; identity and conversation-membership refreshes run only
in a bounded `build-cache --derived-only` child process.

**Tech Stack:** Go, DuckDB through `github.com/duckdb/duckdb-go/v2`, Parquet,
SQLite, Cobra, Huma HTTP handlers, `testify`, Unix/Windows process-liveness
helpers.

## Global Constraints

- The authoritative design is
  `docs/superpowers/specs/2026-07-27-relationships-index-design.md`.
- The cache schema advances from 14 to 15; version-14 caches always require a
  full rebuild and `--derived-only` must refuse them independently.
- PostgreSQL behavior is unchanged: it has no Parquet cache and does not gain
  these analytical endpoints.
- The seven migrated endpoints are relationships ranking, people
  search/detail/summary, and domain search/detail/summary. Explore, timelines,
  and file searches retain their existing views and SQL.
- All filtered semantics apply at message level before chat grouping. Chat
  grouping uses the current `max`/`arg_max` rules exactly.
- Direct and conversation participant edges remain distinguishable through
  filtering and fan-out.
- Canonical direct-edge deduplication uses `bool_or(is_author)`; an incoming
  authored/co-recipient cluster receives exactly one received unit.
- Interactive DuckDB uses `memory_limit='512MB'`, at most four threads, one
  query slot, a 2 GB spill limit, and a daemon-owned temp directory.
- Full and derived cache builders use the reviewed implementation amendment:
  `memory_limit='1536MB'`, at most two threads, and an 8 GB spill limit in
  staging. The production-scale gate found the initially proposed 2 GB policy
  could exceed the 3 GiB process budget with three threads; do not restore
  that outlier-prone configuration without new evidence and a reviewed design
  amendment.
- Production identity refreshes never open DuckDB inside the daemon.
- New and modified Go tests use `testify`; every `go test` command includes
  `-tags "fts5 sqlite_vec"`.
- Do not add SQL-source grep tests or command-stub tests. Exercise the real
  builder, query engine, cache publication, or HTTP path.
- After Go changes, run `go fmt ./...`, `go vet -tags "fts5 sqlite_vec" ./...`,
  and the relevant tagged tests before committing.
- Before every commit, use the `kenn:commit` skill.

---

## File and responsibility map

- `internal/identityindex/schema.go`: dataset names, modality bits, and shared
  message/chat classification SQL.
- `internal/identityindex/build.go`: full, incremental, and derived-only index
  build orchestration over staged/committed Parquet.
- `internal/identityindex/build_sql.go`: set-wise SQL for facts, edges,
  directory, logical units, and rollups.
- `internal/identityindex/validate.go`: schema, uniqueness, anchor,
  referential, and relationship-daily validation.
- `internal/identityindex/build_test.go`: real Parquet derivation and semantic
  fixtures independent of HTTP shaping.
- `internal/duckdbutil/connection.go`: apply a complete DuckDB resource policy
  to a one-connection `database/sql` handle.
- `internal/duckdbutil/connection_test.go`: query effective DuckDB settings.
- `internal/query/cache_state.go`: version-15 marker fields, revision inputs,
  readiness, and required datasets.
- `internal/query/duckdb.go`: configured daemon engine, one-slot admission,
  and registered version-15 views.
- `internal/query/duckdb_spill.go`,
  `internal/query/duckdb_spill_unix.go`,
  `internal/query/duckdb_spill_windows.go`: daemon spill lifecycle and
  cross-platform PID liveness.
- `cmd/msgvault/cmd/build_cache.go`: invoke identity-index derivation after
  base exports and dispatch full/incremental/derived modes.
- `cmd/msgvault/cmd/cache_publication.go`: explicit per-mode append/replace
  publication plans.
- `cmd/msgvault/cmd/cache_staleness.go`: conversation-participant fingerprint
  drift.
- `internal/cacheops/identity_refresh.go`: stage and atomically publish the
  complete derived dataset set from a child process.
- `internal/cacheops/stats.go`: marker-only statistics reads.
- `internal/query/identity_activity.go`: origin-aware filtered logical-unit
  CTEs used only by the migrated endpoints.
- `internal/query/people.go`: directory/rollup people and domain queries.
- `internal/query/relationships.go`: rollup-backed default ranking and
  narrow-fact filtered ranking.
- `cmd/msgvault/cmd/relationships_index_http_e2e_test.go`: production
  builder-to-daemon HTTP-path guard for all seven migrated endpoints.
- `internal/api/relationships_test.go`: production HTTP-path guard and cursor
  behavior for relationships ranking.
- `internal/query/identity_index_benchmark_test.go`: synthetic scale fixture
  and cold query benchmarks.
- `cmd/msgvault/cmd/build_cache_test.go`: cache-build wall-time/RSS fixture and
  incremental publication checks.

---

### Task 1: Define relationship-index schema primitives and marker fields

**Files:**

- Create: `internal/identityindex/schema.go`
- Modify: `internal/query/cache_state.go`
- Modify: `internal/query/cache_state_test.go`
- Modify: `internal/query/shared.go`
- Modify: `internal/query/text_models.go`
- Modify: `internal/query/entry_key.go`
- Modify: `internal/query/explore.go`
- Modify: `internal/query/explore_test.go`

**Interfaces:**

- Produces:
  `identityindex.RequiredDatasets []string`,
  `identityindex.IsChat(messageType, conversationType string) bool`,
  `identityindex.IsChatSQL(messageType, conversationType string) string`,
  `identityindex.EntryKindSQL(messageType string) string`,
  `identityindex.CacheStatsSummary`, and new `query.CacheSyncState` fields.
- Consumed by every later task.

- [ ] **Step 1: Write failing cache-state and classification tests**

Add assertions covering the eight-dataset catalog, anchor revision drift,
nested stats JSON, and unchanged chat classification:

```go
func TestRelationshipIndexDatasetCatalog(t *testing.T) {
	assert.ElementsMatch(t, []string{
		"identity_entry_facts", "identity_direct_edges",
		"identity_conversation_edges", "identity_directory",
		"identity_rollups", "domain_rollups",
		"relationship_rollups", "relationship_daily",
	}, identityindex.RequiredDatasets)
}

func TestCacheRevisionIncludesRelationshipAnchor(t *testing.T) {
	state := CacheSyncState{
		SchemaVersion: 15,
		PublishedAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		RelationshipAnchorDate: "2026-07-27",
	}
	changed := state
	changed.RelationshipAnchorDate = "2026-07-28"
	assert.NotEqual(t, state.Revision(), changed.Revision())
}
```

- [ ] **Step 2: Run the focused tests and verify the missing contracts**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query -run 'TestRelationshipIndexDatasetCatalog|TestCacheRevisionIncludesRelationshipAnchor|TestIsChat'
```

Expected: FAIL because the package and marker fields are absent.

- [ ] **Step 3: Add shared dataset, classification, and marker types**

Create `internal/identityindex/schema.go` with these stable names and masks:

```go
package identityindex

const (
	DatasetEntryFacts        = "identity_entry_facts"
	DatasetDirectEdges       = "identity_direct_edges"
	DatasetConversationEdges = "identity_conversation_edges"
	DatasetDirectory         = "identity_directory"
	DatasetRollups           = "identity_rollups"
	DatasetDomainRollups     = "domain_rollups"
	DatasetRelationships     = "relationship_rollups"
	DatasetRelationshipDaily = "relationship_daily"

	ModalityEmail   uint8 = 1
	ModalityChat    uint8 = 2
	ModalityMeeting uint8 = 4
)

var (
	TextMessageTypes = []string{
		"whatsapp", "imessage", "sms", "mms", "rcs",
		"google_voice_text", "teams", "discord", "beeper", "fbmessenger",
	}
	ChatFallbackMessageTypes = []string{"", "chat", "text"}
	ChatConversationTypes = []string{"direct_chat", "group_chat", "channel", "chat"}
)

var RequiredDatasets = []string{
	DatasetEntryFacts,
	DatasetDirectEdges,
	DatasetConversationEdges,
	DatasetDirectory,
	DatasetRollups,
	DatasetDomainRollups,
	DatasetRelationships,
	DatasetRelationshipDaily,
}

func IsChatSQL(messageType, conversationType string) string {
	return "lower(" + messageType + ") IN (" + quotedList(TextMessageTypes) + ")" +
		" OR (lower(" + messageType + ") IN (" + quotedList(ChatFallbackMessageTypes) + ")" +
		" AND lower(" + conversationType + ") IN (" + quotedList(ChatConversationTypes) + "))"
}

func IsChat(messageType, conversationType string) bool {
	messageType = strings.ToLower(messageType)
	if slices.Contains(TextMessageTypes, messageType) {
		return true
	}
	return slices.Contains(ChatFallbackMessageTypes, messageType) &&
		slices.Contains(ChatConversationTypes, strings.ToLower(conversationType))
}

func EntryKindSQL(messageType string) string {
	return "CASE WHEN lower(" + messageType + ") = 'email' OR " + messageType + " = '' THEN 'email'" +
		" WHEN lower(" + messageType + ") = 'calendar_event' THEN 'event'" +
		" WHEN lower(" + messageType + ") IN ('meeting_transcript','meeting_note','meeting_minutes') THEN 'meeting'" +
		" ELSE 'item' END"
}
```

Implement `IsChat` with the same three exported slices. Make legacy
`query.IsChatEntry` and `sqlIsChatPredicate` delegate to
`identityindex.IsChat`/`identityindex.IsChatSQL`, and derive
`TextMessageTypeSQLList` from `identityindex.TextMessageTypes`, so Go and SQL
classification have one definition.

Add marker fields:

```go
// In package identityindex, so both query and the builder can use it without
// an import cycle.
type CacheStatsSummary struct {
	TotalMessages       int64  `json:"total_messages"`
	Sources             int64  `json:"sources"`
	UniqueSenders       int64  `json:"unique_senders"`
	UniqueDomains       int64  `json:"unique_domains"`
	MinYear             *int64 `json:"min_year,omitempty"`
	MaxYear             *int64 `json:"max_year,omitempty"`
	TotalSizeBytes      int64  `json:"total_size_bytes"`
	AttachmentSizeBytes int64  `json:"attachment_size_bytes"`
}

type CacheSyncState struct {
	// existing fields remain
	RelationshipAnchorDate              string            `json:"relationship_anchor_date,omitempty"`
	ConversationParticipantsFingerprint string            `json:"conversation_participants_fingerprint,omitempty"`
	Stats                               identityindex.CacheStatsSummary `json:"stats"`
}
```

Include `RelationshipAnchorDate` in `CacheSyncState.Revision()` and keep
`DatasetFingerprint` excluded as today. Deliberately leave
`CacheSchemaVersion` at 14 and do not append the new required directories yet:
Task 4 activates version 15 in the same commit that makes the builder capable
of publishing every required shard, so no intermediate commit advertises an
unbuildable cache schema.

- [ ] **Step 4: Run the package tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/identityindex/schema.go internal/query/cache_state.go internal/query/cache_state_test.go internal/query/shared.go internal/query/text_models.go internal/query/entry_key.go internal/query/explore.go internal/query/explore_test.go
git commit -m "feat: define relationship index contracts"
```

---

### Task 2: Apply bounded DuckDB policies and manage daemon spill

**Files:**

- Create: `internal/duckdbutil/connection.go`
- Create: `internal/duckdbutil/connection_test.go`
- Create: `internal/query/duckdb_spill.go`
- Create: `internal/query/duckdb_spill_unix.go`
- Create: `internal/query/duckdb_spill_windows.go`
- Create: `internal/query/duckdb_spill_test.go`
- Modify: `internal/query/duckdb.go`
- Modify: `internal/query/duckdb_test.go`
- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/serve.go`
- Modify: `cmd/msgvault/cmd/serve_test.go`

**Interfaces:**

- Produces:
  `duckdbutil.Policy`,
  `duckdbutil.Open(context.Context, Policy) (*sql.DB, error)`,
  and `query.DuckDBOptions.TempDirectory`.
- The full/derived builders consume `duckdbutil.BuilderPolicy`; the daemon
  consumes `duckdbutil.InteractivePolicy`.

- [ ] **Step 1: Write failing effective-setting tests**

Create production-path tests that open both policies and query DuckDB:

```go
func TestInteractivePolicySettings(t *testing.T) {
	policy := Policy{
		MemoryLimit: "512MB", Threads: 4,
		TempDirectory: t.TempDir(), MaxTempDirectorySize: "2GB",
	}
	db, err := Open(context.Background(), policy)
	require.NoError(t, err)
	defer db.Close()

	var threads int
	var memory, temp, spill string
	require.NoError(t, db.QueryRow(`
		SELECT current_setting('threads'),
		       current_setting('memory_limit'),
		       current_setting('temp_directory'),
		       current_setting('max_temp_directory_size')
	`).Scan(&threads, &memory, &temp, &spill))
	assert.Equal(t, 4, threads)
	assert.Equal(t, filepath.Clean(policy.TempDirectory), filepath.Clean(temp))
}
```

Use numeric byte comparisons for memory/spill because DuckDB may normalize
display units. Add an engine test asserting `cap=1` behavior by holding the
first `acquireQuerySlot` and verifying a second context times out.

- [ ] **Step 2: Run the tests and verify they fail**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/duckdbutil ./internal/query -run 'TestInteractivePolicySettings|TestDuckDBEngineUsesOneQuerySlot|TestDaemonDuckDBSpill'
```

Expected: FAIL because the policy package and spill lifecycle do not exist and
the engine still admits two heavy queries.

- [ ] **Step 3: Implement the shared connection policy**

Implement:

```go
type Policy struct {
	MemoryLimit         string
	Threads             int
	TempDirectory       string
	MaxTempDirectorySize string
}

func Open(ctx context.Context, policy Policy) (*sql.DB, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := Apply(ctx, db, policy); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
```

`Apply` must validate `Threads > 0`, non-empty limits, and an absolute temp
path; create the directory with `0700`; SQL-quote the path by doubling single
quotes; then issue:

```sql
SET memory_limit = '<validated limit>';
SET threads = <validated integer>;
SET preserve_insertion_order = false;
SET temp_directory = '<quoted absolute path>';
SET max_temp_directory_size = '<validated limit>';
```

Provide constructors:

```go
func InteractivePolicy(temp string) Policy
func BuilderPolicy(temp string) Policy
```

Their thread counts are `min(runtime.GOMAXPROCS(0), 4)` and
`min(runtime.GOMAXPROCS(0), 8)` respectively.

- [ ] **Step 4: Implement daemon spill ownership and liveness checks**

`prepareDaemonSpillDir(home string)` creates
`<home>/tmp/duckdb-query-<pid>`. Before creating it, scan only `<home>/tmp`;
accept names matching `^duckdb-query-([1-9][0-9]*)$`; remove a directory only
when `processAlive(pid)` is false. On Unix:

```go
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
```

On Windows, open the process with
`windows.PROCESS_QUERY_LIMITED_INFORMATION`, call `windows.GetExitCodeProcess`,
and compare with `windows.STILL_ACTIVE`. Malformed names, permission failures,
and live PIDs remain untouched. `DuckDBEngine.Close` closes DuckDB before
removing only its owned directory.

- [ ] **Step 5: Route production opens through the policies**

Change `NewDuckDBEngine` to call `duckdbutil.Open` and set:

```go
type DuckDBOptions struct {
	DisableSQLiteScanner bool
	TempDirectory        string
	OwnTempDirectory     bool
}
```

Remove `relMemo` and `relationshipsQueryRuns` only in Task 7; in this task,
change `duckDBQueryConcurrency` to 1. In `openDaemonDuckDBEngine`, prepare the
home spill directory and pass it through the options. In `buildCacheLocked`,
create `<staging.root>/duckdb-tmp` before opening/attaching SQLite and apply the
builder policy to that same connection.

- [ ] **Step 6: Run resource and daemon tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/duckdbutil ./internal/query ./cmd/msgvault/cmd -run 'DuckDB|Spill|QuerySlot|OpenDaemon'
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/duckdbutil internal/query/duckdb.go internal/query/duckdb_spill.go internal/query/duckdb_spill_unix.go internal/query/duckdb_spill_windows.go internal/query/duckdb_spill_test.go internal/query/duckdb_test.go cmd/msgvault/cmd/build_cache.go cmd/msgvault/cmd/serve.go cmd/msgvault/cmd/serve_test.go
git commit -m "fix: bound production DuckDB resources"
```

---

### Task 3: Build scalar facts, origin-aware edges, and the identity directory

**Files:**

- Create: `internal/identityindex/build.go`
- Create: `internal/identityindex/build_sql.go`
- Create: `internal/identityindex/build_test.go`
- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/cache_publication.go`
- Modify: `cmd/msgvault/cmd/cache_publication_test.go`
- Modify: `cmd/msgvault/cmd/build_cache_test.go`
- Modify: `cmd/msgvault/cmd/constants.go`
- Modify: `internal/query/testfixtures_test.go`

**Interfaces:**

- Produces:

```go
type Mode uint8
const (
	ModeFull Mode = iota
	ModeIncremental
	ModeDerivedOnly
)

type BuildOptions struct {
	Mode          Mode
	CommittedRoot string
	StagedBaseRoot string
	OutputRoot    string
	AnchorDate    time.Time
}

type BuildResult struct {
	ConversationParticipantsFingerprint string
	Stats CacheStatsSummary
}

func Build(ctx context.Context, db *sql.DB, opts BuildOptions) (BuildResult, error)
```

- Task 4 extends `Build` with rollups without changing its public signature.

- [ ] **Step 1: Write failing real-Parquet derivation tests**

Build a small base Parquet tree containing:

- one non-chat message with a sender and two recipient rows for the same
  participant;
- a separate conversation-only participant;
- two linked aliases with different names/identifiers;
- a zero-message archive.

Assert exact schemas and rows:

```go
assert.Equal(t, int64(1), scalarFactCount)
assert.Equal(t, int64(2), directEdgeCount)
assert.True(t, authoredEdge.IsAuthor)
assert.Equal(t, int64(1), conversationEdgeCount)
assert.Equal(t, canonicalID, directory.CanonicalID)
assert.Equal(t, []int64{canonicalID, aliasID}, directory.MemberIDs)
```

Verify the conversation edge exists for a non-chat conversation and no
conversation edge is copied into `identity_direct_edges`.

- [ ] **Step 2: Run the derivation tests and verify they fail**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/identityindex -run 'TestBuildFactsEdgesAndDirectory|TestBuildEmptySchemas'
```

Expected: FAIL because `Build` is not implemented.

- [ ] **Step 3: Implement mode-aware input resolution**

`Build` validates absolute roots, requires `CommittedRoot` for incremental and
derived modes, and resolves inputs as follows:

```go
func (b *builder) base(dataset string) string {
	if b.mode == ModeDerivedOnly && !b.hasStaged(dataset) {
		return b.committed(dataset)
	}
	return b.staged(dataset)
}

func (b *builder) allFacts() string {
	if b.mode == ModeFull || b.mode == ModeDerivedOnly {
		return b.outputOrCommitted(identityindex.DatasetEntryFacts)
	}
	return parquetUnion(b.committed(DatasetEntryFacts), b.output(DatasetEntryFacts))
}
```

Do not silently use a missing path. Return an error naming the dataset and
mode. All output directories receive a schema-bearing Parquet file even when
their row count is zero.

- [ ] **Step 4: Implement set-wise fact and edge SQL**

The scalar fact `COPY` reads staged `messages`, `sources`, and `conversations`.
Use the shared classification expressions:

```sql
SELECT
    m.id::BIGINT AS message_id,
    m.conversation_id::BIGINT AS conversation_id,
    m.source_id::BIGINT AS source_id,
    s.source_type::VARCHAR AS source_type,
    m.sent_at::TIMESTAMP AS occurred_at,
    m.message_type::VARCHAR AS message_type,
    c.conversation_type::VARCHAR AS conversation_type,
    <identityindex.EntryKindSQL> AS entry_kind,
    <identityindex.IsChatSQL> AS is_chat,
    m.is_from_me::BOOLEAN AS is_from_me,
    m.has_attachments::BOOLEAN AS has_attachments,
    m.attachment_count::INTEGER AS attachment_count,
    (m.deleted_from_source_at IS NOT NULL) AS deleted_from_source,
    year(m.sent_at)::SMALLINT AS occurred_year
FROM read_parquet(<messages>, hive_partitioning=true) m
JOIN read_parquet(<sources>) s ON s.id = m.source_id
LEFT JOIN read_parquet(<conversations>) c ON c.id = m.conversation_id
```

The direct edge `COPY` must use one grouped union:

```sql
WITH raw AS (
    SELECT mr.message_id, mr.participant_id,
           false AS is_sender, mr.recipient_type = 'from' AS is_author
    FROM read_parquet(<message_recipients>) mr
    UNION ALL
    SELECT m.id, m.sender_id, true, true
    FROM read_parquet(<messages>, hive_partitioning=true) m
    WHERE m.sender_id IS NOT NULL
)
SELECT r.message_id, f.occurred_year, r.participant_id,
       lower(coalesce(p.domain, '')) AS participant_domain,
       bool_or(r.is_sender) AS is_sender,
       bool_or(r.is_author) AS is_author
FROM raw r
JOIN <new facts> f ON f.message_id = r.message_id
JOIN read_parquet(<participants>) p ON p.id = r.participant_id
GROUP BY r.message_id, f.occurred_year, r.participant_id, participant_domain
```

Conversation edges read every staged `conversation_participants` row and join
participants for domain; they never filter on chat type.

- [ ] **Step 5: Implement set-wise identity-directory and cache-stat SQL**

Build `canon(participant_id, canonical_id)` with
`coalesce(cluster.canonical_id, participant.id)`. Aggregate member IDs and
normalized search values before joining labels. The final query has no
correlated scan over activity facts:

```sql
SELECT c.canonical_id,
       coalesce(named.display_name, fallback.display_label) AS display_label,
       named.display_name IS NULL AS partial_label,
       members.member_ids,
       searches.search_values,
       bool_or(o.participant_id IS NOT NULL) AS is_owner
FROM canon c
JOIN grouped_members members USING (canonical_id)
JOIN grouped_searches searches USING (canonical_id)
LEFT JOIN best_named_member named USING (canonical_id)
JOIN deterministic_fallback fallback USING (canonical_id)
LEFT JOIN owner_participants o ON o.participant_id = c.participant_id
GROUP BY c.canonical_id, named.display_name, fallback.display_label,
         members.member_ids, searches.search_values
```

`best_named_member` uses the smallest participant ID with a non-empty trimmed
display name. Fallback ordering is phone, email, primary identifier, remaining
identifier `(type,value)`, then `Unknown person #<canonical_id>`. Search values
are distinct lower-cased non-empty display names, emails, phones, identifier
values, and identifier display values.

In the same bounded connection, compute `CacheStatsSummary` over the complete
post-publication base population: staged replacements plus the
committed/staged union for append datasets. Preserve the current definitions
for `recipient_type='from'` unique sender/domain counts, year bounds,
message-size sum, and attachment-size sum. Return it in `BuildResult.Stats`.

- [ ] **Step 6: Integrate full and incremental publication**

After base exports complete, call:

```go
derived, err := identityindex.Build(ctx, exportDB, identityindex.BuildOptions{
	Mode: func() identityindex.Mode {
		if replaceAll {
			return identityindex.ModeFull
		}
		return identityindex.ModeIncremental
	}(),
	CommittedRoot: analyticsDir,
	StagedBaseRoot: staging.root,
	OutputRoot: staging.root,
	AnchorDate: anchorDate,
})
```

Stamp `state.Stats = derived.Stats` before publication. Incremental builds
must compute totals across committed plus staged rows, not only the delta.

Replace the `replaceAll bool` publication decision with:

```go
type cachePublishPlan struct {
	Append  map[string]bool
	Replace map[string]bool
}
```

Full mode replaces everything. Incremental mode appends messages,
message-recipients, message-labels, attachments, entry facts, and direct
edges; it replaces all other base datasets plus conversation edges and the
directory. Task 4 adds every rollup dataset to the replacement set. Preserve
the existing destination-collision check for every appended shard.

- [ ] **Step 7: Add production-derived fixtures without activating schema 15**

Add matching `tableIdentity*` aliases in `cmd/msgvault/cmd/constants.go`.
Teach `cachePublishPlan` to include explicitly named fact, edge, and directory
datasets even though they are not yet in `query.RequiredParquetDirs`.

After `TestDataBuilder` writes base Parquet, invoke
`identityindex.Build(ModeFull)` into the same temporary analytics tree. Delete
any hand-written version-15 fixture tables so query tests use the production
schemas and derivation. Keep `CacheSchemaVersion` at 14 in this task: Task 4
activates version 15 only after every rollup is also built, ensuring an
intermediate cache is forced through a full rebuild after the complete schema
lands.

- [ ] **Step 8: Run builder, publication, and query fixture tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/identityindex ./cmd/msgvault/cmd ./internal/query -run 'Identity|BuildCache|Publication|TestSearchPeople|TestRelationships'
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/identityindex cmd/msgvault/cmd/build_cache.go cmd/msgvault/cmd/build_cache_test.go cmd/msgvault/cmd/cache_publication.go cmd/msgvault/cmd/cache_publication_test.go cmd/msgvault/cmd/constants.go internal/query/testfixtures_test.go
git commit -m "feat: build origin-aware identity facts"
```

---

### Task 4: Derive exact people, domain, and relationship rollups

**Files:**

- Modify: `internal/identityindex/build.go`
- Modify: `internal/identityindex/build_sql.go`
- Create: `internal/identityindex/validate.go`
- Modify: `internal/identityindex/build_test.go`
- Modify: `cmd/msgvault/cmd/build_cache_test.go`
- Modify: `internal/query/cache_state.go`
- Modify: `internal/query/cache_state_test.go`
- Modify: `internal/query/duckdb.go`
- Modify: `internal/query/testfixtures_test.go`

**Interfaces:**

- Extends `identityindex.Build` to produce the five derived aggregate
  datasets and validate all cross-dataset invariants.
- Produces the shared filtered/build-time SQL interface:

```go
type ActivityPaths struct {
	Facts, DirectEdges, ConversationEdges, Directory, Clusters, Owners string
}

func LogicalActivitySQL(paths ActivityPaths, filterSQL string) string
```

`LogicalActivitySQL` emits `canonical_message_edges`, `logical_units`,
`logical_people`, and `logical_domains`; builder rollups pass `"true"` while
filtered query paths pass validated placeholder SQL.

- [ ] **Step 1: Write failing semantic-equivalence fixtures**

Add table-driven cases for:

- non-chat conversation membership: participant filter yes, people fan-out no,
  domain fan-out yes;
- chat membership: both edge origins fan out;
- equal-timestamp chat messages: greatest `message_id` controls direction;
- filtering out the newest chat message: the newest remaining message controls
  direction and `occurred_at`;
- authored alias linked to a co-recipient alias: one received unit;
- owner-cluster exclusion;
- future sent/meeting entries contributing raw counts, modality, and
  full-precision `last_at`;
- an identity with no qualifying interaction producing no relationship row;
- every future row having a corresponding relationship row.

The authorship assertion must be:

```go
assert.Equal(t, int64(1), row.ReceivedUnits)
assert.Equal(t, int64(1), relationshipRowCount)
```

- [ ] **Step 2: Run the focused tests and verify semantic failures**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/identityindex -run 'TestLogical|TestRollup|TestDaily|TestAuthoredAlias'
```

Expected: FAIL because aggregate datasets are still empty.

- [ ] **Step 3: Implement canonical message edges and exact logical units**

Canonicalize direct edges before grouping:

```sql
SELECT d.message_id,
       coalesce(c.canonical_id, d.participant_id) AS canonical_id,
       bool_or(d.is_sender) AS is_sender,
       bool_or(d.is_author) AS is_author
FROM direct_edges d
LEFT JOIN clusters c USING (participant_id)
GROUP BY d.message_id, canonical_id
```

Apply scalar, search-candidate, participant, and domain filters to message
facts first. The unfiltered build uses `WHERE true`. Chat reduction must
contain these expressions verbatim:

```sql
max(occurred_at) AS occurred_at,
arg_max(message_id,
        struct_pack(occurred_at := occurred_at, message_id := message_id))
    AS anchor_message_id,
arg_max(is_from_me,
        struct_pack(occurred_at := occurred_at, message_id := message_id))
    AS is_from_me
```

Non-chat people come only from canonical direct edges. Non-chat domains are
the union of direct and conversation-edge domains. Chat people/domains union
both origins across selected messages. Deduplicate each
`(logical_entry_key, canonical_id)` once.

- [ ] **Step 4: Build people and domain rollups**

Write one row per canonical identity/domain with:

```sql
count(*)::BIGINT AS activity_count,
coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
min(occurred_at) AS first_at,
max(occurred_at) AS last_at,
list(struct_pack(source_type := source_type, count := source_count)
     ORDER BY source_type) AS source_counts
```

Also retain an exact `source_rollups` list grouped by
`(canonical_id, source_id, source_type)` with activity count, file count,
first-at, and last-at. A pure source-only people filter aggregates this narrow
list instead of rebuilding the archive-wide logical reduction. Keep the
generic logical path for every combined filter and validate that source
rollups decompose the identity totals.

For domain `person_count`, filter raw people to their own normalized domain
before counting distinct canonical IDs. This preserves a cluster whose
canonical member belongs to another domain.

- [ ] **Step 5: Build anchored relationship rollups**

Capture `anchorDate := clock().UTC().Format(time.DateOnly)` once in the caller.
For qualifying non-owner interactions, assign modality masks exactly:

```sql
CASE
  WHEN entry_kind IN ('event','meeting') AND with_owner THEN 4::UTINYINT
  WHEN entry_kind = 'conversation' THEN 2::UTINYINT
  WHEN entry_kind IN ('email','item') THEN 1::UTINYINT
  ELSE 0::UTINYINT
END
```

Exclude owner canonical IDs and events/meetings without an owner. For
`occurred_at::DATE <= anchor_date`, aggregate decayed sent, received, and
meeting sums. For all dates, aggregate sent/meeting raw counts, bitwise-OR the
mask, and preserve `max(occurred_at)` at timestamp precision. Emit a rollup
only when at least one qualifying interaction exists.

For every qualifying date, write
`relationship_daily(canonical_id,event_date,sent_units,received_units,
meeting_units,sent_count,meeting_count,modality_mask,last_at)`.
Build the compact anchored `relationship_rollups` from this flat daily dataset.
The unfiltered request path reads daily rows strictly after the anchor; a pure
UTC-midnight date window aggregates its exact daily slice directly.

- [ ] **Step 6: Validate schemas and cross-dataset invariants**

`Validate` runs real DuckDB queries and rejects:

- any required dataset whose exact ordered column names or DuckDB types differ
  from the version-15 schema;
- a cached message without exactly one corresponding entry fact, or a fact
  without a cached message;
- duplicate fact IDs, direct `(message_id,participant_id)` pairs,
  conversation `(conversation_id,participant_id)` pairs, directory IDs, or
  domain-rollup keys;
- edges without a fact/participant/conversation;
- rollup IDs absent from the directory;
- owner IDs in either relationship dataset;
- a daily canonical ID without a relationship row;
- a rollup `anchor_date` different from the operation anchor;
- a non-empty relationship dataset with a null `last_at`;
- daily rows whose raw sent/meeting count components differ from their units
  or whose event date differs from `last_at`;
- daily count/mask/timestamp components that do not exactly decompose the
  corresponding total relationship rollup;
- any missing schema-bearing shard.

Return errors naming the dataset and invariant, for example:

```go
return fmt.Errorf("validate %s: %d daily identities have no relationship rollup",
	DatasetRelationshipDaily, count)
```

- [ ] **Step 7: Activate schema 15 after every required dataset is buildable**

Bump `query.CacheSchemaVersion` to 15, append all
`identityindex.RequiredDatasets` to `query.RequiredParquetDirs`, and update
schema-version/readiness tests. Ensure `TestDataBuilder` now derives all eight
datasets before computing its marker fingerprint. Stamp
`RelationshipAnchorDate` from the same operation anchor stored in rollup rows.
A cache created by Task 3 still carries schema 14 and therefore takes the
mandatory full-rebuild path.

- [ ] **Step 8: Run derivation and build-cache tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/identityindex ./cmd/msgvault/cmd -run 'Identity|Relationship|Daily|BuildCache'
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/identityindex cmd/msgvault/cmd/build_cache_test.go internal/query/cache_state.go internal/query/cache_state_test.go internal/query/duckdb.go internal/query/testfixtures_test.go
git commit -m "feat: precompute identity and relationship rollups"
```

---

### Task 5: Move derived refresh and stats out of in-daemon DuckDB

**Files:**

- Modify: `internal/query/cache_state.go`
- Modify: `internal/query/cache_state_test.go`
- Modify: `cmd/msgvault/cmd/cache_staleness.go`
- Modify: `cmd/msgvault/cmd/cache_staleness_test.go`
- Modify: `cmd/msgvault/cmd/build_cache.go`
- Modify: `cmd/msgvault/cmd/cache_refresh_test.go`
- Modify: `cmd/msgvault/cmd/serve.go`
- Modify: `cmd/msgvault/cmd/serve_test.go`
- Modify: `internal/cacheops/identity_refresh.go`
- Modify: `internal/cacheops/identity_refresh_test.go`
- Modify: `internal/cacheops/stats.go`
- Modify: `internal/cacheops/stats_test.go`

**Interfaces:**

- Produces:
  `cacheStaleness.HasConversationParticipantDrift`,
  hidden `build-cache --derived-only`,
  `runDerivedCacheSubprocess(context.Context) error`, and marker-only
  `cacheops.CollectStats`.
- Identity API mutations consume `runDerivedCacheSubprocess`; no daemon method
  calls `cacheops.RefreshIdentityDatasets` directly.

- [ ] **Step 1: Write failing lifecycle tests**

Add production-path tests for:

1. changing `conversation_participants` on an old non-chat conversation with no
   new message makes `cacheNeedsBuild` report derived drift without
   `FullRebuild`;
2. `--derived-only` refuses a version-14 marker before creating staging;
3. identity refresh launches a child and the parent holds no cache lock;
4. derived publication carries `state.Stats` unchanged, updates anchor,
   identity revision, conversation fingerprint, `PublishedAt`, and dataset
   fingerprint;
5. a child failure leaves every live dataset and marker byte-for-byte
   unchanged;
6. `CollectStats` returns marker values after one Parquet shard is replaced
   with invalid bytes and the test deliberately restamps the marker with that
   tree's new fingerprint. Readiness remains fingerprint-valid, while any
   attempted DuckDB scan would fail.

- [ ] **Step 2: Run focused tests and verify the old in-process behavior fails**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/cacheops -run 'ConversationParticipant|DerivedOnly|IdentityRefresh|CollectStats'
```

Expected: FAIL because there is no conversation fingerprint, no derived child
mode, and stats still opens DuckDB.

- [ ] **Step 3: Compute and stamp conversation-participant fingerprints**

Stream ordered pairs from the SQLite snapshot:

```sql
SELECT conversation_id, participant_id
FROM conversation_participants
ORDER BY conversation_id, participant_id
```

Hash a fixed-width big-endian encoding of both `int64` values with SHA-256.
Use the same helper during staleness and cache build. Stamp the snapshot's hash
as `ConversationParticipantsFingerprint`; if the source changes after the
snapshot, the next staleness check self-heals.

Set:

```go
result.HasConversationParticipantDrift = sourceFingerprint != state.ConversationParticipantsFingerprint
result.NeedsBuild = result.NeedsBuild || result.HasConversationParticipantDrift
```

Identity-only or conversation-only drift selects derived refresh unless
account-identity drift or any existing full-rebuild signal is also present.

- [ ] **Step 4: Add and enforce the derived-only child mode**

Add a hidden flag:

```go
var buildCacheDerivedOnlyFlag bool
buildCacheCmd.Flags().BoolVar(&buildCacheDerivedOnlyFlag, "derived-only", false,
	"Internal: refresh version-15 derived identity datasets")
_ = buildCacheCmd.Flags().MarkHidden("derived-only")
```

The child:

- acquires `acquireCacheBuildLock` itself;
- calls `InspectCacheReadiness`;
- returns a typed `ErrDerivedRefreshRequiresFullBuild` unless readiness is
  `CacheReady`, schema is 15, and every fact/raw-edge dataset exists;
- opens DuckDB with `BuilderPolicy(<derived-staging>/duckdb-tmp)`;
- exports current owner/clusters and, when its fingerprint changed, current
  conversation edges;
- calls `identityindex.Build(ModeDerivedOnly)`;
- preserves `state.Stats`;
- publishes all affected datasets with rollback before replacing the marker.

The daemon catches `ErrDerivedRefreshRequiresFullBuild` and launches
`build-cache --full-rebuild`; it does not ask the child to repair version 14.

- [ ] **Step 5: Route identity API refresh through the subprocess**

Replace `storeAPIAdapter.RefreshIdentityDatasets` lock and direct
`cacheops.RefreshIdentityDatasets` call with:

```go
func (a *storeAPIAdapter) RefreshIdentityDatasets(ctx context.Context) (int64, error) {
	if err := runDerivedCacheSubprocess(ctx); err != nil {
		return 0, err
	}
	state, err := query.ReadCacheSyncState(a.analyticsDir)
	if err != nil {
		return 0, fmt.Errorf("read refreshed cache revision: %w", err)
	}
	return state.IdentityRevision, nil
}
```

`newBuildCacheSubprocessCommand` accepts an explicit mode enum so
`--full-rebuild`, `--auto`, and `--derived-only` cannot be combined
inconsistently.

- [ ] **Step 6: Make stats marker-only**

Delete the DuckDB import and queries from `internal/cacheops/stats.go`.
After readiness and state reads:

```go
result := &CacheStats{
	Status: StatusReady,
	TotalMessages: state.Stats.TotalMessages,
	Sources: state.Stats.Sources,
	UniqueSenders: state.Stats.UniqueSenders,
	UniqueDomains: state.Stats.UniqueDomains,
	MinYear: state.Stats.MinYear,
	MaxYear: state.Stats.MaxYear,
	TotalSizeBytes: state.Stats.TotalSizeBytes,
	AttachmentSizeBytes: state.Stats.AttachmentSizeBytes,
	LastSyncAt: &state.LastSyncAt,
	LastMessageID: &state.LastMessageID,
}
```

Derived refresh copies `state.Stats` exactly.

- [ ] **Step 7: Run lifecycle and stats tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/cacheops ./cmd/msgvault/cmd ./internal/query -run 'CacheStaleness|Derived|IdentityRefresh|CollectStats|CacheRevision'
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/query/cache_state.go internal/query/cache_state_test.go cmd/msgvault/cmd/cache_staleness.go cmd/msgvault/cmd/cache_staleness_test.go cmd/msgvault/cmd/build_cache.go cmd/msgvault/cmd/cache_refresh_test.go cmd/msgvault/cmd/serve.go cmd/msgvault/cmd/serve_test.go internal/cacheops/identity_refresh.go internal/cacheops/identity_refresh_test.go internal/cacheops/stats.go internal/cacheops/stats_test.go
git commit -m "fix: isolate derived cache refreshes"
```

---

### Task 6: Serve people and domains from the index

**Files:**

- Create: `internal/query/identity_activity.go`
- Create: `internal/query/identity_activity_test.go`
- Modify: `internal/query/people.go`
- Modify: `internal/query/people_test.go`
- Modify: `internal/query/duckdb.go`
- Modify: `internal/query/views.go`
- Modify: `internal/query/views_test.go`

**Interfaces:**

- Produces:

```go
func identityRequestIsUnfiltered(ExploreRequest) bool
func buildIdentityLogicalSQL(ExploreRequest, identityCandidateSQL string) (string, []any)
func (e *DuckDBEngine) identityDatasetPath(dataset string) string
```

- Existing `PeopleAnalyzer` signatures and response JSON remain unchanged.

- [ ] **Step 1: Write failing edge-origin and fast-path tests**

Add tests that call the public `PeopleAnalyzer` methods and assert:

- unfiltered people/domain results equal the old results;
- a non-chat conversation-only participant qualifies a participant filter but
  is absent from people fan-out;
- the same participant's domain is present in domain fan-out;
- linked authored/co-recipient aliases count the entry once;
- an identity term first selects `identity_directory` candidates;
- `GetPerson`/`GetDomain` with empty context use rollups;
- date/source/type/deletion/search-candidate filters use facts.

Include a test-only `DuckDBOptions.DisableLegacyAnalyticalViews` engine and
verify all six public methods still succeed with no `analytical_entries`,
`messages`, or `message_recipients` views registered.

- [ ] **Step 2: Run the tests and verify the legacy-view failure**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query -run 'IdentityActivity|SearchPeople|SearchDomains|LegacyAnalyticalViews'
```

Expected: FAIL because `people.go` still calls `buildExploreLogicalSQL`.

- [ ] **Step 3: Implement scalar and edge filters**

Build predicates directly over `identity_entry_facts f`:

```sql
f.source_id IN (...)
f.occurred_at >= ?
f.occurred_at < ?
lower(f.message_type) IN (...)
f.deleted_from_source = <boolean>
f.message_id IN (<resolved search candidate IDs>)
```

Each participant group becomes:

```sql
EXISTS (SELECT 1 FROM identity_direct_edges d
        WHERE d.message_id = f.message_id AND d.participant_id IN (...))
OR EXISTS (SELECT 1 FROM identity_conversation_edges c
           WHERE c.conversation_id = f.conversation_id
             AND c.participant_id IN (...))
```

Each domain group uses the corresponding normalized domain columns. OR values
inside a group; AND the primary and additional groups. Continue calling
`expandParticipantFilterClusters` before rendering these predicates.

- [ ] **Step 4: Implement the exact filtered logical CTE**

Call `identityindex.LogicalActivitySQL` with the version-15 dataset paths and
the validated placeholder filter SQL. The CTE produces:

- `logical_units`;
- `logical_people(entry_key,canonical_id,is_author,...)`;
- `logical_domains(entry_key,domain,...)`.

The chat `arg_max` expressions and origin matrix must remain byte-identical to
the builder fragments. Candidate identity SQL is joined after fan-out so
context filtering does not change chat membership.

- [ ] **Step 5: Rewrite people queries**

Unfiltered path:

```sql
WITH candidates AS (
  SELECT * FROM identity_directory d
  WHERE <exact id or EXISTS UNNEST(search_values) contains term>
), counted AS (
  SELECT d.*, r.activity_count, r.file_count, r.source_counts,
         r.first_at, r.last_at, count(*) OVER () AS total_count
  FROM candidates d JOIN identity_rollups r USING (canonical_id)
)
SELECT ... FROM counted ORDER BY <validated order> LIMIT ? OFFSET ?
```

Filtered path aggregates `logical_people` only for directory candidates.
When participant or domain edge filters are present, first resolve the
complete predicate to at most 10,000 exact fact IDs and feed those IDs into
the logical reduction. Preserve all scalar/search predicates and fall back to
the original query shape when the bounded candidate set saturates.
The source-only specialization aggregates selected
`identity_rollups.source_rollups` rows; all combined predicates retain the
generic path.
Select the page before joining `participant_identifiers` for response shaping.
Decode the stored member/source lists into existing Go response types.

- [ ] **Step 6: Rewrite domain queries**

Unfiltered path selects candidate domains then joins `domain_rollups`.
Filtered path aggregates `logical_domains` and computes `person_count` from
domain-qualified canonical people. Keep existing validated sort behavior and
response types.

- [ ] **Step 7: Keep legacy views for non-migrated endpoints**

`DisableLegacyAnalyticalViews` skips only convenience views used by old
people/relationship aggregation in tests. Normal production registration
continues to create `analytical_entries`, base message views, timeline views,
and file-search views. Do not remove `buildExploreLogicalSQL`.

- [ ] **Step 8: Run people, domain, Explore, timeline, and files tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query ./internal/api -run 'People|Domain|Explore|Timeline|Files'
```

Expected: PASS, including legacy non-migrated endpoint tests.

- [ ] **Step 9: Commit**

```bash
git add internal/query/identity_activity.go internal/query/identity_activity_test.go internal/query/people.go internal/query/people_test.go internal/query/duckdb.go internal/query/views.go internal/query/views_test.go
git commit -m "feat: query people and domains from identity index"
```

---

### Task 7: Serve relationships from anchored rollups and remove the memo

**Files:**

- Modify: `internal/query/relationships.go`
- Modify: `internal/query/relationships_test.go`
- Delete: `internal/query/relationships_memo.go`
- Delete: `internal/query/relationships_memo_test.go`
- Modify: `internal/query/duckdb.go`
- Modify: `internal/api/relationships.go`
- Modify: `internal/api/relationships_test.go`

**Interfaces:**

- Existing `RelationshipAnalyzer.Relationships` signature remains unchanged.
- Produces:
  `buildRelationshipRollupSQL(now time.Time)`,
  `buildFilteredRelationshipsSQL(ExploreRequest, now time.Time)`, and
  `modalitiesFromMask(uint8) int`.

- [ ] **Step 1: Replace memo tests with cold-query and cancellation tests**

Delete memo hit-count assertions. Add tests proving:

- two identical requests each execute safely without shared state;
- a canceled second request returns its own context error;
- result ordering is identical across calls;
- cache/identity/anchor revision drift still changes response revision;
- post-anchor daily rows affect decay, raw gate counts, modality population count, and
  full timestamp `last_at`;
- a backward request date clamps anchor advancement to zero;
- ShowAll includes only identities with at least one qualifying interaction.

- [ ] **Step 2: Run focused tests and verify they fail**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query ./internal/api -run 'Relationship'
```

Expected: FAIL until the memo and legacy archive-wide SQL are removed.

- [ ] **Step 3: Implement the unfiltered rollup query**

Read the marker anchor and validate it as a UTC date. The query computes:

```sql
past_factor =
  exp(-? * greatest(0, date_diff('day', r.anchor_date, ?::DATE)))

sent =
  r.sent_decayed * past_factor +
  coalesce(sum(f.sent_units *
    exp(-? * greatest(0, date_diff('day', f.event_date, ?::DATE)))), 0)
```

Repeat for received and meeting units. Use raw sent/meeting counts,
`modality_mask`, and full `last_at` directly from `relationship_rollups`
because those totals already include every daily row. Convert modalities with:

```go
func modalitiesFromMask(mask uint8) int {
	return bits.OnesCount8(mask & (identityindex.ModalityEmail |
		identityindex.ModalityChat | identityindex.ModalityMeeting))
}
```

Join `identity_directory` for labels/member IDs. Apply
`RelationshipScore`, the reciprocity gate, and the existing total ordering in
Go.

- [ ] **Step 4: Implement filtered ranking over narrow facts**

For non-empty `Context`, call `buildIdentityLogicalSQL`, derive owner presence
and canonical interactions, and retain the current signal predicates:

```sql
is_from_me AND entry_kind IN ('email','conversation','item')
NOT is_from_me AND
  (entry_kind = 'conversation' OR
   (entry_kind IN ('email','item') AND is_author))
entry_kind IN ('event','meeting') AND with_owner
```

Use per-entry decay against the cursor-pinned `Now`; this path remains exact
for arbitrary context without reading legacy base views.

- [ ] **Step 5: Remove memo and stale comments**

Delete the memo files, `DuckDBEngine.relMemo`,
`relationshipsQueryRuns`, `relationshipsMemoKey`, and memo-specific API
comments. Keep the cursor's pinned decay date and deterministic canonical-ID
tie-break. Before deleting `buildRelationshipsSQL`, copy it into
`relationships_legacy_equivalence_test.go` as
`buildLegacyRelationshipsSQLForEquivalence`; it is test-only input for Task 8
and must never be called by production code.

- [ ] **Step 6: Run relationship and cursor tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query ./internal/api -run 'Relationship|Cursor'
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add -A internal/query/relationships.go internal/query/relationships_test.go internal/query/relationships_memo.go internal/query/relationships_memo_test.go internal/query/duckdb.go internal/api/relationships.go internal/api/relationships_test.go
git commit -m "feat: rank relationships from cached rollups"
```

---

### Task 8: Prove migrated endpoint equivalence and legacy endpoint isolation

**Files:**

- Create: `cmd/msgvault/cmd/relationships_index_http_e2e_test.go`
- Modify: `internal/api/relationships_test.go`
- Modify: `internal/api/people_snapshot_e2e_test.go`
- Modify: `internal/api/relationship_timeline_test.go`
- Modify: `internal/query/people_test.go`
- Modify: `internal/query/relationships_test.go`
- Modify: `cmd/msgvault/cmd/build_cache_test.go`

**Interfaces:**

- No new production interface.
- Tests exercise the version-15 builder, DuckDB engine, and real HTTP router.

- [ ] **Step 1: Add the seven-endpoint HTTP guard**

From the `cmd` package, build a real SQLite store, call the real
`buildCache(..., true)`, open `NewDuckDBEngine` with
`DisableLegacyAnalyticalViews`, wire a real `api.Server`, and send:

```text
POST /api/v1/relationships
POST /api/v1/people/search
GET  /api/v1/people/{id}
POST /api/v1/people/{id}/summary
POST /api/v1/domains/search
GET  /api/v1/domains/{domain}
POST /api/v1/domains/{domain}/summary
```

Assert HTTP 200, exact rows/totals, and non-empty cache revisions. Include
filtered variants with participant, domain, date, source, message type,
deletion, and resolved candidate-message filters.

- [ ] **Step 2: Add adversarial equivalence cases**

For each case, execute the version-14 legacy SQL helper retained in the test
and the version-15 public method against the same base fixture, then compare
normalized responses:

- linked/unlinked identities and owner aliases;
- authored/co-recipient linked aliases: exactly one received unit;
- conversation-only members on non-chat messages;
- filtered chat direction and timestamp tie-breaking;
- every modality and ownerless events;
- future dates and backward request clock;
- empty archive and unnamed identities;
- sort/page/cursor revision drift.

Keep the legacy helper test-only; production endpoints must not branch to it.

- [ ] **Step 3: Prove non-migrated endpoints still use legacy views**

With a normal engine, verify:

```text
POST /api/v1/people/{id}/timeline
POST /api/v1/domains/{domain}/timeline
POST /api/v1/relationships/{id}/timeline
POST /api/v1/people/{id}/files/search
POST /api/v1/domains/{domain}/files/search
POST /api/v1/files/search
```

Then open the guard engine without legacy analytical views and assert these
routes return the existing analytical-unavailable/error response. This proves
the migration boundary behaviorally.

- [ ] **Step 4: Test conversation-membership self-healing**

Build a cache, add a conversation participant to an old non-chat conversation
without inserting a message, verify staleness, run the real derived child, and
assert:

- participant filter now matches;
- people fan-out still excludes the conversation-only identity;
- domain fan-out includes its domain;
- marker message ID and stats summary are unchanged;
- anchor, fingerprint, publication time, and revision advance.

- [ ] **Step 5: Run correctness suites**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/identityindex ./internal/query ./internal/cacheops ./internal/api ./cmd/msgvault/cmd
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/msgvault/cmd/relationships_index_http_e2e_test.go internal/api/relationships_test.go internal/api/people_snapshot_e2e_test.go internal/api/relationship_timeline_test.go internal/query/people_test.go internal/query/relationships_test.go cmd/msgvault/cmd/build_cache_test.go
git commit -m "test: prove relationship index equivalence"
```

---

### Task 9: Add the production-scale performance and RSS gate

**Files:**

- Create: `internal/query/identity_index_benchmark_test.go`
- Modify: `internal/query/benchmark_test.go`
- Modify: `cmd/msgvault/cmd/build_cache_test.go`
- Modify: `internal/fakevault/fakevault.go`
- Modify: `internal/fakevault/fakevault_test.go`
- Modify: `cmd/msgvault/cmd/fake_vault.go`
- Create: `cmd/msgvault/cmd/fake_vault_test.go`
- Create: `scripts/benchmark-relationships-index.sh`
- Create: `docs/internal/relationships-index-benchmark.md`

**Interfaces:**

- Produces an opt-in benchmark selected by
  `MSGVAULT_RELATIONSHIPS_SCALE_BENCH=1`.
- The script prints one JSON record containing build/query latency, peak RSS,
  settled RSS delta, and spill bytes.

- [ ] **Step 1: Add a deterministic 2.5-million-message generator**

Generate data set-wise in DuckDB/SQLite, not with 2.5 million Go insert calls:

```go
const (
	scaleMessages     = 2_500_000
	scaleEdges        = 6_000_000
	scaleParticipants = 75_000
)
```

Extend `fakevault.Options` and the hidden CLI with:

```go
Participants    int64
ParticipantEdges int64
```

and flags:

```text
--participants 75000
--participant-edges 6000000
```

The generator distributes `ParticipantEdges-Messages` recipient rows
deterministically across messages, excludes each message's sender from its
recipient set, prevents duplicate recipients within a message, and rejects
`ParticipantEdges < Messages`. The union of sender and recipient edges is
therefore exactly six million. Preserve existing defaults when the flags are
zero so backup benchmarks do not change.

Generate mixed email/chat/meeting facts, linked aliases, owners, future rows,
and attachments from a fixed seed. Use set-wise SQL for any additional
benchmark-only link/owner/future mutations after `fake-vault` completes so
runs remain comparable.

- [ ] **Step 2: Add cold query benchmarks**

Benchmarks must create a fresh engine per measured cold operation:

```go
func BenchmarkRelationshipIndexCold(b *testing.B) {
	if os.Getenv("MSGVAULT_RELATIONSHIPS_SCALE_BENCH") != "1" {
		b.Skip("set MSGVAULT_RELATIONSHIPS_SCALE_BENCH=1")
	}
	b.Run("relationships", ...)
	b.Run("people-search", ...)
	b.Run("domain-search", ...)
	b.Run("source-only-people", ...)
	b.Run("date-window-relationships", ...)
	b.Run("filtered-people", ...)
	b.Run("filtered-relationships", ...)
}
```

Record `testing.B.ReportMetric` values for scanned rows and spill bytes from
DuckDB profiling in addition to `ns/op`. Persist those parsed metrics as
machine-readable JSON and include them in the shell gate's single JSON result;
the human-formatted `go test -bench` stream is diagnostics, not the evidence
artifact.

- [ ] **Step 3: Add a real subprocess RSS harness**

`scripts/benchmark-relationships-index.sh` must:

1. build the current `msgvault` binary;
2. create a temporary `MSGVAULT_HOME`;
3. invoke `msgvault fake-vault --messages 2500000 --participants 75000
   --participant-edges 6000000 --attachment-bytes 0 --seed 1`, then
   `/usr/bin/time -l msgvault build-cache --full-rebuild`;
4. start `msgvault serve` with DuckDB mode;
5. measure baseline RSS with `ps -o rss=`;
6. issue cold relationships, people, and filtered requests with `curl`;
7. sample peak RSS at 100 ms intervals;
8. wait for five seconds of query idleness and record settled RSS;
9. record the daemon spill directory's peak bytes;
10. terminate only the spawned daemon PID and remove the temporary home.

Use `trap` for cleanup and validate every PID/path before `kill` or removal.
Emit JSON, not human-formatted prose, so runs can be compared.

- [ ] **Step 4: Enforce the reviewed gates**

The harness exits nonzero when:

- cold relationships or unfiltered people/domain exceeds 250 ms;
- interactive peak RSS grows by more than 1.5 GiB;
- settled RSS remains more than 256 MiB above baseline;
- full build exceeds 25 seconds or 3 GiB peak RSS.

Report the filtered one-second result but mark it provisional. If it misses,
retain the four-thread cap and fail the candidate pending query/partition
optimization or a reviewed design amendment.

- [ ] **Step 5: Run small benchmarks and the full validation suite**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query -run '^$' -bench 'RelationshipIndex' -benchtime=1x
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
make test
make lint-ci
make docs-check
```

Expected: all commands PASS. Run the scale gate on the reference Apple-silicon
machine:

```bash
MSGVAULT_RELATIONSHIPS_SCALE_BENCH=1 scripts/benchmark-relationships-index.sh
```

Expected: exit 0 and a JSON record satisfying the reviewed release gates.

- [ ] **Step 6: Record measured results**

In `docs/internal/relationships-index-benchmark.md`, record the machine model,
RAM, OS, commit SHA, DuckDB version, archive cardinalities, and the JSON result.
Do not weaken a gate in this file; any gate change requires revising the
authoritative design first.

- [ ] **Step 7: Commit**

```bash
git add internal/query/identity_index_benchmark_test.go internal/query/benchmark_test.go cmd/msgvault/cmd/build_cache_test.go internal/fakevault/fakevault.go internal/fakevault/fakevault_test.go cmd/msgvault/cmd/fake_vault.go cmd/msgvault/cmd/fake_vault_test.go scripts/benchmark-relationships-index.sh docs/internal/relationships-index-benchmark.md
git commit -m "perf: gate relationship index resource use"
```

---

## Final verification

- [ ] Confirm `rg -n "identity_activity" internal cmd` finds no obsolete
  version-15 dataset name.
- [ ] Confirm `rg -n 'sql.Open\\(\"duckdb\"' internal cmd --glob '*.go'`
  shows production opens only inside `internal/duckdbutil`; test-only opens
  may remain.
- [ ] Confirm every new/modified Go test uses `assert`/`require` and all test
  commands use `-tags "fts5 sqlite_vec"`.
- [ ] Run:

```bash
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
make test
make lint-ci
make docs-check
```

- [ ] Run the production-scale gate and attach its JSON output to the final
  implementation review.
- [ ] Review the final diff for private names, credentials, real identifiers,
  or unrelated workspace changes.
- [ ] Use `superpowers:requesting-code-review` before branch integration.
