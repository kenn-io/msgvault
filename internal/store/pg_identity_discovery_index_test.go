package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// concurrentlyBuiltIndexes are the indexes Store.buildLargeIndexesConcurrently
// owns on PostgreSQL; none of them appear in schema_pg.sql, so these tests are
// the only guard that the concurrent path actually creates them.
var concurrentlyBuiltIndexes = []struct{ index, table string }{
	{"idx_messages_source_id", "messages"},
	{"idx_participants_email_lower", "participants"},
	{"idx_participant_identifiers_value_lower", "participant_identifiers"},
}

// Both probes below check pg_index.indisvalid rather than just counting rows
// in pg_indexes: pg_indexes lists an INVALID leftover (from a CREATE INDEX
// CONCURRENTLY that failed or was cancelled partway through) the same as a
// healthy index, so a plain row count cannot distinguish "built and usable"
// from "present but unusable" — the failure this package's concurrent-build
// recovery path exists to catch.

func probeValidIndex(t *testing.T, st *store.Store, index, table string) bool {
	t.Helper()
	var valid bool
	require.NoError(t, st.DB().QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_class c
			JOIN pg_index i ON i.indexrelid = c.oid
			JOIN pg_class t ON t.oid = i.indrelid
			WHERE c.relname = $1
			  AND t.relname = $2
			  AND c.relnamespace = current_schema()::regnamespace
			  AND i.indisvalid = true
		)
	`, index, table).Scan(&valid))
	return valid
}

func TestInitSchema_PGCreatesIdentityDiscoveryIndexForFreshAndUpgradedStores(t *testing.T) {
	require := require.New(t)
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PG-only: identity discovery index maintenance build requires PostgreSQL")
	}

	st := testutil.NewTestStore(t)
	for _, target := range concurrentlyBuiltIndexes {
		require.True(probeValidIndex(t, st, target.index, target.table),
			"fresh schema must concurrently build %s", target.index)
		_, err := st.DB().Exec(`DROP INDEX ` + target.index)
		require.NoError(err, "simulate a store created before %s existed", target.index)
		require.False(probeValidIndex(t, st, target.index, target.table))
	}

	require.NoError(st.InitSchema(), "concurrent index build must restore the dropped indexes")
	for _, target := range concurrentlyBuiltIndexes {
		require.True(probeValidIndex(t, st, target.index, target.table),
			"upgraded schema must restore a valid %s", target.index)
	}
}

// TestInitSchema_PGConcurrentIndexBuildIsIdempotent exercises repeated calls
// against already-valid indexes. buildLargeIndexesConcurrently is
// unexported, so this drives it indirectly through InitSchema, matching how
// production calls it on every store open.
//
// The drop-retry branch (recovering from a leftover INVALID index left by a
// CREATE INDEX CONCURRENTLY that failed or was cancelled partway through) is
// not covered here: forcing that state requires cancelling a concurrent
// build from a second connection mid-build, which is racy and
// environment-dependent in CI. That branch is reviewed by inspection in
// Store.buildLargeIndexesConcurrently / dropInvalidIndexConcurrently.
func TestInitSchema_PGConcurrentIndexBuildIsIdempotent(t *testing.T) {
	require := require.New(t)
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PG-only: identity discovery index maintenance build requires PostgreSQL")
	}

	st := testutil.NewTestStore(t)
	for range 3 {
		for _, target := range concurrentlyBuiltIndexes {
			require.True(probeValidIndex(t, st, target.index, target.table),
				"repeated concurrent build must keep %s valid and idempotent", target.index)
		}
		require.NoError(st.InitSchema(), "repeated call must be a no-op, not an error")
	}
}
