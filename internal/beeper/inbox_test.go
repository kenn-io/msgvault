package beeper

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxBeeperNativeMarkersAndPreflight(t *testing.T) {
	for _, tc := range []struct {
		name             string
		archived, marked *bool
		unread           *int
		capability       *bool
		merge            any
		wantRead         *bool
		denied           bool
		unsupported      bool
	}{
		{name: "explicit unread with zero count", archived: new(false), marked: new(true), unread: new(0), capability: new(true), wantRead: new(false)},
		{name: "explicit read", archived: new(true), marked: new(false), unread: new(0), capability: new(true), wantRead: new(true)},
		{name: "missing markers", capability: new(true), unsupported: true},
		{name: "missing capability", archived: new(false), marked: new(false), unread: new(0), wantRead: new(true), unsupported: true},
		{name: "false capability", archived: new(false), marked: new(false), unread: new(0), capability: new(false), wantRead: new(true), unsupported: true},
		{name: "merged routing", archived: new(false), marked: new(false), unread: new(0), capability: new(true), merge: map[string]any{"chatIDs": []string{"member-a", "member-b"}, "defaultChatID": "member-a"}, denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
				if serveEmptyInboxMessages(w, r) {
					return
				}
				if r.Method != http.MethodGet {
					writes++
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				assert.Equal(t, "/v1/chats/chat-a", r.URL.Path)
				data := map[string]any{"id": "chat-a", "accountID": "account-a", "lastActivity": "2026-10-05T00:00:00Z", "draft": map[string]any{"text": "Synthetic occupied draft", "attachments": []any{map[string]any{"filePath": "/synthetic/example.txt"}}}, "capabilities": map[string]any{"archive": tc.capability, "markAsUnread": true}}
				if tc.archived != nil {
					data["isArchived"] = tc.archived
				}
				if tc.marked != nil {
					data["isMarkedUnread"] = tc.marked
				}
				if tc.unread != nil {
					data["unreadCount"] = tc.unread
				}
				if tc.merge != nil {
					data["merge"] = tc.merge
				}
				body, err := json.Marshal(data)
				assert.NoError(t, err)
				_, err = w.Write(body)
				assert.NoError(t, err)
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
			client := NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
			provider := NewInboxProvider(client, source)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			if tc.denied {
				require.ErrorIs(t, err, inboxcontrol.ErrDenied)
				assertions.Equal(0, writes)
				return
			}
			requirements.NoError(err)
			assertions.Equal(tc.wantRead, before.Read)
			assertions.Equal(tc.marked, before.MarkedUnread)
			projected, err := provider.Preview(t.Context(), request, before)
			if tc.unsupported {
				require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			} else {
				requirements.NoError(err)
				requirements.NotNil(projected.Inbox)
				assertions.False(*projected.Inbox)
				assertions.Equal(before.Read, projected.Read)
			}
			assertions.Equal(0, writes, "state and preview must never mark read or alter drafts")
		})
	}
}

func TestInboxBeeperRejectsForeignResponsesAndUnsafeReads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		status   int
		want     error
	}{
		{"foreign chat", `{"id":"other-chat","accountID":"account-a"}`, 200, inboxcontrol.ErrDenied},
		{"foreign account", `{"id":"chat-a","accountID":"other-account"}`, 200, inboxcontrol.ErrDenied},
		{"merged member", `{"id":"chat-a","accountID":"account-a","mergedIntoChatID":"aggregate"}`, 200, inboxcontrol.ErrDenied},
		{"old or malformed response", `Bearer synthetic-secret`, 200, inboxcontrol.ErrUnavailable},
		{"oversized response", strings.Repeat("x", maxInboxResponseBytes+1), 200, inboxcontrol.ErrUnavailable},
		{"unavailable", `Bearer synthetic-secret`, 503, inboxcontrol.ErrUnavailable},
		{"redirect", ``, 302, inboxcontrol.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			redirected := 0
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++; w.WriteHeader(http.StatusOK) }))
			defer destination.Close()
			reads, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				reads++
				if tc.status == 302 {
					w.Header().Set("Location", destination.URL)
				}
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.response))
				assert.NoError(t, err)
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
			provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
			_, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
			require.ErrorIs(t, err, tc.want)
			if err != nil {
				assertions.NotContains(err.Error(), "synthetic-secret")
			}
			assertions.Equal(1, reads, "control GET must not retry or follow redirects")
			assertions.Equal(0, writes)
			assertions.Equal(0, redirected)
			target.AccountID = "foreign"
			_, err = provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
			require.ErrorIs(t, err, inboxcontrol.ErrDenied)
			assertions.Equal(1, reads, "foreign bindings fail before provider access")
		})
	}
}

