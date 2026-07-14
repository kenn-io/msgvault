package daemonclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestEngineListMessagesPreservesDeletedAt(t *testing.T) {
	require := require.New(t)
	deletedAt := "2026-03-18T15:00:00Z"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/messages/filter", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":    1,
			"has_more": false,
			"offset":   0,
			"limit":    100,
			"messages": []map[string]any{
				{
					"id":         1,
					"source_id":  42,
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

	store := newTestStore(srv, "")
	engine := NewEngineAdapter(store)

	msgs, err := engine.ListMessages(context.Background(), query.MessageFilter{})
	require.NoError(err, "ListMessages()")
	require.Len(msgs, 1)
	assert.Equal(t, int64(42), msgs[0].SourceID, "SourceID")
	require.NotNil(msgs[0].DeletedAt, "DeletedAt should be parsed")
	assert.Equal(t, deletedAt, msgs[0].DeletedAt.UTC().Format(time.RFC3339), "DeletedAt")
}

func TestEngineListMessagesUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/filter", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("sender"), "sender")
		assert.Equal("sms", r.URL.Query().Get("message_type"), "message_type")
		assert.Equal("true", r.URL.Query().Get("hide_deleted"), "hide_deleted")
		assert.Equal("25", r.URL.Query().Get("limit"), "limit")
		assert.Equal("date", r.URL.Query().Get("sort"), "sort")
		assert.Equal("desc", r.URL.Query().Get("direction"), "direction")
		writeJSONResponse(t, w, map[string]any{
			"count":    1,
			"has_more": false,
			"offset":   0,
			"limit":    25,
			"messages": []map[string]any{
				{
					"id":                42,
					"source_message_id": "msg-42",
					"conversation_id":   7,
					"subject":           "Generated list message",
					"message_type":      "sms",
					"from":              "Alice <alice@example.com>",
					"from_email":        "alice@example.com",
					"from_name":         "Alice",
					"from_phone":        "+15555550123",
					"to":                []string{"bob@example.com"},
					"cc":                []string{"carol@example.com"},
					"bcc":               []string{"dave@example.com"},
					"sent_at":           "2024-01-15T10:30:00Z",
					"snippet":           "preview",
					"labels":            []string{"INBOX"},
					"has_attachments":   true,
					"size_bytes":        1234,
				},
			},
		})
	})

	engine := NewEngineAdapter(store)

	msgs, err := engine.ListMessages(
		context.Background(),
		query.MessageFilter{
			Sender:                "alice@example.com",
			MessageType:           "sms",
			HideDeletedFromSource: true,
			Pagination:            query.Pagination{Limit: 25},
			Sorting:               query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc},
		},
	)
	require.NoError(err, "ListMessages")
	require.Len(msgs, 1)
	assert.Equal(int64(42), msgs[0].ID)
	assert.Equal("msg-42", msgs[0].SourceMessageID)
	assert.Equal(int64(7), msgs[0].ConversationID)
	assert.Equal("alice@example.com", msgs[0].FromEmail)
	assert.Equal("Alice", msgs[0].FromName)
	assert.Equal("+15555550123", msgs[0].FromPhone)
	assert.Equal([]query.Address{{Email: "bob@example.com"}}, msgs[0].To)
	assert.Equal([]query.Address{{Email: "carol@example.com"}}, msgs[0].Cc)
	assert.Equal([]query.Address{{Email: "dave@example.com"}}, msgs[0].Bcc)
	assert.True(msgs[0].HasAttachments)
}

func TestEngineListAccountsUsesSourceBackedCLIAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/accounts", r.URL.Path, "path")
		writeJSONResponse(t, w, map[string]any{
			"accounts": []map[string]any{
				{
					"id":            42,
					"email":         "imported@example.com",
					"type":          "imessage",
					"display_name":  "Imported",
					"message_count": 12,
				},
			},
		})
	})

	engine := NewEngineAdapter(store)
	accounts, err := engine.ListAccounts(context.Background())
	require.NoError(err, "ListAccounts")
	require.Len(accounts, 1, "accounts")
	assert.Equal(int64(42), accounts[0].ID)
	assert.Equal("imessage", accounts[0].SourceType)
	assert.Equal("imported@example.com", accounts[0].Identifier)
	assert.Equal("Imported", accounts[0].DisplayName)
}

func TestEngineTextMethodsUseGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/text/conversations":
			assert.Equal("sms", r.URL.Query().Get("source_type"), "source_type")
			assert.Equal("+15555550123", r.URL.Query().Get("contact_phone"), "contact_phone")
			assert.Equal("25", r.URL.Query().Get("limit"), "limit")
			assert.Equal("count", r.URL.Query().Get("sort"), "sort")
			writeJSONResponse(t, w, map[string]any{
				"count":    1,
				"has_more": false,
				"offset":   0,
				"limit":    25,
				"conversations": []map[string]any{
					{
						"conversation_id":   77,
						"title":             "Family",
						"source_type":       "sms",
						"message_count":     3,
						"participant_count": 2,
						"last_message_at":   "2024-01-15T10:30:00Z",
						"last_preview":      "see you soon",
					},
				},
			})
		case "/api/v1/text/aggregates":
			assert.Equal("labels", r.URL.Query().Get("view_type"), "view_type")
			assert.Equal("family", r.URL.Query().Get("search_query"), "search_query")
			assert.Empty(r.URL.Query().Get("time_granularity"), "unset time granularity")
			writeJSONResponse(t, w, map[string]any{
				"view_type": "labels",
				"rows": []map[string]any{
					{"key": "Friends", "count": 3, "total_size": 123},
				},
			})
		case "/api/v1/text/conversations/77/messages":
			assert.Equal("10", r.URL.Query().Get("limit"), "limit")
			writeJSONResponse(t, w, textMessagesResponseJSON("timeline body"))
		case "/api/v1/text/search":
			assert.Equal("dinner", r.URL.Query().Get("q"), "q")
			assert.Equal("5", r.URL.Query().Get("offset"), "offset")
			writeJSONResponse(t, w, textMessagesResponseJSON("search body"))
		case "/api/v1/text/stats":
			assert.Equal("family", r.URL.Query().Get("search_query"), "search_query")
			writeJSONResponse(t, w, map[string]any{
				"message_count":    3,
				"total_size":       123,
				"attachment_count": 1,
				"attachment_size":  45,
				"label_count":      2,
				"account_count":    1,
			})
		default:
			assert.Fail("unexpected path", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusInternalServerError)
		}
	})

	engine := NewEngineAdapter(store)
	textEngine, ok := any(engine).(query.TextEngine)
	require.True(ok, "daemon engine must satisfy TextEngine")

	conversations, err := textEngine.ListConversations(context.Background(), query.TextFilter{
		ContactPhone: "+15555550123",
		SourceType:   "sms",
		Pagination:   query.Pagination{Limit: 25},
		SortField:    query.TextSortByCount,
	})
	require.NoError(err, "ListConversations")
	require.Len(conversations, 1, "conversations")
	assert.Equal(int64(77), conversations[0].ConversationID)
	assert.Equal("Family", conversations[0].Title)
	assert.Equal("2024-01-15T10:30:00Z", conversations[0].LastMessageAt.UTC().Format(time.RFC3339))

	aggregates, err := textEngine.TextAggregate(context.Background(), query.TextViewLabels, query.TextAggregateOptions{
		SearchQuery: "family",
	})
	require.NoError(err, "TextAggregate")
	require.Len(aggregates, 1, "aggregates")
	assert.Equal("Friends", aggregates[0].Key)
	assert.Equal(int64(3), aggregates[0].Count)

	timeline, err := textEngine.ListConversationMessages(context.Background(), 77, query.TextFilter{
		Pagination: query.Pagination{Limit: 10},
	})
	require.NoError(err, "ListConversationMessages")
	require.Len(timeline, 1, "timeline")
	assert.Equal("timeline body", timeline[0].BodyText)
	assert.Equal("Family", timeline[0].ConversationTitle)
	assert.Equal("+15555550123", timeline[0].FromPhone)

	searchResults, err := textEngine.TextSearch(context.Background(), "dinner", 10, 5)
	require.NoError(err, "TextSearch")
	require.Len(searchResults, 1, "searchResults")
	assert.Equal("search body", searchResults[0].BodyText)

	stats, err := textEngine.GetTextStats(context.Background(), query.TextStatsOptions{SearchQuery: "family"})
	require.NoError(err, "GetTextStats")
	require.NotNil(stats, "stats")
	assert.Equal(int64(3), stats.MessageCount)
	assert.Equal(int64(45), stats.AttachmentSize)
}

