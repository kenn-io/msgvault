package chatwoot

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/time/rate"
)

// contractAPI models the verified account API at its HTTP boundary. Like
// Chatwoot, it sets a conversation's activity to its newest message's creation
// time, sorts messages by created_at and filters bounded ranges by ID; it never
// uses the importer's range helpers to construct an expected result.
type contractAPI struct {
	mu                  sync.Mutex
	server              *httptest.Server
	conversations       map[int64][]map[string]any
	conversationInboxID int64
	pageSize            int
	cap                 int
	ignoreBounds        bool
	contact             map[string]any
	assignee            map[string]any
	requests            []string
	mediaRouter         *chatwootMediaRouter
}

// newContractAPI serves messages as conversation 42 of inbox 7.
func newContractAPI(t *testing.T, pageCap int, messages []map[string]any) *contractAPI {
	t.Helper()
	api := &contractAPI{
		conversations: map[int64][]map[string]any{}, pageSize: 25, cap: pageCap,
		contact:  map[string]any{"id": int64(7), "type": "contact", "name": "Example Contact", "phone_number": "+12025550101"},
		assignee: map[string]any{"id": int64(99), "type": "user", "name": "Example Assignee"},
	}
	if messages != nil {
		api.conversations[42] = messages
	}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		if !assert.Equal(t, "synthetic-token", r.Header.Get("Api_access_token")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var result any
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/accounts/3")
		var id int64
		switch {
		case path == "/agents":
			api.requests = append(api.requests, "agents")
			result = []any{map[string]any{"id": 7, "name": "Example Agent"}, map[string]any{"id": 8, "name": "Example Owner"}, api.assignee}
		case path == "/conversations":
			assert.Equal(t, "all", r.URL.Query().Get("status"), "resolved conversations must remain discoverable")
			sortBy := r.URL.Query().Get("sort_by")
			api.requests = append(api.requests, "list "+sortBy)
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if !assert.NoError(t, err) {
				return
			}
			ids := slices.Sorted(maps.Keys(api.conversations))
			if sortBy == sortByActivity {
				slices.SortStableFunc(ids, func(a, b int64) int { return cmp.Compare(api.activity(b), api.activity(a)) })
			}
			payload := []any{}
			for index := (page - 1) * api.pageSize; index < min(page*api.pageSize, len(ids)); index++ {
				payload = append(payload, api.conversation(ids[index]))
			}
			result = map[string]any{"data": map[string]any{"payload": payload}}
		case strings.HasSuffix(path, "/messages"):
			_, err := fmt.Sscanf(path, "/conversations/%d/messages", &id)
			if !assert.NoError(t, err) {
				return
			}
			// The importer always sends both bounds; after is inclusive with them.
			after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
			api.requests = append(api.requests, fmt.Sprintf("messages %d %d %d", id, after, before))
			payload := []map[string]any{}
			for _, message := range api.conversations[id] {
				if messageID := fixtureInt(message, "id"); api.ignoreBounds || (messageID >= after && messageID < before) {
					payload = append(payload, message)
				}
			}
			slices.SortStableFunc(payload, func(a, b map[string]any) int {
				return cmp.Compare(fixtureInt(a, "created_at"), fixtureInt(b, "created_at"))
			})
			result = map[string]any{"meta": map[string]any{"contact": api.contact, "assignee": api.assignee}, "payload": payload[:min(len(payload), api.cap)]}
		default:
			_, err := fmt.Sscanf(path, "/conversations/%d", &id)
			if !assert.NoError(t, err) {
				return
			}
			api.requests = append(api.requests, fmt.Sprintf("conversation %d", id))
			result = api.conversation(id)
		}
		encoded, err := json.Marshal(result)
		if !assert.NoError(t, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(encoded)
		assert.NoError(t, err)
	}))
	t.Cleanup(api.server.Close)
	return api
}

// fixtureInt reads an int64 fixture field; fixtures always set them.
func fixtureInt(message map[string]any, key string) int64 {
	value, _ := message[key].(int64)
	return value
}

