package archive_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

// Version 33 has policy tables but does not require policy initialization.
// Readers must refuse that version, including after an unsuccessful upgrade.
func exercisePolicySetup(t *testing.T, st *store.Store, setup func() error, open func() (*archive.Archive, error)) {
	t.Helper()
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(t, err)
	room, err := st.EnsureConversation(t.Context(), source.ID, "general", "General")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: room, SourceMessageID: "chat-1", MessageType: "slack",
		Subject: sql.NullString{String: "Observatory", Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, st.UpsertFTS(id, "Observatory", "The telescope is ready.", "", "", ""))
	for _, statement := range []string{
		"DELETE FROM conversation_policy",
		"DELETE FROM archive_metadata WHERE key LIKE 'conversation_policy_%'",
		"DELETE FROM applied_migrations WHERE name = 'conversation_policy_bootstrap_v1'",
		"UPDATE archive_metadata SET value = '33' WHERE key = 'schema_version'",
	} {
		_, err := st.DB().ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	assertUnready := func() {
		t.Helper()
		runtime, err := open()
		if runtime != nil {
			require.NoError(t, runtime.Close())
		}
		require.ErrorContains(t, err, "run setup")
		version, err := st.SchemaVersionContext(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 33, version, "only a completed upgrade can publish the new version")
		var rows int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM conversation_policy").Scan(&rows))
		assert.Zero(t, rows, "runtime opening must not bootstrap policy")
	}
	assertUnready()

	// A malformed saved setting fails after DDL, inside policy initialization.
	_, err = st.DB().ExecContext(t.Context(), "INSERT INTO archive_metadata(key, value) VALUES ('conversation_policy_settings', '{}')")
	require.NoError(t, err)
	require.Error(t, setup())
	assertUnready()
	_, err = st.DB().ExecContext(t.Context(), "DELETE FROM archive_metadata WHERE key = 'conversation_policy_settings'")
	require.NoError(t, err)

	// The store's setup contract must also be sufficient without the archive
	// wrapper's startup refresh.
	require.NoError(t, st.InitSchemaContext(t.Context()))
	runtime, err := open()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	searchIDs(t, runtime, "telescope", 1)
	// Without a participant cap, even an unknown-size chat remains searchable.
	require.NoError(t, setup(), "repeated setup remains valid")
	searchIDs(t, runtime, "telescope", 1)
}