func TestInboxBeeperNativeArchiveAndUnreadWriteOnce(t *testing.T) {
	for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetUnread} {
		t.Run(string(operation), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			archived := operation == inboxcontrol.OpUnarchive
			marked := false
			posts := 0
			draft := map[string]any{"text": "Synthetic occupied composer", "attachments": []any{map[string]any{"filePath": "/synthetic/example.txt"}}}
			response := func() map[string]any {
				return map[string]any{"id": "chat-a", "accountID": "account-a", "isArchived": archived, "isMarkedUnread": marked, "unreadCount": 0, "draft": draft, "lastActivity": "2026-10-05T00:00:00Z", "capabilities": map[string]any{"archive": true, "markAsUnread": true}}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
				if serveEmptyInboxMessages(w, r) {
					return
				}
				if r.Method == http.MethodGet {
					body, err := json.Marshal(response())
					assert.NoError(t, err)
					_, err = w.Write(body)
					assert.NoError(t, err)
					return
				}
				posts++
				assert.Equal(t, http.MethodPost, r.Method)
				var body map[string]any
				assert.NoError(t, json.UnmarshalRead(r.Body, &body))
				if operation == inboxcontrol.OpSetUnread {
					assert.Equal(t, "/v1/chats/chat-a/unread", r.URL.Path)
					assert.Empty(t, body)
					marked = true
					encoded, err := json.Marshal(response())
					assert.NoError(t, err)
					_, err = w.Write(encoded)
					assert.NoError(t, err)
				} else {
					assert.Equal(t, "/v1/chats/chat-a/archive", r.URL.Path)
					assert.Equal(t, map[string]any{"archived": operation == inboxcontrol.OpArchive}, body)
					archived = operation == inboxcontrol.OpArchive
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
			provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
			request := inboxcontrol.Request{Operation: operation, Target: &target, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			projected, err := provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			request.DryRun = false
			_, err = provider.Dispatch(t.Context(), request, before)
			requirements.NoError(err)
			after, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			requirements.NoError(provider.Verify(request, before, projected, after))
			assertions.Equal(1, posts)
			assertions.Equal(before.Revision, after.Revision, "occupied/composite drafts and unrelated metadata are preserved")
		})
	}
}

func TestInboxBeeperUncertainWritesNeverRetryAndPreserveReadState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation inboxcontrol.Operation
		status    int
		body      string
		alterRead bool
		want      error
	}{
		{"server error after archive", inboxcontrol.OpArchive, 503, "synthetic-secret", false, inboxcontrol.ErrOutcomeUnknown},
		{"redirect after archive", inboxcontrol.OpArchive, 302, "", false, inboxcontrol.ErrOutcomeUnknown},
		{"foreign unread response", inboxcontrol.OpSetUnread, 200, `{"id":"foreign","accountID":"foreign"}`, false, inboxcontrol.ErrOutcomeUnknown},
		{"oversized archive response", inboxcontrol.OpArchive, 200, strings.Repeat("x", maxInboxResponseBytes+1), false, inboxcontrol.ErrOutcomeUnknown},
		{"read changed with archive", inboxcontrol.OpArchive, 204, "", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			posts, redirected := 0, 0
			archived, marked := false, false
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++; w.WriteHeader(http.StatusNoContent) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveEmptyInboxMessages(w, r) {
					return
				}
				if r.Method == http.MethodGet {
					body, err := json.Marshal(map[string]any{"id": "chat-a", "accountID": "account-a", "isArchived": archived, "isMarkedUnread": marked, "unreadCount": 0, "draft": nil, "lastActivity": "2026-10-05T00:00:00Z", "capabilities": map[string]any{"archive": true, "markAsUnread": true}})
					assert.NoError(t, err)
					_, err = w.Write(body)
					assert.NoError(t, err)
					return
				}
				posts++
				if tc.operation == inboxcontrol.OpArchive {
					archived = true
				} else {
					marked = true
				}
				if tc.alterRead {
					marked = true
				}
				if tc.status == 302 {
					w.Header().Set("Location", destination.URL)
				}
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
			provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
			request := inboxcontrol.Request{Operation: tc.operation, Target: &target}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			projected, err := provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			_, err = provider.Dispatch(t.Context(), request, before)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assertions.NotContains(err.Error(), "synthetic-secret")
			} else {
				requirements.NoError(err)
			}
			assertions.Equal(1, posts)
			assertions.Equal(0, redirected)
			after, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			verifyErr := provider.Verify(request, before, projected, after)
			if tc.alterRead {
				require.ErrorIs(t, verifyErr, inboxcontrol.ErrOutcomeUnknown)
			} else {
				requirements.NoError(verifyErr, "independent readback can prove an uncertain POST later")
			}
			assertions.Equal(1, posts, "verification is read-only")
		})
	}
}

