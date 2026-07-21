# Relationships Backend Implementation Plan (Plan 2 of 3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the backend for the relationships-centric web UI per `docs/superpowers/specs/2026-07-20-web-ui-relationships-design.md`: reversible identity-link clusters, identity-aware cache exports (`is_from_me` derivation, `owner_participants`, `participant_clusters`), the relationship ranking and day-burst timeline endpoints, identity link/unlink APIs with synchronous cache refresh, and conversation time-window bounds.

**Architecture:** New `participant_links` table with a forest invariant and an `identity_revision` counter in `archive_metadata` (store layer); three cache changes behind a `CacheSchemaVersion` bump (derived `is_from_me`, two new identity datasets); a lightweight identity-dataset refresh in `internal/cacheops` callable from both `build-cache` and the API; DuckDB ranking/timeline queries following the `PeopleAnalyzer` pattern; Huma-registered endpoints following the explore cursor/authority conventions.

**Tech Stack:** Go + testify; SQLite/PostgreSQL via `internal/store` dialects; DuckDB over Parquet (`internal/query`); Huma OpenAPI routes (`internal/api`); regenerated clients via `make openapi` + `web/scripts/generate-web-client.mjs`.

## Global Constraints

- Go tests use testify only: `require.X` halts, `assert.X` continues; argument order `(want, got)`. Never `t.Errorf`/`t.Fatalf`.
- Run Go tests with `-tags "fts5 sqlite_vec"`. After Go changes: `go fmt ./...` and `go vet ./...`; stage all resulting changes.
- Every `internal/store` schema/DDL change lands in BOTH `schema.sql` (SQLite) and `schema_pg.sql` (PostgreSQL, `DATETIME`→`TIMESTAMPTZ`). Store methods must be dialect-aware (`s.dialect.InsertOrIgnore`, `Rebind`, `Now()` — see existing patterns in `internal/store/account_identities.go`).
- Cache COPY column changes require bumping `query.CacheSchemaVersion` (currently 12 → 13, `internal/query/cache_state.go:15-17`) and updating the pinned assertion in `internal/query/cache_state_test.go:141`.
- New/changed API routes: bump `APISchemaVersion` in `internal/api/openapi.go:72` (1.18.0 → 1.19.0) with a changelog comment, run `make openapi`, and commit the regenerated `api/openapi.yaml`, `pkg/client/`, and (after `cd web && bun run generate`) `web/src/lib/api/generated/schema.d.ts`.
- Never use real PII in fixtures — synthetic names/addresses only.
- Commit after every task; pre-commit hooks must pass; never `--no-verify`. Run `make lint-ci` before finishing.
- Spec weights are binding: `sent_to_them × 2.0`, `meetings_together × 3.0`, `received_from_them × 1.0`; decay half-life 365 days; breadth boost `1 + 0.25 × (modalities − 1)`; gate `sent_to_them ≥ 1 OR meetings_together ≥ 1`; ties break by most-recent interaction then display name.

---

### Task 1: `participant_links` table, identity revision, and store link API

**Files:**
- Modify: `internal/store/schema.sql` (after `account_identities`, ~line 543)
- Modify: `internal/store/schema_pg.sql` (parallel location)
- Create: `internal/store/participant_links.go`
- Create: `internal/store/participant_links_test.go`

**Interfaces (produces):**
```go
var ErrAlreadyLinked = errors.New("participants are already linked through other identities")

// LinkParticipants asserts a and b are the same person. Idempotent for the
// exact existing edge; ErrAlreadyLinked for a new redundant edge between
// indirectly-connected participants. Returns the identity revision.
func (s *Store) LinkParticipants(a, b int64) (int64, error)
// UnlinkParticipants removes the edge (idempotent). Returns the revision.
func (s *Store) UnlinkParticipants(a, b int64) (int64, error)
// ParticipantClusters returns participant_id → canonical cluster ID
// (smallest member ID) for every participant that appears in a link edge.
// Unlinked participants are their own cluster and are not returned.
func (s *Store) ParticipantClusters() (map[int64]int64, error)
// ClusterMembers returns all participant IDs in the cluster containing id
// (including id itself), sorted ascending. Single-element for unlinked ids.
func (s *Store) ClusterMembers(id int64) ([]int64, error)
// IdentityRevision returns the archive_metadata identity revision (0 if unset).
func (s *Store) IdentityRevision() (int64, error)
```

- [ ] **Step 1: Add the schema (both dialects)**

`internal/store/schema.sql`, after the `account_identities` index:

```sql
-- User-asserted identity links between participants. Edges are normalized
-- (participant_a < participant_b) and the graph is kept a forest: every
-- edge joins two previously distinct clusters, so deleting an edge
-- deterministically splits one cluster in two. Connected components resolve
-- to a canonical cluster (smallest member ID) at read time.
CREATE TABLE IF NOT EXISTS participant_links (
    participant_a INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    participant_b INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (participant_a, participant_b),
    CHECK (participant_a < participant_b)
);

CREATE INDEX IF NOT EXISTS idx_participant_links_b
    ON participant_links(participant_b);
```

Mirror in `schema_pg.sql` with `TIMESTAMPTZ`.

- [ ] **Step 2: Write failing store tests**

`internal/store/participant_links_test.go` (package `store_test`, using `storetest.New(t)` like `account_identities_test.go`). Table of behaviors — each is one test:

