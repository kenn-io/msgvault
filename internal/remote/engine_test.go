package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/query"
)

func TestEngineListMessagesPreservesDeletedAt(t *testing.T) {
	deletedAt := "2026-03-18T15:00:00Z"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/messages/filter" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/api/v1/messages/filter")
		}
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
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].DeletedAt == nil {
		t.Fatal("DeletedAt = nil, want parsed timestamp")
	}
	if got := msgs[0].DeletedAt.UTC().Format(time.RFC3339); got != deletedAt {
		t.Fatalf("DeletedAt = %q, want %q", got, deletedAt)
	}
}

// TestEngineGetMessageSummariesByIDs_CarriesFromAndAttachmentCount
// regresses the remote-mode bulk hydration path: it must populate the
// sender email, sender name, and attachment count fields on each
// MessageSummary it returns, matching the shape callers would have
// seen from the older per-hit GetMessage-to-summary projection.
func TestEngineGetMessageSummariesByIDs_CarriesFromAndAttachmentCount(t *testing.T) {
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
	if err != nil {
		t.Fatalf("GetMessageSummariesByIDs: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("len(summaries) = %d, want 1", len(summaries))
	}
	s := summaries[0]
	if s.FromEmail != "alice@example.com" {
		t.Errorf("FromEmail = %q, want %q", s.FromEmail, "alice@example.com")
	}
	if s.AttachmentCount != 2 {
		t.Errorf("AttachmentCount = %d, want 2", s.AttachmentCount)
	}
	if !s.HasAttachments {
		t.Error("HasAttachments = false, want true")
	}
}
