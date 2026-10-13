package meetingimport

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeMeetingNotifyingDerivedFTSFailureDoesNotLoseReadySnapshot(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	testutil.SkipIfPostgres(t, "injects SQLite FTS5 failure")
	configureNativeMeetingImports(t, st, true, true)
	require.True(st.FTS5Available())
	_, err := st.DB().Exec(`DROP TABLE messages_fts`)
	require.NoError(err)
	req := validImportRequest(t)
	req.NotifyImports = true
	result, err := NewImporter(st, Hooks{}).Import(t.Context(), req)
	require.NoError(err, "derived search failure must not roll back the ready notifying snapshot")
	assert.NotZero(result.MessageID)
	for _, table := range []string{"messages", "message_bodies", "message_raw", "import_message_decisions", "mcp_event_log"} {
		var n int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
		assert.Equal(1, n, table)
	}
}
