package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestEngineListMessagesPreservesDeletedAt(t *testing.T) {
	require := requirepkg.New(t)
	deletedAt := "2026-03-18T15:00:00Z"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertpkg.Equal(t, "/api/v1/messages/filter", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":    1,
			"has_more": false,
			"offset":   0,
			"limit":    100,
			"messages": []map[string]any{
				{
					"id":         1,
					"subject":    "Deleted message",
					"from":       "sender@example.com",
					"to":         []string{"receiver@example.com"},
					"sent_at":    "2024-01-15T10:30:00Z",
					"snippet":    "preview",
					"labels":     []string{"INBOX"},
					"size_bytes": 1234,
					"deleted_at": deletedAt,
				},
			},
		})
	}))
	defer srv.Close()

	store := &Store{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
	}

	engine := NewEngineFromStore(store)

	msgs, err := engine.ListMessages(context.Background(), query.MessageFilter{})
	require.NoError(err, "ListMessages()")
	require.Len(msgs, 1)
	require.NotNil(msgs[0].DeletedAt, "DeletedAt should be parsed")
	assertpkg.Equal(t, deletedAt, msgs[0].DeletedAt.UTC().Format(time.RFC3339), "DeletedAt")
}

// TestEngineGetMessageSummariesByIDs_CarriesFromAndAttachmentCount
// regresses the remote-mode bulk hydration path: it must populate the
// sender email, sender name, and attachment count fields on each
// MessageSummary it returns, matching the shape callers would have
// seen from the older per-hit GetMessage-to-summary projection.
func TestEngineGetMessageSummariesByIDs_CarriesFromAndAttachmentCount(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         42,
			"subject":    "Hello",
			"from":       "alice@example.com",
			"to":         []string{"bob@example.com"},
			"sent_at":    "2024-01-15T10:30:00Z",
			"snippet":    "preview",
			"body":       "body text",
			"labels":     []string{"INBOX"},
			"size_bytes": 1234,
			"attachments": []map[string]any{
				{"filename": "a.pdf", "mime_type": "application/pdf", "size": 100},
				{"filename": "b.txt", "mime_type": "text/plain", "size": 50},
			},
		})
	}))
	defer srv.Close()

	store := &Store{baseURL: srv.URL, httpClient: srv.Client()}
	engine := NewEngineFromStore(store)

	summaries, err := engine.GetMessageSummariesByIDs(context.Background(), []int64{42})
	require.NoError(err, "GetMessageSummariesByIDs")
	require.Len(summaries, 1)
	s := summaries[0]
	assert.Equal("alice@example.com", s.FromEmail, "FromEmail")
	assert.Equal(2, s.AttachmentCount, "AttachmentCount")
	assert.True(s.HasAttachments, "HasAttachments")
}
