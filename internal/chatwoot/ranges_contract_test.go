package chatwoot

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/time/rate"
)

// contractAPI models the verified account API at its HTTP boundary. The server
// sorts by created_at while its range predicate uses IDs; it never uses the
// importer's range helpers to construct an expected result.
type contractAPI struct {
	mu                  sync.Mutex
	server              *httptest.Server
	messages            []map[string]any
	conversationInboxID int64
	cap                 int
	ignoreBounds        bool
	contact             map[string]any
	assignee            map[string]any
	messageCalls        int
	mediaRouter         *chatwootMediaRouter
}

func newContractAPI(t *testing.T, pageCap int, messages []map[string]any) *contractAPI {
	t.Helper()
	api := &contractAPI{
		cap: pageCap, messages: messages,
		contact:  map[string]any{"id": int64(7), "type": "contact", "name": "Example Contact", "phone_number": "+12025550101"},
		assignee: map[string]any{"id": int64(99), "type": "user", "name": "Example Assignee"},
	}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		if !assert.Equal(t, "synthetic-token", r.Header.Get("Api_access_token")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		assert.Equal(t, http.MethodGet, r.Method)
		var result any
		switch r.URL.Path {
		case "/api/v1/accounts/3/inboxes":
			result = map[string]any{"payload": []any{
				map[string]any{"id": 7, "name": "Example Inbox", "channel_type": "Channel::TwilioSms", "auth_token": "excluded-channel-secret"},
				map[string]any{"id": 8, "name": "Other Inbox", "channel_type": "Channel::Api"},
			}}
		case "/api/v1/accounts/3/agents":
			result = []any{map[string]any{"id": 7, "name": "Example Agent"}, map[string]any{"id": 8, "name": "Example Owner"}, api.assignee}
		case "/api/v1/accounts/3/conversations":
			assert.Equal(t, "all", r.URL.Query().Get("status"), "resolved conversations must remain discoverable")
			assert.Equal(t, "created_at_asc", r.URL.Query().Get("sort_by"))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			payload := []any{}
			if page == 1 {
				payload = append(payload, api.conversation())
			}
			result = map[string]any{"data": map[string]any{"payload": payload, "meta": map[string]any{"all_count": 1}}}
		case "/api/v1/accounts/3/conversations/42":
			result = api.conversation()
		case "/api/v1/accounts/3/conversations/42/messages":
			api.messageCalls++
			afterText, beforeText := r.URL.Query().Get("after"), r.URL.Query().Get("before")
			after, _ := strconv.ParseInt(afterText, 10, 64)
			before, _ := strconv.ParseInt(beforeText, 10, 64)
			bounded := afterText != "" && beforeText != ""
			payload := make([]map[string]any, 0)
			for _, message := range api.messages {
				id, ok := message["id"].(int64)
				if !assert.True(t, ok, "fixture message ID must be int64") {
					http.Error(w, "invalid fixture ID", http.StatusInternalServerError)
					return
				}
				matches := true
				if afterText != "" {
					matches = id > after
				}
				if bounded {
					matches = id >= after
				}
				if beforeText != "" && before <= math.MaxInt32 {
					matches = matches && id < before
				}
				if api.ignoreBounds || matches {
					payload = append(payload, message)
				}
			}
			sort.SliceStable(payload, func(i, j int) bool {
				left, leftOK := payload[i]["created_at"].(int64)
				right, rightOK := payload[j]["created_at"].(int64)
				if !assert.True(t, leftOK, "fixture creation time must be int64") {
					return false
				}
				if !assert.True(t, rightOK, "fixture creation time must be int64") {
					return false
				}
				return left < right
			})
			switch {
			case bounded:
				if len(payload) > api.cap {
					payload = payload[:api.cap]
				}
			case afterText != "":
				if len(payload) > 100 {
					payload = payload[:100]
				}
			default:
				if len(payload) > 20 {
					payload = payload[len(payload)-20:]
				}
			}
			result = map[string]any{"meta": map[string]any{"contact": api.contact, "assignee": api.assignee}, "payload": payload}
		default:
			assert.Fail(t, "unexpected Chatwoot API path", "%s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		encoded, err := json.Marshal(result)
		if !assert.NoError(t, err) {
			http.Error(w, "encode fixture", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(encoded)
		assert.NoError(t, err)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *contractAPI) messageCallCount() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.messageCalls
}

func (api *contractAPI) conversation() map[string]any {
	// Conversation seeds are intentionally private, independently of the public
	// message list, to catch leakage through raw conversation context.
	private := map[string]any{"id": int64(9000), "private": true, "content": "excluded-private-seed"}
	inboxID := api.conversationInboxID
	if inboxID == 0 {
		inboxID = 7
	}
	return map[string]any{
		"id": int64(42), "account_id": 3, "inbox_id": inboxID, "status": "resolved", "created_at": int64(1801526300), "updated_at": float64(1801526400),
		"meta":     map[string]any{"sender": api.contact, "assignee": api.assignee},
		"messages": []any{private}, "last_non_activity_message": private,
	}
}

func (api *contractAPI) client(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(api.server.URL, 3, "synthetic-token")
	require.NoError(t, err)
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	if api.mediaRouter != nil {
		api.mediaRouter.attach(client)
	}
	return client
}

func contractMessage(id, at int64, sender map[string]any) map[string]any {
	message := map[string]any{
		"id": id, "inbox_id": int64(7), "conversation_id": int64(42), "content": fmt.Sprintf("Synthetic message %d", id),
		"content_type": "text", "message_type": 0, "created_at": at, "private": false, "status": "sent", "content_attributes": map[string]any{},
	}
	if sender != nil {
		message["sender"] = sender
	}
	return message
}

func contractRegister(t *testing.T, st *store.Store, api *contractAPI) (*Importer, *store.Source) {
	t.Helper()
	importer := NewImporter(st, api.client(t))
	sources, err := importer.Register(t.Context(), []int64{7})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "chatwoot", sources[0].SourceType)
	return importer, sources[0]
}

func contractMessageIDs(t *testing.T, st *store.Store) []int64 {
	t.Helper()
	rows, err := st.DB().Query(`SELECT source_message_id FROM messages WHERE message_type = 'chatwoot'`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var raw string
		require.NoError(t, rows.Scan(&raw))
		id, err := strconv.ParseInt(raw, 10, 64)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	slices.Sort(ids)
	return ids
}

// Replacing range traversal with a min/max cursor or treating a short page as
// complete must lose records in at least one of these independent fixtures.
func TestImportContractRangesPreserveDisorderedIDs(t *testing.T) {
	for _, pageCap := range []int{1, 3, 20, 1000} {
		t.Run(fmt.Sprintf("cap_%d", pageCap), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			want := []int64{1, 2, 7, 9, 15, 33, 41, 70, 111, 901, 904, 1301, 2100}
			messages := make([]map[string]any, 0, len(want))
			for i, id := range want {
				messages = append(messages, contractMessage(id, 1801526400+int64((i*7)%5), nil))
			}
			api := newContractAPI(t, pageCap, messages)
			st := testutil.NewTestStore(t)
			importer, _ := contractRegister(t, st, api)
			_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
			require.NoError(err)
			assert.Equal(want, contractMessageIDs(t, st))
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
			require.NoError(err)
			assert.Equal(want, contractMessageIDs(t, st), "restart must not create duplicates")
		})
	}
}

func TestImportContractLimitsResumeAcrossImporterRestarts(t *testing.T) {
	want := []int64{1, 2, 7, 9, 15, 33, 41}
	messages := make([]map[string]any, 0, len(want))
	for i, id := range want {
		messages = append(messages, contractMessage(id, 1801526400+int64(len(want)-i), nil))
	}
	api := newContractAPI(t, 3, messages)
	st := testutil.NewTestStore(t)
	contractRegister(t, st, api)
	previous := 0
	for range len(want) + 2 {
		_, err := NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true, Limit: 2, ReconcileInterval: 24 * time.Hour})
		require.NoError(t, err)
		ids := contractMessageIDs(t, st)
		assert.LessOrEqual(t, len(ids)-previous, 2, "limit counts persisted messages per conversation")
		previous = len(ids)
		if len(ids) == len(want) {
			break
		}
	}
	assert.Equal(t, want, contractMessageIDs(t, st), "unfinished gaps must survive importer restarts")
}

func TestImportContractFullReconciliationResumesAcrossFullRuns(t *testing.T) {
	want := []int64{1, 2, 7, 9, 15, 33, 41}
	messages := make([]map[string]any, 0, len(want))
	for i, id := range want {
		messages = append(messages, contractMessage(id, 1801526400+int64(len(want)-i), nil))
	}
	api := newContractAPI(t, 20, messages)
	st := testutil.NewTestStore(t)
	contractRegister(t, st, api)
	for range len(want) + 2 {
		sum, err := NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{
			InboxID: 7, IncludePrivate: true, Limit: 2, Full: true,
		})
		require.NoError(t, err)
		require.LessOrEqual(t, sum.MessagesProcessed, 2, "a full run must respect its message limit")
		if len(contractMessageIDs(t, st)) == len(want) {
			break
		}
	}
	assert.Equal(t, want, contractMessageIDs(t, st), "repeated full runs must resume the saved reconciliation ranges")
}

