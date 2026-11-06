package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

func TestLineServeMountsOnlyReadyRegisteredChannels(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	cfg, st, fake := lineWiringFixture(t)
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	must.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	loaded.Line = append(loaded.Line,
		config.LineSource{ChannelID: "2000000002", SecretEnv: "EXAMPLE_LINE_SECRET", Enabled: true},
		config.LineSource{ChannelID: "2000000003", SecretEnv: "MISSING_LINE_SECRET", Enabled: true},
		config.LineSource{ChannelID: "2000000004", SecretEnv: "EXAMPLE_LINE_SECRET", Enabled: false},
		config.LineSource{ChannelID: "2000000005", SecretEnv: "EXAMPLE_LINE_SECRET", Enabled: true},
		config.LineSource{ChannelID: "2000000006", SecretEnv: "EXAMPLE_LINE_SECRET", Enabled: true},
	)
	// Channels 3 and 4 are otherwise ready, so only the missing secret or the
	// disabled flag keeps each one unmounted.
	t.Setenv("MISSING_LINE_SECRET", "")
	for _, channelID := range []string{"2000000003", "2000000004", "2000000006"} {
		source, err := st.GetOrCreateSource("line", channelID)
		must.NoError(err)
		must.NoError(st.SetLineBotContext(t.Context(), source.ID, wiringBot))
	}
	_, err = st.GetOrCreateSource("line", "2000000005")
	must.NoError(err)
	retired, err := st.GetSourceByTypeAndIdentifier("line", "2000000006")
	must.NoError(err)
	active, err := st.GetSourceByTypeAndIdentifier("line", "2000000001")
	must.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: retired.ID, IntoSourceID: active.ID})
	must.NoError(err)
	sched := scheduler.New(nil).WithLogger(testLoggerValue())
	t.Cleanup(func() { <-sched.Stop().Done() })
	state := testInvocationWithConfig(loaded)
	runtime, err := newLineRuntime(t.Context(), st, state, sched, nil, testLoggerValue())
	must.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("line", "2000000001")
	must.NoError(err)
	checks.True(runtime.receiver.Active(source.ID, source.Identifier))
	checks.False(runtime.receiver.Active(retired.ID, retired.Identifier))
	jobs := sched.JobStatus()
	must.Len(jobs, 1)
	checks.Equal("line:2000000001", jobs[0].Name)
	checks.Equal("@every 30s", jobs[0].Schedule)
	checks.Equal(1, fake.Count("GET", "/v2/bot/info"), "serve initialization must not call LINE")
	checks.Equal(1, fake.Count("POST", "/oauth2/v3/token"))
}

func TestLineServeWebhookAckDoesNotWaitForEnrichment(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	cfg, st, _ := lineWiringFixture(t)
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	must.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	profileStarted := make(chan struct{}, 2)
	release := make(chan struct{})
	api := lineapi.New()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/profile/") {
			profileStarted <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		api.ServeHTTP(w, r)
	}))
	defer provider.Close()
	defer close(release)
	lineClientFactory = func(id, secret string) *line.Client {
		return line.NewClient(id, secret, line.WithBaseURLs(provider.URL, provider.URL))
	}
	sched := scheduler.New(nil).WithLogger(testLoggerValue())
	runtime, err := newLineRuntime(t.Context(), st, testInvocationWithConfig(loaded), sched, nil, testLoggerValue())
	must.NoError(err)
	sched.Start()
	defer func() { <-sched.Stop().Done() }()
	body := `{"destination":"` + wiringBot + `","events":[{"type":"message","webhookEventId":"wake-event","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}]}`
	request := httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000001", strings.NewReader(body))
	request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(body)))
	acknowledged := make(chan int, 1)
	go func() {
		response := httptest.NewRecorder()
		runtime.receiver.Receive(response, request, nil)
		acknowledged <- response.Code
	}()
	select {
	case status := <-acknowledged:
		checks.Equal(200, status)
	case <-time.After(5 * time.Second):
		must.FailNow("webhook acknowledgement waited for processor")
	}
	select {
	case <-profileStarted:
	case <-time.After(5 * time.Second):
		must.FailNow("wake did not start processor")
	}
}

