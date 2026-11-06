package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

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
