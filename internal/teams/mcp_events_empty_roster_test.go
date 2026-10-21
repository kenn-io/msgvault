package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPTeamsEmptyChatKeepsRoster(t *testing.T) {
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"synthetic-chat","chatType":"group","topic":"Synthetic chat"}]}`))
		case "/chats/synthetic-chat/members":
			_, _ = w.Write([]byte(`{"value":[{"id":"member-owner","userId":"owner-id","email":"owner@example.test","displayName":"Synthetic Owner"}]}`))
		case "/me/chats/synthetic-chat/messages":
			_, _ = w.Write([]byte(`{"value":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 50))
	_, err := imp.Import(t.Context(), ImportOptions{Email: "owner@example.test"})
	require.NoError(err)
	var members int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM conversation_participants`).Scan(&members))
	assert.Equal(t, 1, members, "message-free chats still archive their known roster")
}
