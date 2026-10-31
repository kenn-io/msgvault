package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLineSourceStatusReportsDurableInboxAndActiveReceiver(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	must.NoError(st.SetLineBotContext(context.Background(), source.ID, lineTestBot))
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{{ChannelID: source.Identifier, SourceID: source.ID, BotUserID: lineTestBot, Secret: lineTestSecret}}, nil)
	must.NoError(err)
	bad := httptest.NewRequest(http.MethodPost, lineTestPath, strings.NewReader(`{}`))
	receiver.ServeHTTP(httptest.NewRecorder(), bad)
	received := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	must.NoError(st.EnqueueLineEventsContext(context.Background(), source.ID, source.Identifier, lineTestBot, []store.LineWebhookEvent{
		{WebhookEventID: "pending", EventType: "message", TargetMessageID: "1", EventTimestamp: 1, RawJSON: `{"type":"message","timestamp":1,"webhookEventId":"pending","source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"text","text":"synthetic"}}`, ReceivedAt: received},
		{WebhookEventID: "failed", EventType: "message", TargetMessageID: "2", EventTimestamp: 2, RawJSON: `{"type":"message","timestamp":2,"webhookEventId":"failed","source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"2","type":"text","text":"synthetic"}}`, ReceivedAt: received},
	}))
	rows, err := st.DueLineEventsContext(context.Background(), source.ID, time.Now(), 10)
	must.NoError(err)
	must.Len(rows, 2)
	must.NoError(st.FinishLineEventContext(context.Background(), rows[1].ID, "failed", "synthetic failure", time.Time{}))
	srv := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, Logger: testLogger(), LineWebhook: receiver})
	recorder := httptest.NewRecorder()
	srv.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/sources/status?source_type=line", nil))
	must.Equal(http.StatusOK, recorder.Code)
	var response struct {
		Sources []struct {
			LineWebhook map[string]any `json:"line_webhook"`
		} `json:"sources"`
	}
	must.NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
	must.Len(response.Sources, 1)
	status := response.Sources[0].LineWebhook
	must.NotNil(status)
	checks.Equal(true, status["active"])
	checks.Equal(received.Format(time.RFC3339), status["last_received_at"])
	checks.InDelta(float64(1), status["pending"], 0)
	checks.InDelta(float64(1), status["failed"], 0)
	// Runtime receiver state must not be inferred from disk config alone.
	srv = NewServer(config.NewDefaultConfig(), st, nil, testLogger())
	recorder = httptest.NewRecorder()
	srv.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/sources/status?source_type=line", nil))
	must.Equal(http.StatusOK, recorder.Code)
	must.NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
	checks.Equal(false, response.Sources[0].LineWebhook["active"])
}
