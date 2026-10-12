package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportRunViewPinsNativeAttemptAndOfflineProvenance(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := storetest.New(t)
	const owner = "owner:synthetic-import"
	_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(err)
	execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), f.Source.ID)
	require.NoError(err)
	t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
	syncID, err := execution.StartSyncContext(t.Context(), "import-eml", "")
	require.NoError(err)
	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	scoped := live.ScopedToSync(f.Source.ID, syncID)
	run, err := scoped.BeginImportAttempt(t.Context(), store.ImportAttemptRequest{Principal: owner, SourceID: f.Source.ID, Format: "eml", SyncRunID: syncID})
	require.NoError(err)
	_, err = f.Store.WithImportRun(t.Context(), run.ID)
	require.Error(err, "run IDs alone never replace native execution authority")
	_, err = scoped.WithImportRun(t.Context(), "import_nonexistent")
	require.Error(err)
	view, err := scoped.WithImportRun(t.Context(), run.ID)
	require.NoError(err)
	persist := func(st *store.Store, id string) {
		t.Helper()
		_, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID(id).Build(), BodyText: sql.NullString{String: "Synthetic import view", Valid: true}})
		Require.NoError(t, err)
	}
	persist(view, "import-view-offline")
	var count int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&count))
	assert.Zero(count, "validated import view cannot inherit live Gmail provenance")
	persist(live, "import-view-independent-live")
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&count))
	assert.Equal(1, count, "view construction never mutates shared ingestion mode")
	require.NoError(f.Store.CompleteSyncContext(t.Context(), syncID, ""))
	_, err = scoped.WithImportRun(t.Context(), run.ID)
	require.Error(err, "completed native attempts cannot regain a writable import view")
}
