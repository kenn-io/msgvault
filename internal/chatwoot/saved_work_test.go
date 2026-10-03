package chatwoot

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/time/rate"
)

func TestSavedHistoryResumesWhileDiscoveryKeepsGrowing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	visitedLater := false
	conversation := func(id int64) map[string]any { return map[string]any{"id": id, "account_id": 1, "inbox_id": 1} }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any
		switch r.URL.Path {
		case "/api/v1/accounts/1/inboxes":
			result = map[string]any{"payload": []any{map[string]any{"id": 1}}}
		case "/api/v1/accounts/1/agents":
			result = []any{}
		case "/api/v1/accounts/1/conversations":
			page, err := strconv.ParseInt(r.URL.Query().Get("page"), 10, 64)
			if !assert.NoError(err) {
				return
			}
			// New conversations continue arriving; discovery never returns an empty page.
			result = map[string]any{"data": map[string]any{"payload": []any{conversation(page)}}}
		default:
			var id int64
			_, err := fmt.Sscanf(r.URL.Path, "/api/v1/accounts/1/conversations/%d", &id)
			if !assert.NoError(err) {
				return
			}
			if !strings.HasSuffix(r.URL.Path, "/messages") {
				result = conversation(id)
				break
			}
			if id > 1 {
				visitedLater = true
			}
			after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			if !assert.NoError(err) {
				return
			}
			before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
			if !assert.NoError(err) {
				return
			}
			payload := []any{}
			if id == 1 {
				for messageID := int64(101); messageID <= 120; messageID++ {
					if messageID >= after && messageID < before {
						message := contractMessage(messageID, 1801526400+messageID, nil)
						message["account_id"] = int64(1)
						message["inbox_id"] = int64(1)
						message["conversation_id"] = int64(1)
						payload = append(payload, message)
						break
					}
				}
			}
			result = map[string]any{"payload": payload}
		}
		encoded, err := json.Marshal(result)
		if !assert.NoError(err) {
			return
		}
		_, err = w.Write(encoded)
		assert.NoError(err)
	}))
	defer server.Close()
	st := testutil.NewTestStore(t)
	client, err := NewClient(server.URL, 1, "synthetic-token")
	require.NoError(err)
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	imp := NewImporter(st, client)
	sources, err := imp.Register(t.Context(), []int64{1})
	require.NoError(err)
	require.Len(sources, 1)
	imp.requestBudget = 8
	first, err := imp.Import(t.Context(), ImportOptions{InboxID: 1})
	require.NoError(err)
	require.True(first.Partial)
	before, err := st.MessageExistsBatch(sources[0].ID, []string{"106"})
	require.NoError(err)
	require.NotContains(before, "106", "first run must leave a real saved tail")
	second, err := imp.Import(t.Context(), ImportOptions{InboxID: 1})
	require.NoError(err)
	after, err := st.MessageExistsBatch(sources[0].ID, []string{"106"})
	require.NoError(err)
	assert.Contains(after, "106", "saved history must resume without waiting for discovery EOF")
	assert.True(visitedLater, "saved history must leave budget for new conversations")
	assert.True(second.Partial)
}

func TestSavedArtifactLimitRemainsPartialAtDiscoveryEOF(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	messages := []map[string]any{}
	for id := int64(901); id <= 902; id++ {
		message := contractMessage(id, 1801526400+id, nil)
		message["attachments"] = []any{map[string]any{
			"id": id + 1000, "file_type": "audio", "content_type": "audio/ogg",
			"data_url": fmt.Sprintf("https://chatwoot.example.com/audio-%d.ogg", id),
		}}
		messages = append(messages, message)
	}
	api := newContractAPI(t, 2, messages)
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, NoMedia: true}
	initial, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	require.False(initial.Partial)
	opts.Limit = 1
	limited, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, limited.MessagesProcessed)
	assert.True(limited.Partial, "discovery EOF must preserve unfinished saved artifact work")
	last, err := st.GetLastSuccessfulSyncByType(source.ID, SourceType)
	require.NoError(err)
	require.NotNil(last)
	state, err := parseSyncState(last.CursorAfter.String, source.Identifier)
	require.NoError(err)
	conversation := state.Conversations["42"]
	require.NotNil(conversation)
	assert.Empty(conversation.Pending, "the remaining work is artifact refresh, not history")
	assert.Len(conversation.Artifacts, 2)
	assert.Equal("902", conversation.NextArtifact)
	opts.Limit = 0
	finished, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.False(finished.Partial)
}

