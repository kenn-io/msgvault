package beeper

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxCapabilitiesNeverPromiseChatWrites(t *testing.T) {
	assertions := assert.New(t)

	for _, info := range []string{inboxTestInfo, `{}`, `{"app":null}`, `{"app":{"version":4}}`, strings.Replace(inboxTestInfo, "4.2.808", "4.2.807", 1), strings.Replace(inboxTestInfo, "4.2.808", "4.2.809", 1)} {
		peer := &inboxHTTPPeer{info: info, chat: inboxTestChat, page: inboxTestPage}
		provider := NewInboxProvider(peer.client(t), inboxTestSource())
		source := inboxTestSource()
		caps, err := provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source})
		require.NoError(t, err)
		assertions.Equal(source, caps.Source)
		assertions.Equal("chat-archive-flag", caps.LocationModel)
		assertions.False(caps.ConditionalWrite)
		assertions.False(caps.ObservedAt.IsZero())
		for _, op := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread} {
			var found bool
			for _, capability := range caps.Operations {
				if capability.Operation == op {
					found = true
					assertions.Equal(inboxcontrol.CapabilityUnavailable, capability.Status)
				}
			}
			assertions.True(found, string(op))
		}
		assertions.Equal([]string{"GET /v1/info"}, peer.calls(), "discovery URLs must never be followed")
	}
}

func TestInboxProfileVersionAloneIsInsufficient(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want bool
	}{
		{inboxTestInfo, true},
		{`{"app":{"version":"4.2.808"}}`, false},
		{strings.Replace(inboxTestInfo, `"version":"4.2.808"`, `"version":null`, 1), false},
		{strings.Replace(inboxTestInfo, `4.2.808`, `4.2.809`, 1), false},
		{strings.Replace(inboxTestInfo, `"spec":"https://example.invalid/spec"`, `"spec":false`, 1), false},
	} {
		info, err := decodeInboxObject([]byte(tt.raw))
		require.NoError(t, err)
		assert.Equal(t, tt.want, inboxProfileShape(info))
	}
}

func TestInboxAllMutationsRefusedWithoutHTTP(t *testing.T) {
	requirements := require.New(t)

	peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: inboxTestPage}
	provider := NewInboxProvider(peer.client(t), inboxTestSource())
	target := inboxTestTarget()
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpTags, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder} {
		r := inboxcontrol.Request{Operation: op, Target: &target, DryRun: true}
		before := inboxcontrol.State{Target: target, Inbox: new(false), Read: new(false)}
		_, err := provider.Preview(t.Context(), r, before)
		requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
		_, err = provider.Dispatch(t.Context(), r, before)
		requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	}
	_, err := provider.Folders(t.Context(), inboxTestSource())
	requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	assert.Empty(t, peer.calls())
}

func TestInboxMessageDirectionFromRaw(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	for _, tt := range []struct {
		field string
		want  bool
	}{
		{``, false}, {`,"isSender":null`, false}, {`,"isSender":"false"`, false}, {`,"isSender":true`, false}, {`,"isSender":false`, true},
	} {
		raw := `{"id":"message-A","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"10:opaque","timestamp":"2026-01-01T00:00:00Z"` + tt.field + `}`
		candidate, incoming, err := inboxIncomingCandidate([]byte(raw), inboxTestTarget())
		if tt.want {
			requirements.NoError(err)
			requirements.True(incoming)
			assertions.Equal("message-A", candidate.ID)
			assertions.Equal("10:opaque", candidate.SortKey)
		} else {
			assertions.False(incoming)
		}
		if tt.field != `,"isSender":"false"` {
			var message Message
			requirements.NoError(json.Unmarshal([]byte(raw), &message))
			assertions.Equal(raw, string(message.Raw), "ordinary importer Raw bytes remain intact")
		}
	}
}

