package cmd

import (
	"context"
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
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/line"
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
	for _, channelID := range []string{"2000000003", "2000000004", "2000000005"} {
		source, err := st.GetSourceByTypeAndIdentifier("line", channelID)
		must.NoError(err)
		checks.False(runtime.receiver.Active(source.ID, source.Identifier))
	}
	jobs := sched.JobStatus()
	must.Len(jobs, 1)
	checks.Equal("line:2000000001", jobs[0].Name)
	checks.Equal("@every 30s", jobs[0].Schedule)
	checks.Equal(1, fake.Count("GET", "/v2/bot/info"), "serve initialization must not call LINE")
	checks.Equal(1, fake.Count("POST", "/oauth2/v3/token"))
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

func TestLineServeReceiptsDuringEnrichment(t *testing.T) {
	for _, enrichment := range []string{"profile", "media"} {
		t.Run(enrichment, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			cfg, st, fake := lineWiringFixture(t)
			_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
			must.NoError(err)
			loaded, err := config.Load(cfg.ConfigFilePath(), "")
			must.NoError(err)
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if enrichment == "media" && strings.HasSuffix(r.URL.Path, "/content") ||
					enrichment == "profile" && strings.Contains(r.URL.Path, "/profile/") {
					select {
					case started <- struct{}{}:
					case <-r.Context().Done():
						return
					}
					select {
					case <-r.Context().Done():
						return
					case <-release:
					}
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
			first := `{"id":"0","type":"file","fileName":"example.txt","fileSize":17,"contentProvider":{"type":"line"}}`
			if enrichment == "profile" {
				first = `{"id":"0","type":"text","text":"synthetic"}`
			}
			must.Equal(http.StatusOK, receive(0, first))
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				must.FailNow("scheduled enrichment did not begin")
			}
			_, _, held := gate.Holder()
			must.True(held, "enrichment must hold the operation gate during the second receipt")
			must.Equal(http.StatusOK, receive(1, `{"id":"1","type":"text","text":"later searchable text"}`))
			if enrichment == "profile" {
				return
			}
			for range 10 {
				runtime.wake("2000000001")
			}
			close(release)
			checks.EventuallyWithT(func(c *assert.CollectT) {
				var count int
				require.NoError(c, st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_message_id='1'").Scan(&count))
				assert.Equal(c, 1, count, "later text must archive in the queued pass after media completes")
				source, err := st.GetSourceByTypeAndIdentifier("line", "2000000001")
				require.NoError(c, err)
				row, err := st.LineMessageContext(t.Context(), source.ID, "0")
				require.NoError(c, err)
				refs, err := st.MessageProviderAttachments(row.ID, "line:")
				require.NoError(c, err)
				assert.Equal(c, attachmentpolicy.StateStored, refs["line:0"].State)
			}, 5*time.Second, 20*time.Millisecond)
		})
	}
}
