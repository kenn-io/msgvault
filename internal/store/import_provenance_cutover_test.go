package store_test

import (
	"database/sql"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportProvenanceCutoverBeforeCaptureOrPhysicalDeletion(t *testing.T) {
	for _, scenario := range []string{"enable-before-delete", "delete-before-enable", "existing-row-fast-path"} {
		t.Run(scenario, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f := storetest.New(t)
			const owner = "owner:synthetic-cutover"
			const nativeID = "synthetic-retained-identity"
			cfg := store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}}
			id := f.CreateMessage(nativeID)
			if scenario != "delete-before-enable" {
				_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
				require.NoError(err)
			}
			if scenario != "existing-row-fast-path" {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?`), time.Now().UTC(), "synthetic-cutover-batch", id)
				require.NoError(err)
				removed, err := f.Store.DeleteDedupedBatchContext(t.Context(), "synthetic-cutover-batch")
				require.NoError(err)
				require.Equal(int64(1), removed)
			}
			if scenario == "delete-before-enable" {
				_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
				require.NoError(err)
			}
			view, finish := newSyntheticImportView(t, f, owner, true)
			message := f.NewMessage().WithSourceMessageID(nativeID).Build()
			_, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "Synthetic retained body", Valid: true}})
			require.NoError(err)
			finish()
			var count int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&count))
			assert.Zero(count, "legacy identities never become first-import notifications")
			var runID, archived sql.NullString
			var notified bool
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT import_run_id,archived_at,notified FROM import_message_decisions WHERE source_message_id=?`), nativeID).Scan(&runID, &archived, &notified))
			assert.False(runID.Valid, "legacy adoption must not invent a historical run")
			assert.False(archived.Valid, "legacy adoption must not invent a historical ingestion time")
			assert.False(notified)
		})
	}
}

func TestImportProvenanceSurvivesFreshRunsPruningAndPhysicalDeletion(t *testing.T) {
	for _, firstNotify := range []bool{false, true} {
		name := "silent"
		if firstNotify {
			name = "notifying"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f := storetest.New(t)
			const owner = "owner:synthetic-replay"
			_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
			require.NoError(err)
			nativeID := "synthetic-replay-identity"
			message := f.NewMessage().WithSourceMessageID(nativeID).Build()
			view, finish := newSyntheticImportView(t, f, owner, firstNotify)
			id, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "Synthetic body", Valid: true}})
			require.NoError(err)
			finish()
			var initial int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&initial))
			expected := 0
			if firstNotify {
				expected = 1
			}
			require.Equal(expected, initial)
			require.NoError(f.Store.PruneMCPEvents(t.Context(), time.Now().UTC().Add(48*time.Hour), time.Hour))
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?`), time.Now().UTC(), "synthetic-replay-batch", id)
			require.NoError(err)
			removed, err := f.Store.DeleteDedupedBatchContext(t.Context(), "synthetic-replay-batch")
			require.NoError(err)
			require.Equal(int64(1), removed)
			next, complete := newSyntheticImportView(t, f, owner, true)
			_, err = next.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "Synthetic body", Valid: true}})
			require.NoError(err)
			complete()
			var count int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&count))
			assert.Zero(count, "neither a new logical run nor journal/message retention resets first-import identity")
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM import_message_decisions WHERE source_message_id=?`), nativeID).Scan(&count))
			assert.Equal(1, count)
		})
	}
}

func newSyntheticImportView(t *testing.T, f *storetest.Fixture, owner string, notify bool) (*store.Store, func()) {
	t.Helper()
	execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), f.Source.ID)
	Require.NoError(t, err)
	t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
	syncID, err := execution.StartSyncContext(t.Context(), "import-eml", "")
	Require.NoError(t, err)
	scoped := f.Store.ScopedToSync(f.Source.ID, syncID)
	run, err := scoped.BeginImportAttempt(t.Context(), store.ImportAttemptRequest{Principal: owner, SourceID: f.Source.ID, Format: "eml", NotifyImports: notify, SyncRunID: syncID})
	Require.NoError(t, err)
	view, err := scoped.WithImportRun(t.Context(), run.ID)
	Require.NoError(t, err)
	return view, func() {
		Require.NoError(t, scoped.CompleteSyncContext(t.Context(), syncID, ""))
		Require.NoError(t, execution.Release())
	}
}
