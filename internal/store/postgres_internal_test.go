package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresColumnExistsSQLScopesToCurrentSchema(t *testing.T) {
	query := postgresColumnExistsSQL("messages", "search_fts")

	for _, want := range []string{
		"table_schema = current_schema()",
		"table_name = 'messages'",
		"column_name = 'search_fts'",
	} {
		assert.Contains(t, query, want, "postgres column query")
	}
}

func TestPostgresConnConfigRuntimeParams(t *testing.T) {
	cfg, err := postgresConnConfig("postgres://user:pass@example.com:5432/msgvault", true)
	require.NoError(t, err, "postgresConnConfig")

	assert.Equal(t, "30s", cfg.RuntimeParams["statement_timeout"])
	assert.Equal(t, "on", cfg.RuntimeParams["default_transaction_read_only"])
}

func TestStoreCloseRunsRegisteredCleanup(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err, "open sqlite")

	called := false
	st := &Store{
		db:      newLoggedDB(db, nil),
		dialect: &SQLiteDialect{},
		closeCleanup: func() {
			called = true
		},
	}

	require.NoError(t, st.Close(), "Close")
	assert.True(t, called, "Close did not run registered cleanup")
}
