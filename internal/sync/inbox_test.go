package sync

import (
	"errors"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"testing"
)

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
	assertions.ErrorContains(err, "repair Gmail metadata access and retry sync")
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

func TestInboxGmailReceiveParsePersistsProviderObservation(t *testing.T) {
	assert := assert.New(t)

	require := require.New(t)

	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.AddMessage("received-message", testMIME(), []string{"INBOX", "UNREAD"})
	env.Mock.Messages["received-message"].HistoryID = 170
	source := env.CreateSource(t)
	runFullSync(t, env)
	ids, err := env.Store.MessageExistsBatch(source.ID, []string{"received-message"})
	require.NoError(err)
	require.Contains(ids, "received-message")
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: ids["received-message"], ProviderID: "received-message"}
	state, err := env.Store.GetInboxProviderState(t.Context(), target)
	require.NoError(err)
	require.NotNil(state)
	require.NotNil(state.Inbox)
	require.NotNil(state.Read)
	assert.True(*state.Inbox)
	assert.False(*state.Read)
	assert.Equal("170", state.Revision)
	assert.ElementsMatch([]string{"INBOX", "UNREAD"}, state.Tags)
	// The sync parser also persisted the MIME body; this is not an observation-only fixture.
	var body string
	err = env.Store.DB().QueryRow(env.Store.Rebind("SELECT body_text FROM message_bodies WHERE message_id = ?"), target.ItemID).Scan(&body)
	require.NoError(err)
	assert.NotEmpty(body)
}