func TestEngineTextAggregatePreservesExplicitYearGranularity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/text/aggregates", r.URL.Path, "path")
		assert.Equal("time", r.URL.Query().Get("view_type"), "view_type")
		assert.Equal("year", r.URL.Query().Get("time_granularity"), "time_granularity")
		writeJSONResponse(t, w, map[string]any{
			"view_type": "time",
			"rows": []map[string]any{
				{"key": "2026", "count": 3, "total_size": 123},
			},
		})
	})

	engine := NewEngineAdapter(store)
	textEngine, ok := any(engine).(query.TextEngine)
	require.True(ok, "daemon engine must satisfy TextEngine")

	rows, err := textEngine.TextAggregate(context.Background(), query.TextViewTime, query.TextAggregateOptions{
		TimeGranularity:    query.TimeYear,
		TimeGranularitySet: true,
	})
	require.NoError(err, "TextAggregate")
	require.Len(rows, 1, "rows")
	assert.Equal("2026", rows[0].Key)
}

func textMessagesResponseJSON(body string) map[string]any {
	return map[string]any{
		"count":    1,
		"has_more": false,
		"offset":   0,
		"limit":    10,
		"messages": []map[string]any{
			{
				"id":                     99,
				"source_message_id":      "sms-99",
				"conversation_id":        77,
				"source_conversation_id": "thread-77",
				"subject":                "",
				"snippet":                "preview",
				"from_email":             "",
				"from_name":              "Alice",
				"from_phone":             "+15555550123",
				"to": []map[string]any{
					{"Email": "+15555550999", "Name": "Bob"},
				},
				"sent_at":            "2024-01-15T10:30:00Z",
				"size_estimate":      123,
				"has_attachments":    true,
				"attachment_count":   1,
				"labels":             []string{"Friends"},
				"message_type":       "sms",
				"conversation_title": "Family",
				"body_text":          body,
			},
		},
	}
}

func TestEngineGetMessagePreservesGeneratedDetailMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	deletedAt := "2026-03-18T15:00:00Z"
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path, "path")
		writeJSONResponse(t, w, map[string]any{
			"id":                42,
			"source_message_id": "msg-42",
			"conversation_id":   7,
			"subject":           "Generated detail",
			"message_type":      "sms",
			"from":              "Alice <alice@example.com>",
			"from_email":        "alice@example.com",
			"from_name":         "Alice",
			"to":                []string{"bob@example.com"},
			"sent_at":           "2024-01-15T10:30:00Z",
			"deleted_at":        deletedAt,
			"snippet":           "preview",
			"labels":            []string{"INBOX"},
			"has_attachments":   true,
			"size_bytes":        2048,
			"body":              "Hello, generated world!",
			"attachments": []map[string]any{{
				"id":           52,
				"filename":     "doc.pdf",
				"mime_type":    "application/pdf",
				"size_bytes":   1024,
				"content_hash": "hash-123",
				"url":          "/api/v1/attachments/52",
			}},
		})
	})

	engine := NewEngineAdapter(store)

	msg, err := engine.GetMessage(context.Background(), 42)
	require.NoError(err, "GetMessage")
	require.NotNil(msg, "GetMessage returned nil")
	assert.Equal("msg-42", msg.SourceMessageID, "SourceMessageID")
	assert.Equal("sms", msg.MessageType, "MessageType")
	require.NotNil(msg.DeletedAt, "DeletedAt")
	assert.Equal(deletedAt, msg.DeletedAt.UTC().Format(time.RFC3339), "DeletedAt")
	assert.True(msg.HasAttachments, "HasAttachments")
	require.Len(msg.From, 1, "len(From)")
	assert.Equal("alice@example.com", msg.From[0].Email, "From[0].Email")
	assert.Equal("Alice", msg.From[0].Name, "From[0].Name")
	require.Len(msg.Attachments, 1, "len(Attachments)")
	assert.Equal(int64(52), msg.Attachments[0].ID, "Attachments[0].ID")
}

func TestEngineGetMessageRendersLargeIDInPath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/24489626", r.URL.Path, "path")
		writeJSONResponse(t, w, map[string]any{
			"id":      24489626,
			"subject": "Large ID detail",
			"from":    "alice@example.com",
			"sent_at": "2024-01-15T10:30:00Z",
		})
	})

	engine := NewEngineAdapter(store)

	msg, err := engine.GetMessage(context.Background(), 24489626)
	require.NoError(err, "GetMessage")
	require.NotNil(msg, "GetMessage returned nil")
	assert.Equal("Large ID detail", msg.Subject, "Subject")
}

func TestEngineGetMessagePreservesPhoneOnlyGeneratedSender(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path, "path")
		writeJSONResponse(t, w, map[string]any{
			"id":              42,
			"conversation_id": 7,
			"subject":         "Generated text",
			"message_type":    "sms",
			"from":            "+15555550123",
			"from_name":       "Alice",
			"from_phone":      "+15555550123",
			"to":              []string{"+15555550124"},
			"sent_at":         "2024-01-15T10:30:00Z",
			"snippet":         "preview",
			"labels":          []string{"SMS"},
			"body":            "Hello by text",
			"attachments":     []map[string]any{},
		})
	})

	engine := NewEngineAdapter(store)

	msg, err := engine.GetMessage(context.Background(), 42)
	require.NoError(err, "GetMessage")
	require.NotNil(msg, "GetMessage returned nil")
	require.Len(msg.From, 1, "len(From)")
	assert.Equal("+15555550123", msg.From[0].Email, "From[0].Email")
	assert.Equal("Alice", msg.From[0].Name, "From[0].Name")
}

