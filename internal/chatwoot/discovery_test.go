package chatwoot

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/time/rate"
)

func TestDiscoveryResumesBoundedInboxPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var pages []int
	seen := map[int64]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any
		switch r.URL.Path {
		case "/api/v1/accounts/1/inboxes":
			result = map[string]any{"payload": []any{map[string]any{"id": 1}}}
		case "/api/v1/accounts/1/agents":
			result = []any{}
		case "/api/v1/accounts/1/conversations":
			assert.Equal("1", r.URL.Query().Get("inbox_id"))
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if !assert.NoError(err) {
				return
			}
			pages = append(pages, page)
			payload := []any{}
			if page <= 5 {
				for offset := 1; offset <= 3; offset++ {
					payload = append(payload, map[string]any{"id": int64((page-1)*3 + offset), "inbox_id": 1})
				}
			}
			result = map[string]any{"data": map[string]any{"payload": payload}}
		default:
			var id int64
			_, err := fmt.Sscanf(r.URL.Path, "/api/v1/accounts/1/conversations/%d/messages", &id)
			assert.NoError(err)
			seen[id] = true
			result = map[string]any{"payload": []any{}}
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
	_, err = imp.Register(t.Context(), []int64{1})
	require.NoError(err)
	imp.requestBudget = 2
	for run := range 8 {
		pages = nil
		summary, err := imp.Import(t.Context(), ImportOptions{InboxID: 1})
		require.NoError(err)
		assert.LessOrEqual(len(pages), 2, "discovery must have a finite per-run budget")
		if run < 2 {
			assert.True(summary.Partial)
		}
	}
	assert.Len(seen, 15, "unvisited conversations and later pages must remain reachable on retries")
}

func TestDiscoveryEOFRemainsPartialWithEarlierPendingHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var pages []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any
		switch r.URL.Path {
		case "/api/v1/accounts/1/inboxes":
			result = map[string]any{"payload": []any{map[string]any{"id": 1}}}
		case "/api/v1/accounts/1/agents":
			result = []any{}
		case "/api/v1/accounts/1/conversations":
			assert.Equal("1", r.URL.Query().Get("inbox_id"))
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if !assert.NoError(err) {
				return
			}
			pages = append(pages, page)
			payload := []any{}
			if page <= 2 {
				payload = append(payload, map[string]any{"id": page, "inbox_id": 1})
			}
			result = map[string]any{"data": map[string]any{"payload": payload}}
		case "/api/v1/accounts/1/conversations/1/messages":
			after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			if !assert.NoError(err) {
				return
			}
			before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
			if !assert.NoError(err) {
				return
			}
			payload := []any{}
			for _, id := range []int64{101, 102} {
				if id >= after && id < before {
					message := contractMessage(id, 1801526400+id, nil)
					message["inbox_id"] = int64(1)
					message["conversation_id"] = int64(1)
					payload = append(payload, message)
				}
			}
			result = map[string]any{"payload": payload}
		case "/api/v1/accounts/1/conversations/2/messages":
			result = map[string]any{"payload": []any{}}
		default:
			assert.Fail("unexpected Chatwoot API path", "%s", r.URL.Path)
			http.NotFound(w, r)
			return
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
	imp.requestBudget = 2
	opts := ImportOptions{InboxID: 1, Limit: 1}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, first.MessagesProcessed)
	state, err := imp.resumeState(sources[0].ID, sources[0].Identifier)
	require.NoError(err)
	require.NotNil(state.Conversations["1"])
	require.NotEmpty(state.Conversations["1"].Pending)
	require.Equal(2, state.NextPage)

	pages = nil
	second, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal([]int{2, 3}, pages, "the resumed discovery reaches EOF after the empty tail")
	assert.Zero(second.MessagesProcessed)
	state, err = imp.resumeState(sources[0].ID, sources[0].Identifier)
	require.NoError(err)
	require.NotNil(state.Conversations["1"])
	require.NotEmpty(state.Conversations["1"].Pending)
	assert.True(second.Partial, "discovery EOF must not hide earlier unfinished history")
}
