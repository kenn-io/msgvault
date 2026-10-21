package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// A partial successful member response must not turn unknown recipients into
// known absence, or establish a new complete chat cursor.
func TestMCPTeamsPartialRosterPreserved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var partial atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"synthetic-chat","chatType":"group","topic":"Synthetic chat"}]}`))
		case "/chats/synthetic-chat/members":
			peer := `{"id":"member-peer","userId":"peer-id","email":"peer@example.test","displayName":"Synthetic Peer"}`
			if partial.Load() {
				peer = `{"email":"peer@example.test","displayName":"Synthetic Peer"}`
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"member-owner","userId":"owner-id","email":"owner@example.test","displayName":"Synthetic Owner"},` + peer + `]}`))
		case "/me/chats/synthetic-chat/messages":
			modified := "2026-01-01T00:00:00Z"
			if partial.Load() {
				modified = "2026-01-02T00:00:00Z"
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"message-one","createdDateTime":"2026-01-01T00:00:00Z","lastModifiedDateTime":"` + modified + `","from":{"user":{"id":"sender@example.test","displayName":"Synthetic Sender","userIdentityType":"emailUser"}},"body":{"contentType":"text","content":"Synthetic readable message"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 50))
	opts := ImportOptions{Email: "owner@example.test"}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	partial.Store(true)
	second, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Positive(second.Errors)
	var recipients int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE recipient_type='to'`).Scan(&recipients))
	assert.Equal(2, recipients, "the unresolved peer must retain its previous recipient row")
	last, err := st.GetLastSuccessfulSync(first.SourceID)
	require.NoError(err)
	state, err := LoadSyncState(last.CursorAfter.String)
	require.NoError(err)
	assert.Equal("2026-01-01T00:00:00Z", state.ChatCursor("synthetic-chat"))
}