func TestEngineGetMessageRawUsesGeneratedCLIEndpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	raw := []byte("From: alice@example.com\r\nSubject: Raw\r\n\r\nBody")

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/message/raw", r.URL.Path, "path")
		assert.Equal("42", r.URL.Query().Get("id"), "id")
		w.Header().Set("Content-Type", "message/rfc822")
		_, _ = w.Write(raw)
	})

	engine := NewEngineAdapter(store)

	got, err := engine.GetMessageRaw(context.Background(), 42)
	require.NoError(err, "GetMessageRaw")
	assert.Equal(raw, got, "raw")
}

func TestEngineGetAttachmentUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/attachments/42", r.URL.Path, "path")
		writeJSONResponse(t, w, map[string]any{
			"id":           42,
			"filename":     "report.pdf",
			"mime_type":    "application/pdf",
			"size_bytes":   12345,
			"content_hash": "hash-42",
		})
	})

	engine := NewEngineAdapter(store)

	att, err := engine.GetAttachment(context.Background(), 42)
	require.NoError(err, "GetAttachment")
	require.NotNil(att, "attachment")
	assert.Equal(int64(42), att.ID, "ID")
	assert.Equal("report.pdf", att.Filename, "Filename")
	assert.Equal("application/pdf", att.MimeType, "MimeType")
	assert.Equal(int64(12345), att.Size, "Size")
	assert.Equal("hash-42", att.ContentHash, "ContentHash")
}

func TestEngineGetGmailIDsByFilterUsesAuthoritativeEndpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/gmail-ids", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("sender"), "sender")
		assert.Empty(r.URL.Query().Get("offset"), "offset must not be client-paginated")
		assert.Equal("10", r.URL.Query().Get("limit"), "limit remains a staging safety cap")
		assert.Empty(r.URL.Query().Get("sort"), "sort must not affect ID enumeration")
		assert.Empty(r.URL.Query().Get("direction"), "direction must not affect ID enumeration")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"gmail_ids": []string{"gm-1", "gm-2", "gm-3"},
		})
	}))
	defer srv.Close()

	store := newTestStore(srv, "")
	engine := NewEngineAdapter(store)

	ids, err := engine.GetGmailIDsByFilter(
		context.Background(),
		query.MessageFilter{
			Sender:     "alice@example.com",
			Pagination: query.Pagination{Offset: 50, Limit: 10},
			Sorting:    query.MessageSorting{Field: query.MessageSortBySubject, Direction: query.SortAsc},
		},
	)
	require.NoError(err, "GetGmailIDsByFilter")

	assert.Equal([]string{"gm-1", "gm-2", "gm-3"}, ids)
}

func TestEngineGetGmailIDsByFilterUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/gmail-ids", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("sender"), "sender")
		assert.Equal("sms", r.URL.Query().Get("message_type"), "message_type")
		assert.Equal("true", r.URL.Query().Get("hide_deleted"), "hide_deleted")
		writeJSONResponse(t, w, map[string]any{
			"gmail_ids": []string{"gm-1", "gm-2"},
		})
	})

	engine := NewEngineAdapter(store)

	ids, err := engine.GetGmailIDsByFilter(
		context.Background(),
		query.MessageFilter{
			Sender:                "alice@example.com",
			MessageType:           "sms",
			HideDeletedFromSource: true,
		},
	)
	require.NoError(err, "GetGmailIDsByFilter")
	assert.Equal([]string{"gm-1", "gm-2"}, ids)
}

func TestEngineSearchByDomainsUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	after := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search/domains", r.URL.Path, "path")
		assert.Equal("example.com,test.org", r.URL.Query().Get("domains"), "domains")
		assert.Equal(after.Format(time.RFC3339), r.URL.Query().Get("after"), "after")
		assert.Equal(before.Format(time.RFC3339), r.URL.Query().Get("before"), "before")
		assert.Equal("25", r.URL.Query().Get("limit"), "limit")
		assert.Equal("50", r.URL.Query().Get("offset"), "offset")
		writeJSONResponse(t, w, map[string]any{
			"count":    1,
			"has_more": false,
			"offset":   50,
			"limit":    25,
			"messages": []map[string]any{
				{
					"id":              84,
					"subject":         "Domain match",
					"from":            "Alice <alice@example.com>",
					"from_email":      "alice@example.com",
					"sent_at":         "2024-01-15T10:30:00Z",
					"snippet":         "preview",
					"labels":          []string{"INBOX"},
					"has_attachments": false,
					"size_bytes":      4096,
				},
			},
		})
	})

	engine := NewEngineAdapter(store)

	results, err := engine.SearchByDomains(
		context.Background(),
		[]string{"example.com", "test.org"},
		&after,
		&before,
		25,
		50,
	)
	require.NoError(err, "SearchByDomains")
	require.Len(results, 1, "results")
	assert.Equal(int64(84), results[0].ID, "ID")
	assert.Equal("alice@example.com", results[0].FromEmail, "FromEmail")
}

func TestEngineTimeFiltersPreserveUTCNanoseconds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	after := time.Date(2024, 1, 15, 10, 30, 15, 123456789,
		time.FixedZone("UTC-5", -5*60*60))
	before := time.Date(2024, 1, 16, 18, 45, 30, 987654321,
		time.FixedZone("UTC+2", 2*60*60))
	wantAfter := after.UTC().Format(time.RFC3339Nano)
	wantBefore := before.UTC().Format(time.RFC3339Nano)
	seen := make(map[string]bool)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = true
		assert.Equal(wantAfter, r.URL.Query().Get("after"), "after for %s", r.URL.Path)
		assert.Equal(wantBefore, r.URL.Query().Get("before"), "before for %s", r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/aggregates":
			writeJSONResponse(t, w, map[string]any{"view_type": "senders", "rows": []map[string]any{}})
		case "/api/v1/search/domains":
			writeJSONResponse(t, w, map[string]any{
				"count": 0, "has_more": false, "offset": 0, "limit": 10,
				"messages": []map[string]any{},
			})
		default:
			assert.Fail("unexpected request path", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusInternalServerError)
		}
	})
	engine := NewEngineAdapter(store)

	_, err := engine.Aggregate(context.Background(), query.ViewSenders, query.AggregateOptions{
		After: &after, Before: &before,
	})
	require.NoError(err)
	_, err = engine.SearchByDomains(context.Background(), []string{"example.com"}, &after, &before, 10, 0)
	require.NoError(err)
	assert.True(seen["/api/v1/aggregates"])
	assert.True(seen["/api/v1/search/domains"])
}

func TestFindSimilarMessagesUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	after := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	hasAttachment := true

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search/similar", r.URL.Path, "path")
		assert.Equal("42", r.URL.Query().Get("message_id"), "message_id")
		assert.Equal("25", r.URL.Query().Get("limit"), "limit")
		assert.Equal("alice@example.com", r.URL.Query().Get("account"), "account")
		assert.Equal("sms", r.URL.Query().Get("message_type"), "message_type")
		assert.Equal(after.Format(time.RFC3339), r.URL.Query().Get("after"), "after")
		assert.Equal(before.Format(time.RFC3339), r.URL.Query().Get("before"), "before")
		assert.Equal("true", r.URL.Query().Get("has_attachment"), "has_attachment")
		writeJSONResponse(t, w, map[string]any{
			"seed_message_id": 42,
			"returned":        1,
			"generation": map[string]any{
				"id":          7,
				"model":       "fake",
				"dimension":   4,
				"fingerprint": "fake:4",
				"state":       "active",
			},
			"messages": []map[string]any{
				{
					"id":              84,
					"subject":         "Similar",
					"from":            "Alice <alice@example.com>",
					"from_email":      "alice@example.com",
					"sent_at":         "2024-01-15T10:30:00Z",
					"snippet":         "preview",
					"labels":          []string{"INBOX"},
					"has_attachments": false,
					"size_bytes":      4096,
				},
			},
		})
	})

	resp, err := store.FindSimilarMessages(context.Background(), SimilarSearchRequest{
		MessageID:     42,
		Limit:         25,
		Account:       "alice@example.com",
		MessageType:   "sms",
		After:         &after,
		Before:        &before,
		HasAttachment: &hasAttachment,
	})
	require.NoError(err, "FindSimilarMessages")
	require.NotNil(resp, "response")
	assert.Equal(int64(42), resp.SeedMessageID, "SeedMessageID")
	assert.Equal(int64(7), resp.Generation.ID, "Generation.ID")
	require.Len(resp.Messages, 1, "Messages")
	assert.Equal(int64(84), resp.Messages[0].ID, "message id")
	assert.Equal("alice@example.com", resp.Messages[0].FromEmail, "from email")
}

func TestEngineAggregateUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sourceID := int64(7)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/aggregates", r.URL.Path, "path")
		assert.Equal("senders", r.URL.Query().Get("view_type"), "view_type")
		assert.Equal("name", r.URL.Query().Get("sort"), "sort")
		assert.Equal("asc", r.URL.Query().Get("direction"), "direction")
		assert.Equal("25", r.URL.Query().Get("limit"), "limit")
		assert.Equal("7", r.URL.Query().Get("source_id"), "source_id")
		writeJSONResponse(t, w, map[string]any{
			"view_type": "senders",
			"rows": []map[string]any{{
				"key":              "alice@example.com",
				"count":            3,
				"total_size":       42,
				"attachment_size":  11,
				"attachment_count": 2,
				"total_unique":     3,
			}},
		})
	})

	engine := NewEngineAdapter(store)

	rows, err := engine.Aggregate(context.Background(), query.ViewSenders, query.AggregateOptions{
		SourceID:      &sourceID,
		SortField:     query.SortByName,
		SortDirection: query.SortAsc,
		Limit:         25,
	})
	require.NoError(err, "Aggregate")
	require.Len(rows, 1, "rows")
	assert.Equal("alice@example.com", rows[0].Key)
	assert.Equal(int64(3), rows[0].Count)
	assert.Equal(int64(11), rows[0].AttachmentSize)
}

func TestEngineSubAggregateUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/aggregates/sub", r.URL.Path, "path")
		assert.Equal("labels", r.URL.Query().Get("view_type"), "view_type")
		assert.Equal("alice@example.com", r.URL.Query().Get("sender"), "sender")
		assert.Equal("count", r.URL.Query().Get("sort"), "sort")
		assert.Equal("desc", r.URL.Query().Get("direction"), "direction")
		assert.Equal("10", r.URL.Query().Get("limit"), "limit")
		assert.Equal("urgent", r.URL.Query().Get("search_query"), "search_query")
		writeJSONResponse(t, w, map[string]any{
			"view_type": "labels",
			"rows": []map[string]any{{
				"key":              "INBOX",
				"count":            2,
				"total_size":       24,
				"attachment_size":  0,
				"attachment_count": 0,
				"total_unique":     2,
			}},
		})
	})

	engine := NewEngineAdapter(store)

	rows, err := engine.SubAggregate(
		context.Background(),
		query.MessageFilter{Sender: "alice@example.com"},
		query.ViewLabels,
		query.AggregateOptions{
			SortField:       query.SortByCount,
			SortDirection:   query.SortDesc,
			Limit:           10,
			TimeGranularity: query.TimeMonth,
			SearchQuery:     "urgent",
		},
	)
	require.NoError(err, "SubAggregate")
	require.Len(rows, 1, "rows")
	assert.Equal("INBOX", rows[0].Key)
	assert.Equal(int64(2), rows[0].Count)
}

func TestEngineGetTotalStatsUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sourceID := int64(7)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/stats/total", r.URL.Path, "path")
		assert.Equal("7", r.URL.Query().Get("source_id"), "source_id")
		assert.Equal("true", r.URL.Query().Get("attachments_only"), "attachments_only")
		assert.Equal("true", r.URL.Query().Get("hide_deleted"), "hide_deleted")
		assert.Equal("urgent", r.URL.Query().Get("search_query"), "search_query")
		assert.Equal("true", r.URL.Query().Get("search_scope"), "search_scope")
		assert.Equal("labels", r.URL.Query().Get("group_by"), "group_by")
		writeJSONResponse(t, w, map[string]any{
			"message_count":           5,
			"active_messages":         4,
			"source_deleted_messages": 1,
			"total_size":              100,
			"attachment_count":        2,
			"attachment_size":         25,
			"label_count":             3,
			"account_count":           1,
			"applied_search_scope":    true,
		})
	})

	engine := NewEngineAdapter(store)

	stats, err := engine.GetTotalStats(context.Background(), query.StatsOptions{
		SourceID:              &sourceID,
		WithAttachmentsOnly:   true,
		HideDeletedFromSource: true,
		SearchQuery:           "urgent",
		SearchScope:           true,
		GroupBy:               query.ViewLabels,
	})
	require.NoError(err, "GetTotalStats")
	require.NotNil(stats, "stats")
	assert.Equal(int64(5), stats.MessageCount)
	assert.Equal(int64(4), stats.ActiveMessageCount, "active breakdown must survive the adapter")
	assert.Equal(int64(1), stats.SourceDeletedMessageCount, "source-deleted breakdown must survive the adapter")
	assert.Equal(int64(25), stats.AttachmentSize)
	assert.Equal(int64(1), stats.AccountCount)
}

func TestEngineGetTotalStatsOmitsSearchScopeByDefault(t *testing.T) {
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/stats/total", r.URL.Path, "path")
		_, hasSearchScope := r.URL.Query()["search_scope"]
		assert.False(t, hasSearchScope, "default stats must not opt into search scope")
		writeJSONResponse(t, w, map[string]any{"message_count": 5})
	})

	engine := NewEngineAdapter(store)
	stats, err := engine.GetTotalStats(context.Background(), query.StatsOptions{SearchQuery: "urgent"})
	require.NoError(t, err, "GetTotalStats")
	require.NotNil(t, stats, "stats")
	assert.Equal(t, int64(5), stats.MessageCount)
}

