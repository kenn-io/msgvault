package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func TestNativeTeamsRefreshInvalidGrantDuringReadsRevokesPermission(t *testing.T) {
	for _, test := range []struct {
		name         string
		armAfterPath string
	}{
		{name: "self_chat_probe", armAfterPath: "/me/chats"},
		{name: "chat_message_read", armAfterPath: "/chats/event-chat/members"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
				Enabled: true, Principal: "synthetic-owner",
				Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "teams", Kinds: []string{"message"}}},
			})
			require.NoError(err)

			var failNextToken, profileSucceeded atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/me":
					profileSucceeded.Store(true)
					_, _ = w.Write([]byte(`{"id":"synthetic-owner-id","mail":"owner@example.test","userPrincipalName":"owner@example.test"}`))
				case "/me/chats":
					if r.URL.Path == test.armAfterPath {
						failNextToken.Store(true)
					}
					_, _ = w.Write([]byte(`{"value":[{"id":"event-chat","chatType":"group","topic":"Synthetic event chat"}]}`))
				case "/chats/event-chat/members":
					if r.URL.Path == test.armAfterPath {
						failNextToken.Store(true)
					}
					_, _ = w.Write([]byte(`{"value":[{"id":"synthetic-owner-id","userId":"synthetic-owner-id","email":"owner@example.test","displayName":"Synthetic Owner"}]}`))
				case "/me/chats/" + SelfChatID + "/messages":
					http.NotFound(w, r)
				case "/me/chats/event-chat/messages":
					_, _ = w.Write([]byte(`{"value":[]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			client := NewClient(srv.URL, func(context.Context) (string, error) {
				if failNextToken.Swap(false) {
					return "", &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: "synthetic refresh token revoked"}
				}
				return "synthetic-token", nil
			}, 1000, WithGrantedScopesReader(func() ([]string, error) { return []string{"Chat.Read"}, nil }))
			imp := NewImporter(st, client)
			sum, err := imp.Import(t.Context(), ImportOptions{Email: "owner@example.test"})
			require.True(profileSucceeded.Load(), "the denial must happen after account verification")
			require.Error(err)
			var refreshErr *oauth2.RetrieveError
			require.ErrorAs(err, &refreshErr)
			assert.Equal("invalid_grant", refreshErr.ErrorCode)

			allowed, err := st.NativeSourceMessagePermission(t.Context(), sum.SourceID)
			require.NoError(err)
			assert.False(allowed, "a refresh revocation after profile verification pauses Events authority")
		})
	}
}