```go
func TestLinkParticipantsCreatesEdgeAndBumpsRevision(t *testing.T) {
	f := storetest.New(t)
	a := f.AddParticipant("alice@example.com", "Alice")
	b := f.AddParticipant("alice@personal.example", "Alice P")
	rev, err := f.Store.LinkParticipants(b, a) // reversed order: must normalize
	require.NoError(t, err)
	assert.Equal(t, int64(1), rev)
	clusters, err := f.Store.ParticipantClusters()
	require.NoError(t, err)
	assert.Equal(t, map[int64]int64{a: min64(a, b), b: min64(a, b)}, clusters)
}

func TestLinkParticipantsExactEdgeIsIdempotent(t *testing.T) // second identical Link → same revision, no error
func TestLinkParticipantsRejectsSelfAndUnknown(t *testing.T) // a==a → error; nonexistent id → error
func TestLinkParticipantsRedundantIndirectEdgeIsAlreadyLinked(t *testing.T) {
	// link a-b, b-c; then Link(a, c) → ErrAlreadyLinked (they are indirectly connected)
}
func TestUnlinkParticipantsSplitsClusterDeterministically(t *testing.T) {
	// link a-b, b-c; Unlink(b, c) → clusters: {a,b} and c alone; revision bumped
}
func TestUnlinkParticipantsMissingEdgeIsIdempotent(t *testing.T) // no error, revision unchanged
func TestClusterMembersForUnlinkedParticipant(t *testing.T)     // returns just the id
```

If `storetest` has no `AddParticipant` helper, use the store's existing participant upsert (find it: `rg -n "func (s \*Store) EnsureParticipant" internal/store/`) or insert via `f.Store` exec — follow whatever `account_identities_test.go` and `collection_test.go` do to create participants.

- [ ] **Step 3: Run tests to verify they fail**

`go test -tags "fts5 sqlite_vec" ./internal/store/ -run TestLinkParticipants -v` — FAIL (undefined methods).

- [ ] **Step 4: Implement `internal/store/participant_links.go`**

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

var ErrAlreadyLinked = errors.New("participants are already linked through other identities")

const identityRevisionKey = "identity_revision"

// IdentityRevision returns the current identity revision (0 if never bumped).
func (s *Store) IdentityRevision() (int64, error) {
	var value string
	err := s.db.QueryRow(
		`SELECT value FROM archive_metadata WHERE key = ?`, identityRevisionKey,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read identity revision: %w", err)
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse identity revision %q: %w", value, err)
	}
	return revision, nil
}

// bumpIdentityRevision increments the revision inside tx and returns the new value.
func (s *Store) bumpIdentityRevision(tx *loggedTx) (int64, error) {
	if _, err := tx.Exec(s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		identityRevisionKey); err != nil {
		return 0, fmt.Errorf("seed identity revision: %w", err)
	}
	var revision int64
	if err := tx.QueryRow(
		`UPDATE archive_metadata SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		 WHERE key = ? RETURNING CAST(value AS INTEGER)`,
		identityRevisionKey).Scan(&revision); err != nil {
		return 0, fmt.Errorf("bump identity revision: %w", err)
	}
	return revision, nil
}
```

(If the PG dialect needs different casts, use `s.dialect.Rebind` and test under `make test-pg` when `MSGVAULT_TEST_DB` is set; `CAST(value AS INTEGER)` is valid in both.)

Link/unlink — the graph is small (user-asserted edges), so load all edges and resolve components in Go:

```go
type linkEdge struct{ a, b int64 }

func (s *Store) loadLinkEdges(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) ([]linkEdge, error) { /* SELECT participant_a, participant_b FROM participant_links */ }

// componentOf walks the edge list from id; returns the member set.
func componentOf(id int64, edges []linkEdge) map[int64]struct{} { /* BFS over adjacency */ }

func normalizeEdge(a, b int64) (int64, int64) { if a > b { return b, a }; return a, b }

