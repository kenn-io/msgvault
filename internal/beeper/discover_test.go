package beeper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfParticipantJSON builds an isSelf chat member for the fake server.
func selfParticipantJSON(id, phone, email string) map[string]any {
	return map[string]any{
		"id": id, "phoneNumber": phone, "email": email,
		"fullName": "Test User", "isSelf": true,
	}
}

// otherParticipantJSON builds a non-self chat member for the fake server.
func otherParticipantJSON(id, name string) map[string]any {
	return map[string]any{"id": id, "fullName": name, "isSelf": false}
}

// searchRequests counts chat-search calls in the fake server's request log.
func searchRequests(reqs []string) int {
	n := 0
	for _, r := range reqs {
		if strings.HasPrefix(r, "/v1/chats/search") {
			n++
		}
	}
	return n
}

// An account Beeper serves chats for but leaves out of /v1/accounts (how it
// treats its native platform-sdk networks) must still be discovered, with the
// network name and the account owner's identity taken from chat data.
func TestDiscoverAccountsFindsAccountMissingFromAccountsEndpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addAccount(map[string]any{
		"accountID": "signal",
		"network":   "Signal",
		"user":      map[string]any{"id": "@signal_me:beeper.local", "phoneNumber": "+15550000001"},
	})
	f.addChat(&fakeChat{ID: "!s:x", AccountID: "signal", Network: "Signal", Title: "S", Type: "single",
		LastActivity: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)})
	f.addChat(&fakeChat{ID: "!i:x", AccountID: "imessage_abc", Network: "iMessage", Title: "Bob", Type: "single",
		LastActivity: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Participants: []map[string]any{
			otherParticipantJSON("bob@example.com", "Bob"),
			selfParticipantJSON("alice@example.com", "+15550000002", "alice@example.com"),
		}})
	srv := f.server()
	defer srv.Close()

	accounts, err := NewClient(srv.URL, testToken, 1000).DiscoverAccounts(context.Background())
	require.NoError(err)
	require.Len(accounts, 2)

	assert.Equal("signal", accounts[0].AccountID)
	assert.False(accounts[0].Discovered, "accounts the endpoint reported are not discovered")

	found := accounts[1]
	assert.Equal("imessage_abc", found.AccountID)
	assert.True(found.Discovered)
	assert.Equal("iMessage", found.Network, "network name comes from the chat")
	assert.Equal("+15550000002", found.User.PhoneNumber, "identity comes from the isSelf member")
	assert.Equal("alice@example.com", found.User.Email)
}

// An account reported by /v1/accounts must not be re-added just because it
// also owns chats, and its endpoint-provided identity must survive.
func TestDiscoverAccountsDoesNotDuplicateListedAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addAccount(map[string]any{
		"accountID": "signal",
		"network":   "Signal",
		"user":      map[string]any{"id": "@signal_me:beeper.local", "phoneNumber": "+15550000001"},
	})
	for i := range 3 {
		f.addChat(&fakeChat{ID: "!s" + strconv.Itoa(i) + ":x", AccountID: "signal", Network: "Signal",
			Type: "single", LastActivity: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			Participants: []map[string]any{selfParticipantJSON("@other:x", "+15559999999", "")}})
	}
	srv := f.server()
	defer srv.Close()

	accounts, err := NewClient(srv.URL, testToken, 1000).DiscoverAccounts(context.Background())
	require.NoError(err)
	require.Len(accounts, 1)
	assert.Equal("signal", accounts[0].AccountID)
	assert.False(accounts[0].Discovered)
	assert.Equal("+15550000001", accounts[0].User.PhoneNumber, "chat data must not overwrite the account's own identity")
	for _, req := range f.requests() {
		assert.NotContains(req, "/v1/chats/!s0:x", "no chat detail fetch for an already known account")
	}
}

