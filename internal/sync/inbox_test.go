package sync

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestFullSyncMetadataRefreshFailureKeepsCursorAndRetriesPage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("metadata-refresh", testMIME(), []string{"INBOX"})
	source := env.CreateSource(t)
	runFullSync(t, env)

	env.Mock.Profile.HistoryID = 12350
	env.Mock.Messages["metadata-refresh"].LabelIDs = []string{"INBOX", "STARRED"}
	env.Mock.Messages["metadata-refresh"].HistoryID = 12350
	env.Syncer = New(&labelMetadataSnapshotAPI{
		MockAPI: env.Mock,
		labelErrors: map[string]error{
			"metadata-refresh": errors.New("temporary metadata failure"),
		},
	}, env.Store, nil)
	_, err := env.Syncer.Full(env.Context, testEmail)
	requirements.ErrorContains(err, "temporary metadata failure")
	assertMessageNotHasLabel(t, env.Store, "metadata-refresh", "STARRED")

	current, err := env.Store.GetSourceByID(source.ID)
	requirements.NoError(err)
	requirements.True(current.SyncCursor.Valid)
	assertions.Equal("12340", current.SyncCursor.String, "a failed full metadata refresh must leave the history cursor retryable")
	failedRun, err := env.Store.GetLatestSync(source.ID)
	requirements.NoError(err)
	requirements.Equal("failed", failedRun.Status)
	requirements.Equal(int64(1), failedRun.ErrorsCount)
	failedItems, err := env.Store.ListSyncRunItems(failedRun.ID, store.SyncRunItemStatusError, 10)
	requirements.NoError(err)
	requirements.Len(failedItems, 1)
	assertions.Equal("metadata-refresh", failedItems[0].SourceMessageID)
	assertions.Equal("metadata_refresh_error", failedItems[0].ErrorKind)

	env.Syncer = New(&labelMetadataSnapshotAPI{MockAPI: env.Mock}, env.Store, nil)
	_, err = env.Syncer.Full(env.Context, testEmail)
	requirements.NoError(err)
	assertMessageHasLabel(t, env.Store, "metadata-refresh", "STARRED")
	updated, err := env.Store.GetSourceByID(source.ID)
	requirements.NoError(err)
	assertions.Equal("12350", updated.SyncCursor.String)
}

func TestIncrementalSyncMetadataRefreshFailureKeepsHistoryCursor(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("metadata-refresh", testMIME(), []string{"INBOX"})
	source := env.CreateSource(t)
	runFullSync(t, env)

	env.Mock.Messages["metadata-refresh"].LabelIDs = []string{"INBOX", "STARRED"}
	env.Mock.Messages["metadata-refresh"].HistoryID = 12350
	env.SetHistory(12350, historyLabelAdded("metadata-refresh", "STARRED"))
	failingAPI := &labelMetadataSnapshotAPI{
		MockAPI: env.Mock,
		labelErrors: map[string]error{
			"metadata-refresh": errors.New("temporary metadata failure"),
		},
	}
	env.Syncer = New(failingAPI, env.Store, nil)
	current, err := env.Store.GetSourceByID(source.ID)
	requirements.NoError(err)
	_, err = env.Syncer.Incremental(env.Context, current)
	requirements.ErrorContains(err, "metadata-refresh")
	requirements.ErrorContains(err, "repair Gmail metadata access and retry sync")
	assertMessageNotHasLabel(t, env.Store, "metadata-refresh", "STARRED")

	failedRun, err := env.Store.GetLatestSync(source.ID)
	requirements.NoError(err)
	requirements.Equal("failed", failedRun.Status)
	requirements.Equal(int64(1), failedRun.ErrorsCount)
	requirements.True(failedRun.ErrorMessage.Valid)
	assertions.Contains(failedRun.ErrorMessage.String, "repair Gmail metadata access and retry sync")
	failedItems, err := env.Store.ListSyncRunItems(failedRun.ID, store.SyncRunItemStatusError, 10)
	requirements.NoError(err)
	requirements.Len(failedItems, 1)
	assertions.Equal("metadata-refresh", failedItems[0].SourceMessageID)
	assertions.Equal("metadata_refresh_error", failedItems[0].ErrorKind)

	current, err = env.Store.GetSourceByID(source.ID)
	requirements.NoError(err)
	requirements.True(current.SyncCursor.Valid)
	assertions.Equal("12340", current.SyncCursor.String, "failed metadata refresh must leave the history cursor retryable")

	env.Syncer = New(&labelMetadataSnapshotAPI{MockAPI: env.Mock}, env.Store, nil)
	_, err = env.Syncer.Incremental(env.Context, current)
	requirements.NoError(err)
	assertMessageHasLabel(t, env.Store, "metadata-refresh", "STARRED")
	updated, err := env.Store.GetSourceByID(source.ID)
	requirements.NoError(err)
	assertions.Equal("12350", updated.SyncCursor.String)
}