func (s *Store) LinkParticipants(a, b int64) (int64, error) {
	if a == b || a <= 0 || b <= 0 {
		return 0, fmt.Errorf("link participants: ids must be distinct positive IDs (got %d, %d)", a, b)
	}
	lo, hi := normalizeEdge(a, b)
	var revision int64
	err := s.withTx(func(tx *loggedTx) error {
		// Verify both participants exist (clear error beats FK failure).
		// SELECT COUNT(*) FROM participants WHERE id IN (?, ?) == 2 ...
		edges, err := s.loadLinkEdgesTx(tx)
		if err != nil {
			return err
		}
		for _, e := range edges {
			if e.a == lo && e.b == hi { // exact edge: idempotent
				revision, err = s.currentIdentityRevisionTx(tx)
				return err
			}
		}
		if _, connected := componentOf(lo, edges)[hi]; connected {
			return ErrAlreadyLinked
		}
		if _, err := tx.Exec(
			`INSERT INTO participant_links (participant_a, participant_b) VALUES (?, ?)`,
			lo, hi); err != nil {
			return fmt.Errorf("insert participant link: %w", err)
		}
		revision, err = s.bumpIdentityRevision(tx)
		return err
	})
	return revision, err
}
```

`UnlinkParticipants` mirrors it: delete the normalized edge; bump only if a row was deleted, else return current revision. `ParticipantClusters` loads edges, unions components, maps every member to the component's min ID. `ClusterMembers(id)` returns the sorted component (or `[id]`).

- [ ] **Step 5: Tests pass; fmt/vet; commit**

`go test -tags "fts5 sqlite_vec" ./internal/store/ -run "TestLink|TestUnlink|TestCluster" -v` → PASS.
Full package: `go test -tags "fts5 sqlite_vec" ./internal/store/` → PASS.

```bash
go fmt ./... && go vet ./... && git add -A
git commit -m "feat(store): add participant link clusters with identity revision"
```

---

### Task 2: Link-aware `MergeParticipants` with spanning-forest recompute

**Files:**
- Modify: `internal/store/messages.go` (`MergeParticipants`, ~line 2054)
- Modify: `internal/store/migrate_phone_unique.go` (`mergeParticipant` tx variant, ~line 135)
- Test: `internal/store/participant_links_test.go` (extend)

**Interfaces:**
- Consumes: Task 1's `bumpIdentityRevision`, `loadLinkEdgesTx`, `componentOf`, `normalizeEdge`.
- Produces: merge semantics later tasks rely on — after any merge, links form a canonical spanning star rooted at the cluster's smallest member ID and the identity revision is bumped (only when links were touched).

- [ ] **Step 1: Write failing tests**

```go
func TestMergeParticipantsRewritesLinkEdges(t *testing.T) {
	// participants a < b < c; link b-c; merge b INTO a (a survives)
	// → edge becomes a-c; b gone; ParticipantClusters maps {a,c} → a; revision bumped
}
func TestMergeParticipantsPathContractionKeepsForest(t *testing.T) {
	// participants a<x<y<b; links a-x, x-y, y-b; merge b INTO a
	// (contracting the path endpoints, the spec's cycle case A-X-Y-A)
	// → after merge: exactly 2 edges forming a star rooted at min(a,x,y);
	//   cluster {a,x,y} intact; no self-edges; no duplicate edges;
	//   unlinking any single edge splits exactly one member off
}
func TestMergeParticipantsWithoutLinksDoesNotBumpRevision(t *testing.T)
```

- [ ] **Step 2: Run to verify failure** (the path-contraction test should fail against current behavior — the merge either violates FK or leaves edges referencing the deleted participant).

- [ ] **Step 3: Implement**

Add a helper used inside both merge transaction bodies, called just before the final `DELETE FROM participants WHERE id = ?`:

```go
// rewriteLinksForMerge repoints link edges from loser to winner, then
// restores the forest invariant for the affected cluster by rebuilding its
// edges as a canonical star rooted at the smallest member ID (vertex
// contraction can otherwise create cycles: merging the endpoints of path
// A-X-Y-B yields cycle A-X-Y-A). Returns whether any links were touched.
func rewriteLinksForMerge(tx *loggedTx, loser, winner int64) (bool, error) {
	edges, err := loadLinkEdgesFromTx(tx)
	if err != nil {
		return false, err
	}
	// Collect the combined component BEFORE contraction (loser ∪ winner reach).
	members := componentOf(loser, edges)
	for m := range componentOf(winner, edges) {
		members[m] = struct{}{}
	}
	delete(members, loser)
	members[winner] = struct{}{}
	touched := false
	// Was the loser or winner in any edge at all?
	for _, e := range edges {
		if e.a == loser || e.b == loser || e.a == winner || e.b == winner {
			touched = true
			break
		}
	}
	if !touched || len(members) < 2 {
		if touched { // loser had edges but contraction leaves a singleton
			_, err := tx.Exec(`DELETE FROM participant_links WHERE participant_a IN (?, ?) OR participant_b IN (?, ?)`,
				loser, winner, loser, winner)
			return touched, err
		}
		return false, nil
	}
	// Delete every edge inside the affected component, then re-insert a
	// deterministic star from the canonical (smallest) member.
	ids := make([]int64, 0, len(members))
	for m := range members {
		ids = append(ids, m)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// DELETE ... WHERE participant_a IN (ids...) OR participant_b IN (ids...)
	// (build the IN list with placeholders; also removes loser-touching edges)
	canonical := ids[0]
	for _, other := range ids[1:] {
		// INSERT INTO participant_links (participant_a, participant_b) VALUES (canonical, other)
	}
	return true, nil
}
```

In `MergeParticipants` (and `mergeParticipant`): call `rewriteLinksForMerge(tx, oldID, newID)` before the participant delete; if it returns `touched`, call `bumpIdentityRevision(tx)`. The `ON DELETE CASCADE` FK remains a safety net only.

- [ ] **Step 4: Tests pass; run the pre-existing merge tests too**

`go test -tags "fts5 sqlite_vec" ./internal/store/ -run "TestMerge" -v` → PASS (including existing merge coverage).

- [ ] **Step 5: fmt/vet; commit**

```bash
go fmt ./... && go vet ./... && git add -A
git commit -m "feat(store): make participant merges link-aware with forest restoration"
```

---

### Task 3: Cache exports — derived `is_from_me`, identity datasets, schema v13

**Files:**
- Modify: `cmd/msgvault/cmd/build_cache.go` (messages COPY ~line 763; auxiliary exports ~line 636-751; state assembly ~line 879)
- Modify: `internal/query/cache_state.go` (`CacheSchemaVersion` 12→13; `CacheSyncState` gains `IdentityRevision int64 \`json:"identity_revision,omitempty"\``; include it in `Revision()` hashing)
- Modify: `internal/query/cache_state_test.go:141` (pin 13)
- Modify: `cmd/msgvault/cmd/cache_staleness.go` (new signal)
- Modify: `cmd/msgvault/cmd/cache_publication.go` (`replacesCacheDataset` gains the two new datasets)
- Modify: `internal/query/duckdb.go` (`RequiredParquetDirs` ~line 2360; `hasCol` guard for `is_from_me`)
- Test: `cmd/msgvault/cmd/build_cache_test.go` (or the existing build test file — find with `rg -ln "func TestBuild" cmd/msgvault/cmd/`)