func TestInboxLocalEnumerationNeverAdmitsFrontier(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	message := `{"id":"message-A","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"999:opaque","timestamp":"2026-01-01T00:00:00Z","isSender":false}`
	page := `{"items":[` + message + `],"hasMore":false,"oldestCursor":"opaque:old","newestCursor":"opaque:new"}`
	peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: page}
	provider := NewInboxProvider(peer.client(t), inboxTestSource())
	window := provider.localInboxWindow(t.Context(), inboxTestTarget())
	assertions.True(window.LocalComplete)
	assertions.Equal([]inboxCandidate{{ID: "message-A", SortKey: "999:opaque"}}, window.Candidates)
	for range 2 {
		target := inboxTestTarget()
		state, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		requirements.NoError(err)
		assertions.Nil(state.Read)
		assertions.Empty(state.LastMessageID)
		_, err = provider.Preview(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}, state)
		requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	}
	for _, call := range peer.calls() {
		assertions.True(strings.HasPrefix(call, "GET "), call)
		assertions.NotContains(call, "search")
	}
}

func TestInboxLocalEnumerationLimits(t *testing.T) {
	message := `{"id":"message-A","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"opaque","timestamp":"2026-01-01T00:00:00Z","isSender":false}`
	tests := []struct {
		name, page, wantStopped string
		pages                   map[string]string
		wantCalls               int
	}{
		{name: "invalid response", page: `{}`, wantStopped: "envelope", wantCalls: 1},
		{name: "null items", page: `{"items":null,"hasMore":false,"oldestCursor":"a","newestCursor":"b"}`, wantStopped: "envelope", wantCalls: 1},
		{name: "missing hasMore", page: `{"items":[],"oldestCursor":"a","newestCursor":"b"}`, wantStopped: "envelope", wantCalls: 1},
		{name: "null hasMore", page: `{"items":[],"hasMore":null,"oldestCursor":"a","newestCursor":"b"}`, wantStopped: "envelope", wantCalls: 1},
		{name: "wrong hasMore type", page: `{"items":[],"hasMore":"false","oldestCursor":"a","newestCursor":"b"}`, wantStopped: "envelope", wantCalls: 1},
		{name: "more with empty page", page: `{"items":[],"hasMore":true,"oldestCursor":"a","newestCursor":"b"}`, wantStopped: "cursor-progress", wantCalls: 1},
		{name: "more without oldest cursor", page: `{"items":[` + message + `],"hasMore":true,"oldestCursor":"","newestCursor":"b"}`, wantStopped: "cursor-progress", wantCalls: 1},
		{name: "duplicate message IDs", page: `{"items":[` + message + `,` + message + `],"hasMore":false,"oldestCursor":"a","newestCursor":"b"}`, wantStopped: "duplicate-or-invalid-id", wantCalls: 1},
		{name: "foreign account", page: `{"items":[` + strings.Replace(message, "account-example", "foreign-account", 1) + `],"hasMore":false,"oldestCursor":"a","newestCursor":"b"}`, wantStopped: "message", wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: tt.page, pages: tt.pages}
			provider := NewInboxProvider(peer.client(t), inboxTestSource())
			window := provider.localInboxWindow(t.Context(), inboxTestTarget())
			assert.False(t, window.LocalComplete, tt.page)
			assert.Equal(t, tt.wantStopped, window.Stopped)
			assert.Len(t, peer.calls(), tt.wantCalls)
		})
	}
}

func TestInboxRepeatedCursorStopsAfterDistinctPageIDs(t *testing.T) {
	assertions := assert.New(t)

	message := func(id string) string {
		return `{"id":"` + id + `","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"opaque","timestamp":"2026-01-01T00:00:00Z","isSender":false}`
	}
	page := func(item, cursor string) string {
		return `{"items":[` + item + `],"hasMore":true,"oldestCursor":"` + cursor + `","newestCursor":"new"}`
	}
	peer := &inboxHTTPPeer{
		info: inboxTestInfo, chat: inboxTestChat,
		page:  page(message("message-A"), "loop"),
		pages: map[string]string{"loop": page(message("message-B"), "loop")},
	}
	provider := NewInboxProvider(peer.client(t), inboxTestSource())
	window := provider.localInboxWindow(t.Context(), inboxTestTarget())

	assertions.False(window.LocalComplete)
	assertions.Equal("cursor-progress", window.Stopped)
	assertions.Len(window.Candidates, 2)
	assertions.Len(peer.calls(), 2)
}