func TestImportContractRejectsIgnoredRangeBoundsWithoutPoisoningResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 20, []map[string]any{contractMessage(101, 1801526400, nil), contractMessage(102, 1801526401, nil)})
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	api.mu.Lock()
	api.ignoreBounds = true
	api.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := importer.Import(ctx, ImportOptions{InboxID: 7, IncludePrivate: true})
	require.Error(err, "an incompatible range API must fail explicitly")
	require.NotErrorIs(err, context.DeadlineExceeded, "reject violated bounds rather than looping until cancellation")
	assert.NotContains(err.Error(), "synthetic-token")
	api.mu.Lock()
	api.ignoreBounds = false
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	assert.Equal([]int64{101, 102}, contractMessageIDs(t, st))
}

func TestImportContractRangeAbovePinnedThousandRecordCap(t *testing.T) {
	const count = 1005
	messages := make([]map[string]any, 0, count)
	want := make([]int64, 0, count)
	for id := int64(1); id <= count; id++ {
		messages = append(messages, contractMessage(id, 1801526400+count-id, nil))
		want = append(want, id)
	}
	api := newContractAPI(t, 1000, messages)
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(t, err)
	assert.Equal(t, want, contractMessageIDs(t, st), "a full page must retain unseen low IDs as well as the forward remainder")
}
