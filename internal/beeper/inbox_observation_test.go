package beeper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

const inboxTestInfo = `{"app":{"version":"4.2.808","name":"Beeper","bundle_id":"example.beeper"},"endpoints":{"spec":"https://example.invalid/spec"}}`
const inboxTestChat = `{"id":"!chat:example.invalid","accountID":"account-example","type":"single","isArchived":false,"unreadCount":0,"isMarkedUnread":false,"lastReadMessageSortKey":"opaque:9","capabilities":{"archive":true,"markAsUnread":true,"readReceipts":true}}`
const inboxTestPage = `{"items":[],"hasMore":false,"oldestCursor":"old","newestCursor":"new"}`

func inboxTestSource() inboxcontrol.SourceIdentity {
	return inboxcontrol.SourceIdentity{SourceID: 11, SourceType: "beeper", SourceIdentifier: "account-example", AccountID: "account-example"}
}

func inboxTestTarget() inboxcontrol.Target {
	return inboxcontrol.Target{SourceID: 11, SourceType: "beeper", SourceIdentifier: "account-example", AccountID: "account-example", Scope: inboxcontrol.ScopeChat, ItemID: 22, ProviderID: "!chat:example.invalid"}
}

type inboxHTTPPeer struct {
	mu               sync.Mutex
	requests         []string
	info, chat, page string
	pages            map[string]string
	status           int
}

func (p *inboxHTTPPeer) client(tb testing.TB) *Client {
	tb.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.requests = append(p.requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if p.status != 0 {
			w.WriteHeader(p.status)
			return
		}
		switch r.URL.Path {
		case "/v1/info":
			_, _ = w.Write([]byte(p.info))
		case "/v1/chats/!chat:example.invalid":
			_, _ = w.Write([]byte(p.chat))
		case "/v1/chats/!chat:example.invalid/messages":
			page := p.page
			if next, ok := p.pages[r.URL.Query().Get("cursor")]; ok {
				page = next
			}
			_, _ = w.Write([]byte(page))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	tb.Cleanup(srv.Close)
	return NewClient(srv.URL, testToken, 10000)
}

func (p *inboxHTTPPeer) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

func TestInboxObservePresence(t *testing.T) {
	for _, tt := range []struct {
		name, fields        string
		inbox, read, marked *bool
	}{
		{"absent", ``, nil, nil, nil},
		{"null", `,"isArchived":null,"unreadCount":null,"isMarkedUnread":null`, nil, nil, nil},
		{"wrong types", `,"isArchived":"false","unreadCount":"0","isMarkedUnread":0`, nil, nil, nil},
		{"explicit false", `,"isArchived":false,"unreadCount":0,"isMarkedUnread":false`, new(true), nil, new(false)},
		{"archived unread", `,"isArchived":true,"unreadCount":3,"isMarkedUnread":false`, new(false), new(false), new(false)},
		{"marked unread zero", `,"isArchived":false,"unreadCount":0,"isMarkedUnread":true`, new(true), new(false), new(true)},
		{"negative count", `,"unreadCount":-1`, nil, nil, nil},
		{"fractional count", `,"unreadCount":0.5`, nil, nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			peer := &inboxHTTPPeer{info: inboxTestInfo, chat: `{"id":"!chat:example.invalid","accountID":"account-example","type":"single"` + tt.fields + `}`, page: inboxTestPage}
			provider := NewInboxProvider(peer.client(t), inboxTestSource())
			target := inboxTestTarget()
			state, err := provider.Observe(context.Background(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
			requirements.NoError(err)
			assertions.Equal(target, state.Target)
			_, fingerprintErr := inboxcontrol.SemanticFingerprint(state)
			requirements.NoError(fingerprintErr, "item observations must be accepted by the existing native state contract")
			assertions.Equal(inboxcontrol.SourceIdentity{}, state.Source)
			assertions.Equal(tt.inbox, state.Inbox)
			assertions.Equal(tt.read, state.Read)
			assertions.Equal(tt.marked, state.MarkedUnread)
			assertions.Empty(state.LastMessageID, "local enumeration cannot authorize an incoming boundary")
			assertions.NotEmpty(state.Revision)
			assertions.False(state.ObservedAt.IsZero())
			for _, call := range peer.calls() {
				assertions.True(strings.HasPrefix(call, "GET "), call)
			}
		})
	}
}

func TestInboxObserveExactIdentity(t *testing.T) {
	for _, chat := range []string{
		strings.Replace(inboxTestChat, `!chat:example.invalid`, `!foreign:example.invalid`, 1),
		strings.Replace(inboxTestChat, `account-example`, `foreign-account`, 1),
		strings.TrimSuffix(inboxTestChat, "}") + `,"merge":{"chatIDs":["!member:example.invalid"],"defaultChatID":"!chat:example.invalid"}}`,
		strings.TrimSuffix(inboxTestChat, "}") + `,"mergedIntoChatID":"!merged:example.invalid"}`,
		strings.TrimSuffix(inboxTestChat, "}") + `,"merge":false}`,
		`null`,
	} {
		peer := &inboxHTTPPeer{info: inboxTestInfo, chat: chat, page: inboxTestPage}
		provider := NewInboxProvider(peer.client(t), inboxTestSource())
		target := inboxTestTarget()
		_, err := provider.Observe(context.Background(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		require.Error(t, err)
		for _, call := range peer.calls() {
			assert.NotContains(t, call, "/messages")
		}
	}
}

func TestInboxObserveBindingBeforeHTTP(t *testing.T) {
	for _, change := range []func(*inboxcontrol.Target){
		func(v *inboxcontrol.Target) { v.AccountID = "foreign" },
		func(v *inboxcontrol.Target) { v.SourceIdentifier = "foreign" },
		func(v *inboxcontrol.Target) { v.SourceID++ },
		func(v *inboxcontrol.Target) { v.Scope = inboxcontrol.ScopeMessage },
		func(v *inboxcontrol.Target) { v.SourceType = "gmail" },
	} {
		peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: inboxTestPage}
		provider := NewInboxProvider(peer.client(t), inboxTestSource())
		target := inboxTestTarget()
		change(&target)
		_, err := provider.Observe(context.Background(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		require.Error(t, err)
		assert.Empty(t, peer.calls())
	}
}

func TestInboxObserveDurableMarkerAndRevision(t *testing.T) {
	assertions := assert.New(t)

	observe := func(chat string) inboxcontrol.State {
		peer := &inboxHTTPPeer{info: inboxTestInfo, chat: chat, page: inboxTestPage}
		provider := NewInboxProvider(peer.client(t), inboxTestSource())
		target := inboxTestTarget()
		state, err := provider.Observe(context.Background(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		require.NoError(t, err)
		return state
	}
	state := observe(inboxTestChat)
	assertions.Contains(state.Flags, `beeper:last-read-message-sort-key:opaque:9`)
	assertions.Equal(state.Revision, observe(inboxTestChat).Revision)
	assertions.Equal(state.Revision, observe(strings.TrimSuffix(inboxTestChat, "}")+`,"title":"Cosmetic title"}`).Revision)
	for _, replacement := range []struct{ old, new string }{
		{`"opaque:9"`, `"opaque:10"`},
		{`"isArchived":false`, `"isArchived":true`},
		{`"archive":true`, `"archive":false`},
		{`"isMarkedUnread":false`, `"isMarkedUnread":null`},
	} {
		assertions.NotEqual(state.Revision, observe(strings.Replace(inboxTestChat, replacement.old, replacement.new, 1)).Revision)
	}
}
