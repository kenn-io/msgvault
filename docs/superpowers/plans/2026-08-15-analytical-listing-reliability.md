# Analytical Listing Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make default Everything and Files listings succeed on multi-million-message archives within the existing interactive DuckDB budgets.

**Architecture:** Add a reusable narrow analytical-entry relation that omits archive-wide participant-list aggregation. The fast listing paths page and count over this scalar relation, then retain their existing page-only participant enrichment. Legacy and participant-list-filtered queries remain unchanged.

**Tech Stack:** Go 1.26.6, DuckDB 1.5 through `duckdb-go/v2`, Parquet fixtures, Testify, Bun documentation tooling.

## Global Constraints

- Keep the interactive memory limit at 512 MB and ordinary request deadlines unchanged.
- Do not change the persisted analytics-cache schema or require a cache rebuild.
- Preserve exact ordering, pagination, totals, participant enrichment, and API response shapes.
- Do not scan or join `message_bodies` from listing queries.
- Use only synthetic identities and data in tests and public artifacts.
- Run Go tests with `-tags "fts5 sqlite_vec"`.
- Use Testify assertions, never `testing.T` fatal/error helpers.
- Run `go fmt ./...` and `go vet ./...` after Go changes.

---

## File map

- `internal/query/views.go`: owns the scalar analytical-entry projection shared by bounded listing queries.
- `internal/query/explore.go`: builds the Everything fast path over the scalar projection.
- `internal/query/files.go`: builds the Files fast path over the scalar projection.
- `internal/query/listing_resource_test.go`: proves real listing behavior under constrained DuckDB memory.
- `internal/query/explore_fastpath_test.go`: preserves fast-path and legacy semantic equivalence.
- `internal/query/explore_benchmark_test.go`: records first-page performance for both listings on the 100K-message fixture.
- `docs/architecture/storage.md`: records the page-before-enrichment resource boundary.
- `docs/changelog.md`: records the user-visible large-archive fix.

### Task 1: Narrow the Everything listing working set

**Files:**
- Create: `internal/query/listing_resource_test.go`
- Modify: `internal/query/views.go`
- Modify: `internal/query/explore.go`

**Interfaces:**
- Produces: `buildNarrowAnalyticalEntriesCTE(name string) string`, a SQL CTE containing scalar message, source, conversation, sender, deletion, and attachment-summary fields but no participant lists.
- Produces: `buildExploreNarrowFilteredClassifiedCTE(conditions, candidateRankExpression string) string` for the default Everything fast path.
- Consumes: existing `buildExploreConditions`, `exploreLogicalEntriesCTE(false)`, page-enrichment CTEs, and `buildBenchData`.

- [ ] **Step 1: Write the constrained-memory Everything test**

Create `internal/query/listing_resource_test.go` with a real engine and the generated mixed archive:

```go
package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func constrainedListingEngine(t *testing.T) *DuckDBEngine {
	t.Helper()
	engine := buildBenchData(t)
	_, err := engine.db.Exec("SET memory_limit='64MB'")
	require.NoError(t, err)
	return engine
}

func TestExploreFastPathFitsConstrainedMemory(t *testing.T) {
	engine := constrainedListingEngine(t)
	result, err := engine.Explore(context.Background(), ExploreRequest{
		Page: PageSpec{Limit: 50},
	})
	require.NoError(t, err)
	assert.Len(t, result.Rows, 50)
	assert.Equal(t, int64(104), result.TotalCount)
}
```

