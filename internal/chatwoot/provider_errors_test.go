package chatwoot

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/chatwootapi"
	"golang.org/x/time/rate"
)

// addRecentCalls adds voice calls to conversation 1, which keep refresh work
// saved after their first sync. Offsets set each call's creation time.
func addRecentCalls(api *contractAPI, offsets map[int64]time.Duration) {
	for id, offset := range offsets {
		api.AddMessage(1, id, now().Add(offset))
	}
	for _, message := range api.Conversations[1] {
		message["content_type"] = "voice_call"
	}
}

func TestNotFoundRetiresOnlyUnavailableWork(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cap           int
		hidden        bool
		notFound      func(string) bool
		wantArtifacts []string
	}{
		{name: "conversation_detail", cap: 20, hidden: true,
			notFound: func(request string) bool { return request == "conversation 1" }},
		{name: "artifact_range", cap: 20,
			notFound: func(request string) bool { return strings.HasPrefix(request, "messages 1 ") }},
		// A capped range returns 12, created first; 11 is then read by its exact ID.
		{name: "exact_artifact", cap: 1, wantArtifacts: []string{"12"},
			notFound: func(request string) bool { return request == "messages 1 11 12" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			api := newContractAPI(t, tc.cap, nil)
			addRecentCalls(api, map[int64]time.Duration{11: 0, 12: -time.Second})
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
			require.NoError(err)
			require.Len(savedState(t, st, source).Conversations["1"].Artifacts, 2)

			api.Mu.Lock()
			api.Hidden = map[int64]bool{1: tc.hidden}
			api.NotFound = tc.notFound
			api.Mu.Unlock()
			api.TakeRequests()
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7})
			require.NoError(err)
			assert.True(slices.ContainsFunc(api.TakeRequests(), tc.notFound), "the unavailable object was requested")

			saved := savedState(t, st, source).Conversations["1"]
			var artifacts []string
			if saved != nil {
				for id := range saved.Artifacts {
					artifacts = append(artifacts, id)
				}
			}
			assert.Equal(tc.wantArtifacts, artifacts)
			assert.Equal([]int64{11, 12}, contractMessageIDs(t, st), "archived records remain")
		})
	}
}

// A missing history range is not proof the history is gone, so its pending
// work stays saved and the sync reports the failure.
func TestNotFoundHistoryRangeKeepsPendingHistory(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	api := newContractAPI(t, 20, nil)
	api.AddMessage(1, 11, now())
	api.AddMessage(1, 12, now().Add(time.Second))
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7, Limit: 1})
	require.NoError(err)

	api.Mu.Lock()
	api.NotFound = func(request string) bool { return strings.HasPrefix(request, "messages 1 ") }
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7})
	require.ErrorIs(err, ErrNotFound)
	require.ErrorContains(err, "chatwoot conversation 1")

	state, err := NewImporter(st, api.client(t)).resumeState(source.ID)
	require.NoError(err)
	require.Contains(state.Conversations, "1")
	assert.NotEmpty(state.Conversations["1"].Pending)
	assert.Equal([]int64{11}, contractMessageIDs(t, st))
}

func TestOutOfScopeProviderDataIsNotArchived(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(api *contractAPI)
		wantErr string
	}{
		{"message_inbox", func(api *contractAPI) { api.Conversations[1][0]["inbox_id"] = int64(8) },
			"message scope does not match"},
		{"message_conversation", func(api *contractAPI) { api.Conversations[1][0]["conversation_id"] = int64(2) },
			"message scope does not match"},
		{"message_account", func(api *contractAPI) { api.Conversations[1][0]["account_id"] = int64(4) },
			"message scope does not match"},
		{"attachment_message", func(api *contractAPI) {
			api.Conversations[1][0]["attachments"] = []any{map[string]any{"id": int64(2001), "message_id": int64(99), "file_type": "file"}}
		}, "attachment scope mismatch"},
		{"listing_account", func(api *contractAPI) { api.ConversationAccountID = 4 }, "invalid conversation scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newContractAPI(t, 20, nil)
			api.AddMessage(1, 11, now())
			tc.mutate(api)
			st := testutil.NewTestStore(t)
			imp, _ := contractRegister(t, st, api)
			_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
			require.ErrorContains(t, err, tc.wantErr)
			assert.Empty(t, contractMessageIDs(t, st))
		})
	}
}

func TestSavedConversationFromAnotherAccountIsRejected(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	api := newContractAPI(t, 20, nil)
	addRecentCalls(api, map[int64]time.Duration{11: 0})
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)

	api.Mu.Lock()
	api.Hidden = map[int64]bool{1: true}
	api.ConversationAccountID = 4
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7})
	require.ErrorContains(err, "saved conversation scope mismatch")
	state, err := NewImporter(st, api.client(t)).resumeState(source.ID)
	require.NoError(err)
	require.Contains(state.Conversations, "1")
	assert.Contains(state.Conversations["1"].Artifacts, "11", "a rejected conversation keeps its saved work")
}

// A server that returns messages for an empty combined range cannot prove a
// range complete, so the importer stops before archiving from it.
func TestImportRejectsRangesThatIgnoreEmptyBounds(t *testing.T) {
	require := require.New(t)
	api := newContractAPI(t, 20, []map[string]any{chatwootapi.Message(101, 1767225600, nil)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if after := query.Get("after"); after != "" && after == query.Get("before") {
			id, err := strconv.ParseInt(after, 10, 64)
			if err != nil {
				http.Error(w, "invalid bounds", http.StatusBadRequest)
				return
			}
			query.Set("before", strconv.FormatInt(id+1, 10))
			r.URL.RawQuery = query.Encode()
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, 3, "synthetic-token")
	require.NoError(err)
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	client.messageRangeCap = api.Cap
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client)
	_, err = imp.Register(t.Context(), []Inbox{{ID: 7, Name: "Example Inbox"}})
	require.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.ErrorContains(err, "does not support empty combined message bounds")
	assert.Empty(t, contractMessageIDs(t, st))
}
