package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportRunRechecksActualProducerAvailability(t *testing.T) {
	for _, boundary := range []string{"attempt", "view", "snapshot"} {
		t.Run(boundary, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f := storetest.New(t)
			cfg := store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-availability", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}}
			_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), f.Source.ID)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
			syncID, err := execution.StartSyncContext(t.Context(), "import-eml", "")
			require.NoError(err)
			scoped := f.Store.ScopedToSync(f.Source.ID, syncID)
			request := store.ImportAttemptRequest{Principal: cfg.Principal, SourceID: f.Source.ID, Format: "eml", NotifyImports: true, SyncRunID: syncID}
			run, err := scoped.BeginImportAttempt(t.Context(), request)
			require.NoError(err)
			view, err := scoped.WithImportRun(t.Context(), run.ID)
			require.NoError(err)
			cfg.Capabilities = nil
			_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			switch boundary {
			case "attempt":
				_, err = scoped.BeginImportAttempt(t.Context(), request)
			case "view":
				_, err = scoped.WithImportRun(t.Context(), run.ID)
			case "snapshot":
				_, err = view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("producer-unavailable").Build(), BodyText: sql.NullString{String: "Synthetic unavailable producer", Valid: true}})
			}
			require.ErrorContains(err, "import_notifications_unavailable")
			var count int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Zero(count)
		})
	}
}
