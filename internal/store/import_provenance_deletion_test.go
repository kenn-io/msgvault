package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportProvenanceAdoptsNativeIdentityLossBeforeConfiguration(t *testing.T) {
	for _, operation := range []string{"all-dedup", "gc", "alias-rewrite", "alias-merge", "dedup-rewrite"} {
		t.Run(operation, func(t *testing.T) {
			require := Require.New(t)

			f := storetest.New(t)
			if operation == "gc" && f.Store.IsPostgreSQL() {
				t.Skip("native GC is SQLite-only")
			}
			const owner = "owner:synthetic-native-adoption"
			nativeID := "synthetic-old-native-identity"
			id := f.CreateMessage(nativeID)
			wantedID := nativeID
			switch operation {
			case "all-dedup":
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?`), time.Now().UTC(), "synthetic-adopt-all", id)
				require.NoError(err)
				removed, _, err := f.Store.DeleteAllDedupedContext(t.Context())
				require.NoError(err)
				require.Equal(int64(1), removed)
			case "gc":
				require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, nativeID))
				plan, err := f.Store.PlanGCContext(t.Context())
				require.NoError(err)
				removed, err := f.Store.ExecuteGCContext(t.Context(), plan)
				require.NoError(err)
				require.Equal(int64(1), removed)
			case "alias-rewrite", "alias-merge", "dedup-rewrite":
				wantedID = "synthetic-new-native-identity"
				if operation == "alias-merge" {
					f.CreateMessage(wantedID)
				}
				if operation == "dedup-rewrite" {
					_, err := f.Store.UpdateMessageOnDedup(id, wantedID, nil)
					require.NoError(err)
				} else {
					require.NoError(f.Store.MigrateSourceMessageID(f.Source.ID, f.ConvID, nativeID, wantedID))
				}
				// Delete the surviving alias through the native path: both old and new
				// occurrence names must already be protected before the identity is lost.
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE source_id=? AND source_message_id=?`), time.Now().UTC(), "synthetic-adopt-alias", f.Source.ID, wantedID)
				require.NoError(err)
				removed, err := f.Store.DeleteDedupedBatchContext(t.Context(), "synthetic-adopt-alias")
				require.NoError(err)
				require.Equal(int64(1), removed)
			}
			_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
			require.NoError(err)
			for _, reimportID := range []string{nativeID, wantedID} {
				view, finish := newSyntheticImportView(t, f, owner, true)
				message := f.NewMessage().WithSourceMessageID(reimportID).Build()
				_, err = view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message})
				require.NoError(err)
				finish()
			}
			var count int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&count))
			Assert.Zero(t, count, "native deletion and alias reconciliation cannot erase silent cutover evidence")
		})
	}
}
