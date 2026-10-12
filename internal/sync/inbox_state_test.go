package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// This fixture implements the provider's metadata-only batch contract. The
// production Full path, MIME parser, Store and label refresh run unchanged.
type inboxSyncMetadataAPI struct {
	*gmail.MockAPI

	metadataReads int
}

func (a *inboxSyncMetadataAPI) GetMessageLabelsBatch(_ context.Context, ids []string) ([]gmail.MessageLabelsBatchResult, error) {
	results := make([]gmail.MessageLabelsBatchResult, len(ids))
	for i, id := range ids {
		a.metadataReads++
		raw := a.Messages[id]
		results[i] = gmail.MessageLabelsBatchResult{ID: id, LabelIDs: raw.LabelIDs, HistoryID: raw.HistoryID}
	}
	return results, nil
}

func TestInboxGmailSyncRecordsAndRefreshesProviderState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	env := newTestEnv(t)
	source := env.CreateSource(t)
	env.Mock.Labels = []*gmail.Label{{ID: "INBOX", Name: "INBOX", Type: "system"}, {ID: "UNREAD", Name: "UNREAD", Type: "system"}, {ID: "LabelNext", Name: "Next", Type: "user"}}
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.AddMessage("sync-inbox-fixture", testMIME(), []string{"INBOX", "UNREAD", "LabelNext"})
	env.Mock.Messages["sync-inbox-fixture"].HistoryID = 123
	client := &inboxSyncMetadataAPI{MockAPI: env.Mock}
	env.Syncer = New(client, env.Store, nil)
	runFullSync(t, env)
	archived, err := env.Store.SourceMessageMetadata(source.ID)
	requirements.NoError(err)
	row, ok := archived["sync-inbox-fixture"]
	requirements.True(ok)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: row.ID, ProviderID: "sync-inbox-fixture"}
	initial, err := env.Store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(initial)
	requirements.NotNil(initial.Inbox)
	requirements.NotNil(initial.Read)
	assertions.True(*initial.Inbox)
	assertions.False(*initial.Read)
	assertions.Equal("123", initial.Revision)
	var uiRead bool
	requirements.NoError(env.Store.DB().QueryRow(env.Store.Rebind(`SELECT is_read FROM messages WHERE id=?`), row.ID).Scan(&uiRead))
	assertions.True(uiRead, "provider unread must not change default UI read state")
	bodyCalls := len(env.Mock.GetMessageCalls)
	env.Mock.Messages["sync-inbox-fixture"].LabelIDs = []string{"LabelNext"}
	env.Mock.Messages["sync-inbox-fixture"].HistoryID = 456
	env.SetOptions(t, func(o *Options) { o.NoResume = true })
	env.Syncer = New(client, env.Store, env.Syncer.opts)
	runFullSync(t, env)
	refreshed, err := env.Store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(refreshed)
	requirements.NotNil(refreshed.Inbox)
	requirements.NotNil(refreshed.Read)
	assertions.False(*refreshed.Inbox)
	assertions.True(*refreshed.Read)
	assertions.Equal("456", refreshed.Revision)
	assertions.ElementsMatch([]string{"LabelNext"}, refreshed.Tags)
	assertions.True(refreshed.ObservedAt.After(initial.ObservedAt))
	assertions.Len(env.Mock.GetMessageCalls, bodyCalls, "existing rows refresh metadata without fetching bodies")
	assertions.Equal(1, client.metadataReads)
	local, err := env.Store.GetMessage(row.ID)
	requirements.NoError(err)
	assertions.Equal([]string{"Next"}, local.Labels)

	// Incremental label events also refresh the provider's current metadata,
	// without deriving read state from the UI or fetching MIME again.
	env.Mock.Messages["sync-inbox-fixture"].LabelIDs = []string{"INBOX", "UNREAD", "LabelNext"}
	env.Mock.Messages["sync-inbox-fixture"].HistoryID = 1001
	env.SetHistory(1001, gmail.HistoryRecord{ID: 1001, LabelsAdded: []gmail.HistoryLabelChange{{Message: gmail.MessageID{ID: "sync-inbox-fixture"}, LabelIDs: []string{"INBOX", "UNREAD"}}}})
	current, err := env.Store.GetSourceByID(source.ID)
	requirements.NoError(err)
	_, err = env.Syncer.Incremental(env.Context, current)
	requirements.NoError(err)
	incremental, err := env.Store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(incremental)
	requirements.NotNil(incremental.Inbox)
	requirements.NotNil(incremental.Read)
	assertions.True(*incremental.Inbox)
	assertions.False(*incremental.Read)
	assertions.Equal("1001", incremental.Revision)
	assertions.ElementsMatch([]string{"INBOX", "UNREAD", "LabelNext"}, incremental.Tags)
	assertions.Len(env.Mock.GetMessageCalls, bodyCalls)
}

func TestInboxGmailSyncMissingWatermarkRemainsUnknown(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	env := newTestEnv(t)
	source := env.CreateSource(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.AddMessage("sync-missing-history", testMIME(), []string{"INBOX", "UNREAD"})
	runFullSync(t, env)
	archived, err := env.Store.SourceMessageMetadata(source.ID)
	requirements.NoError(err)
	row, ok := archived["sync-missing-history"]
	requirements.True(ok)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: row.ID, ProviderID: "sync-missing-history"}
	state, err := env.Store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(state)
	assertions.Nil(state.Inbox)
	assertions.Nil(state.Read)
}
