package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGCMessageDeleteUsesIMAPMembershipIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "gc-plan.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())

	var foreignKeys int
	require.NoError(st.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys))
	require.Equal(1, foreignKeys)

	plan := explainPlan(t, st, "DELETE FROM messages WHERE id = ?", 1)
	assert.Contains(plan, "SEARCH imap_message_memberships USING COVERING INDEX idx_imap_message_memberships_message (message_id=?)")
	assert.NotContains(plan, "SCAN imap_message_memberships")
}

func TestGCRefusesPostgreSQLBeforeDatabaseAccess(t *testing.T) {
	st := &Store{dialect: &PostgreSQLDialect{}}

	_, err := st.ExecuteGCContext(t.Context(), GCPlan{})
	require.ErrorIs(t, err, ErrGCUnsupported)
	require.ErrorIs(t, st.VacuumContext(t.Context()), ErrGCUnsupported)
}