func TestLineServeSecondReceiptDuringEnrichment(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	cfg, st, fake := lineWiringFixture(t)
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	must.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	started := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/profile/") {
			select {
			case started <- struct{}{}:
			case <-r.Context().Done():
				return
			}
			<-r.Context().Done()
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer provider.Close()
	lineClientFactory = func(id, secret string) *line.Client {
		return line.NewClient(id, secret, line.WithBaseURLs(provider.URL, provider.URL))
	}
	gate := api.NewSerialOperationGate()
	sched := scheduler.New(nil).WithWorkTracker(labelWorkTracker(gate, "LINE inbox")).WithLogger(testLoggerValue())
	runtime, err := newLineRuntime(t.Context(), st, testInvocationWithConfig(loaded), sched, nil, testLoggerValue())
	must.NoError(err)
	sched.Start()
	defer func() { <-sched.Stop().Done() }()
	server := api.NewServerWithOptions(api.ServerOptions{Config: loaded, Logger: testLoggerValue(), LineWebhook: runtime.receiver, OperationGate: gate})
	defer func() { must.NoError(server.Shutdown(context.Background())) }()
	listener := httptest.NewServer(server.Router())
	defer listener.Close()
	client := &http.Client{Timeout: 2 * time.Second} // LINE requires acknowledgement within two seconds.
	for id := range 2 {
		if id == 1 {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				must.FailNow("scheduled profile lookup did not begin")
			}
			_, _, held := gate.Holder()
			must.True(held, "enrichment must hold the operation gate during the second receipt")
		}
		body := fmt.Sprintf(`{"destination":"%s","events":[{"type":"message","webhookEventId":"deadline-%d","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"%d","type":"text","text":"synthetic"}}]}`, wiringBot, id, id)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, listener.URL+"/webhooks/line/2000000001", strings.NewReader(body))
		must.NoError(err)
		request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(body)))
		response, err := client.Do(request)
		must.NoError(err, "signed receipt must be acknowledged within LINE's two-second deadline")
		_, err = io.Copy(io.Discard, response.Body)
		must.NoError(response.Body.Close())
		must.NoError(err)
		must.Equal(http.StatusOK, response.StatusCode)
	}
	var receipts int
	must.NoError(st.DB().QueryRow("SELECT count(*) FROM line_webhook_events WHERE webhook_event_id IN ('deadline-0', 'deadline-1')").Scan(&receipts))
	checks.Equal(2, receipts, "both acknowledged receipts must be durable while enrichment is blocked")
}

func TestLineServeStartupDrainsPendingWork(t *testing.T) {
	must := require.New(t)

	cfg, st, _ := lineWiringFixture(t)
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	must.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("line", "2000000001")
	must.NoError(err)
	must.NoError(st.EnqueueLineEventsContext(t.Context(), source.ID, source.Identifier, []store.LineWebhookEvent{{WebhookEventID: "startup-event", EventType: "future", EventTimestamp: 123, RawJSON: `{"type":"future","webhookEventId":"startup-event","timestamp":123}`}}))
	prior := rebuildLineCacheAfterScheduledSync
	t.Cleanup(func() { rebuildLineCacheAfterScheduledSync = prior })
	refreshed := make(chan struct{}, 1)
	rebuildLineCacheAfterScheduledSync = func(context.Context, string) error { refreshed <- struct{}{}; return nil }
	sched := scheduler.New(nil).WithLogger(testLoggerValue())
	runtime, err := newLineRuntime(t.Context(), st, testInvocationWithConfig(loaded), sched, nil, testLoggerValue())
	must.NoError(err)
	sched.Start()
	defer func() { <-sched.Stop().Done() }()
	runtime.queueStartup()
	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		must.FailNow("startup did not process pending work")
	}
	status, err := st.LineInboxStatusContext(t.Context(), source.ID)
	must.NoError(err)
	assert.Zero(t, status.Pending)
}

