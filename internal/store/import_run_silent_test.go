package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportRunSilentWithoutEventsOwner(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("eml", "synthetic-silent@example.net")
	require.NoError(err)
	execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err)
	t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
	syncID, err := execution.StartSyncContext(t.Context(), "import-eml", "")
	require.NoError(err)
	request := store.ImportAttemptRequest{SourceID: source.ID, Format: "eml", SyncRunID: syncID}
	scoped := f.Store.ScopedToSync(source.ID, syncID)
	run, err := scoped.BeginImportAttempt(t.Context(), request)
	require.NoError(err, "default silent imports must work without Events configuration")
	assert.NotEmpty(run.ID)
	assert.Empty(run.Principal)
	assert.False(run.NotifyImports)
	request.NotifyImports = true
	_, err = scoped.BeginImportAttempt(t.Context(), request)
	require.Error(err, "silent operation never grants notification authority")
}