// Discovery is additive: when the chat sweep fails, registration must still
// proceed with whatever /v1/accounts returned.
func TestDiscoverAccountsDegradesWhenChatSweepFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addAccount(map[string]any{"accountID": "signal", "network": "Signal"})
	f.addChat(&fakeChat{ID: "!i:x", AccountID: "imessage_abc", Network: "iMessage", Type: "single",
		LastActivity: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	f.setChatSearchFailure(true)
	srv := f.server()
	defer srv.Close()

	accounts, err := NewClient(srv.URL, testToken, 1000).DiscoverAccounts(context.Background())
	require.NoError(err, "a failed sweep must not fail discovery")
	require.Len(accounts, 1)
	assert.Equal("signal", accounts[0].AccountID)
}

// A rejected accounts endpoint is fatal: the token is bad, which the chat
// sweep cannot paper over and must not mask.
func TestDiscoverAccountsFailsWhenAccountsEndpointFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.setAccountsFailure(true)
	f.addChat(&fakeChat{ID: "!i:x", AccountID: "imessage_abc", Network: "iMessage", Type: "single",
		LastActivity: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	srv := f.server()
	defer srv.Close()

	_, err := NewClient(srv.URL, testToken, 1000).DiscoverAccounts(context.Background())
	require.Error(err)
	assert.Contains(err.Error(), "unauthorized")
	assert.Equal(0, searchRequests(f.requests()), "a rejected token must not trigger a chat sweep")
}

// Interrupting add-beeper must abort registration rather than half-finish it.
// Both fetch loops tolerate failures by design, so cancellation has to be
// caught at each: during the sweep, and during the identity probe that follows
// it. Neither may be reported as success.
func TestDiscoverAccountsPropagatesCancellation(t *testing.T) {
	// cancelOn names the request path at which the caller gives up.
	for _, tc := range []struct {
		name     string
		cancelOn string
	}{
		{name: "during the chat sweep", cancelOn: "/v1/chats/search"},
		{name: "during the identity probe", cancelOn: "/v1/chats/!i:x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.cancelOn {
					cancel()
					http.Error(w, `{"error":"aborted"}`, http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/accounts":
					_, _ = w.Write([]byte(`[]`))
				case "/v1/chats/search":
					_, _ = w.Write([]byte(`{"items":[{"id":"!i:x","accountID":"imessage_abc",` +
						`"network":"iMessage","type":"single"}],"hasMore":false}`))
				default:
					http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
				}
			}))
			defer srv.Close()

			_, err := NewClient(srv.URL, testToken, 1000).DiscoverAccounts(ctx)
			require.ErrorIs(err, context.Canceled)
		})
	}
}

// The sweep must stay bounded on a busy install rather than walking every
// chat the user has ever had.
func TestDiscoverAccountsBoundsTheChatSweep(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.chatPageSize = 2
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := range 4 * discoverChatPages {
		f.addChat(&fakeChat{ID: "!c" + strconv.Itoa(i) + ":x", AccountID: "imessage_abc", Network: "iMessage",
			Type: "single", LastActivity: base.Add(-time.Duration(i) * time.Hour)})
	}
	srv := f.server()
	defer srv.Close()

	accounts, err := NewClient(srv.URL, testToken, 1000).DiscoverAccounts(context.Background())
	require.NoError(err)
	require.Len(accounts, 1)
	assert.Equal("imessage_abc", accounts[0].AccountID)
	assert.Equal(discoverChatPages, searchRequests(f.requests()), "sweep must stop at its page bound")
}

// Beeper exposes different identity fields on different chats, so the identity
// probe merges what it finds instead of trusting the first chat it sees.
func TestDiscoverAccountsMergesSelfIdentityAcrossChats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.chatPageSize = 10
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Newest chat first: no self member at all, then email only, then phone.
	f.addChat(&fakeChat{ID: "!a:x", AccountID: "imessage_abc", Network: "iMessage", Type: "group",
		LastActivity: base,
		Participants: []map[string]any{otherParticipantJSON("bob@example.com", "Bob")}})
	f.addChat(&fakeChat{ID: "!b:x", AccountID: "imessage_abc", Network: "iMessage", Type: "single",
		LastActivity: base.Add(-time.Hour),
		Participants: []map[string]any{selfParticipantJSON("alice@example.com", "", "alice@example.com")}})
	f.addChat(&fakeChat{ID: "!c:x", AccountID: "imessage_abc", Network: "iMessage", Type: "single",
		LastActivity: base.Add(-2 * time.Hour),
		Participants: []map[string]any{selfParticipantJSON("+15550000002", "+15550000002", "")}})
	// Beyond the probe bound: never fetched.
	f.addChat(&fakeChat{ID: "!d:x", AccountID: "imessage_abc", Network: "iMessage", Type: "single",
		LastActivity: base.Add(-3 * time.Hour),
		Participants: []map[string]any{selfParticipantJSON("+15559999999", "+15559999999", "")}})
	srv := f.server()
	defer srv.Close()

	accounts, err := NewClient(srv.URL, testToken, 1000).DiscoverAccounts(context.Background())
	require.NoError(err)
	require.Len(accounts, 1)
	assert.Equal("alice@example.com", accounts[0].User.Email, "email survives from the chat that had it")
	assert.Equal("+15550000002", accounts[0].User.PhoneNumber, "phone comes from a later chat")
	for _, req := range f.requests() {
		assert.NotEqual("/v1/chats/!d:x?maxParticipantCount=-1", req, "identity probing must stay bounded")
	}
}

// An account whose chats cannot be read is still worth registering: the sync
// path only needs the account ID.
func TestDiscoverAccountsRegistersAccountWithUnreadableChat(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addChat(&fakeChat{ID: "!gone:x", AccountID: "imessage_abc", Network: "iMessage", Type: "single",
		LastActivity: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	srv := f.server()
	defer srv.Close()

	f.setChatGetFailure("!gone:x", true)

	accounts, err := NewClient(srv.URL, testToken, 1000).DiscoverAccounts(context.Background())
	require.NoError(err)
	require.Len(accounts, 1)
	assert.Equal("imessage_abc", accounts[0].AccountID)
	assert.True(accounts[0].Discovered)
	assert.Empty(accounts[0].User.PhoneNumber)
}
