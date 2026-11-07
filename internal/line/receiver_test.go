package line_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

const receiverBot = "U00000000000000000000000000000001"

func receiverChannel(sourceID int64) line.ReceiverChannel {
	return line.ReceiverChannel{ChannelID: "2000000000", SourceID: sourceID, BotUserID: receiverBot, Secret: "synthetic-secret"}
}

func TestReceiverConfigurationAndHandles(t *testing.T) {
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	channel := receiverChannel(1)
	receiver := line.NewReceiver(st, []line.ReceiverChannel{channel}, nil)
	checks.True(receiver.Handles(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/2000000000", nil)))
	checks.True(receiver.Handles(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/unknown", nil)))
	for _, target := range []string{"/webhooks/line/", "/webhooks/line/2000000000/", "/webhooks/line/2000000000/extra", "/webhooks/line/%32%30%30%30%30%30%30%30%30%30", "/webhooks/line/2000000000%2Fextra", "/webhooks/line/2000000000%252Fextra", "/api/v1/webhooks/line/2000000000"} {
		checks.False(receiver.Handles(httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, nil)), target)
	}
	for _, method := range []string{"GET", "HEAD", "PUT", "OPTIONS"} {
		checks.False(receiver.Handles(httptest.NewRequestWithContext(t.Context(), method, "/webhooks/line/2000000000", nil)))
	}
	empty := line.NewReceiver(st, nil, nil)
	checks.False(empty.Handles(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/unknown", nil)))
}

func TestReceiverWakeAfterCommit(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	must.NoError(st.SetLineBotContext(t.Context(), source.ID, receiverBot))
	response := httptest.NewRecorder()
	wakeCalls := 0
	receiver := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(channelID string) {
		wakeCalls++
		checks.Equal("2000000000", channelID)
		rows, err := st.DueLineEventsContext(t.Context(), source.ID, time.Now(), 10)
		must.NoError(err)
		must.Len(rows, 1)
		checks.Equal("event-1", rows[0].WebhookEventID)
		checks.NotContains(rows[0].RawJSON, "replyToken")
	})
	body := `{"destination":"` + receiverBot + `","events":[{"type":"message","replyToken":"ephemeral","webhookEventId":"event-1","timestamp":123,"source":{"type":"user"},"message":{"id":"123","type":"text","text":"synthetic","quoteToken":"retained","nested":[{"markAsReadToken":"ephemeral"}]}}]}`
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/2000000000", strings.NewReader(body))
	request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(body)))
	receiver.Receive(response, request, nil)
	checks.Equal(200, response.Code, response.Body.String())
	checks.Equal(1, wakeCalls)
}

func receiverEnvelope(events string) string {
	return `{"destination":"` + receiverBot + `","events":` + events + `}`
}

func serveSignedReceiver(tb testing.TB, receiver *line.Receiver, body string) *httptest.ResponseRecorder {
	tb.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, "/webhooks/line/2000000000", strings.NewReader(body))
	request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(body)))
	receiver.Receive(response, request, nil)
	return response
}