func TestEngineGetTotalStatsForwardsSourceIDs(t *testing.T) {
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/stats/total", r.URL.Path, "path")
		assert.ElementsMatch(t, []string{"8", "7", "8"}, r.URL.Query()["source_ids"], "source_ids")
		writeJSONResponse(t, w, map[string]any{
			"message_count":        2,
			"applied_search_scope": true,
			"applied_source_ids":   []int64{7, 8},
		})
	})

	engine := NewEngineAdapter(store)
	stats, err := engine.GetTotalStats(context.Background(), query.StatsOptions{
		SourceIDs:   []int64{8, 7, 8},
		SearchQuery: "urgent",
		SearchScope: true,
	})
	require.NoError(t, err, "GetTotalStats")
	require.NotNil(t, stats, "stats")
	assert.Equal(t, int64(2), stats.MessageCount)
}

func TestEngineGetTotalStatsRequiresCapabilityEchoes(t *testing.T) {
	tests := []struct {
		name string
		opts query.StatsOptions
		want string
	}{
		{
			name: "search scope",
			opts: query.StatsOptions{SearchQuery: "urgent", SearchScope: true},
			want: "search scope",
		},
		{
			name: "source IDs",
			opts: query.StatsOptions{SourceIDs: []int64{7, 8}},
			want: "source IDs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSONResponse(t, w, map[string]any{"message_count": 99})
			})
			engine := NewEngineAdapter(store)

			stats, err := engine.GetTotalStats(context.Background(), tc.opts)
			require.Error(err, "unconfirmed opt-in must fail closed")
			assert.Nil(stats)
			assert.Contains(err.Error(), tc.want)
			assert.Contains(err.Error(), "upgrade")
		})
	}
}

func TestEngineGetTotalStatsRejectsMismatchedSourceIDEcho(t *testing.T) {
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, map[string]any{
			"message_count": 99, "applied_source_ids": []int64{7, 9},
		})
	})
	engine := NewEngineAdapter(store)

	stats, err := engine.GetTotalStats(context.Background(), query.StatsOptions{
		SourceIDs: []int64{7, 8},
	})
	require.Error(t, err, "mismatched source echo must fail closed")
	assert.Nil(t, stats)
	assert.Contains(t, err.Error(), "source IDs")
}

func TestEngineGetTotalStatsExplicitEmptySourceIDsSkipsHTTP(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	called := false
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSONResponse(t, w, map[string]any{"message_count": 99})
	})
	engine := NewEngineAdapter(store)

	stats, err := engine.GetTotalStats(context.Background(), query.StatsOptions{
		SourceIDs:   []int64{},
		SearchQuery: "urgent",
		SearchScope: true,
	})
	require.NoError(err, "GetTotalStats")
	require.NotNil(stats, "stats")
	assert.Zero(stats.MessageCount, "explicit empty source scope")
	assert.False(called, "explicit empty source scope must skip HTTP")
}

func TestEngineGetMessageCarriesAttachmentContentHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         42,
			"subject":    "Attachment",
			"from":       "alice@example.com",
			"sent_at":    "2024-01-15T10:30:00Z",
			"body":       "body text",
			"size_bytes": 1234,
			"attachments": []map[string]any{
				{
					"filename":     "invoice.pdf",
					"mime_type":    "application/pdf",
					"size_bytes":   100,
					"content_hash": "abc123",
				},
			},
		})
	}))
	defer srv.Close()

	store := newTestStore(srv, "")
	engine := NewEngineAdapter(store)

	detail, err := engine.GetMessage(context.Background(), 42)
	require.NoError(err, "GetMessage")
	require.NotNil(detail, "detail")
	require.Len(detail.Attachments, 1)
	assert.Equal("abc123", detail.Attachments[0].ContentHash)
}

// TestEngineGetMessageSummariesByIDs_CarriesFromAndAttachmentCount
// regresses the remote-mode bulk hydration path: it must populate the
// sender email, sender name, and attachment count fields on each
// MessageSummary it returns, matching the shape callers would have
// seen from the older per-hit GetMessage-to-summary projection.
func TestEngineGetMessageSummariesByIDs_CarriesFromAndAttachmentCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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

	store := newTestStore(srv, "")
	engine := NewEngineAdapter(store)

	summaries, err := engine.GetMessageSummariesByIDs(context.Background(), []int64{42})
	require.NoError(err, "GetMessageSummariesByIDs")
	require.Len(summaries, 1)
	s := summaries[0]
	assert.Equal("alice@example.com", s.FromEmail, "FromEmail")
	assert.Equal(2, s.AttachmentCount, "AttachmentCount")
	assert.True(s.HasAttachments, "HasAttachments")
}
func TestEngineSearchSerializesMessageTypes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search/deep", r.URL.Path, "path")
		gotQuery := r.URL.Query().Get("q")
		assert.Contains(gotQuery, "message_type:sms", "q should preserve parsed message_type filters")
		assert.Contains(gotQuery, "lunch", "q should preserve text terms")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{},
			"count":    0,
			"has_more": false,
			"offset":   0,
			"limit":    10,
			"query":    gotQuery,
		})
	}))
	defer srv.Close()

	store := newTestStore(srv, "")
	engine := NewEngineAdapter(store)

	_, err := engine.Search(context.Background(), &search.Query{
		TextTerms:    []string{"lunch"},
		MessageTypes: []string{"sms"},
	}, 10, 0)
	require.NoError(err, "Search")
}
func TestEngineSearchForwardsMessageTypeOnlyTerms(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	called := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Equal("/api/v1/search/deep", r.URL.Path, "path")
		assert.Equal("message_type:sms", r.URL.Query().Get("q"), "q")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"query":    r.URL.Query().Get("q"),
			"messages": []map[string]any{},
			"count":    0,
			"has_more": false,
			"offset":   0,
			"limit":    10,
		})
	}))
	defer srv.Close()

	store := newTestStore(srv, "")
	engine := NewEngineAdapter(store)

	msgs, err := engine.Search(context.Background(), &search.Query{
		MessageTypes: []string{"sms"},
	}, 10, 0)

	require.NoError(err, "Search")
	assert.Empty(msgs, "messages")
	assert.True(called, "message_type-only searches must not be treated as empty")
}

func TestEngineSearchUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search/deep", r.URL.Path, "path")
		assert.Empty(r.URL.Query().Get("scope"), "generic Search must omit scope")
		gotQuery := r.URL.Query().Get("q")
		assert.Contains(gotQuery, "lunch", "q should preserve text terms")
		assert.Contains(gotQuery, "message_type:sms", "q should preserve message type filters")
		assert.Equal("17", r.URL.Query().Get("source_id"), "source_id")
		assert.Equal("true", r.URL.Query().Get("hide_deleted"), "hide_deleted")
		assert.Equal("5", r.URL.Query().Get("offset"), "offset")
		assert.Equal("10", r.URL.Query().Get("limit"), "limit")
		writeJSONResponse(t, w, map[string]any{
			"query":    gotQuery,
			"count":    1,
			"has_more": false,
			"offset":   5,
			"limit":    10,
			"messages": []map[string]any{
				{
					"id":              99,
					"subject":         "Deep search result",
					"message_type":    "sms",
					"from":            "Alice <alice@example.com>",
					"from_email":      "alice@example.com",
					"from_name":       "Alice",
					"sent_at":         "2024-01-15T10:30:00Z",
					"snippet":         "preview",
					"labels":          []string{"INBOX"},
					"has_attachments": true,
					"size_bytes":      2048,
				},
			},
		})
	})

	engine := NewEngineAdapter(store)

	msgs, err := engine.Search(
		context.Background(),
		&search.Query{
			TextTerms:    []string{"lunch"},
			AccountIDs:   []int64{17},
			HideDeleted:  true,
			MessageTypes: []string{"sms"},
		},
		10,
		5,
	)
	require.NoError(err, "Search")
	require.Len(msgs, 1)
	assert.Equal(int64(99), msgs[0].ID)
	assert.Equal("alice@example.com", msgs[0].FromEmail)
	assert.Equal("Alice", msgs[0].FromName)
	assert.Equal("sms", msgs[0].MessageType)
}

