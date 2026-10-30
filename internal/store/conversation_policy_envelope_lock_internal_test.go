package store

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Envelope invalidation takes the recipient advisory lock only on
// PostgreSQL, so dialect detection must hold for real store transactions.
func TestPostgresQuerierMatchesStoreDialect(t *testing.T) {
	st := openTestStore(t)
	if testDB := os.Getenv("MSGVAULT_TEST_DB"); strings.HasPrefix(testDB, "postgres://") || strings.HasPrefix(testDB, "postgresql://") {
		st = newPGStoreInternal(t, testDB)
	}
	require.NoError(t, st.withTxContext(t.Context(), func(tx *loggedTx) error {
		assert.Equal(t, st.IsPostgreSQL(), postgresQuerier(tx))
		assert.Equal(t, st.IsPostgreSQL(), postgresQuerier(boundQuerier{ctx: t.Context(), q: tx}))
		return nil
	}))
}