func TestReceiverEnvelopeValidation(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	must.NoError(st.SetLineBotContext(t.Context(), source.ID, receiverBot))
	wakeCalls := 0
	receiver := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) { wakeCalls++ })
	message := `{"type":"message","webhookEventId":"valid-first","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}`
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"empty events", receiverEnvelope("[]"), http.StatusOK},
		{"missing events", `{"destination":"` + receiverBot + `"}`, http.StatusBadRequest},
		{"missing destination", `{"events":[]}`, http.StatusBadRequest},
		{"null events", receiverEnvelope("null"), http.StatusBadRequest},
		{"object events", receiverEnvelope("{}"), http.StatusBadRequest},
		{"bad JSON", `{`, http.StatusBadRequest},
		{"trailing JSON", receiverEnvelope("[]") + `{}`, http.StatusBadRequest},
		{"missing id", receiverEnvelope(`[{"type":"future","timestamp":123}]`), http.StatusBadRequest},
		{"long id", receiverEnvelope(fmt.Sprintf(`[{"type":"future","timestamp":123,"webhookEventId":%q}]`, strings.Repeat("a", 201))), http.StatusBadRequest},
		{"zero time", receiverEnvelope(`[{"type":"future","timestamp":0,"webhookEventId":"event"}]`), http.StatusBadRequest},
		{"missing type", receiverEnvelope(`[{"timestamp":123,"webhookEventId":"event"}]`), http.StatusBadRequest},
		{"missing message", receiverEnvelope(`[{"type":"message","timestamp":123,"webhookEventId":"event"}]`), http.StatusBadRequest},
		{"missing message id", receiverEnvelope(`[{"type":"messageEdited","timestamp":123,"webhookEventId":"event","message":{"type":"text"}}]`), http.StatusBadRequest},
		{"missing message type", receiverEnvelope(`[{"type":"message","timestamp":123,"webhookEventId":"event","message":{"id":"123"}}]`), http.StatusBadRequest},
		{"missing unsend", receiverEnvelope(`[{"type":"unsend","timestamp":123,"webhookEventId":"event","unsend":{}}]`), http.StatusBadRequest},
		{"malformed message", receiverEnvelope(`[{"type":"message","timestamp":123,"webhookEventId":"event","message":[]}]`), http.StatusBadRequest},
		{"null event", receiverEnvelope(`[null]`), http.StatusBadRequest},
		{"atomic invalid batch", receiverEnvelope("[" + message + `,{"type":"message","timestamp":123,"webhookEventId":"invalid-last"}]`), http.StatusBadRequest},
		{"oversized", strings.Repeat(" ", 1<<20+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/2000000000", strings.NewReader(tc.body))
			request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(tc.body)))
			response := httptest.NewRecorder()
			receiver.Receive(response, request, nil)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}
	badSignature := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/2000000000", strings.NewReader(receiverEnvelope("[]")))
	response := httptest.NewRecorder()
	receiver.Receive(response, badSignature, nil)
	checks.Equal(http.StatusUnauthorized, response.Code)
	checks.Equal(uint64(1), receiver.InvalidSignatures("2000000000"))
	rows, err := st.DueLineEventsContext(t.Context(), source.ID, time.Now(), 10)
	must.NoError(err)
	checks.Empty(rows)
	checks.Zero(wakeCalls)
}

func TestReceiverPersistsBatchTargetsDedupesAndPreservesUnknownShapes(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	must.NoError(st.SetLineBotContext(t.Context(), source.ID, receiverBot))
	wakeCalls := 0
	receiver := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) { wakeCalls++ })
	message := `{"type":"message","webhookEventId":"base-event","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}`
	edited := `{"type":"messageEdited","webhookEventId":"edit-event","timestamp":124,"source":{"type":"group","groupId":"C00000000000000000000000000000003"},"message":{"id":"123","type":"text","text":"edited"}}`
	unsend := `{"type":"unsend","webhookEventId":"unsend-event","timestamp":125,"unsend":{"messageId":"123"}}`
	future := `{"type":"futureEvent","webhookEventId":"future-event","timestamp":126,"message":[{"native":"preserve"}],"joined":"future-shape","source":{"type":"future","opaque":"preserve"}}`
	body := receiverEnvelope("[" + strings.Join([]string{message, edited, unsend, future}, ",") + "]")
	for range 2 {
		response := serveSignedReceiver(t, receiver, body)
		must.Equal(http.StatusOK, response.Code, response.Body.String())
	}
	rows, err := st.DueLineEventsContext(t.Context(), source.ID, time.Now(), 10)
	must.NoError(err)
	must.Len(rows, 4)
	for index, row := range rows {
		checks.Equal(source.ID, row.SourceID)
		wantTarget := "123"
		if index == 3 {
			wantTarget = ""
		}
		checks.Equal(wantTarget, row.TargetMessageID)
	}
	checks.Contains(rows[3].RawJSON, `"joined":"future-shape"`)
	checks.Contains(rows[3].RawJSON, `"opaque":"preserve"`)
	checks.Equal(2, wakeCalls)
}

type receiverBlockingBody struct {
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *receiverBlockingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.started <- struct{}{} })
	<-b.release
	return 0, io.EOF
}
func (*receiverBlockingBody) Close() error { return nil }