func TestSavedArtifactCheckpointRetiresDeletedMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	message := contractMessage(901, 1801526400, nil)
	message["attachments"] = []any{map[string]any{
		"id": 2001, "file_type": "audio", "content_type": "audio/ogg",
		"data_url": "https://chatwoot.example.com/audio.ogg",
	}}
	api := newContractAPI(t, 2, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, IncludePrivate: true, NoMedia: true}
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	last, err := st.GetLastSuccessfulSyncByType(source.ID, SourceType)
	require.NoError(err)
	require.NotNil(last)
	state, err := parseSyncState(last.CursorAfter.String, source.Identifier)
	require.NoError(err)
	conversation := state.Conversations["42"]
	require.NotNil(conversation)
	assert.Contains(conversation.Artifacts, "901")

	api.mu.Lock()
	api.messages = nil
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	last, err = st.GetLastSuccessfulSyncByType(source.ID, SourceType)
	require.NoError(err)
	require.NotNil(last)
	state, err = parseSyncState(last.CursorAfter.String, source.Identifier)
	require.NoError(err)
	conversation = state.Conversations["42"]
	require.NotNil(conversation)
	assert.NotContains(conversation.Artifacts, "901", "deleted messages must not consume an artifact refresh request on every sync")
}

func TestSavedArtifactCheckpointRetiresPrivateMessageWhenExcluded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	message := contractMessage(901, 1801526400, nil)
	message["private"] = true
	message["attachments"] = []any{map[string]any{
		"id": 2001, "file_type": "audio", "content_type": "audio/ogg",
		"data_url": "https://chatwoot.example.com/audio.ogg",
	}}
	api := newContractAPI(t, 2, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, IncludePrivate: true, NoMedia: true}
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	last, err := st.GetLastSuccessfulSyncByType(source.ID, SourceType)
	require.NoError(err)
	require.NotNil(last)
	state, err := parseSyncState(last.CursorAfter.String, source.Identifier)
	require.NoError(err)
	require.Contains(state.Conversations["42"].Artifacts, "901")

	opts.IncludePrivate = false
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	last, err = st.GetLastSuccessfulSyncByType(source.ID, SourceType)
	require.NoError(err)
	require.NotNil(last)
	state, err = parseSyncState(last.CursorAfter.String, source.Identifier)
	require.NoError(err)
	conversation := state.Conversations["42"]
	require.NotNil(conversation)
	assert.NotContains(conversation.Artifacts, "901", "excluded private media must be retired from saved refresh work")
	secondRunCalls := api.messageCallCount()

	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, api.messageCallCount()-secondRunCalls, "later syncs should only probe history, not re-fetch excluded private artifacts")
}

func TestSavedConversationMovedToAnotherInboxRetiresOldWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	message := contractMessage(901, 1801526400, nil)
	message["attachments"] = []any{map[string]any{
		"id": 2001, "file_type": "audio", "content_type": "audio/ogg",
		"data_url": "https://chatwoot.example.com/audio.ogg",
	}}
	api := newContractAPI(t, 2, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, source7 := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, IncludePrivate: true, NoMedia: true}
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	last, err := st.GetLastSuccessfulSyncByType(source7.ID, SourceType)
	require.NoError(err)
	require.NotNil(last)
	state, err := parseSyncState(last.CursorAfter.String, source7.Identifier)
	require.NoError(err)
	require.Contains(state.Conversations["42"].Artifacts, "901")

	api.mu.Lock()
	api.conversationInboxID = 8
	for _, message := range api.messages {
		message["inbox_id"] = int64(8)
	}
	api.mu.Unlock()

	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err, "moving a saved conversation must not fail sync for its former inbox")
	last, err = st.GetLastSuccessfulSyncByType(source7.ID, SourceType)
	require.NoError(err)
	require.NotNil(last)
	state, err = parseSyncState(last.CursorAfter.String, source7.Identifier)
	require.NoError(err)
	assert.NotContains(state.Conversations, "42", "saved work must be retired from the former inbox")
	assert.Empty(state.NextSavedConversation, "the saved-work frontier must not point to a retired conversation")
	archivedInOldInbox, err := st.MessageExistsBatch(source7.ID, []string{"901"})
	require.NoError(err)
	assert.Contains(archivedInOldInbox, "901", "archived rows in the former inbox remain available")

	importer8 := NewImporter(st, api.client(t))
	sources8, err := importer8.Register(t.Context(), []int64{8})
	require.NoError(err)
	require.Len(sources8, 1)
	_, err = importer8.Import(t.Context(), ImportOptions{InboxID: 8, IncludePrivate: true, NoMedia: true})
	require.NoError(err)
	archivedInNewInbox, err := st.MessageExistsBatch(sources8[0].ID, []string{"901"})
	require.NoError(err)
	assert.Contains(archivedInNewInbox, "901", "the new inbox source discovers and archives the moved message")
}
