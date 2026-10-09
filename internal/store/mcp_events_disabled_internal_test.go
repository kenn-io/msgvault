package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// Daemon startup configures Events even when they are off. Off must not
// need the archive's write lock, so it cannot block or fail startup, and it
// must not open a new capture epoch.
func TestDisabledMCPEventsConfigureWithoutWriting(t *testing.T) {
	if !sqliteutil.Available {
		t.Skip("the writer-slot holder needs SQLite")
	}
	require := require.New(t)
	check := assert.New(t)
	path := filepath.Join(t.TempDir(), "events-disabled.db")
	st, err := openSQLite(path, "?_journal_mode=WAL&_busy_timeout=0&_foreign_keys=ON")
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	require.NoError(st.InitSchema())

	holder, err := sql.Open(sqliteutil.DriverName(), path+"?_busy_timeout=0")
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, holder.Close()) })
	held, err := holder.Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, held.Close()) })
	_, err = held.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(err)

	clock, err := st.ConfigureMCPEvents(t.Context(), MCPEventsConfig{})
	require.NoError(err, "a never-enabled archive configures Events off without a write")
	check.Equal(int64(0), clock.Epoch)
	check.False(st.captureMCPEnabled())

	_, err = held.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(err)
	enabled := MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	}
	clock, err = st.ConfigureMCPEvents(t.Context(), enabled)
	require.NoError(err)
	check.Equal(int64(1), clock.Epoch)
	clock, err = st.ConfigureMCPEvents(t.Context(), MCPEventsConfig{})
	require.NoError(err)
	check.Equal(int64(2), clock.Epoch, "turning capture off still records the gap")

	_, err = held.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(err)
	t.Cleanup(func() { _, _ = held.ExecContext(t.Context(), "ROLLBACK") })
	clock, err = st.ConfigureMCPEvents(t.Context(), MCPEventsConfig{})
	require.NoError(err, "an archive already off configures Events off without a write")
	check.Equal(int64(2), clock.Epoch)
}
