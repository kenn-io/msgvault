package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available with local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available with local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportRuntimeRequiresCurrentSchemaSetup(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	// The preceding Native Events schema has no durable native import run or replay decisions yet.
	_, err := st.DB().Exec(`DROP TABLE import_message_decisions`)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value='3' WHERE key='schema_version'`)
	require.NoError(err)
	require.Error(st.ValidateSchemaContext(t.Context()), "runtime readers must require setup before using the import schema")
	var exists bool
	query := `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='import_message_decisions')`
	if st.IsPostgreSQL() {
		query = `SELECT to_regclass('import_message_decisions') IS NOT NULL`
	}
	require.NoError(st.DB().QueryRow(query).Scan(&exists))
	assert.False(exists, "readiness checks cannot migrate an archive")
	require.NoError(st.InitSchemaContext(t.Context()))
	require.NoError(st.ValidateSchemaContext(t.Context()))
	require.NoError(st.DB().QueryRow(query).Scan(&exists))
	assert.True(exists)
}
