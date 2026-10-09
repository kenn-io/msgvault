package archive_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

func exercisePreEventsSchemaUpgrade(t *testing.T, setup func() error, open func() (*archive.Archive, error)) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	require.NoError(setup())
	initialized, err := open()
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(initialized.Close()) })
	st := initialized.Store()
	source, err := st.GetOrCreateSource("gmail", "reader@example.com")
	require.NoError(err)

	// Recreate version 1's schema, which predates Events tables and draft creators.
	tables := []string{
		"mcp_event_clock", "mcp_event_log", "mcp_event_subscriptions",
		"mcp_event_dead_letters", "mcp_event_message_refs",
		"mcp_event_conversation_refs",
	}
	for _, table := range tables {
		_, err := st.DB().ExecContext(t.Context(), "DROP TABLE "+table)
		require.NoError(err)
	}
	draftTables := []string{"gmail_drafts", "imap_drafts", "beeper_drafts", "chat_drafts"}
	for _, table := range draftTables {
		_, err := st.DB().ExecContext(t.Context(), "ALTER TABLE "+table+" DROP COLUMN created_by_principal")
		require.NoError(err)
	}
	_, err = st.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value = '1' WHERE key = 'schema_version'")
	require.NoError(err)
	var count int
	require.Error(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM mcp_event_subscriptions").Scan(&count))

	old, err := open()
	if old != nil {
		t.Cleanup(func() { assert.NoError(old.Close()) })
	}
	require.ErrorContains(err, "run setup", "runtime must reject an archive missing Events schema")
	assert.Nil(old)
	require.Error(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM mcp_event_subscriptions").Scan(&count),
		"opening must not upgrade the schema")
	var version string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT value FROM archive_metadata WHERE key = 'schema_version'").Scan(&version))
	assert.Equal("1", version, "opening must not update the setup marker")

	require.NoError(setup())
	upgraded, err := open()
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(upgraded.Close()) })
	for _, table := range tables {
		require.NoError(upgraded.Store().DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
	}
	for _, table := range draftTables {
		require.NoError(upgraded.Store().DB().QueryRowContext(t.Context(), "SELECT COUNT(created_by_principal) FROM "+table).Scan(&count))
	}
	var enabled bool
	require.NoError(upgraded.Store().DB().QueryRowContext(t.Context(), "SELECT enabled FROM mcp_event_clock WHERE singleton = 1").Scan(&enabled))
	assert.False(enabled)
	_, err = upgraded.Store().GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(err, "setup must preserve existing source data")
	require.NoError(upgraded.PurgeSource(t.Context(), source.ID), "source purge needs Events tables even with capture disabled")
	_, err = upgraded.Store().GetSourceByIDContext(t.Context(), source.ID)
	assert.ErrorIs(err, store.ErrSourceNotFound)
}
