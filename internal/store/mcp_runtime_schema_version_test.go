package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested tests.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested tests.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPScopeRuntimeRequiresCurrentSchemaSetup(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	// Model a current-main archive before the first native scope migration.
	_, err := st.DB().Exec(`DROP TABLE native_message_scope_memberships`)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value='3' WHERE key='schema_version'`)
	require.NoError(err)
	require.Error(st.ValidateSchemaContext(t.Context()), "runtime readers must require setup before using the scope schema")
	// Validation must remain read-only; setup alone may recreate the table.
	var exists bool
	query := `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='native_message_scope_memberships')`
	if st.IsPostgreSQL() {
		query = `SELECT to_regclass('native_message_scope_memberships') IS NOT NULL`
	}
	require.NoError(st.DB().QueryRow(query).Scan(&exists))
	assert.False(exists)
	require.NoError(st.InitSchemaContext(t.Context()))
	require.NoError(st.ValidateSchemaContext(t.Context()))
	require.NoError(st.DB().QueryRow(query).Scan(&exists))
	assert.True(exists)
}
