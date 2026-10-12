package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportRunRecoveryPreservesPolicyWithoutCheckpoint(t *testing.T) {
	for _, notify := range []bool{false, true} {
		name := "silent"
		if notify {
			name = "notifying"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f := storetest.New(t)
			const owner = "owner:synthetic-import"
			_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}, {Family: "msgvault.import_message_archived", SourceType: "mbox", Kinds: []string{"message"}}}})
			require.NoError(err)
			source, err := f.Store.GetOrCreateSource("eml", "synthetic-import@example.net")
			require.NoError(err)
			firstExecution, err := f.Store.AcquireSyncExecutionContext(t.Context(), source.ID)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, firstExecution.Release()) })
			firstSync, err := firstExecution.StartSyncContext(t.Context(), "import-eml", "")
			require.NoError(err)
			request := store.ImportAttemptRequest{Principal: owner, SourceID: source.ID, Format: "eml", NotifyImports: notify, SyncRunID: firstSync}
			first, err := f.Store.ScopedToSync(source.ID, firstSync).BeginImportAttempt(t.Context(), request)
			require.NoError(err)
			require.NotEmpty(first.ID)
			assert.Equal(notify, first.NotifyImports)
			// No progress checkpoint or message has been saved. Releasing native
			// ownership simulates an abandoned worker before its first checkpoint.
			require.NoError(firstExecution.Release())
			recoveredExecution, err := f.Store.AcquireSyncExecutionContext(t.Context(), source.ID)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, recoveredExecution.Release()) })
			recoveredSync, err := recoveredExecution.StartSyncContext(t.Context(), "import-eml", "")
			require.NoError(err)
			require.NotEqual(firstSync, recoveredSync)
			request.SyncRunID = recoveredSync
			request.RecoveredSyncRunID = firstSync
			scoped := f.Store.ScopedToSync(source.ID, recoveredSync)
			recovered, err := scoped.BeginImportAttempt(t.Context(), request)
			require.NoError(err)
			assert.Equal(first.ID, recovered.ID)
			assert.Equal(notify, recovered.NotifyImports)
			for _, change := range []string{"choice", "format", "owner"} {
				t.Run(change, func(t *testing.T) {
					bad := request
					switch change {
					case "choice":
						bad.NotifyImports = !notify
					case "format":
						bad.Format = "mbox"
					case "owner":
						bad.Principal = "owner:another-synthetic-import"
					}
					_, err := scoped.BeginImportAttempt(t.Context(), bad)
					Require.Error(t, err, "recovery must not rewrite frozen import authority")
				})
			}
		})
	}
}
