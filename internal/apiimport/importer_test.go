package apiimport

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImporterRefreshCacheDoesNotLogShutdownCancellation(t *testing.T) {
	var logs bytes.Buffer
	importer := NewImporter(nil, "test import", Hooks{
		RefreshCache: func(context.Context, string) error {
			return context.Canceled
		},
	}).WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	importer.refreshCache(t.Context(), "test_import:local", 1, "item:42")
	assert.Empty(t, logs.String())
}

func TestImporterCancellationAtCheckpointLeavesFailedSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	checkpointStarted := make(chan struct{})
	releaseCheckpoint := make(chan struct{})
	importer := NewImporter(st, "test import", Hooks{})
	importer.beforeCheckpoint = func() {
		close(checkpointStarted)
		<-releaseCheckpoint
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, importErr := importer.Import(ctx, Item{
			Source:          Source{Type: "test_import", Identifier: "local", AccountEmail: "user@example.com"},
			SourceMessageID: "item:42",
			Write: func(context.Context, *store.Store, int64) (Write, error) {
				return Write{MessageID: 1, Created: true, Changed: true}, nil
			},
		})
		done <- importErr
	}()

	select {
	case <-checkpointStarted:
	case <-time.After(30 * time.Second):
		require.FailNow("import did not reach checkpoint")
	}
	cancel()
	close(releaseCheckpoint)

	select {
	case err := <-done:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(30 * time.Second):
		require.FailNow("import did not stop after cancellation")
	}

	sources, err := st.ListSources("test_import")
	require.NoError(err)
	require.Len(sources, 1)
	latest, err := st.GetLatestSync(sources[0].ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Equal(int64(1), latest.MessagesAdded)
	assert.Contains(latest.ErrorMessage.String, context.Canceled.Error())
}