func TestEngineDeepSearchAndStatsUseSameQuotedQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var deepQuery string
	var statsQuery string
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/search/deep":
			deepQuery = r.URL.Query().Get("q")
			writeJSONResponse(t, w, map[string]any{
				"query":    deepQuery,
				"messages": []map[string]any{},
				"count":    0,
				"has_more": false,
				"offset":   0,
				"limit":    10,
			})
		case "/api/v1/stats/total":
			statsQuery = r.URL.Query().Get("search_query")
			writeJSONResponse(t, w, map[string]any{
				"message_count": 0, "applied_search_scope": true,
			})
		default:
			assert.Fail("unexpected request path", r.URL.Path)
		}
	})
	engine := NewEngineAdapter(store)
	q := &search.Query{
		TextTerms: []string{"meeting notes"},
		Labels:    []string{`Project "Review"`},
	}

	_, err := engine.Search(context.Background(), q, 10, 0)
	require.NoError(err, "Search")
	_, err = engine.GetTotalStats(context.Background(), query.StatsOptions{
		SearchQuery: search.Format(q),
		SearchScope: true,
	})
	require.NoError(err, "GetTotalStats")

	require.NotEmpty(deepQuery, "deep query")
	assert.Equal(statsQuery, deepQuery, "deep results and stats query serialization")
	assert.Equal(q.TextTerms, search.Parse(deepQuery).TextTerms, "deep text terms")
	assert.Equal(q.Labels, search.Parse(deepQuery).Labels, "deep labels")
}

func TestEngineDeepSearchExplicitEmptyScopeSkipsHTTP(t *testing.T) {
	queries := []struct {
		name  string
		query *search.Query
	}{
		{
			name: "message type conflict",
			query: query.MergeFilterIntoQuery(
				search.Parse("message_type:email needle"),
				query.MessageFilter{MessageType: "sms"},
			),
		},
		{
			name: "empty collection",
			query: query.MergeFilterIntoQuery(
				search.Parse("needle"),
				query.MessageFilter{SourceIDs: []int64{}},
			),
		},
	}
	methods := []struct {
		name string
		run  func(*Engine, *search.Query) ([]query.MessageSummary, error)
	}{
		{
			name: "composite search",
			run: func(engine *Engine, q *search.Query) ([]query.MessageSummary, error) {
				return engine.Search(context.Background(), q, 10, 0)
			},
		},
		{
			name: "body search",
			run: func(engine *Engine, q *search.Query) ([]query.MessageSummary, error) {
				return engine.SearchMessageBodies(context.Background(), q, 10, 0)
			},
		},
	}

	for _, queryCase := range queries {
		for _, method := range methods {
			t.Run(queryCase.name+"/"+method.name, func(t *testing.T) {
				called := false
				store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
					called = true
					writeJSONResponse(t, w, map[string]any{
						"query":    r.URL.Query().Get("q"),
						"scope":    r.URL.Query().Get("scope"),
						"messages": []map[string]any{},
						"count":    0,
						"has_more": false,
						"offset":   0,
						"limit":    10,
					})
				})
				engine := NewEngineAdapter(store)

				messages, err := method.run(engine, queryCase.query)
				require.NoError(t, err)
				assert.Empty(t, messages)
				assert.False(t, called, "explicit empty scope must skip HTTP")
			})
		}
	}
}

func TestEngineSearchMethodsRejectParsedQueryErrorsBeforeHTTP(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Engine, *search.Query) error
	}{
		{
			name: "composite",
			run: func(engine *Engine, q *search.Query) error {
				_, err := engine.Search(context.Background(), q, 10, 0)
				return err
			},
		},
		{
			name: "body",
			run: func(engine *Engine, q *search.Query) error {
				_, err := engine.SearchMessageBodies(context.Background(), q, 10, 0)
				return err
			},
		},
		{
			name: "fast with stats",
			run: func(engine *Engine, q *search.Query) error {
				_, err := engine.SearchFastWithStats(context.Background(), q, "needle before:not-a-date",
					query.MessageFilter{}, query.ViewSenders, 10, 0)
				return err
			},
		},
		{
			name: "fast",
			run: func(engine *Engine, q *search.Query) error {
				_, err := engine.SearchFast(context.Background(), q, query.MessageFilter{}, 10, 0)
				return err
			},
		},
		{
			name: "fast count",
			run: func(engine *Engine, q *search.Query) error {
				_, err := engine.SearchFastCount(context.Background(), q, query.MessageFilter{})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			called := false
			store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				writeJSONResponse(t, w, map[string]any{
					"query":       "needle",
					"messages":    []map[string]any{},
					"count":       0,
					"has_more":    false,
					"offset":      0,
					"limit":       10,
					"total_count": 0,
					"stats":       map[string]any{"message_count": 0},
				})
			})
			engine := NewEngineAdapter(store)
			q := search.Parse("needle before:not-a-date")
			require.Error(q.Err(), "fixture has a parsed restrictive-operator error")

			err := tc.run(engine, q)
			require.Error(err)
			assert.Contains(err.Error(), "invalid search query")
			assert.Contains(err.Error(), "before")
			assert.False(called, "invalid parsed query must skip HTTP")
		})
	}
}

func TestEngineSearchPreservesExactTimeBounds(t *testing.T) {
	after := time.Date(2024, 2, 1, 10, 30, 15, 123456789,
		time.FixedZone("UTC-5", -5*60*60))
	before := time.Date(2024, 3, 1, 0, 0, 0, 0,
		time.FixedZone("UTC+2", 2*60*60))
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		got := search.Parse(r.URL.Query().Get("q"))
		if !assert.NoError(t, got.Err()) ||
			!assert.NotNil(t, got.AfterDate) ||
			!assert.NotNil(t, got.BeforeDate) {
			http.Error(w, "invalid exact-time query", http.StatusBadRequest)
			return
		}
		assert.True(t, after.Equal(*got.AfterDate), "after instant")
		assert.True(t, before.Equal(*got.BeforeDate), "before instant")
		_, wantAfterOffset := after.Zone()
		_, gotAfterOffset := got.AfterDate.Zone()
		_, wantBeforeOffset := before.Zone()
		_, gotBeforeOffset := got.BeforeDate.Zone()
		assert.Equal(t, wantAfterOffset, gotAfterOffset, "after timezone offset")
		assert.Equal(t, wantBeforeOffset, gotBeforeOffset, "before timezone offset")
		writeJSONResponse(t, w, map[string]any{
			"query": r.URL.Query().Get("q"), "messages": []map[string]any{},
			"count": 0, "has_more": false, "offset": 0, "limit": 10,
		})
	})
	engine := NewEngineAdapter(store)

	_, err := engine.Search(context.Background(), &search.Query{
		TextTerms: []string{"needle"}, AfterDate: &after, BeforeDate: &before,
	}, 10, 0)
	require.NoError(t, err)
}

