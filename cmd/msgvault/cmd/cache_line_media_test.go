package cmd

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

func TestLineMediaCacheAppend(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	analyticsDir := t.TempDir()
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	chat, err := st.EnsureConversationWithType(src.ID, "group:synthetic", "group_chat", "Example")
	must.NoError(err)
	sentAt := sql.NullTime{Time: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), Valid: true}
	for _, id := range []string{"1", "2"} {
		_, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: chat, SourceMessageID: id, MessageType: "line", SentAt: sentAt})
		must.NoError(err)
	}
	_, err = buildCache(dbPath, analyticsDir, true)
	must.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:  &store.Message{SourceID: src.ID, ConversationID: chat, SourceMessageID: "3", MessageType: "line", SentAt: sentAt},
		BodyText: sql.NullString{String: "file", Valid: true},
	})
	must.NoError(err)
	must.NoError(st.UpsertLineAttachmentContext(t.Context(), id, store.AttachmentWrite{
		SourceAttachmentID: "line:3", Filename: "example.bin", ContentHash: "synthetic-hash", StoragePath: "synthetic-path", Size: 12,
		State: attachmentpolicy.StateStored,
	}))
	result, err := buildCacheAuto(dbPath, analyticsDir)
	must.NoError(err)
	checks.Equal(int64(1), result.StagedCount, "new media must append only the new message")
}

// Cache publication is SQLite-only.
func TestLineEnrichmentRetryRefreshesCachedResults(t *testing.T) {
	for _, profileRetry := range []bool{false, true} {
		name := "media"
		if profileRetry {
			name = "profile"
		}
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			ctx := t.Context()
			dbPath := filepath.Join(t.TempDir(), "archive.db")
			st := testutil.NewSQLiteTestStoreAt(t, dbPath)
			analyticsDir := t.TempDir()
			api := lineapi.New()
			server := httptest.NewServer(api)
			t.Cleanup(server.Close)
			now := time.Now().UTC().Truncate(time.Second)
			client := line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(server.URL, server.URL), line.WithClock(func() time.Time { return now }))
			source, err := line.Register(ctx, st, client, "2000000001")
			must.NoError(err)
			processor := line.NewProcessor(st, client, source.ID, "2000000001", t.TempDir(), line.WithProcessorClock(func() time.Time { return now }))
			content := lineapi.Response{Status: http.StatusOK, Body: "synthetic bytes"}
			if profileRetry {
				api.Enqueue(http.MethodGet, "/v2/bot/profile/U00000000000000000000000000000002", lineapi.Response{Status: http.StatusServiceUnavailable})
				api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", content)
			} else {
				api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: http.StatusAccepted}, content)
			}
			raw := fmt.Sprintf(`{"type":"message","webhookEventId":"event-1","timestamp":%d,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"type":"file","id":"1","fileName":"synthetic.bin"}}`, now.UnixMilli())
			must.NoError(st.EnqueueLineEventsContext(ctx, source.ID, source.Identifier, []store.LineWebhookEvent{{WebhookEventID: "event-1", EventType: "message", TargetMessageID: "1", EventTimestamp: now.UnixMilli(), RawJSON: raw, ReceivedAt: now}}))
			must.NoError(processor.Process(ctx))
			_, err = buildCache(dbPath, analyticsDir, true)
			must.NoError(err)
			engine, err := query.NewDuckDBEngine(analyticsDir, dbPath, st.DB())
			must.NoError(err)
			t.Cleanup(func() { must.NoError(engine.Close()) })
			before, err := engine.SearchFast(ctx, search.Parse("larger:13"), query.MessageFilter{}, 10, 0)
			must.NoError(err)
			if profileRetry {
				must.Len(before, 1)
			} else {
				checks.Empty(before)
			}
			now = now.Add(time.Minute)
			must.NoError(processor.Process(ctx))
			_, err = buildCacheAuto(dbPath, analyticsDir)
			must.NoError(err)
			messages, err := engine.SearchFast(ctx, search.Parse("larger:13"), query.MessageFilter{}, 10, 0)
			must.NoError(err)
			must.Len(messages, 1)
			checks.Equal(int64(28), messages[0].SizeEstimate)
			checks.Equal("Example User", messages[0].FromName)
		})
	}
}
