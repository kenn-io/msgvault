package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperEventArchivesHTTPMessage(t *testing.T) {
	testBeeperEventArchive(t, false)
}
func TestBeeperEventsUnavailableRetainsPolling(t *testing.T) {
	testBeeperEventArchive(t, true)
}
func testBeeperEventArchive(t *testing.T, outage bool) {
	t.Helper()
	st := testutil.NewTestStore(t)
	included, err := st.GetOrCreateSource(sourceTypeBeeper, "included-example")
	require.NoError(t, err)
	excluded, err := st.GetOrCreateSource(sourceTypeBeeper, "excluded-example")
	require.NoError(t, err)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	cfg.Beeper.Enabled = true
	cfg.Beeper.Events = true
	cfg.Beeper.Schedule = "0 0 1 1 *"
	cfg.Beeper.ExcludeAccounts = []string{"excluded-example"}
	cfg.Beeper.RateLimitQPS = 1000
	if outage {
		cfg.Beeper.Schedule = "* * * * *"
	}
	noMedia := false
	cfg.Beeper.Media = &noMedia
	var exposed atomic.Bool
	var excludedRequests atomic.Int32
	emit := make(chan struct{}, 1)
	wsDone := make(chan struct{})
	discovery := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctxDone := ctx.Done()
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	chat := func(account string) map[string]any {
		return map[string]any{"id": "chat-" + account, "accountID": account, "network": "Signal", "title": "Synthetic chat", "type": "single", "lastActivity": timestamp, "participants": map[string]any{"items": []map[string]any{{"id": "sender-example", "fullName": "Example Sender"}}, "total": 1, "hasMore": false}}
	}
	message := func(account string) map[string]any {
		return map[string]any{"id": "message-example", "chatID": "chat-" + account, "accountID": account, "senderID": "sender-example", "senderName": "Example Sender", "timestamp": timestamp, "sortKey": "1", "type": "TEXT", "text": "Synthetic event import body"}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic", r.Header.Get("Authorization"))
		write := func(v any) {
			b, e := json.Marshal(v)
			if assert.NoError(t, e) {
				_, _ = w.Write(b)
			}
		}
		switch {
		case r.URL.Path == "/v1/info":
			select {
			case discovery <- struct{}{}:
			default:
			}
			if outage {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			write(map[string]any{"endpoints": map[string]string{"ws_events": "/events"}})
		case r.URL.Path == "/events":
			conn, e := websocket.Accept(w, r, nil)
			if !assert.NoError(t, e) {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			defer close(wsDone)
			writeWS := func(s string) bool {
				return assert.NoError(t, conn.Write(r.Context(), websocket.MessageText, []byte(s)))
			}
			if !writeWS(`{"type":"ready","version":1}`) {
				return
			}
			_, b, e := conn.Read(r.Context())
			if !assert.NoError(t, e) {
				return
			}
			var command struct {
				RequestID string `json:"requestID"`
			}
			if !assert.NoError(t, json.Unmarshal(b, &command)) {
				return
			}
			if !writeWS(fmt.Sprintf(`{"type":"subscriptions.updated","requestID":%q,"chatIDs":["*"]}`, command.RequestID)) {
				return
			}
			select {
			case <-emit:
			case <-ctxDone:
				return
			}
			if !writeWS(`{"type":"message.upserted","seq":1,"chatID":"chat-included-example","ids":["message-example"]}`) {
				return
			}
			_, _, _ = conn.Read(r.Context())
		case strings.HasPrefix(r.URL.Path, "/v1/accounts/"):
			account := strings.TrimPrefix(r.URL.Path, "/v1/accounts/")
			write(map[string]any{"accountID": account, "network": "Signal", "user": map[string]any{"id": "self-example"}})
		case r.URL.Path == "/v1/chats/search":
			account := r.URL.Query().Get("accountIDs")
			if account == "excluded-example" {
				excludedRequests.Add(1)
			}
			items := []map[string]any{}
			if exposed.Load() {
				items = append(items, chat(account))
			}
			write(map[string]any{"items": items, "hasMore": false})
		case strings.HasPrefix(r.URL.Path, "/v1/chats/"):
			account := "included-example"
			if strings.Contains(r.URL.Path, "excluded-example") {
				account = "excluded-example"
				excludedRequests.Add(1)
			}
			if strings.Contains(r.URL.Path, "/messages/") {
				write(message(account))
			} else if strings.HasSuffix(r.URL.Path, "/messages") {
				items := []map[string]any{}
				if r.URL.Query().Get("cursor") == "" && exposed.Load() {
					items = append(items, message(account))
				}
				write(map[string]any{"items": items, "hasMore": false, "oldestCursor": "1", "newestCursor": "1"})
			} else {
				write(chat(account))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() { cancel(); server.Close() }()
	cfg.Beeper.URL = server.URL
	require.NoError(t, beeper.SaveToken(cfg.TokensDir(), "synthetic"))
	state := testInvocationWithConfig(cfg)
	var manualCompletions atomic.Int32
	sched := scheduler.New(nil).WithLogger(testDiscardLogger()).WithCompletion(func() { manualCompletions.Add(1) })
	completed := make(chan error, 10)
	run := invocationBoundJobRun(state, func(ctx context.Context) error { return runConfiguredBeeperSync(ctx, st) })
	require.NoError(t, registerScheduledBeeperJob(sched, cfg.Beeper.Schedule, nil, func(ctx context.Context) error { err := run(ctx); completed <- err; return err }))
	sched.Start()
	defer func() { <-sched.Stop().Done() }()
	stop := startBeeperEvents(ctx, cfg, sched, true, testDiscardLogger())
	defer stop()
	select {
	case <-discovery:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no discovery")
	}
	waitRun := func() {
		t.Helper()
		timeout := 10 * time.Second
		if outage {
			timeout = 90 * time.Second
		}
		select {
		case err := <-completed:
			require.NoError(t, err)
		case <-time.After(timeout):
			require.FailNow(t, "importer did not finish")
		}
	}
	if !outage {
		waitRun() // subscription startup catch-up must settle before the event's message exists
		var count int
		require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
		assert.Zero(t, count)
	}
	exposed.Store(true)
	if !outage {
		emit <- struct{}{}
	}
	waitRun()
	var id int64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE source_id = ?"), included.ID).Scan(&id))
	body, raw, err := st.GetMessageBodyAndRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Contains(t, body, "Synthetic event import body")
	assert.Contains(t, string(raw), `"id":"message-example"`)
	var excludedCount int
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), excluded.ID).Scan(&excludedCount))
	assert.Zero(t, excludedCount)
	assert.Zero(t, excludedRequests.Load())
	assert.Zero(t, manualCompletions.Load(), "event admissions must retain background-job priority")
	stop()
	cancel()
	if !outage {
		select {
		case <-wsDone:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "socket remained open")
		}
	}
}