**Interfaces (produces):**
- Messages Parquet gains `is_from_me BOOLEAN` derived at export time.
- New full-replace datasets: `owner_participants` (`source_id BIGINT, participant_id BIGINT`) and `participant_clusters` (`participant_id BIGINT, canonical_id BIGINT`).
- `CacheSyncState.IdentityRevision` mirrors `store.IdentityRevision()` at publish time.

- [ ] **Step 1: Write the failing derivation test**

In the cmd build test file, following its existing seeded-store + build pattern: seed a source with a confirmed account identity (`AddAccountIdentity(sourceID, "owner@example.com", "manual")`), a message whose sender participant has `email_address = "Owner@Example.com"` (case differs) and stored `is_from_me = false`, plus a control message from `other@example.com`. Build the cache. Open the messages Parquet via DuckDB (the test file already has helpers for reading built output; if not, query through `NewDuckDBEngine`) and assert the first message exports `is_from_me = true` (derived) and the control exports `false`. Also assert `owner_participants` contains the owner's participant ID for that source, and that linking two participants (Task 1 API) then rebuilding produces a `participant_clusters` row mapping both to the smaller ID.

- [ ] **Step 2: Run to verify failure** (`go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/ -run <TestName> -v`).

- [ ] **Step 3: Implement the messages COPY change**

In the messages COPY SELECT (build_cache.go ~line 770), add:

```sql
		(m.is_from_me OR EXISTS (
			SELECT 1 FROM sqlite_db.account_identities ai
			JOIN sqlite_db.participants sp ON sp.id = m.sender_id
			WHERE ai.source_id = m.source_id
			  AND sp.email_address IS NOT NULL
			  AND lower(sp.email_address) = lower(ai.address)
		)) AS is_from_me,
```

(Match the surrounding table-qualification style — the COPY reads from the ATTACHed SQLite database; check how existing columns reference `m.` and whether the alias prefix is `sqlite_db.` or set by a `USE`/FROM clause, and mirror it exactly.)

- [ ] **Step 4: Add the two identity-dataset exports**

Next to the other auxiliary `runExport` calls (~line 636-751):

```go
	if err := runExport("owner_participants", `
	COPY (
		SELECT DISTINCT ai.source_id, p.id AS participant_id
		FROM sqlite_db.account_identities ai
		JOIN sqlite_db.participants p
		  ON p.email_address IS NOT NULL AND lower(p.email_address) = lower(ai.address)
		UNION
		SELECT DISTINCT ai.source_id, pi.participant_id
		FROM sqlite_db.account_identities ai
		JOIN sqlite_db.participant_identifiers pi
		  ON pi.identifier_type = 'email' AND lower(pi.identifier_value) = lower(ai.address)
	) TO '%s' (FORMAT PARQUET)`); err != nil { ... }
```

For `participant_clusters`, the mapping is computed in Go (`store.ParticipantClusters()`, Task 1) since components need graph traversal: create a DuckDB temp table, insert the pairs, `COPY (SELECT participant_id, canonical_id FROM tmp_clusters) TO ...`, drop the temp table. An empty mapping still writes an empty (schema-bearing) Parquet file — the dataset must always exist (`RequiredParquetDirs`).

- [ ] **Step 5: Version, state, staleness, publication, reader**

- `CacheSchemaVersion = 13`; fix the pinned test.
- `CacheSyncState`: add `IdentityRevision int64` and fold it into `Revision()`'s hash input (alongside the existing commit-marker fields).
- State assembly (build_cache.go ~879): set `IdentityRevision` from `store.IdentityRevision()`.
- `cacheStaleness`: add `HasIdentityDrift bool`; in the signal collection, compare `store.IdentityRevision()` to `state.IdentityRevision` — drift alone sets `NeedsBuild` with reason `identity revision changed` but NOT `FullRebuild` (the refresh path in Task 4 handles it; the full build path also refreshes it naturally).
- `replacesCacheDataset` (cache_publication.go:79): add `owner_participants`, `participant_clusters`.
- `RequiredParquetDirs` (duckdb.go:2360): add both datasets.
- `hasCol`/optional-column guard for `is_from_me` (default `FALSE`) so a v12 cache that somehow loads doesn't break readers — follow the existing optional-column REPLACE pattern in duckdb.go:554-589.
- Extend the test fixture builder (`internal/query/testfixtures_test.go`): `MessageOpt` gains `IsFromMe bool`; `Build()` writes the two new datasets (builder-level `AddOwnerParticipant(sourceID, participantID)` and `LinkCluster(ids ...int64)` helpers writing the parquet rows directly). Tasks 5–6 need these.

- [ ] **Step 6: Tests pass**

`go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/ ./internal/query/` → PASS.

- [ ] **Step 7: fmt/vet; commit**

```bash
go fmt ./... && go vet ./... && git add -A
git commit -m "feat(cache): derive is_from_me and export identity datasets (schema v13)"
```

---

### Task 4: Identity-dataset refresh in `internal/cacheops` + account-identity revision bump