func TestInboxObservationBoundsAndRedirect(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	for _, body := range []string{`{"id":`, inboxTestChat + strings.Repeat(" ", (1<<20)+1)} {
		peer := &inboxHTTPPeer{info: inboxTestInfo, chat: body, page: inboxTestPage}
		provider := NewInboxProvider(peer.client(t), inboxTestSource())
		target := inboxTestTarget()
		_, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	}
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); _, _ = w.Write([]byte(inboxTestChat)) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer server.Close()
	provider := NewInboxProvider(NewClient(server.URL, testToken, 10000), inboxTestSource())
	target := inboxTestTarget()
	_, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	assertions.EqualValues(0, redirected.Load())
	peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: inboxTestPage}
	provider = NewInboxProvider(peer.client(t), inboxTestSource())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = provider.Observe(ctx, inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	assertions.Empty(peer.calls(), "caller cancellation must prevent provider requests")
}

func TestInboxOpaqueCursorsAndKeys(t *testing.T) {
	message := func(id, key string) string {
		return `{"id":"` + id + `","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"` + key + `","timestamp":"2026-01-01T00:00:00Z","isSender":false}`
	}
	peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: `{"items":[` + message("message-B", "2") + `],"hasMore":true,"oldestCursor":"opaque +/?&","newestCursor":"new"}`, pages: map[string]string{
		"opaque +/?&": `{"items":[` + message("message-A", "10") + `],"hasMore":false,"oldestCursor":"old","newestCursor":"new"}`,
	}}
	provider := NewInboxProvider(peer.client(t), inboxTestSource())
	window := provider.localInboxWindow(t.Context(), inboxTestTarget())
	assert.True(t, window.LocalComplete)
	assert.Equal(t, []inboxCandidate{{ID: "message-B", SortKey: "2"}, {ID: "message-A", SortKey: "10"}}, window.Candidates, "retain returned evidence without numeric or lexical ordering")
	assert.Equal(t, []string{"GET /v1/chats/%21chat:example.invalid/messages", "GET /v1/chats/%21chat:example.invalid/messages?cursor=opaque+%2B%2F%3F%26&direction=before"}, peer.calls())
}

func TestInboxEnumerationPageAndMessageCaps(t *testing.T) {
	assertions := assert.New(t)

	message := func(id int) string {
		return fmt.Sprintf(`{"id":"message-%d","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"opaque","timestamp":"2026-01-01T00:00:00Z","isSender":false}`, id)
	}
	page := func(items []string, more bool, cursor string) string {
		return fmt.Sprintf(`{"items":[%s],"hasMore":%t,"oldestCursor":"%s","newestCursor":"new"}`, strings.Join(items, ","), more, cursor)
	}
	peer := &inboxHTTPPeer{page: page([]string{message(0)}, true, "cursor1"), pages: map[string]string{}}
	for i := 1; i <= 4; i++ {
		peer.pages[fmt.Sprintf("cursor%d", i)] = page([]string{message(i)}, true, fmt.Sprintf("cursor%d", i+1))
	}
	provider := NewInboxProvider(peer.client(t), inboxTestSource())
	window := provider.localInboxWindow(t.Context(), inboxTestTarget())
	assertions.False(window.LocalComplete)
	assertions.Equal("page-limit", window.Stopped)
	assertions.Len(peer.calls(), 4)
	items := make([]string, 129)
	for i := range items {
		items[i] = message(i)
	}
	peer = &inboxHTTPPeer{page: page(items, false, "old")}
	provider = NewInboxProvider(peer.client(t), inboxTestSource())
	window = provider.localInboxWindow(t.Context(), inboxTestTarget())
	assertions.False(window.LocalComplete)
	assertions.Equal("message-limit", window.Stopped)
	assertions.Len(peer.calls(), 1)
}