func TestEngineSearchMessageBodiesForwardsAndRequiresScopeEcho(t *testing.T) {
	assert := assert.New(t)
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search/deep", r.URL.Path, "path")
		assert.Equal("body", r.URL.Query().Get("scope"), "scope")
		assert.Equal("bodyneedle", r.URL.Query().Get("q"), "q")
		assert.Equal("7", r.URL.Query().Get("source_id"), "source_id")
		writeJSONResponse(t, w, map[string]any{
			"query":    "bodyneedle",
			"scope":    "body",
			"count":    1,
			"has_more": false,
			"offset":   2,
			"limit":    5,
			"messages": []map[string]any{{
				"id":         42,
				"subject":    "body hit",
				"from":       "sender@example.com",
				"sent_at":    "2024-01-15T10:30:00Z",
				"snippet":    "preview",
				"labels":     []string{},
				"size_bytes": 100,
			}},
			"body_contexts": []map[string]any{{
				"message_id":                 42,
				"context_snippets":           []string{"exact daemon context"},
				"context_snippets_truncated": true,
			}},
		})
	})
	engine := NewEngineAdapter(store)

	messages, err := engine.SearchMessageBodies(context.Background(), &search.Query{
		TextTerms:  []string{"bodyneedle"},
		AccountIDs: []int64{7},
	}, 5, 2)
	require.NoError(t, err, "SearchMessageBodies")
	require.Len(t, messages, 1)
	assert.Equal(int64(42), messages[0].ID, "body hit ID")
	assert.Equal([]string{"exact daemon context"}, messages[0].BodyContextSnippets)
	assert.True(messages[0].BodyContextSnippetsTruncated)
}

func TestBodySearchSummariesFromGeneratedRejectsInvalidCompanions(t *testing.T) {
	truncated := true
	tests := []struct {
		name     string
		contexts []generated.BodySearchContext
		want     string
	}{
		{name: "missing", want: "omitted body context"},
		{
			name: "unknown message",
			contexts: []generated.BodySearchContext{{
				MessageID: 99, ContextSnippets: []string{"context"},
			}},
			want: "unknown message",
		},
		{
			name: "duplicate",
			contexts: []generated.BodySearchContext{
				{MessageID: 42, ContextSnippets: []string{"first"}},
				{MessageID: 42, ContextSnippetsTruncated: &truncated},
			},
			want: "duplicate body context",
		},
		{
			name:     "empty and untruncated",
			contexts: []generated.BodySearchContext{{MessageID: 42}},
			want:     "empty untruncated body context",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bodySearchSummariesFromGenerated(
				[]generated.MessageSummary{{ID: 42}}, tc.contexts,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEngineSearchMessageBodiesFailsClosedWithoutScopeEcho(t *testing.T) {
	assert := assert.New(t)
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("body", r.URL.Query().Get("scope"), "scope is still sent to old daemon")
		writeJSONResponse(t, w, map[string]any{
			"query":    "bodyneedle",
			"count":    1,
			"has_more": false,
			"offset":   0,
			"limit":    5,
			"messages": []map[string]any{{
				"id":         99,
				"subject":    "generic false positive",
				"from":       "sender@example.com",
				"sent_at":    "2024-01-15T10:30:00Z",
				"snippet":    "preview",
				"labels":     []string{},
				"size_bytes": 100,
			}},
		})
	})
	engine := NewEngineAdapter(store)

	messages, err := engine.SearchMessageBodies(context.Background(),
		&search.Query{TextTerms: []string{"bodyneedle"}}, 5, 0)
	require.Error(t, err, "old daemon must not produce generic false positives")
	assert.Nil(messages, "unconfirmed results must be discarded")
	assert.Contains(err.Error(), "did not confirm body-only search scope")
	assert.Contains(err.Error(), "upgrade")
}

func TestEngineSearchFastWithStatsUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search/fast", r.URL.Path, "path")
		gotQuery := r.URL.Query().Get("q")
		assert.Contains(gotQuery, "lunch", "q should preserve text terms")
		assert.Contains(gotQuery, "message_type:sms", "q should preserve message type filters")
		assert.Equal("alice@example.com", r.URL.Query().Get("sender"), "sender")
		assert.Empty(r.URL.Query()["message_type"], "message_type filter param is unsupported by fast search")
		assert.Equal("true", r.URL.Query().Get("hide_deleted"), "hide_deleted")
		assert.Equal("5", r.URL.Query().Get("offset"), "offset")
		assert.Equal("10", r.URL.Query().Get("limit"), "limit")
		assert.Equal("domains", r.URL.Query().Get("view_type"), "view_type")
		writeJSONResponse(t, w, map[string]any{
			"query":       "lunch",
			"total_count": 12,
			"stats": map[string]any{
				"message_count":    12,
				"total_size":       2048,
				"attachment_count": 2,
				"attachment_size":  512,
				"label_count":      3,
				"account_count":    1,
			},
			"messages": []map[string]any{
				{
					"id":              84,
					"subject":         "Fast search result",
					"message_type":    "sms",
					"from":            "alice@example.com",
					"from_email":      "alice@example.com",
					"sent_at":         "2024-01-15T10:30:00Z",
					"snippet":         "preview",
					"labels":          []string{"INBOX"},
					"has_attachments": true,
					"size_bytes":      4096,
				},
			},
		})
	})

	engine := NewEngineAdapter(store)
	parsedQuery := search.Parse("message_type:sms lunch")

	result, err := engine.SearchFastWithStats(
		context.Background(),
		parsedQuery,
		"message_type:sms lunch",
		query.MessageFilter{
			Sender:                "alice@example.com",
			HideDeletedFromSource: true,
		},
		query.ViewDomains,
		10,
		5,
	)

	require.NoError(err, "SearchFastWithStats")
	require.Len(result.Messages, 1)
	assert.Equal(int64(84), result.Messages[0].ID)
	assert.Equal("alice@example.com", result.Messages[0].FromEmail)
	assert.Equal(int64(12), result.TotalCount)
	require.NotNil(result.Stats, "stats")
	assert.Equal(int64(2048), result.Stats.TotalSize)
	assert.Equal(int64(512), result.Stats.AttachmentSize)
}

