package beeper

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxBeeperArchiveAndUnreadBindIncomingMessageIdentity(t *testing.T) {
	for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetUnread} {
		t.Run(string(operation), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			latest, posts := "message-before", 0
			archived, marked := operation == inboxcontrol.OpUnarchive, false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/messages") {
					assert.Equal(t, http.MethodGet, r.Method)
					body, err := json.Marshal(map[string]any{"items": []map[string]string{{"id": latest, "accountID": "account-a", "chatID": "chat-a", "sortKey": "10"}}})
					assert.NoError(t, err)
					_, _ = w.Write(body)
					return
				}
				if r.Method == http.MethodPost {
					posts++
					if operation == inboxcontrol.OpSetUnread {
						marked = true
					} else {
						archived = operation == inboxcontrol.OpArchive
						w.WriteHeader(http.StatusNoContent)
						return
					}
				}
				body, err := json.Marshal(map[string]any{"id": "chat-a", "accountID": "account-a", "isArchived": archived, "isMarkedUnread": marked, "unreadCount": 0, "lastActivity": "2026-10-05T00:00:00Z", "capabilities": map[string]bool{"archive": true, "markAsUnread": true}})
				assert.NoError(t, err)
				_, _ = w.Write(body)
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
			provider := NewInboxProvider(NewClient(server.URL, testToken, 10000), source)
			request := inboxcontrol.Request{Operation: operation, Target: &target}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			assertions.Equal(latest, before.LastMessageID)
			projected, err := provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			latest = "incoming-before-write"
			_, err = provider.Observe(t.Context(), request)
			requirements.NoError(err)
			_, err = provider.Preview(t.Context(), request, before)
			require.ErrorIs(t, err, inboxcontrol.ErrPlanChanged, "activity timestamp alone does not prove freshness")
			_, err = provider.Dispatch(t.Context(), request, before)
			require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
			assertions.Equal(0, posts)
			latest = "message-before"
			_, err = provider.Observe(t.Context(), request)
			requirements.NoError(err)
			_, err = provider.Dispatch(t.Context(), request, before)
			requirements.NoError(err)
			latest = "incoming-after-write"
			after, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			require.ErrorIs(t, provider.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
			assertions.Equal(1, posts)
		})
	}
}

func TestInboxBeeperEmptyPageRequiresCompleteEvidence(t *testing.T) {
	for _, body := range []string{`{"items":[]}`, `{"items":[],"hasMore":true}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				if strings.HasSuffix(r.URL.Path, "/messages") {
					_, _ = w.Write([]byte(body))
					return
				}
				_, _ = w.Write([]byte(`{"id":"chat-a","accountID":"account-a","isArchived":false,"isMarkedUnread":false,"unreadCount":0,"capabilities":{"archive":true,"markAsUnread":true}}`))
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			target := inboxcontrol.Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a", Scope: inboxcontrol.ScopeChat, ItemID: 1, ProviderID: "chat-a"}
			provider := NewInboxProvider(NewClient(server.URL, testToken, 10000), source)
			_, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target})
			assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
		})
	}
}
