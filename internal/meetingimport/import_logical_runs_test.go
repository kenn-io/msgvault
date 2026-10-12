package meetingimport

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeMeetingImportOwnsFreshSilentLogicalRuns(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	caller := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	native := NewImporter(caller, Hooks{})
	var prior Result
	for range 2 {
		result, err := native.Import(t.Context(), validImportRequest(t))
		require.NoError(err)
		if prior.MessageID != 0 {
			assert.Equal(prior.MessageID, result.MessageID)
		}
		prior = result
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM import_runs WHERE import_format='meeting_import' AND completed=TRUE AND notify_imports=FALSE`: 2,
		`SELECT COUNT(*) FROM sync_runs WHERE status='completed'`:                                                           2,
		`SELECT COUNT(*) FROM messages`:                 1,
		`SELECT COUNT(*) FROM import_message_decisions`: 1,
		`SELECT COUNT(*) FROM mcp_event_log`:            0,
	} {
		var got int
		require.NoError(st.DB().QueryRow(query).Scan(&got))
		assert.Equal(want, got, query)
	}
}