func serveEmptyInboxMessages(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/messages") {
		return false
	}
	_, _ = w.Write([]byte(`{"items":[],"hasMore":false}`))
	return true
}

func TestInboxBeeperDraftDigestIgnoresObjectOrderAndPreservesNumbers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	drafts := []string{
		`{"nested":{"b":2,"a":1},"text":"Synthetic","n":9007199254740992}`,
		`{"n":9007199254740992,"text":"Synthetic","nested":{"a":1,"b":2}}`,
		`{"n":9007199254740993,"text":"Synthetic","nested":{"a":1,"b":2}}`,
	}
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Less(t, reads, len(drafts)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body := `{"id":"chat-a","accountID":"account-a","draft":` + drafts[reads] + `}`
		reads++
		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	}))
	defer server.Close()
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
	target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
	provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}
	first, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	reordered, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	changed, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	assertions.Equal(first.Revision, reordered.Revision, "same nested draft must have one semantic revision")
	assertions.NotEqual(reordered.Revision, changed.Revision, "large integers must not lose precision")
}

func TestInboxBeeperMarkReadUsesExactLatestMessageBoundary(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	marked, unread := true, 2
	watermark := "8"
	latest := "message-ten"
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/messages") {
			assert.Equal(t, http.MethodGet, r.Method)
			body, err := json.Marshal(map[string]any{"items": []map[string]any{{"id": latest, "accountID": "account-a", "chatID": "chat-a", "sortKey": "10", "text": "Synthetic text is excluded from state"}, {"id": "message-nine", "accountID": "account-a", "chatID": "chat-a", "sortKey": "9"}}, "hasMore": false})
			assert.NoError(t, err)
			_, err = w.Write(body)
			assert.NoError(t, err)
			return
		}
		chat := func() map[string]any {
			return map[string]any{"id": "chat-a", "accountID": "account-a", "isArchived": false, "isMarkedUnread": marked, "unreadCount": unread, "lastReadMessageSortKey": watermark, "draft": nil, "lastActivity": "2026-10-05T00:00:00Z", "capabilities": map[string]any{"archive": true, "markAsUnread": true}}
		}
		if r.Method == http.MethodPost {
			posts++
			assert.Equal(t, "/v1/chats/chat-a/read", r.URL.Path)
			var body map[string]string
			assert.NoError(t, json.UnmarshalRead(r.Body, &body))
			assert.Equal(t, map[string]string{"messageID": "message-ten"}, body)
			marked, unread, watermark = false, 0, "10"
		} else {
			assert.Equal(t, http.MethodGet, r.Method)
		}
		body, err := json.Marshal(chat())
		assert.NoError(t, err)
		_, err = w.Write(body)
		assert.NoError(t, err)
	}))
	defer server.Close()
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
	target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
	provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	assertions.Equal("message-ten", before.LastMessageID)
	projected, err := provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	_, err = provider.Dispatch(t.Context(), request, before)
	requirements.NoError(err)
	after, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(request, before, projected, after))
	assertions.Equal(1, posts)
	watermark = ""
	unproved, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	require.ErrorIs(t, provider.Verify(request, before, projected, unproved), inboxcontrol.ErrOutcomeUnknown, "missing read watermark cannot prove the requested boundary")
	watermark = "10"
	latest = "incoming-message"
	incoming, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	_, err = provider.Preview(t.Context(), request, before)
	require.ErrorIs(t, err, inboxcontrol.ErrPlanChanged, "an incoming message invalidates the old read boundary")
	require.ErrorIs(t, provider.Verify(request, before, projected, incoming), inboxcontrol.ErrOutcomeUnknown)
	assertions.Equal(1, posts)
}