- [ ] **Step 2: Run the test and confirm the old query exceeds the test budget**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query -run '^TestExploreFastPathFitsConstrainedMemory$' -count=1 -v
```

Expected: FAIL from DuckDB memory exhaustion while the old fast path evaluates archive-wide participant facts. If the host can spill enough to pass at 64 MB, lower only this test connection to `48MB`; do not change production policy.

- [ ] **Step 3: Add the reusable narrow analytical-entry CTE**

Add this helper beside `sqlAnalyticalEntries` in `internal/query/views.go`, keeping expressions identical to the scalar portion of that view:

```go
func buildNarrowAnalyticalEntriesCTE(name string) string {
	return name + ` AS (
	SELECT
		m.id AS message_id,
		m.source_id,
		COALESCE(s.source_type, '') AS source_type,
		COALESCE(s.account_email, '') AS source_identifier,
		m.source_message_id,
		m.conversation_id,
		COALESCE(c.source_conversation_id, '') AS source_conversation_id,
		COALESCE(c.conversation_type, '') AS conversation_type,
		COALESCE(c.title, '') AS conversation_title,
		COALESCE(m.message_type, '') AS message_type,
		m.sender_id,
		COALESCE(NULLIF(sender.email_address, ''), NULLIF(sender.phone_number, ''), '') AS sender_identifier,
		COALESCE(NULLIF(sender.display_name, ''), NULLIF(sender.phone_number, ''), sender.email_address, '') AS sender_display,
		COALESCE(sender.domain, '') AS sender_domain,
		m.sent_at AS occurred_at,
		COALESCE(m.subject, '') AS subject,
		COALESCE(m.snippet, '') AS snippet,
		m.is_from_me,
		m.size_estimate,
		m.deleted_at IS NOT NULL AS internally_deleted,
		m.deleted_from_source_at IS NOT NULL AS deleted_from_source,
		COALESCE(m.has_attachments, false) AS has_attachments,
		COALESCE(att.attachment_count, 0) AS attachment_count,
		COALESCE(att.attachment_size, 0) AS attachment_size
	FROM messages m
	JOIN sources s ON s.id = m.source_id
	LEFT JOIN conversations c ON c.id = m.conversation_id
	LEFT JOIN participants sender ON sender.id = m.sender_id
	LEFT JOIN (
		SELECT message_id, COUNT(*) AS attachment_count,
			COALESCE(SUM(size), 0) AS attachment_size
		FROM attachments GROUP BY message_id
	) att ON att.message_id = m.id
)`
}
```

- [ ] **Step 4: Route the Everything fast path through the narrow relation**

Add this constructor in `internal/query/explore.go`:

```go
func buildExploreNarrowFilteredClassifiedCTE(conditions, candidateRankExpression string) string {
	return "WITH " + buildNarrowAnalyticalEntriesCTE("entry_core") + `,
filtered AS (
	SELECT * FROM entry_core AS analytical_entries WHERE ` + conditions + `
), classified AS (
	SELECT *, ` + candidateRankExpression + ` AS candidate_rank,
		` + sqlIsChatPredicate("message_type", "conversation_type") + ` AS is_chat,
		` + identityindex.EntryKindSQL("message_type") + ` AS entry_kind
	FROM filtered
)`
}
```

In `buildExploreFastListingSQL`, replace `buildExploreFilteredClassifiedCTE` with this narrow constructor. Change the `membership` and `total` scans from `analytical_entries` to `entry_core AS analytical_entries`; the alias preserves identity-predicate qualification without reading the wide convenience view.

- [ ] **Step 5: Run focused behavior and equivalence tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query \
  -run '^(TestExploreFastPathFitsConstrainedMemory|TestExploreListingFastPath)' \
  -count=1 -v
```

Expected: PASS. The constrained test returns exactly 50 of 104 logical entries, and all existing fast/legacy equivalence cases remain equal.

- [ ] **Step 6: Format and commit Task 1**

Run `go fmt ./...`, review `git diff`, then use the `kenn:commit` workflow with subject:

```text
fix(analytics): bound Everything listing memory
```

### Task 2: Narrow the Files listing working set

**Files:**
- Modify: `internal/query/listing_resource_test.go`
- Modify: `internal/query/files.go`

**Interfaces:**
- Consumes: `buildNarrowAnalyticalEntriesCTE(name string)` from Task 1.
- Produces: `fileNarrowFilteredCTE(exploreConditions, fileConditions string) string` for the default Files fast path.
- Preserves: existing `fileFilteredCTE` and `fileSearchSQL` for legacy and participant-list-filtered queries.

- [ ] **Step 1: Add the constrained-memory Files test**

Append:

```go
func TestFileSearchFastPathFitsConstrainedMemory(t *testing.T) {
	engine := constrainedListingEngine(t)
	result, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Page: PageSpec{Limit: 500},
	})
	require.NoError(t, err)
	assert.Len(t, result.Files, 500)
	assert.Equal(t, int64(20_000), result.TotalCount)
}
```

- [ ] **Step 2: Run the test and confirm the wide page sort fails**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query -run '^TestFileSearchFastPathFitsConstrainedMemory$' -count=1 -v
```

Expected: FAIL with DuckDB memory exhaustion because `fileFilteredCTE` carries participant lists through `page ORDER BY`. Use the same test-only memory value established in Task 1.

- [ ] **Step 3: Add the narrow Files CTE**

Add beside `fileFilteredCTE`:

```go
func fileNarrowFilteredCTE(exploreConditions, fileConditions string) string {
	return "WITH " + buildNarrowAnalyticalEntriesCTE("entry_core") + `,
selected AS (
	SELECT * FROM entry_core AS analytical_entries WHERE ` + exploreConditions + `
), classified AS (
	SELECT
		a.attachment_id, a.message_id, COALESCE(a.size, 0)::BIGINT AS size,
		COALESCE(a.filename, '') AS filename,
		COALESCE(a.mime_type, '') AS mime_type,
		` + sqlFileMIMEFamilyExpr("a") + ` AS mime_family,
		s.*
	FROM selected s JOIN attachments a ON a.message_id = s.message_id
), filtered AS (
	SELECT * FROM classified WHERE ` + fileConditions + `
)`
}
```

- [ ] **Step 4: Route only the Files fast path through the narrow relation**

In `buildFileSearchFastSQL`, replace `fileFilteredCTE(...)` with `fileNarrowFilteredCTE(...)`. In its `total` CTE, replace the inner `analytical_entries` scan with `entry_core AS analytical_entries`. Keep `page_facts` unchanged so participant IDs, labels, and domains still resolve only for bounded page rows.

- [ ] **Step 5: Run Files behavior and equivalence tests**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query \
  -run '^(TestFileSearchFastPathFitsConstrainedMemory|TestSearchFilesFastPath)' \
  -count=1 -v
```

