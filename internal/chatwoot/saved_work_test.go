package chatwoot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSavedArtifactCheckpointRetiresPrivateMessageWhenExcluded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	message := contractMessage(901, 1767225600, nil)
	message["private"] = true
	message["attachments"] = []any{map[string]any{
		"id": 2001, "file_type": "audio", "content_type": "audio/ogg",
		"data_url": "https://chatwoot.example.com/audio.ogg",
	}}
	api := newContractAPI(t, 2, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, IncludePrivate: true}
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	require.Contains(savedState(t, st, source).Conversations["42"].Artifacts, "901")

	opts.IncludePrivate = false
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	assert.NotContains(savedState(t, st, source).Conversations, "42", "excluded private media must be retired from saved refresh work")
	secondRunCalls := api.messageCallCount()

	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(0, api.messageCallCount()-secondRunCalls, "later syncs must not refetch excluded private artifacts")
}

func TestSavedConversationMovedToAnotherInboxRetiresOldWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	message := contractMessage(901, 1767225600, nil)
	message["attachments"] = []any{map[string]any{
		"id": 2001, "file_type": "audio", "content_type": "audio/ogg",
		"data_url": "https://chatwoot.example.com/audio.ogg",
	}}
	api := newContractAPI(t, 2, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, source7 := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, IncludePrivate: true}
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	require.Contains(savedState(t, st, source7).Conversations["42"].Artifacts, "901")

	api.mu.Lock()
	api.conversationInboxID = 8
	for _, message := range api.messages {
		message["inbox_id"] = int64(8)
	}
	api.mu.Unlock()

	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err, "moving a saved conversation must not fail sync for its former inbox")
	assert.NotContains(savedState(t, st, source7).Conversations, "42", "saved work must be retired from the former inbox")
	archivedInOldInbox, err := st.MessageExistsBatch(source7.ID, []string{"901"})
	require.NoError(err)
	assert.Contains(archivedInOldInbox, "901", "archived rows in the former inbox remain available")

	importer8 := NewImporter(st, api.client(t))
	sources8, err := importer8.Register(t.Context(), []Inbox{{ID: 8}})
	require.NoError(err)
	require.Len(sources8, 1)
	_, err = importer8.Import(t.Context(), ImportOptions{InboxID: 8, IncludePrivate: true})
	require.NoError(err)
	archivedInNewInbox, err := st.MessageExistsBatch(sources8[0].ID, []string{"901"})
	require.NoError(err)
	assert.Contains(archivedInNewInbox, "901", "the new inbox source discovers and archives the moved message")
}