func TestInboxBeeperReadBoundaryRejectsAmbiguousMetadataBeforeWrite(t *testing.T) {
	for _, tc := range []struct {
		name, page string
		want       error
	}{
		{"foreign chat", `{"items":[{"id":"m","accountID":"account-a","chatID":"foreign","sortKey":"10"}]}`, inboxcontrol.ErrDenied},
		{"foreign account", `{"items":[{"id":"m","accountID":"foreign","chatID":"chat-a","sortKey":"10"}]}`, inboxcontrol.ErrDenied},
		{"duplicate message", `{"items":[{"id":"m","accountID":"account-a","chatID":"chat-a","sortKey":"10"},{"id":"m","accountID":"account-a","chatID":"chat-a","sortKey":"9"}]}`, inboxcontrol.ErrUnavailable},
		{"ambiguous ordering", `{"items":[{"id":"m1","accountID":"account-a","chatID":"chat-a","sortKey":"10"},{"id":"m2","accountID":"account-a","chatID":"chat-a","sortKey":"10"}]}`, inboxcontrol.ErrUnavailable},
		{"empty chat", `{"items":[]}`, inboxcontrol.ErrUnavailable},
		{"missing page", `{}`, inboxcontrol.ErrUnavailable},
		{"missing ordering", `{"items":[{"id":"m","accountID":"account-a","chatID":"chat-a"}]}`, inboxcontrol.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			page := `{"items":[{"id":"message-ten","accountID":"account-a","chatID":"chat-a","sortKey":"10"}]}`
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					posts++
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				body := `{"id":"chat-a","accountID":"account-a","isArchived":false,"isMarkedUnread":true,"unreadCount":2,"lastReadMessageSortKey":"8","capabilities":{"markAsUnread":true}}`
				if strings.HasSuffix(r.URL.Path, "/messages") {
					body = page
				}
				_, err := w.Write([]byte(body))
				assert.NoError(t, err)
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
			provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target}
			before, err := provider.Observe(t.Context(), request)
			require.NoError(t, err)
			page = tc.page
			_, err = provider.Observe(t.Context(), request)
			require.ErrorIs(t, err, tc.want)
			_, err = provider.Dispatch(t.Context(), request, before)
			require.ErrorIs(t, err, inboxcontrol.ErrNoWrite, "failed observation must discard cached authority")
			assertions.Equal(0, posts)
		})
	}
}

