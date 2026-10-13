package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportRunCompletionSurvivesAttemptPruning(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := storetest.New(t)
	const owner = "owner:synthetic-import"
	_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}, {Family: "msgvault.import_message_archived", SourceType: "mbox", Kinds: []string{"message"}}}})
	require.NoError(err)
	source, err := f.Store.GetOrCreateSource("eml", "synthetic-completion@example.net")
	require.NoError(err)
	firstExecution, err := f.Store.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err)
	t.Cleanup(func() { Assert.NoError(t, firstExecution.Release()) })
	firstSync, err := firstExecution.StartSyncContext(t.Context(), "import-eml", "")
	require.NoError(err)
	request := store.ImportAttemptRequest{Principal: owner, SourceID: source.ID, Format: "eml", NotifyImports: true, SyncRunID: firstSync}
	first, err := f.Store.ScopedToSync(source.ID, firstSync).BeginImportAttempt(t.Context(), request)
	require.NoError(err)
	require.NoError(firstExecution.Release())
	recoveredExecution, err := f.Store.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err)
	t.Cleanup(func() { Assert.NoError(t, recoveredExecution.Release()) })
	completedSync, err := recoveredExecution.StartSyncContext(t.Context(), "import-eml", "")
	require.NoError(err)
	request.SyncRunID = completedSync
	request.RecoveredSyncRunID = firstSync
	recovered, err := f.Store.ScopedToSync(source.ID, completedSync).BeginImportAttempt(t.Context(), request)
	require.NoError(err)
	assert.Equal(first.ID, recovered.ID)
	require.NoError(f.Store.CompleteSyncContext(t.Context(), completedSync, ""))
	require.NoError(recoveredExecution.Release())
	// Native attempt retention is independent of logical import policy. The
	// retained failed attempt must never reopen a completed logical operation.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM sync_runs WHERE id=?`), completedSync)
	require.NoError(err)
	nextExecution, err := f.Store.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err)
	t.Cleanup(func() { Assert.NoError(t, nextExecution.Release()) })
	nextSync, err := nextExecution.StartSyncContext(t.Context(), "import-eml", "")
	require.NoError(err)
	request.SyncRunID = nextSync
	_, err = f.Store.ScopedToSync(source.ID, nextSync).BeginImportAttempt(t.Context(), request)
	require.Error(err, "completed logical run cannot be reopened through a retained abandoned attempt")
	request.RecoveredSyncRunID = 0
	fresh, err := f.Store.ScopedToSync(source.ID, nextSync).BeginImportAttempt(t.Context(), request)
	require.NoError(err)
	assert.NotEqual(first.ID, fresh.ID, "a fresh invocation starts a new logical import run")
}