func (api *contractAPI) activity(id int64) int64 {
	activity := int64(1767225500)
	for _, message := range api.conversations[id] {
		activity = max(activity, fixtureInt(message, "created_at"))
	}
	return activity
}

func (api *contractAPI) conversation(id int64) map[string]any {
	inboxID := api.conversationInboxID
	if inboxID == 0 {
		inboxID = 7
	}
	// Chatwoot seeds the listing with the newest message by creation time. The
	// seed's content is private and differs from the message list, to catch
	// leakage through raw conversation context.
	var newest map[string]any
	for _, message := range api.conversations[id] {
		if newest == nil || cmp.Or(cmp.Compare(fixtureInt(message, "created_at"), fixtureInt(newest, "created_at")), cmp.Compare(fixtureInt(message, "id"), fixtureInt(newest, "id"))) > 0 {
			newest = message
		}
	}
	private := map[string]any{"private": true, "content": "excluded-private-seed"}
	if newest != nil {
		private["id"] = newest["id"]
	}
	return map[string]any{
		"id": id, "account_id": 3, "inbox_id": inboxID, "status": "resolved", "created_at": int64(1767225500), "updated_at": float64(1767225600), "last_activity_at": api.activity(id),
		"meta":     map[string]any{"sender": api.contact, "assignee": api.assignee},
		"messages": []any{private}, "last_non_activity_message": private,
	}
}

func (api *contractAPI) addMessage(conversationID, messageID int64, at time.Time, attachments ...map[string]any) {
	api.mu.Lock()
	defer api.mu.Unlock()
	message := contractMessage(messageID, at.Unix(), nil)
	message["conversation_id"] = conversationID
	if len(attachments) > 0 {
		message["attachments"] = attachments
	}
	api.conversations[conversationID] = append(api.conversations[conversationID], message)
}

// takeRequests returns and clears the requests seen since the last call.
func (api *contractAPI) takeRequests() []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	requests := api.requests
	api.requests = nil
	return requests
}

func (api *contractAPI) client(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(api.server.URL, 3, "synthetic-token")
	require.NoError(t, err)
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	client.messageRangeCap = api.cap
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
	sources, err := importer.Register(t.Context(), []Inbox{{ID: 7, Name: "Example Inbox"}})
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
	for _, pageCap := range []int{1, 3, 1000} {
		t.Run(fmt.Sprintf("cap_%d", pageCap), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			want := []int64{1, 2, 7, 9, 15, 33, 41, 70, 111, 901, 904, 1301, 2100}
			messages := make([]map[string]any, 0, len(want))
			for i, id := range want {
				messages = append(messages, contractMessage(id, 1767225600+int64((i*7)%5), nil))
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

// A capped response keeps every unhandled hole; a --full walk resumes its saved
// ranges instead of restarting at the first page.
func TestImportContractLimitedRunsResume(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pageCap int
		full    bool
	}{{"capped", 3, false}, {"full", 20, true}} {
		t.Run(tc.name, func(t *testing.T) {
			want := []int64{1, 2, 7, 9, 15, 33, 41}
			messages := make([]map[string]any, 0, len(want))
			for i, id := range want {
				messages = append(messages, contractMessage(id, 1767225600+int64(len(want)-i), nil))
			}
			api := newContractAPI(t, tc.pageCap, messages)
			st := testutil.NewTestStore(t)
			contractRegister(t, st, api)
			for range len(want) + 2 {
				sum, err := NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, Limit: 2, Full: tc.full})
				require.NoError(t, err)
				require.LessOrEqual(t, sum.MessagesProcessed, 2, "a run respects its message limit")
				if len(contractMessageIDs(t, st)) == len(want) {
					break
				}
			}
			assert.Equal(t, want, contractMessageIDs(t, st), "unfinished ranges survive importer restarts")
		})
	}
}

func TestImportContractRejectsIgnoredRangeBoundsWithoutPoisoningResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 20, []map[string]any{contractMessage(101, 1767225600, nil), contractMessage(102, 1767225601, nil)})
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
	api.mu.Lock()
	api.ignoreBounds = false
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	assert.Equal([]int64{101, 102}, contractMessageIDs(t, st))
}