func TestInboxBeeperMissingIndependentMarkersRejectsMutations(t *testing.T) {
	for _, tc := range []struct {
		name             string
		op               inboxcontrol.Operation
		archived, marked *bool
		count            *int
	}{
		{"archive unknown read", inboxcontrol.OpArchive, new(false), new(false), nil},
		{"unarchive unknown unread", inboxcontrol.OpUnarchive, new(true), nil, new(0)},
		{"unread unknown archive", inboxcontrol.OpSetUnread, nil, new(false), new(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if serveEmptyInboxMessages(w, r) {
					return
				}
				body, err := json.Marshal(map[string]any{"id": "chat-a", "accountID": "account-a", "isArchived": tc.archived, "isMarkedUnread": tc.marked, "unreadCount": tc.count, "capabilities": map[string]any{"archive": true, "markAsUnread": true}})
				assert.NoError(t, err)
				_, err = w.Write(body)
				assert.NoError(t, err)
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
			provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
			request := inboxcontrol.Request{Operation: tc.op, Target: &target, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			require.NoError(t, err)
			_, err = provider.Preview(t.Context(), request, before)
			require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			_, err = provider.Dispatch(t.Context(), request, before)
			require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
			assertions.Zero(posts.Load())
		})
	}
}
func TestInboxBeeperArchivedUnreadRequiresSeparateUnarchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var archived atomic.Bool
	archived.Store(true)
	var marked atomic.Bool
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveEmptyInboxMessages(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			posts.Add(1)
			switch r.URL.Path {
			case "/v1/chats/chat-a/archive":
				var body map[string]bool
				assert.NoError(t, json.UnmarshalRead(r.Body, &body))
				assert.Equal(t, map[string]bool{"archived": false}, body)
				archived.Store(false)
				w.WriteHeader(http.StatusNoContent)
				return
			case "/v1/chats/chat-a/unread":
				marked.Store(true)
			default:
				assert.Fail(t, "unexpected native write", r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		body, err := json.Marshal(map[string]any{"id": "chat-a", "accountID": "account-a", "isArchived": archived.Load(), "isMarkedUnread": marked.Load(), "unreadCount": 0, "capabilities": map[string]any{"archive": true, "markAsUnread": true}})
		assert.NoError(t, err)
		_, err = w.Write(body)
		assert.NoError(t, err)
	}))
	defer server.Close()
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
	target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
	provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
	unread := inboxcontrol.Request{Operation: inboxcontrol.OpSetUnread, Target: &target, DryRun: true}
	before, err := provider.Observe(t.Context(), unread)
	requirements.NoError(err)
	_, err = provider.Preview(t.Context(), unread, before)
	require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
	_, err = provider.Dispatch(t.Context(), unread, before)
	require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
	assertions.Zero(posts.Load())
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpUnarchive, inboxcontrol.OpSetUnread} {
		request := inboxcontrol.Request{Operation: op, Target: &target, DryRun: true}
		before, err := provider.Observe(t.Context(), request)
		requirements.NoError(err)
		projected, err := provider.Preview(t.Context(), request, before)
		requirements.NoError(err)
		request.DryRun = false
		_, err = provider.Dispatch(t.Context(), request, before)
		requirements.NoError(err)
		after, err := provider.Observe(t.Context(), request)
		requirements.NoError(err)
		requirements.NoError(provider.Verify(request, before, projected, after))
	}
	assertions.Equal(int32(2), posts.Load())
	assertions.False(archived.Load())
	assertions.True(marked.Load())
	// Already-unread archived metadata needs no native write or unarchive.
	archived.Store(true)
	before, err = provider.Observe(t.Context(), unread)
	requirements.NoError(err)
	projected, err := provider.Preview(t.Context(), unread, before)
	requirements.NoError(err)
	_, err = provider.Dispatch(t.Context(), unread, before)
	requirements.NoError(err)
	after, err := provider.Observe(t.Context(), unread)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(unread, before, projected, after))
	assertions.Equal(int32(2), posts.Load())
	assertions.True(archived.Load())
}