func TestLineServeSignedReceiptsDoNotStarveLaterText(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	cfg, st, fake := lineWiringFixture(t)
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	must.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	started := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			select {
			case started <- struct{}{}:
			case <-r.Context().Done():
				return
			}
			<-r.Context().Done()
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer provider.Close()
	lineClientFactory = func(id, secret string) *line.Client {
		return line.NewClient(id, secret, line.WithBaseURLs(provider.URL, provider.URL))
	}
	sched := scheduler.New(nil).WithLogger(testLoggerValue())
	runtime, err := newLineRuntime(t.Context(), st, testInvocationWithConfig(loaded), sched, nil, testLoggerValue())
	must.NoError(err)
	sched.Start()
	defer func() { <-sched.Stop().Done() }()
	server := api.NewServerWithOptions(api.ServerOptions{Config: loaded, Logger: testLoggerValue(), LineWebhook: runtime.receiver})
	defer func() { must.NoError(server.Shutdown(context.Background())) }()
	listener := httptest.NewServer(server.Router())
	defer listener.Close()
	receive := func(id int, message string) int {
		t.Helper()
		body := fmt.Sprintf(`{"destination":"%s","events":[{"type":"message","webhookEventId":"event-%d","timestamp":%d,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":%s}]}`, wiringBot, id, 1000+id, message)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, listener.URL+"/webhooks/line/2000000001", strings.NewReader(body))
		must.NoError(err)
		request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(body)))
		response, err := listener.Client().Do(request)
		must.NoError(err, "signed receipt must be acknowledged within LINE's two-second deadline")
		_, err = io.Copy(io.Discard, response.Body)
		must.NoError(response.Body.Close())
		must.NoError(err)
		return response.StatusCode
	}
	must.Equal(http.StatusOK, receive(0, `{"id":"0","type":"file","fileName":"example.txt","fileSize":17,"contentProvider":{"type":"line"}}`))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		must.FailNow("scheduled media download did not begin")
	}
	must.Equal(http.StatusOK, receive(1, `{"id":"1","type":"text","text":"later searchable text"}`))
	checks.EventuallyWithT(func(c *assert.CollectT) {
		var count int
		require.NoError(c, st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_message_id='1'").Scan(&count))
		assert.Equal(c, 1, count, "later text must archive while earlier media is blocked")
		matches, err := query.NewSQLiteEngine(st.DB()).TextSearch(t.Context(), "searchable", nil, 10, 0)
		require.NoError(c, err)
		assert.Len(c, matches, 1, "later text must be searchable")
	}, 5*time.Second, 20*time.Millisecond)
	checks.EventuallyWithT(func(c *assert.CollectT) {
		var state string
		var next time.Time
		require.NoError(c, st.DB().QueryRow("SELECT state,next_attempt_at FROM line_webhook_events WHERE webhook_event_id='event-0'").Scan(&state, &next))
		assert.Equal(c, "retry", state)
		assert.True(c, next.After(time.Now()), "interrupted media must wait for backoff")
	}, 5*time.Second, 20*time.Millisecond)
	freeze := httptest.NewRequest(http.MethodPost, "/api/v1/backup/freeze/begin", nil)
	freeze.RemoteAddr = "127.0.0.1:54321"
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, freeze)
	must.Equal(http.StatusOK, response.Code, response.Body.String())
	var frozen struct {
		Token string `json:"token"`
	}
	must.NoError(json.Unmarshal(response.Body.Bytes(), &frozen))
	defer func() {
		end := httptest.NewRequest(http.MethodPost, "/api/v1/backup/freeze/end", strings.NewReader(fmt.Sprintf(`{"token":%q}`, frozen.Token)))
		end.RemoteAddr = "127.0.0.1:54321"
		end.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, end)
		must.Equal(http.StatusOK, response.Code, response.Body.String())
	}()
	checks.Equal(http.StatusServiceUnavailable, receive(2, `{"id":"2","type":"text","text":"frozen text"}`))
	var count int
	must.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM line_webhook_events WHERE webhook_event_id='event-2'").Scan(&count))
	checks.Zero(count, "backup freeze must reject the receipt before persistence")
}