func TestReceiverBoundedIngress(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	require.NoError(t, err)
	require.NoError(t, st.SetLineBotContext(t.Context(), source.ID, receiverBot))
	receiver := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, nil)
	t.Run("unsigned body reads", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		const readLimit = 16
		started := make(chan struct{}, readLimit)
		release := make(chan struct{})
		responses := make(chan int, readLimit)
		var requests sync.WaitGroup
		unblock := sync.OnceFunc(func() { close(release); requests.Wait() })
		t.Cleanup(unblock)
		for range readLimit {
			requests.Go(func() {
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/2000000000", nil)
				request.Body = io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat("x", 4096)), &receiverBlockingBody{started: started, release: release}))
				response := httptest.NewRecorder()
				receiver.Receive(response, request, nil)
				responses <- response.Code
			})
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				must.FailNow("timed out waiting for unsigned body reads")
			}
		}

		overflowRead := make(chan struct{}, 1)
		overflowDone := make(chan int, 1)
		requests.Go(func() {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/2000000000", nil)
			request.Body = &receiverBlockingBody{started: overflowRead, release: release}
			response := httptest.NewRecorder()
			receiver.Receive(response, request, nil)
			overflowDone <- response.Code
		})
		select {
		case status := <-overflowDone:
			checks.Equal(http.StatusServiceUnavailable, status)
		case <-overflowRead:
			checks.Fail("overflow unsigned request read its body before admission")
		case <-time.After(5 * time.Second):
			must.FailNow("overflow unsigned request neither completed nor started reading")
		}
		unblock()
		for range readLimit {
			checks.Equal(http.StatusUnauthorized, <-responses)
		}
		checks.Equal(http.StatusOK, serveSignedReceiver(t, receiver, receiverEnvelope("[]")).Code, "finished unsigned reads release their capacity")
	})
	t.Run("committed deliveries", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		responses := make(chan int, 16)
		var requests sync.WaitGroup
		var releaseCommit func()
		unblock := sync.OnceFunc(func() {
			defer requests.Wait()
			if releaseCommit != nil {
				releaseCommit()
			}
		})
		t.Cleanup(unblock)
		var pendingCommits func() bool
		if st.IsPostgreSQL() {
			st.DB().SetMaxOpenConns(18) // blocker, sixteen deliveries, and the lock observer
			lock, err := st.DB().BeginTx(t.Context(), nil)
			must.NoError(err)
			releaseCommit = func() { must.NoError(lock.Rollback()) }
			_, err = lock.ExecContext(t.Context(), `LOCK TABLE line_webhook_events IN ACCESS EXCLUSIVE MODE`)
			must.NoError(err)
			pendingCommits = func() bool {
				var waiting int
				err := st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting)
				return err == nil && waiting == 16
			}
		} else {
			st.DB().SetMaxOpenConns(1)
			started := make(chan struct{}, 16)
			release := make(chan struct{})
			releaseCommit = func() { close(release) }
			conn, err := st.DB().Conn(t.Context())
			must.NoError(err)
			err = conn.Raw(func(driverConn any) error {
				sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
				if !ok {
					return fmt.Errorf("expected SQLite connection, got %T", driverConn)
				}
				return sqliteConn.RegisterFunc("wait_for_line_commit", func() int {
					started <- struct{}{}
					<-release
					return 0
				}, false)
			})
			closeErr := conn.Close()
			must.NoError(err)
			must.NoError(closeErr)
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER wait_for_line_commit BEFORE INSERT ON line_webhook_events BEGIN SELECT wait_for_line_commit(); END`)
			must.NoError(err)
			waiting := st.DB().Stats().WaitCount
			pendingCommits = func() bool {
				return len(started) > 0 && st.DB().Stats().WaitCount >= waiting+15
			}
		}
		for index := range 16 {
			requests.Go(func() {
				eventID := fmt.Sprintf("authenticated-%d", index)
				body := receiverEnvelope(fmt.Sprintf(`[{"type":"future","webhookEventId":%q,"timestamp":123}]`, eventID))
				responses <- serveSignedReceiver(t, receiver, body).Code
			})
		}
		must.Eventually(pendingCommits, 5*time.Second, 10*time.Millisecond, "authenticated deliveries must wait for inbox commits")
		blocked := receiverEnvelope(`[{"type":"future","webhookEventId":"overflow","timestamp":123}]`)
		overflowDone := make(chan int, 1)
		requests.Go(func() { overflowDone <- serveSignedReceiver(t, receiver, blocked).Code })
		select {
		case status := <-overflowDone:
			checks.Equal(http.StatusServiceUnavailable, status)
		case <-time.After(5 * time.Second):
			must.FailNow("overflow delivery waited for inbox persistence instead of admission rejection")
		}
		unblock()
		for range 16 {
			checks.Equal(http.StatusOK, <-responses)
		}
	})
}

func TestReceiverChannelBinding(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	second, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	secondBot := "U00000000000000000000000000000003"
	must.NoError(st.SetLineBotContext(t.Context(), first.ID, receiverBot))
	must.NoError(st.SetLineBotContext(t.Context(), second.ID, secondBot))
	a, b := receiverChannel(first.ID), receiverChannel(second.ID)
	b.ChannelID = "2000000001"
	b.BotUserID = secondBot
	b.Secret = "second-synthetic-secret"
	receiver := line.NewReceiver(st, []line.ReceiverChannel{b, a}, nil)
	unknown := httptest.NewRecorder()
	receiver.Receive(unknown, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/2000000002", nil), nil)
	checks.Equal(http.StatusNotFound, unknown.Code)
	checks.Empty(unknown.Body.String())
	body := func(bot string) string {
		return `{"destination":"` + bot + `","events":[{"type":"future","webhookEventId":"same-event","timestamp":123,"source_id":` + "999" + `}]}`
	}
	send := func(channel, bot, secret string) int {
		raw := body(bot)
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/line/"+channel, strings.NewReader(raw))
		request.Header.Set("X-Line-Signature", lineapi.Sign(secret, []byte(raw)))
		response := httptest.NewRecorder()
		receiver.Receive(response, request, nil)
		return response.Code
	}
	checks.Equal(401, send(b.ChannelID, b.BotUserID, a.Secret))
	checks.Equal(401, send(b.ChannelID, a.BotUserID, b.Secret))
	for _, channel := range []line.ReceiverChannel{a, b} {
		checks.Equal(200, send(channel.ChannelID, channel.BotUserID, channel.Secret))
		rows, err := st.DueLineEventsContext(t.Context(), channel.SourceID, time.Now(), 10)
		must.NoError(err)
		must.Len(rows, 1)
		checks.Equal(channel.SourceID, rows[0].SourceID)
	}
}

func TestReceiverStorageFailuresDoNotWakeOrLeavePendingRows(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	must.NoError(st.SetLineBotContext(t.Context(), source.ID, receiverBot))
	wakeCalls := 0
	receiver := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) { wakeCalls++ })
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE OR REPLACE FUNCTION msgvault_test_reject_line_event() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
	IF NEW.webhook_event_id = 'reject-second' THEN RAISE EXCEPTION 'synthetic failure'; END IF;
	RETURN NEW;
END; $$`)
		must.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER line_receiver_fail_second BEFORE INSERT ON line_webhook_events
FOR EACH ROW EXECUTE FUNCTION msgvault_test_reject_line_event()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER line_receiver_fail_second BEFORE INSERT ON line_webhook_events
 WHEN NEW.webhook_event_id='reject-second' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
	}
	must.NoError(err)
	first := `{"type":"message","webhookEventId":"accepted-first","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"text","text":"synthetic"}}`
	second := `{"type":"message","webhookEventId":"reject-second","timestamp":124,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"2","type":"text","text":"synthetic"}}`
	response := serveSignedReceiver(t, receiver, receiverEnvelope("["+first+","+second+"]"))
	checks.Equal(http.StatusServiceUnavailable, response.Code)
	rows, err := st.DueLineEventsContext(t.Context(), source.ID, time.Now(), 10)
	must.NoError(err)
	checks.Empty(rows)
	checks.Zero(wakeCalls)
}
