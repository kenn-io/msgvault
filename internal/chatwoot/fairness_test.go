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

func TestBudgetResumesAcrossConversations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	seen := map[int64]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any
		switch r.URL.Path {
		case "/api/v1/accounts/1/inboxes":
			result = map[string]any{"payload": []any{map[string]any{"id": 1}}}
		case "/api/v1/accounts/1/agents":
			result = []any{}
		case "/api/v1/accounts/1/conversations":
			payload := []any{}
			if r.URL.Query().Get("page") == "1" {
				for id := int64(1); id <= 5; id++ {
					payload = append(payload, map[string]any{"id": id, "inbox_id": 1})
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
			http.Error(w, "encode fixture", http.StatusInternalServerError)
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
	for range 3 {
		_, err = imp.Import(t.Context(), ImportOptions{InboxID: 1})
		require.NoError(err)
	}
	assert.Len(seen, 5, "old empty conversations must not starve later conversations")
}

func TestBudgetRotatesCompletedArtifacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var messages []map[string]any
	for id := int64(101); id <= 103; id++ {
		m := contractMessage(id, 1801526400+id, nil)
		m["attachments"] = []any{map[string]any{"id": id, "file_type": "audio", "transcribed_text": "original"}}
		messages = append(messages, m)
	}
	api := newContractAPI(t, 1000, messages)
	st := testutil.NewTestStore(t)
	imp, _ := contractRegister(t, st, api)
	_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	func() {
		api.mu.Lock()
		defer api.mu.Unlock()
		for _, m := range api.messages {
			attachments, ok := m["attachments"].([]any)
			require.True(ok)
			require.NotEmpty(attachments)
			attachment, ok := attachments[0].(map[string]any)
			require.True(ok)
			attachment["transcribed_text"] = "corrected"
		}
	}()
	imp.requestBudget = 2
	for range 3 {
		_, err = imp.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
		require.NoError(err)
	}
	for id := int64(101); id <= 103; id++ {
		body, err := st.GetMessageBodyText(contractArchivedMessageID(t, st, strconv.FormatInt(id, 10)))
		require.NoError(err)
		assert.Contains(body, "corrected")
	}
}