**Files:**
- Create: `internal/cacheops/identity_refresh.go`
- Create: `internal/cacheops/identity_refresh_test.go`
- Modify: `cmd/msgvault/cmd/build_cache.go` (reuse for the identity-drift-only staleness path)
- Modify: `internal/store/account_identities.go` (`AddAccountIdentity`/`RemoveAccountIdentity` bump the identity revision in the same transaction, per the spec: "confirming or removing an account identity ... changes which participants are owners for a source"; without the bump, `owner_participants` stays silently stale). Acquire `lockIdentityMutationTx` first, mirroring `LinkParticipants`. Bump only on actual mutation (adding an identity that already exists or removing one that doesn't must NOT bump — idempotency per spec). Tests: add → revision+1; duplicate add → unchanged; remove → +1; remove missing → unchanged.

**Interfaces (produces):**
```go
// RefreshIdentityDatasets re-exports owner_participants and
// participant_clusters from the archive into the committed cache without
// touching message datasets, and stamps the new identity revision into
// _last_sync.json. Safe to call concurrently with readers: datasets are
// staged and renamed like a full publication. Returns the stamped revision.
func RefreshIdentityDatasets(ctx context.Context, st *store.Store, analyticsDir string) (int64, error)
```

- [ ] **Step 1: Write the failing test**

Build a small cache from a seeded store (reuse the cmd build helpers if exported, else seed + run the same export the cmd test does — if the cmd helpers are unexported, construct the test at the `cacheops` level: write a minimal valid cache with the query fixture builder, then point `RefreshIdentityDatasets` at a real seeded store + that analyticsDir). Assert: after `LinkParticipants(a, b)` + refresh, the `participant_clusters` Parquet contains the new mapping, `_last_sync.json`'s `identity_revision` equals `store.IdentityRevision()`, and the message datasets' files are untouched (same mtimes/fingerprint).

- [ ] **Step 2: Implement**

The implementation moves/extracts the minimal staging pieces: create a same-parent staging dir (mirror `newCacheStaging`'s `.name.build-<id>` convention from `cmd/msgvault/cmd/cache_publication.go` — extract shared helpers into `internal/cacheops` if the cmd package can consume them back without an import cycle; `cmd` imports `internal/cacheops`, never the reverse). Export the two datasets (same SQL as Task 4 exports — owner_participants via a short-lived DuckDB connection ATTACHing the SQLite file read-only, participant_clusters from `st.ParticipantClusters()` written via DuckDB temp table). Publish: read current `CacheSyncState`, invalidate, rename the two dataset dirs into place, re-fingerprint, write state with updated `IdentityRevision` + `PublishedAt`. If no committed cache exists (no `_last_sync.json`), return a typed error — the API maps it to the standard unavailable-cache response.

PostgreSQL note: the DuckDB SQLite-ATTACH path only works for SQLite archives. When `st.IsPostgreSQL()`, derive `owner_participants` rows in Go via store queries (`ListAccountIdentities` + participant lookups) and write both datasets through DuckDB temp tables. (Relationships ranking itself is DuckDB-cache-only per the spec, but the refresh must not crash a PG archive.)

- [ ] **Step 3: Wire the identity-drift staleness path**

In the build-cache flow (where `cacheStaleness.HasIdentityDrift` is the only signal), call `cacheops.RefreshIdentityDatasets` instead of a message rebuild.

- [ ] **Step 4: Tests pass; fmt/vet; commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/cacheops/ ./cmd/msgvault/cmd/
go fmt ./... && go vet ./... && git add -A
git commit -m "feat(cacheops): refresh identity datasets without message rebuild"
```

---

### Task 5: Relationship ranking — score function, DuckDB query, endpoint

**Files:**
- Create: `internal/query/relationships.go`
- Create: `internal/query/relationships_test.go`
- Create: `internal/api/relationships.go`
- Create: `internal/api/relationships_test.go`
- Modify: `internal/api/routes.go` (~line 209: `s.registerRelationshipRoutes(apiV1)`)
- Modify: `internal/api/openapi.go` (version 1.19.0 + changelog note)

**Interfaces (produces):**
```go
// internal/query
type RelationshipSignals struct {
	SentToThem        float64 // decayed sum
	ReceivedFromThem  float64
	MeetingsTogether  float64
	SentCount         int64   // raw counts (for the gate)
	MeetingCount      int64
	Modalities        int     // distinct of email/chat/meeting, 1..3
	LastInteractionAt time.Time
}
// RelationshipScore applies the spec's weights, decay having been applied in
// SQL: score = (2.0*sent + 3.0*meetings + 1.0*received) * (1 + 0.25*(modalities-1)).
func RelationshipScore(s RelationshipSignals) float64

type RelationshipRow struct {
	CanonicalID   int64    `json:"canonical_id"`
	DisplayLabel  string   `json:"display_label"`
	MemberIDs     []int64  `json:"member_ids"`
	Score         float64  `json:"score"`
	Signals       RelationshipSignals `json:"signals"`
	LastAt        time.Time `json:"last_at"`
}
type RelationshipsRequest struct {
	Context  Context
	ShowAll  bool  // lifts the reciprocity gate
	Limit    int
	Offset   int
	Now      time.Time // injected for deterministic decay in tests
}
type RelationshipsResponse struct {
	Rows          []RelationshipRow
	TotalCount    int64
	CacheRevision string
	IdentityRevision int64
}
type RelationshipAnalyzer interface {
	Relationships(ctx context.Context, request RelationshipsRequest) (*RelationshipsResponse, error)
}
```
- HTTP: `POST /api/v1/relationships` — body `{show_all?, limit?, cursor?, filters?}`; there is no `query`/`search_mode` (ranking is over reciprocity signals, not lexical/semantic search candidates — the shipped `RelationshipsHTTPRequest` rejects unknown fields); 503 unavailable-cache when the engine is not a `RelationshipAnalyzer` (exactly the `people.go:106-108` pattern); cursor/revision authority per the explore conventions.

- [ ] **Step 1: Score function TDD**

Table-driven `TestRelationshipScore` covering: weights (a single decayed sent unit scores 2.0; meeting 3.0; received 1.0), breadth boost (same signals with 3 modalities = 1.5×), and zero signals → 0. Then implement `RelationshipScore` — one documented function, exactly the spec constants:

```go
const (
	relationshipWeightSent     = 2.0
	relationshipWeightMeetings = 3.0
	relationshipWeightReceived = 1.0
	relationshipBreadthStep    = 0.25
	relationshipHalfLifeDays   = 365.0
)
```

- [ ] **Step 2: Ranking query TDD (fixture builder)**

`TestRelationshipsRanksByReciprocityAndGatesNewsletters`: builder fixture with an owner participant (`AddOwnerParticipant`), person A (owner sent 3 messages to them — `IsFromMe: true` with A as recipient — plus 1 meeting together), person B (newsletter: 50 inbound messages, zero sent/meetings), a cluster linking A with a second identity A2 (`LinkCluster(A, A2)`) where A2 has chat messages. Assert: default response ranks A first with cluster-combined signals under `canonical_id = min(A, A2)`, B is absent (gate); `ShowAll: true` includes B; deterministic order with injected `Now`.

- [ ] **Step 3: Implement the DuckDB query**

`(e *DuckDBEngine) Relationships(...)` in `internal/query/relationships.go`, following `searchPeople`'s structure (query slot, `ReadCacheSyncState` for revision, `buildExploreLogicalSQL(conditions)` for context scoping). Core SQL shape (decay computed in SQL, score in Go):

```sql
WITH clusters AS (
    SELECT participant_id, canonical_id FROM read_parquet('<analytics>/participant_clusters/*.parquet')
), owners AS (
    SELECT DISTINCT participant_id FROM read_parquet('<analytics>/owner_participants/*.parquet')
), canon AS (  -- every participant resolves to itself unless clustered
    SELECT p.id AS participant_id, COALESCE(c.canonical_id, p.id) AS canonical_id
    FROM participants_view p LEFT JOIN clusters c ON c.participant_id = p.id
), owner_canon AS (
    SELECT DISTINCT cn.canonical_id FROM owners o JOIN canon cn ON cn.participant_id = o.participant_id
), interactions AS (
    SELECT
        cn.canonical_id,
        le.entry_kind,
        le.occurred_at,
        le.is_from_me,
        exp(-0.6931471805599453 * date_diff('day', le.occurred_at, ?) / 365.0) AS decay
    FROM logical_entries le
    CROSS JOIN UNNEST(le.participant_ids) AS pid(participant_id)
    JOIN canon cn ON cn.participant_id = pid.participant_id
    WHERE cn.canonical_id NOT IN (SELECT canonical_id FROM owner_canon)
)
SELECT
    canonical_id,
    SUM(CASE WHEN is_from_me AND entry_kind IN ('email','conversation','item') THEN decay ELSE 0 END) AS sent_decayed,
    COUNT(CASE WHEN is_from_me AND entry_kind IN ('email','conversation','item') THEN 1 END)          AS sent_count,
    SUM(CASE WHEN NOT is_from_me AND entry_kind IN ('email','conversation','item') THEN decay ELSE 0 END) AS received_decayed,
    SUM(CASE WHEN entry_kind IN ('event','meeting') THEN decay ELSE 0 END) AS meetings_decayed,
    COUNT(CASE WHEN entry_kind IN ('event','meeting') THEN 1 END)          AS meeting_count,
    COUNT(DISTINCT CASE
        WHEN entry_kind IN ('event','meeting') THEN 'meeting'
        WHEN entry_kind = 'conversation' THEN 'chat'
        ELSE 'email' END) AS modalities,
    MAX(occurred_at) AS last_at
FROM interactions
GROUP BY canonical_id
```

Notes for the implementer: `logical_entries` must carry `is_from_me` — extend `buildExploreLogicalSQL` (explore.go:365-448) to project it (anchor-message value via `arg_max` for conversation rows), guarded by the `hasCol` optional-column mechanism; a meeting "together" is any event/meeting entry whose participants include both an owner and the person — since `interactions` already excludes owner clusters and events carry all attendees in `participant_ids`, add `AND EXISTS`-style owner check for the meeting rows (`list_has_any` of the entry's participant_ids against the owners list) — implement as an additional boolean column `with_owner` computed before the UNNEST, and require it in the meeting CASE arms. Display label + member IDs come from a second small query joining `canon` back to `participants_view` (min-ID display name, `list(participant_id)`). Gate + score + sort (score DESC, last_at DESC, label ASC) in Go via `RelationshipScore`; apply `ShowAll` by skipping the gate filter. Paginate with limit/offset like `searchPeople`.

- [ ] **Step 4: API endpoint**

`internal/api/relationships.go`: `registerRelationshipRoutes` with `POST /relationships` (Huma, mirroring `registerPeopleRoutes`' helper usage); handler: decode via `decodeExploreJSON`, clamp limit, resolve search/snapshot via `prepareIdentitySearch`-equivalent (reuse it if its shape fits — it lives in people.go and is server-scoped), type-assert `query.RelationshipAnalyzer` else `writeExploreUnavailable(w, query.CacheAbsent)`, cursor binds `{Request, Revision, IdentityRevision, Offset}` in a signed cursor (extend `exploreCursor` with `IdentityRevision int64 \`json:"identity_revision,omitempty"\`` rather than a new type), 409 `archive_revision_changed` on cache-revision drift and `identity_revision_changed` on identity drift. Response echoes `CacheRevision` and `IdentityRevision`. API test: seeded engine fixture (builder) through `newTestServerWithEngine`, asserting ranking order, the gate, `show_all`, 409 on doctored cursor, and 503 under a non-analyzer engine.

- [ ] **Step 5: OpenAPI + clients**

Bump `APISchemaVersion` to 1.19.0 with a changelog comment; `make openapi`; `cd web && bun run generate`; commit generated files.

- [ ] **Step 6: Tests, fmt/vet, commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/query/ ./internal/api/
go fmt ./... && go vet ./... && git add -A
git commit -m "feat(api): relationship ranking endpoint over identity clusters"
```

---

### Task 6: Relationship timeline endpoint with day bursts

**Files:**
- Create: `internal/query/relationship_timeline.go` (+ `_test.go`)
- Modify: `internal/api/relationships.go` (+ tests)

**Interfaces (produces):**
```go
// internal/query
type RelationshipTimelineRequest struct {
	CanonicalID int64
	Timezone    string // validated IANA name; "" = UTC
	Context     Context
	Limit       int
	Offset      int
}
type TimelineRow struct {
	Key            string    `json:"key"`  // message:<id> | burst:<source>:<conversation>:<yyyy-mm-dd>
	Kind           string    `json:"kind"` // email | event | meeting | chat_burst
	Title          string    `json:"title"`
	Preview        string    `json:"preview"`
	OccurredAt     time.Time `json:"occurred_at"` // burst: latest message time
	FirstAt        time.Time `json:"first_at,omitempty"`
	MessageCount   int64     `json:"message_count"`
	SourceID       int64     `json:"source_id"`
	ConversationID *int64    `json:"conversation_id,omitempty"`
	AnchorMessageID *int64   `json:"anchor_message_id,omitempty"`
	HasAttachments bool      `json:"has_attachments"`
}
// added to RelationshipAnalyzer:
RelationshipTimeline(ctx context.Context, request RelationshipTimelineRequest) (*RelationshipTimelineResponse, error)
```
- HTTP: `POST /api/v1/relationships/{id}/timeline` — `{timezone?, filters?, limit?, cursor?}`; no `query`/`search_mode` (same rationale as the ranking endpoint above — the shipped `RelationshipTimelineHTTPRequest` rejects unknown fields). `{id}` accepts any member participant ID and resolves to the canonical cluster, echoing `canonical_id` in the response. Cursor binds canonical ID + timezone + request hash + cache revision + identity revision; any mismatch → 409 `cursor_invalidated`.

- [ ] **Step 1: Query TDD**

`TestRelationshipTimelineBurstsChatByLocalDay`: builder fixture — cluster person with 3 chat messages in one conversation on 2026-07-13 (two at 23:00/23:30 UTC, one at 00:30 UTC next day) plus an email and a meeting. With `Timezone: "America/Chicago"` all three chat messages fall on the same local day → assert ONE `chat_burst` row with `MessageCount: 3`, correct first/last times, and the latest snippet as preview; with UTC they split into two bursts. Emails/meetings stay one row each. Ordering `(occurred_at DESC, key)`.

- [ ] **Step 2: Implement the query**

Validate the timezone in Go first (`time.LoadLocation`); reject invalid names with `ErrInvalidExploreRequest` context. In DuckDB, named-timezone conversion needs the ICU extension — load it in `NewDuckDBEngine` next to the sqlite extension (`INSTALL icu; LOAD icu;`, mirroring duckdb.go:116-153; verify with a probe query in the engine test; if the bundled build already auto-loads ICU, the probe documents it). SQL shape: start from `logical_entries` scoped to the cluster's member IDs (resolve members in Go via the clusters dataset or store), but replace the chat grouping key: instead of the existing per-conversation-lifetime grouping, group chat by `(source_id, conversation_id, strftime(timezone(?, occurred_at), '%Y-%m-%d'))`, `arg_max` for snippet/anchor, `COUNT(*)`, `MIN/MAX(occurred_at)`; non-chat rows pass through one-per-message. Reuse `FlattenSnippet` on previews. Order `(occurred_at DESC, entry_key ASC)`, limit/offset.

Everything's existing conversation-row semantics are untouched — this is a separate query path, not a change to `buildExploreLogicalSQL`'s chat branch (extract shared CTE-text helpers rather than forking string constants wholesale).

- [ ] **Step 3: API handler + cursor**

Route `POST /relationships/{id}/timeline`. Resolve `{id}` → canonical + members (via the engine's clusters dataset so the API needs no store round-trip; a miss = single-member cluster). Cursor: the extended `exploreCursor` (Task 5) plus `Timezone string` and `CanonicalID int64` fields; on any mismatch (request hash, revision, identity revision, timezone, canonical) → 409 `cursor_invalidated` with the message "The timeline context changed; restart pagination". Tests: burst pagination across two pages with a stable cursor; 409 after a simulated identity-revision change (rebuild fixture with a new link); 400 on a bad timezone.

- [ ] **Step 4: OpenAPI regen; tests; fmt/vet; commit**

```bash
make openapi && (cd web && bun run generate)
go test -tags "fts5 sqlite_vec" ./internal/query/ ./internal/api/
go fmt ./... && go vet ./... && git add -A
git commit -m "feat(api): relationship timeline with local-day chat bursts"
```

---

### Task 7: Identity link/unlink endpoints with synchronous refresh

**Files:**
- Create: `internal/api/identity_links.go` (+ `identity_links_test.go`)
- Modify: `internal/api/routes.go`; `cmd/msgvault/cmd/serve.go` (adapter wiring)

**Interfaces (produces):**
```go
// internal/api capability interfaces (implemented by storeAPIAdapter in serve.go):
type IdentityLinkStore interface {
	LinkParticipants(a, b int64) (int64, error)
	UnlinkParticipants(a, b int64) (int64, error)
}
type IdentityCacheRefresher interface {
	RefreshIdentityDatasets(ctx context.Context) (int64, error) // wraps cacheops with the daemon's analyticsDir
}

type IdentityLinkRequest struct {
	ParticipantA int64 `json:"participant_a"`
	ParticipantB int64 `json:"participant_b"`
}
type IdentityLinkResponse struct {
	IdentityRevision int64  `json:"identity_revision"`
	CacheState       string `json:"cache_state" enum:"ready,stale"`
}
```
- HTTP: `POST /api/v1/identity/links` (link) and `POST /api/v1/identity/unlinks` (unlink — Huma DELETE-with-body is awkward; a verb-named POST matches the existing `/explore/preflight` style). Semantics per spec: idempotent; mutation commits first; refresh attempted synchronously; `200 {identity_revision, cache_state: ready|stale}`; `ErrAlreadyLinked` → `409 already_linked` with a message explaining the participants are already connected through other links; invalid IDs → 400.

- [ ] **Step 1: API tests first**

Through `newTestServerWithMockStore`-style setup with a real `testutil.NewTestStore` behind the capability interfaces (follow `conversations_test.go`'s real-store pattern): link two seeded participants → 200 with revision 1 and `cache_state: "ready"` (stub refresher returns success); repeat the same link → 200 same revision (idempotent); link two indirectly-connected → 409 `already_linked`; refresher fails → still 200, `cache_state: "stale"`; unlink → 200 revision bumped; unlink again → 200 unchanged.

- [ ] **Step 2: Implement handlers + wiring**

Handlers: validate IDs, call the store method, on `ErrAlreadyLinked` → `writeError(w, http.StatusConflict, "already_linked", ...)`; then call the refresher with the request context — on error, log and set `cache_state: "stale"` (never fail the response; the mutation is durable). Wire `storeAPIAdapter` pass-throughs in serve.go plus a small refresher struct closing over the analytics dir (`cacheops.RefreshIdentityDatasets(ctx, a.store, analyticsDir)`); register the interface assertions alongside the existing `var _ api.X = (*storeAPIAdapter)(nil)` block.

- [ ] **Step 3: OpenAPI regen; tests; fmt/vet; commit**

```bash
make openapi && (cd web && bun run generate)
go test -tags "fts5 sqlite_vec" ./internal/api/
go fmt ./... && go vet ./... && git add -A
git commit -m "feat(api): identity link endpoints with synchronous cache refresh"
```

---

### Task 8: Conversation endpoint UTC time-window bounds

**Files:**
- Modify: `internal/api/conversations.go` (+ `conversations_test.go`)
- Modify: `internal/store/conversations.go` (`GetConversationWindowContext`)

**Interfaces (produces):**
- `GET /api/v1/conversations/{id}?anchor=N&before=B&after=A&start=RFC3339&end=RFC3339` — optional `start`/`end` (UTC, half-open `[start, end)`). When present, the window and the before/after counts are computed only over messages inside the range; `HasBefore`/`HasAfter` are relative to the range. Invalid timestamps or `start >= end` → 400. The bounded-counts contract (default 25, max 50) is unchanged within the range.
- Store: `GetConversationWindowContext` gains optional `start, end *time.Time` parameters (extend the existing signature or add a variant per the file's conventions — check call sites first with `rg -n "GetConversationWindowContext" internal/`).

- [ ] **Step 1: Failing tests**

Extend `conversations_test.go` using `seedConversation`: seed 6 messages across two days; request with `start`/`end` covering day 2 and an anchor inside it → only day-2 messages, `HasBefore=false` even though earlier messages exist outside the range; anchor outside the range → 400 with a named error; malformed `start` → 400.

- [ ] **Step 2: Implement**

Store: thread the bounds into the window CTE — the `ROW_NUMBER()` ordering CTE gains `AND COALESCE(sent_at, received_at, internal_date) >= ? AND ... < ?` when bounds are set (dialect `Rebind`, conditional SQL fragments matching the file's `LiveMessagesWhere` templating style). API: parse RFC3339 `start`/`end` query params (reject non-UTC-parseable values with the existing `newParamError` helper), validate ordering, pass through.

- [ ] **Step 3: OpenAPI regen (route description/params changed); tests; fmt/vet; commit**

```bash
make openapi && (cd web && bun run generate)
go test -tags "fts5 sqlite_vec" ./internal/api/ ./internal/store/
go fmt ./... && go vet ./... && git add -A
git commit -m "feat(api): optional UTC time-window bounds on conversation windows"
```

---

### Task 9: Final verification

- [ ] **Step 1: Full suites + lint**

```bash
make test
make lint-ci
cd web && bun run check && bun run test   # generated schema.d.ts must not break the web build
```

If `MSGVAULT_TEST_DB` is configured in the environment, also run `make test-pg` (participant_links + revision live in both schemas).

- [ ] **Step 2: End-to-end smoke on a synthetic archive**

Build a fake vault (`./msgvault fake-vault -o <scratch> --messages 2000`), add a confirmed identity for its source (`sqlite3` insert or the CLI if one exists — check `./msgvault --help` for an identities command), `MSGVAULT_HOME=<scratch> ./msgvault build-cache`, `serve`, then curl:
- `POST /api/v1/relationships` → ranked rows, gate visibly excluding no-reply senders.
- `POST /api/v1/identity/links` on two participant IDs → 200 ready; re-run relationships → combined cluster.
- `POST /api/v1/relationships/{id}/timeline` with a timezone → burst rows.
- `GET /api/v1/conversations/{id}?...&start=&end=` → bounded window.
Stop the daemon (`msgvault daemon stop`) when done.

- [ ] **Step 3: Commit any stragglers**

```bash
git status && git add -A && git commit -m "chore: relationships backend follow-ups"
```
(Skip if clean.)
