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
	for _, info := range []string{inboxTestInfo, `{}`, `{"app":null}`, `{"app":{"version":4}}`, strings.Replace(inboxTestInfo, "4.2.808", "4.2.807", 1), strings.Replace(inboxTestInfo, "4.2.808", "4.2.809", 1)} {
		peer := &inboxHTTPPeer{info: info, chat: inboxTestChat, page: inboxTestPage}
		provider := NewInboxProvider(peer.client(t), inboxTestSource())
		source := inboxTestSource()
		caps, err := provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source})
		require.NoError(t, err)
		assert.Equal(t, source, caps.Source)
		assert.Equal(t, "chat-archive-flag", caps.LocationModel)
		assert.False(t, caps.ConditionalWrite)
		assert.False(t, caps.ObservedAt.IsZero())
		for _, op := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread} {
			var found bool
			for _, capability := range caps.Operations {
				if capability.Operation == op {
					found = true
					assert.Equal(t, inboxcontrol.CapabilityUnavailable, capability.Status)
				}
			}
			assert.True(t, found, string(op))
		}
		assert.Equal(t, []string{"GET /v1/info"}, peer.calls(), "discovery URLs must never be followed")
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
	peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: inboxTestPage}
	provider := NewInboxProvider(peer.client(t), inboxTestSource())
	target := inboxTestTarget()
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpTags, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder} {
		r := inboxcontrol.Request{Operation: op, Target: &target, DryRun: true}
		before := inboxcontrol.State{Target: target, Inbox: inboxBool(false), Read: inboxBool(false)}
		_, err := provider.Preview(t.Context(), r, before)
		assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
		_, err = provider.Dispatch(t.Context(), r, before)
		assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
		assert.ErrorIs(t, provider.Verify(r, before, before, before), inboxcontrol.ErrOutcomeUnknown)
	}
	_, err := provider.Folders(t.Context(), inboxTestSource())
	assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
	assert.Empty(t, peer.calls())
}

func TestInboxMessageDirectionFromRaw(t *testing.T) {
	for _, tt := range []struct {
		field string
		want  bool
	}{
		{``, false}, {`,"isSender":null`, false}, {`,"isSender":"false"`, false}, {`,"isSender":true`, false}, {`,"isSender":false`, true},
	} {
		raw := `{"id":"message-A","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"10:opaque","timestamp":"2026-01-01T00:00:00Z"` + tt.field + `}`
		candidate, err := inboxIncomingCandidate([]byte(raw), inboxTestTarget())
		if tt.want {
			require.NoError(t, err)
			require.NotNil(t, candidate)
			assert.Equal(t, "message-A", candidate.ID)
			assert.Equal(t, "10:opaque", candidate.SortKey)
		} else {
			assert.Nil(t, candidate)
		}
		if tt.field != `,"isSender":"false"` {
			var message Message
			require.NoError(t, json.Unmarshal([]byte(raw), &message))
			assert.Equal(t, raw, string(message.Raw), "ordinary importer Raw bytes remain intact")
		}
	}
}

func TestInboxLocalEnumerationNeverAdmitsFrontier(t *testing.T) {
	message := `{"id":"message-A","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"999:opaque","timestamp":"2026-01-01T00:00:00Z","isSender":false}`
	page := `{"items":[` + message + `],"hasMore":false,"oldestCursor":"opaque:old","newestCursor":"opaque:new"}`
	peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: page}
	provider := NewInboxProvider(peer.client(t), inboxTestSource())
	window := provider.localInboxWindow(t.Context(), inboxTestTarget())
	assert.True(t, window.LocalComplete)
	assert.Equal(t, []inboxCandidate{{ID: "message-A", SortKey: "999:opaque"}}, window.Candidates)
	for range 2 {
		target := inboxTestTarget()
		state, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		require.NoError(t, err)
		assert.Nil(t, state.Read)
		assert.Empty(t, state.LastMessageID)
		_, err = provider.Preview(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}, state)
		assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
	}
	for _, call := range peer.calls() {
		assert.True(t, strings.HasPrefix(call, "GET "), call)
		assert.NotContains(t, call, "search")
	}
}

func TestInboxLocalEnumerationLimits(t *testing.T) {
	message := `{"id":"message-A","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"opaque","timestamp":"2026-01-01T00:00:00Z","isSender":false}`
	for _, page := range []string{
		`{}`, `{"items":null,"hasMore":false,"oldestCursor":"a","newestCursor":"b"}`,
		`{"items":[],"oldestCursor":"a","newestCursor":"b"}`,
		`{"items":[],"hasMore":null,"oldestCursor":"a","newestCursor":"b"}`,
		`{"items":[],"hasMore":"false","oldestCursor":"a","newestCursor":"b"}`,
		`{"items":[],"hasMore":true,"oldestCursor":"a","newestCursor":"b"}`,
		`{"items":[` + message + `],"hasMore":true,"oldestCursor":"","newestCursor":"b"}`,
		`{"items":[` + message + `,` + message + `],"hasMore":false,"oldestCursor":"a","newestCursor":"b"}`,
		`{"items":[` + strings.Replace(message, "account-example", "foreign-account", 1) + `],"hasMore":false,"oldestCursor":"a","newestCursor":"b"}`,
		`{"items":[` + message + `],"hasMore":true,"oldestCursor":"loop","newestCursor":"b"}`,
	} {
		peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: page}
		provider := NewInboxProvider(peer.client(t), inboxTestSource())
		window := provider.localInboxWindow(t.Context(), inboxTestTarget())
		assert.False(t, window.LocalComplete, page)
		assert.LessOrEqual(t, len(peer.calls()), 4)
	}
}

func TestInboxObservationBoundsAndRedirect(t *testing.T) {
	for _, body := range []string{`{"id":`, strings.Repeat(" ", (1<<20)+1)} {
		peer := &inboxHTTPPeer{info: inboxTestInfo, chat: body, page: inboxTestPage}
		provider := NewInboxProvider(peer.client(t), inboxTestSource())
		target := inboxTestTarget()
		_, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
	}
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); _, _ = w.Write([]byte(inboxTestChat)) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer server.Close()
	provider := NewInboxProvider(NewClient(server.URL, testToken, 10000), inboxTestSource())
	target := inboxTestTarget()
	_, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
	assert.EqualValues(t, 0, redirected.Load())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = provider.Observe(ctx, inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
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
	assert.False(t, window.LocalComplete)
	assert.Equal(t, "page-limit", window.Stopped)
	assert.Len(t, peer.calls(), 4)
	items := make([]string, 129)
	for i := range items {
		items[i] = message(i)
	}
	peer = &inboxHTTPPeer{page: page(items, false, "old")}
	provider = NewInboxProvider(peer.client(t), inboxTestSource())
	window = provider.localInboxWindow(t.Context(), inboxTestTarget())
	assert.False(t, window.LocalComplete)
	assert.Equal(t, "message-limit", window.Stopped)
	assert.Len(t, peer.calls(), 1)
}