Expected: PASS. The constrained test returns exactly 500 of 20,000 files, and every existing fast/legacy equivalence case remains equal.

- [ ] **Step 6: Format and commit Task 2**

Run `go fmt ./...`, review `git diff`, then use the `kenn:commit` workflow with subject:

```text
fix(analytics): bound Files listing memory
```

### Task 3: Extend diagnostic performance coverage and living docs

**Files:**
- Modify: `internal/query/explore_benchmark_test.go`
- Modify: `docs/architecture/storage.md`
- Modify: `docs/changelog.md`

**Interfaces:**
- Consumes: production `Explore` and `SearchFiles` behavior from Tasks 1 and 2.
- Produces: `BenchmarkExploreLargeArchive/files_first_page` diagnostic benchmark.

- [ ] **Step 1: Add the Files first-page benchmark**

Inside `BenchmarkExploreLargeArchive`, add:

```go
b.Run("files_first_page", func(b *testing.B) {
	for b.Loop() {
		result, err := engine.SearchFiles(ctx, FileSearchRequest{
			Page: PageSpec{Limit: 500},
		})
		require.NoError(b, err)
		require.Len(b, result.Files, 500)
		require.Equal(b, int64(20_000), result.TotalCount)
	}
})
```

Update the benchmark comment to state that the fixture contains 100,004 messages, 104 logical entries, and 20,000 attachments, and that constrained-memory tests—not benchmark timing—enforce the resource contract.

- [ ] **Step 2: Run one diagnostic iteration**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query -run '^$' \
  -bench 'BenchmarkExploreLargeArchive/(first_page|files_first_page)$' \
  -benchtime=1x -count=1
```

Expected: both sub-benchmarks complete successfully and report one operation. Record timings in the work log only; do not add machine-specific thresholds.

- [ ] **Step 3: Update the living storage contract**

Add this paragraph after the cache query description in `docs/architecture/storage.md`:

```markdown
Ungrouped Everything and Files listings page a scalar message or attachment
population before resolving participant lists for the returned rows. Exact
totals use separate narrow scans. This page-before-enrichment boundary keeps
multi-million-message listings inside the daemon's interactive DuckDB memory
budget without changing cache format or query semantics.
```

- [ ] **Step 4: Add the changelog entry**

Under the current release's **Fixes** or **Improvements** section, add:

```markdown
- Everything and Files now page narrow analytical metadata before enriching
  participant details, preventing default listings on multi-million-message
  archives from exhausting the interactive DuckDB memory budget.
```

- [ ] **Step 5: Check docs and commit Task 3**

Run:

```bash
make docs-check
```

Expected: PASS.

Review the rendered Markdown diff, then use the `kenn:commit` workflow with subject:

```text
docs: record bounded analytical listings
```

### Task 4: Verify and open the generic pull request

**Files:**
- No production files expected.
- Modify only files required by formatting or generated-contract checks.

**Interfaces:**
- Consumes: all prior task commits.
- Produces: one reviewable pull request for bounded analytical listings.

- [ ] **Step 1: Run focused verification from a clean test process**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/query -count=1
go vet ./...
```

Expected: PASS.

- [ ] **Step 2: Run repository verification**

Run:

```bash
make test
make docs-check
```

Expected: PASS. If an unrelated platform or network gate fails, preserve the exact evidence and do not weaken tests.

- [ ] **Step 3: Review scope and scrub public artifacts**

Run:

```bash
git status --short
git diff --check origin/main...HEAD
git diff --stat origin/main...HEAD
git log --oneline origin/main..HEAD
```

Then run the `kenn:scrub-private-data` workflow over the complete branch diff.
Expected: a clean worktree, no whitespace errors, only the intended
query/tests/docs commits, and no private or personal data in public artifacts.

- [ ] **Step 4: Push and open the pull request**

Push `fix/analytical-query-reliability` and open a concise PR titled:

```text
fix(analytics): bound Everything and Files listings
```

The description must state only the problem, the bounded page-before-enrichment behavior, compatibility with existing caches, and the user-visible outcome. Do not add validation, test-plan, cloud-gate, or external-project sections.
