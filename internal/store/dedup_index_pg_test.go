package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgCanonicalIndexValid reports whether the canonical RFC822 Message-ID
// expression index exists and is valid (not an INVALID leftover from a
// failed build) in the current schema. Mirrors the store_test probe in
// pg_identity_discovery_index_test.go; that one is not reachable from
// package-internal tests.
func pgCanonicalIndexValid(t *testing.T, s *Store) bool {
	t.Helper()
	var valid bool
	require.NoError(t, s.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_class c
			JOIN pg_index i ON i.indexrelid = c.oid
			JOIN pg_class t ON t.oid = i.indrelid
			WHERE c.relname = $1
			  AND t.relname = 'messages'
			  AND c.relnamespace = current_schema()::regnamespace
			  AND i.indisvalid = true
		)
	`, rfc822CanonicalIndexName).Scan(&valid))
	return valid
}

// TestFindDuplicatesByRFC822ID_PGCreatesAndUsesCanonicalExpressionIndex
// verifies the PostgreSQL half of the canonical Message-ID expression index:
// InitSchema builds it for fresh schemas, rebuilds it for existing databases
// that predate it, and the planner can match FindDuplicatesByRFC822ID's
// GROUP BY expression to the index expression for both account and collection
// source-count shapes.
//
// The plan evidence disables sequential scans, hash aggregation, and explicit
// sorts inside a rolled-back transaction: PostgreSQL's default cost-based
// planner legitimately prefers HashAggregate over a sequential scan for small
// tables, which would make an unforced plan assertion flaky. The forced ordered
// path proves the index expression matches the query's GROUP BY expression —
// the exact failure mode (silently falling back to a scan) that a drifted
// expression would produce.
func TestFindDuplicatesByRFC822ID_PGCreatesAndUsesCanonicalExpressionIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	s := newPGStoreInternal(t, dbURL)

	require.True(pgCanonicalIndexValid(t, s),
		"fresh schema must build "+rfc822CanonicalIndexName)

	_, err := s.db.Exec(`DROP INDEX ` + rfc822CanonicalIndexName)
	require.NoError(err, "drop the index to simulate a pre-index database")
	require.False(pgCanonicalIndexValid(t, s), "index must be gone after the drop")
	require.NoError(s.InitSchema(), "re-init must upgrade the existing database")
	require.True(pgCanonicalIndexValid(t, s),
		"upgraded database must rebuild "+rfc822CanonicalIndexName)

	conn, err := s.db.Conn(t.Context())
	require.NoError(err, "acquire connection")
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(err, "begin plan-explain transaction")
	defer func() { _ = tx.Rollback() }()

	for _, setting := range []string{"enable_seqscan", "enable_hashagg", "enable_sort"} {
		_, err := tx.ExecContext(t.Context(), "SET LOCAL "+setting+" = off")
		require.NoError(err, "set "+setting+" = off")
	}

	for _, sourceIDs := range [][]int64{{1}, {1, 2}} {
		query, args := s.findDuplicatesByRFC822IDQuery(sourceIDs)
		plan := func() string {
			rows, err := tx.QueryContext(t.Context(), "EXPLAIN "+s.Rebind(query), args...)
			require.NoError(err, "explain duplicate discovery plan")
			defer func() { require.NoError(rows.Close(), "close explain rows") }()

			var plan strings.Builder
			for rows.Next() {
				var line string
				require.NoError(rows.Scan(&line))
				plan.WriteString(line)
				plan.WriteString("\n")
			}
			require.NoError(rows.Err(), "iterate explain rows")
			return plan.String()
		}()

		assert.Contains(plan, "using "+rfc822CanonicalIndexName,
			"forced-ordered scoped grouping must scan the canonical/source index:\n%s", plan)
		assert.NotContains(plan, "Sort",
			"grouping must not fall back to an explicit sort when the index matches:\n%s", plan)
	}
}
