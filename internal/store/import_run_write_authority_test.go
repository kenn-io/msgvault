package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A previously validated view must not authorize a later write after policy
// or native identity changes. Checking only the constructor misses this break.
func TestImportRunViewRevalidatesAuthorityBeforePersistence(t *testing.T) {
	for _, change := range []string{"owner", "policy", "source", "completed", "unvalidated_mode"} {
		t.Run(change, func(t *testing.T) {
			require := Require.New(t)

			f := storetest.New(t)
			const owner = "owner:synthetic-import-write"
			cfg := store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}, {Family: "msgvault.import_message_archived", SourceType: "mbox", Kinds: []string{"message"}}}}
			_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), f.Source.ID)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
			syncID, err := execution.StartSyncContext(t.Context(), "import-eml", "")
			require.NoError(err)
			scoped := f.Store.ScopedToSync(f.Source.ID, syncID)
			run, err := scoped.BeginImportAttempt(t.Context(), store.ImportAttemptRequest{Principal: owner, SourceID: f.Source.ID, Format: "eml", NotifyImports: true, SyncRunID: syncID})
			require.NoError(err)
			view, err := scoped.WithImportRun(t.Context(), run.ID)
			require.NoError(err)
			switch change {
			case "owner":
				cfg.Principal = "owner:synthetic-replacement"
				_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
			case "policy":
				cfg.ImportNotifications = false
				_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
			case "source":
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), "replacement@example.com", f.Source.ID)
			case "completed":
				err = f.Store.CompleteSyncContext(t.Context(), syncID, "")
			case "unvalidated_mode":
				view = scoped.WithIngestContext(store.IngestContext{Mode: store.IngestImport})
			}
			require.NoError(err)
			_, err = view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("revoked-import-write").Build(), BodyText: sql.NullString{String: "Synthetic revoked import", Valid: true}})
			require.Error(err, "stale or forged import authority must reject the canonical write")
			var count int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&count))
			Assert.Zero(t, count, "rejected import authority cannot leave a partial archive snapshot")
		})
	}
}