func TestEngineSearchFastWithStatsForwardsSourceIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal([]string{"7", "8"}, r.URL.Query()["source_ids"])
		writeJSONResponse(t, w, map[string]any{
			"query": "needle",
			"messages": []map[string]any{
				{"id": 71, "sent_at": "2024-01-15T10:30:00Z", "labels": []string{}, "size_bytes": 1},
				{"id": 82, "sent_at": "2024-01-16T10:30:00Z", "labels": []string{}, "size_bytes": 1},
			},
			"total_count":        2,
			"applied_source_ids": []int64{7, 8},
			"stats":              map[string]any{"message_count": 2, "account_count": 2},
		})
	})
	engine := NewEngineAdapter(store)

	result, err := engine.SearchFastWithStats(context.Background(), search.Parse("needle"), "needle",
		query.MessageFilter{SourceIDs: []int64{7, 8}}, query.ViewSenders, 10, 0)
	require.NoError(err)
	require.Len(result.Messages, 2)
	assert.ElementsMatch([]int64{71, 82}, []int64{result.Messages[0].ID, result.Messages[1].ID})
	assert.Equal(int64(2), result.TotalCount)
	require.NotNil(result.Stats)
	assert.Equal(int64(2), result.Stats.MessageCount)
	assert.Equal(int64(2), result.Stats.AccountCount)
}

func TestEngineSearchFastWithStatsRequiresSourceIDEcho(t *testing.T) {
	tests := []struct {
		name string
		echo any
	}{
		{name: "missing"},
		{name: "mismatch", echo: []int64{7, 9}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, _ *http.Request) {
				response := map[string]any{
					"query": "needle", "messages": []map[string]any{}, "total_count": 0,
					"stats": map[string]any{"message_count": 0},
				}
				if tc.echo != nil {
					response["applied_source_ids"] = tc.echo
				}
				writeJSONResponse(t, w, response)
			})
			engine := NewEngineAdapter(store)

			result, err := engine.SearchFastWithStats(context.Background(), search.Parse("needle"), "needle",
				query.MessageFilter{SourceIDs: []int64{7, 8}}, query.ViewSenders, 10, 0)
			require.Error(err, "unconfirmed fast source scope must fail closed")
			assert.Nil(result)
			assert.Contains(err.Error(), "source IDs")
			assert.Contains(err.Error(), "upgrade")
		})
	}
}

func TestEngineFastSearchExplicitEmptySourceIDsSkipsHTTP(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	called := false
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSONResponse(t, w, map[string]any{"total_count": 99})
	})
	engine := NewEngineAdapter(store)
	filter := query.MessageFilter{SourceIDs: []int64{}}
	q := search.Parse("needle")

	messages, err := engine.SearchFast(context.Background(), q, filter, 10, 0)
	require.NoError(err)
	assert.Empty(messages)
	count, err := engine.SearchFastCount(context.Background(), q, filter)
	require.NoError(err)
	assert.Zero(count)
	result, err := engine.SearchFastWithStats(context.Background(), q, "needle", filter, query.ViewSenders, 10, 0)
	require.NoError(err)
	require.NotNil(result.Stats)
	assert.Zero(result.TotalCount)
	assert.Zero(result.Stats.MessageCount)
	assert.False(called, "explicit empty source scope must skip HTTP")
}

func TestEngineSearchFastWithStatsUsesCanonicalParsedQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	q := &search.Query{
		TextTerms: []string{"meeting notes"},
		Labels:    []string{`Project "Review"`},
	}
	wantQuery := search.Format(q)
	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search/fast", r.URL.Path, "path")
		gotQuery := r.URL.Query().Get("q")
		assert.Equal(wantQuery, gotQuery, "parsed query is authoritative")
		parsed := search.Parse(gotQuery)
		assert.Equal(q.TextTerms, parsed.TextTerms, "text terms")
		assert.Equal(q.Labels, parsed.Labels, "labels")
		writeJSONResponse(t, w, map[string]any{
			"query":       gotQuery,
			"total_count": 1,
			"stats": map[string]any{
				"message_count": 1,
				"total_size":    2048,
			},
			"messages": []map[string]any{{
				"id":         84,
				"subject":    "Project Review meeting notes",
				"from":       "alice@example.com",
				"from_email": "alice@example.com",
				"sent_at":    "2024-01-15T10:30:00Z",
				"snippet":    "matching result",
				"labels":     []string{`Project "Review"`},
				"size_bytes": 2048,
			}},
		})
	})
	engine := NewEngineAdapter(store)

	result, err := engine.SearchFastWithStats(
		context.Background(),
		q,
		`lossy query label:Wrong`,
		query.MessageFilter{},
		query.ViewSenders,
		10,
		0,
	)
	require.NoError(err, "SearchFastWithStats")
	require.Len(result.Messages, 1, "messages")
	assert.Equal(int64(84), result.Messages[0].ID)
	assert.Equal(int64(1), result.TotalCount)
	require.NotNil(result.Stats, "stats")
	assert.Equal(int64(1), result.Stats.MessageCount)
	assert.Equal(int64(2048), result.Stats.TotalSize)
}

func TestEngineSearchFastWithStatsMergesFilterMessageTypeIntoQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search/fast", r.URL.Path, "path")
		gotQuery := r.URL.Query().Get("q")
		assert.Contains(gotQuery, "lunch", "q should preserve text terms")
		assert.Contains(gotQuery, "message_type:sms", "q should carry filter-only message type")
		assert.Empty(r.URL.Query()["message_type"], "message_type filter param is unsupported by fast search")
		writeJSONResponse(t, w, map[string]any{
			"query":       "message_type:sms lunch",
			"total_count": 1,
			"stats": map[string]any{
				"message_count": 1,
			},
			"messages": []map[string]any{
				{
					"id":           84,
					"message_type": "sms",
					"from":         "alice@example.com",
					"from_email":   "alice@example.com",
					"sent_at":      "2024-01-15T10:30:00Z",
					"snippet":      "preview",
					"size_bytes":   4096,
				},
			},
		})
	})

	engine := NewEngineAdapter(store)
	parsedQuery := search.Parse("lunch")

	result, err := engine.SearchFastWithStats(
		context.Background(),
		parsedQuery,
		"lunch",
		query.MessageFilter{MessageType: "sms"},
		query.ViewSenders,
		10,
		0,
	)

	require.NoError(err, "SearchFastWithStats")
	require.Len(result.Messages, 1)
	assert.Equal("sms", result.Messages[0].MessageType)
	assert.Empty(parsedQuery.MessageTypes, "base query MessageTypes must not be mutated")
}

func TestEngineSearchFastWithStatsMessageTypeConflictReturnsNoMatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.Fail("conflicting message-type scope should not issue a remote request")
		http.Error(w, "unexpected remote request", http.StatusInternalServerError)
	})

	engine := NewEngineAdapter(store)
	parsedQuery := search.Parse("message_type:email lunch")

	result, err := engine.SearchFastWithStats(
		context.Background(),
		parsedQuery,
		"message_type:email lunch",
		query.MessageFilter{MessageType: "sms"},
		query.ViewSenders,
		10,
		0,
	)

	require.NoError(err, "SearchFastWithStats")
	require.NotNil(result, "SearchFastWithStats result")
	assert.Empty(result.Messages, "conflicting message types should match no messages")
	assert.Equal(int64(0), result.TotalCount, "total count")
	require.NotNil(result.Stats, "Stats")
	assert.Equal(int64(0), result.Stats.MessageCount, "Stats.MessageCount")
	assert.Equal([]string{"email"}, parsedQuery.MessageTypes, "base query MessageTypes must not be mutated")
}
