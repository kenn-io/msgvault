package line_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

const receiverBot = "U00000000000000000000000000000001"

func receiverChannel(sourceID int64) line.ReceiverChannel {
	return line.ReceiverChannel{ChannelID: "2000000000", SourceID: sourceID, BotUserID: receiverBot, SecretEnv: "EXAMPLE_LINE_SECRET", Secret: "synthetic-secret"}
}

func TestReceiverConfigurationAndHandles(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	channel := receiverChannel(1)
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{channel}, nil)
	must.NoError(err)
	checks.True(receiver.Handles(httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000000", nil)))
	checks.True(receiver.Handles(httptest.NewRequest(http.MethodPost, "/webhooks/line/unknown", nil)))
	for _, target := range []string{"/webhooks/line/", "/webhooks/line/2000000000/", "/webhooks/line/2000000000/extra", "/webhooks/line/%32%30%30%30%30%30%30%30%30%30", "/webhooks/line/2000000000%2Fextra", "/webhooks/line/2000000000%252Fextra", "/api/v1/webhooks/line/2000000000"} {
		checks.False(receiver.Handles(httptest.NewRequest(http.MethodPost, target, nil)), target)
	}
	for _, method := range []string{"GET", "HEAD", "PUT", "OPTIONS"} {
		checks.False(receiver.Handles(httptest.NewRequest(method, "/webhooks/line/2000000000", nil)))
	}
	empty, err := line.NewReceiver(st, nil, nil)
	must.NoError(err)
	checks.False(empty.Handles(httptest.NewRequest(http.MethodPost, "/webhooks/line/unknown", nil)))
	for _, mutate := range []func(*line.ReceiverChannel){
		func(c *line.ReceiverChannel) { c.ChannelID = "0" }, func(c *line.ReceiverChannel) { c.ChannelID = "01" }, func(c *line.ReceiverChannel) { c.ChannelID = " 1 " }, func(c *line.ReceiverChannel) { c.ChannelID = "18446744073709551616" },
		func(c *line.ReceiverChannel) { c.SourceID = 0 }, func(c *line.ReceiverChannel) { c.BotUserID = "" }, func(c *line.ReceiverChannel) { c.Secret = "" },
	} {
		invalid := channel
		mutate(&invalid)
		_, err := line.NewReceiver(st, []line.ReceiverChannel{invalid}, nil)
		must.Error(err)
		checks.NotContains(err.Error(), "synthetic-secret")
	}
	_, err = line.NewReceiver(st, []line.ReceiverChannel{channel, channel}, nil)
	must.Error(err)
	channels := []line.ReceiverChannel{channel, receiverChannel(2)}
	channels[1].ChannelID = "2000000001"
	receiver, err = line.NewReceiver(st, channels, nil)
	must.NoError(err)
	snapshot := receiver.ActiveChannels()
	must.Len(snapshot, 2)
	checks.Equal("2000000000", snapshot[0].ChannelID)
	snapshot[0].ChannelID = "mutated"
	checks.Equal("2000000000", receiver.ActiveChannels()[0].ChannelID)
	encoded, err := json.Marshal(receiver.ActiveChannels())
	must.NoError(err)
	checks.NotContains(string(encoded), "synthetic-secret")
	checks.Contains(string(encoded), "EXAMPLE_LINE_SECRET")
}

func TestReceiverWakeAfterCommit(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	must.NoError(st.SetLineBotContext(context.Background(), source.ID, receiverBot))
	response := httptest.NewRecorder()
	wakeCalls := 0
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(channelID string) {
		wakeCalls++
		checks.Equal("2000000000", channelID)
		rows, err := st.DueLineEventsContext(context.Background(), source.ID, time.Now(), 10)
		must.NoError(err)
		must.Len(rows, 1)
		checks.Equal("event-1", rows[0].WebhookEventID)
	})
	must.NoError(err)
	body := `{"destination":"` + receiverBot + `","events":[{"type":"message","webhookEventId":"event-1","timestamp":123,"source":{"type":"user"},"message":{"id":"123","type":"text","text":"synthetic"}}]}`
	request := httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000000", strings.NewReader(body))
	request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(body)))
	receiver.ServeHTTP(response, request)
	checks.Equal(200, response.Code, response.Body.String())
	checks.Equal(1, wakeCalls)
}

func receiverEnvelope(events string) string {
	return `{"destination":"` + receiverBot + `","events":` + events + `}`
}

func serveSignedReceiver(receiver *line.Receiver, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000000", strings.NewReader(body))
	request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(body)))
	receiver.ServeHTTP(response, request)
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
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) { wakeCalls++ })
	must.NoError(err)
	message := `{"type":"message","webhookEventId":"valid-first","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}`
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"empty events", receiverEnvelope("[]"), http.StatusOK},
		{"missing events", `{"destination":"` + receiverBot + `"}`, http.StatusBadRequest},
		{"missing destination", `{"events":[]}`, http.StatusBadRequest},
		{"empty destination", `{"destination":"","events":[]}`, http.StatusBadRequest},
		{"null events", receiverEnvelope("null"), http.StatusBadRequest},
		{"object events", receiverEnvelope("{}"), http.StatusBadRequest},
		{"wrong destination", `{"destination":"other","events":[]}`, http.StatusUnauthorized},
		{"bad JSON", `{`, http.StatusBadRequest},
		{"trailing JSON", receiverEnvelope("[]") + `{}`, http.StatusBadRequest},
		{"missing id", receiverEnvelope(`[{"type":"future","timestamp":123}]`), http.StatusBadRequest},
		{"reserved id", receiverEnvelope(`[{"type":"future","timestamp":123,"webhookEventId":"_internal"}]`), http.StatusBadRequest},
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
			request := httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000000", strings.NewReader(tc.body))
			request.Header.Set("X-Line-Signature", lineapi.Sign("synthetic-secret", []byte(tc.body)))
			response := httptest.NewRecorder()
			receiver.ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}
	badSignature := httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000000", strings.NewReader(receiverEnvelope("[]")))
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, badSignature)
	checks.Equal(http.StatusUnauthorized, response.Code)
	checks.Equal(uint64(1), receiver.InvalidSignatures("2000000000"))
	rows, err := st.DueLineEventsContext(context.Background(), source.ID, time.Now(), 10)
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
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) { wakeCalls++ })
	must.NoError(err)
	message := `{"type":"message","webhookEventId":"base-event","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}`
	edited := `{"type":"messageEdited","webhookEventId":"edit-event","timestamp":124,"source":{"type":"group","groupId":"C00000000000000000000000000000003"},"message":{"id":"123","type":"text","text":"edited"}}`
	unsend := `{"type":"unsend","webhookEventId":"unsend-event","timestamp":125,"unsend":{"messageId":"123"}}`
	future := `{"type":"futureEvent","webhookEventId":"future-event","timestamp":126,"message":[{"native":"preserve"}],"joined":"future-shape","source":{"type":"future","opaque":"preserve"}}`
	body := receiverEnvelope("[" + strings.Join([]string{message, edited, unsend, future}, ",") + "]")
	for range 2 {
		response := serveSignedReceiver(receiver, body)
		must.Equal(http.StatusOK, response.Code, response.Body.String())
	}
	rows, err := st.DueLineEventsContext(context.Background(), source.ID, time.Now(), 10)
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

func TestReceiverUnsignedBodyReadsAreBounded(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(1)}, nil)
	must.NoError(err)

	const readLimit = 16
	started := make(chan struct{}, readLimit)
	release := make(chan struct{})
	responses := make(chan int, readLimit)
	var requests sync.WaitGroup
	unblock := sync.OnceFunc(func() { close(release); requests.Wait() })
	t.Cleanup(unblock)
	for range readLimit {
		requests.Go(func() {
			request := httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000000", nil)
			// Fill a prefix before blocking, so each admitted reader retains bytes.
			request.Body = io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat("x", 4096)), &receiverBlockingBody{started: started, release: release}))
			response := httptest.NewRecorder()
			receiver.ServeHTTP(response, request)
			responses <- response.Code
		})
	}
	for range readLimit {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			must.FailNow("timed out waiting for unsigned body reads")
		}
	}

	overflowRead := make(chan struct{}, 1)
	overflowDone := make(chan int, 1)
	requests.Go(func() {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000000", nil)
		request.Body = &receiverBlockingBody{started: overflowRead, release: release}
		response := httptest.NewRecorder()
		receiver.ServeHTTP(response, request)
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
	checks.Equal(http.StatusOK, serveSignedReceiver(receiver, receiverEnvelope("[]")).Code, "finished unsigned reads release their capacity")
}

func TestReceiverBoundedIngress(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	must.NoError(st.SetLineBotContext(t.Context(), source.ID, receiverBot))
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, nil)
	must.NoError(err)
	waitForEight := func(started <-chan struct{}, label string) {
		t.Helper()
		for range 8 {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				must.FailNow("timed out waiting for " + label)
			}
		}
	}
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	responses := make(chan int, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			request := httptest.NewRequest(http.MethodPost, "/webhooks/line/2000000000", nil)
			request.Body = &receiverBlockingBody{started: started, release: release}
			response := httptest.NewRecorder()
			receiver.ServeHTTP(response, request)
			responses <- response.Code
		})
	}
	waitForEight(started, "unauthenticated request bodies")
	body := receiverEnvelope(`[{"type":"message","webhookEventId":"signed-through","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"text","text":"synthetic"}}]`)
	response := serveSignedReceiver(receiver, body)
	checks.Equal(http.StatusOK, response.Code)
	close(release)
	wg.Wait()
	for range 8 {
		checks.Equal(http.StatusUnauthorized, <-responses)
	}

	startedAuthenticated := make(chan struct{}, 8)
	releaseAuthenticated := make(chan struct{})
	limited, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) {
		startedAuthenticated <- struct{}{}
		<-releaseAuthenticated
	})
	must.NoError(err)
	var authenticated sync.WaitGroup
	for index := range 8 {
		authenticated.Go(func() {
			eventID := fmt.Sprintf("authenticated-%d", index)
			body := receiverEnvelope(fmt.Sprintf(`[{"type":"future","webhookEventId":%q,"timestamp":123}]`, eventID))
			response := serveSignedReceiver(limited, body)
			responses <- response.Code
		})
	}
	waitForEight(startedAuthenticated, "authenticated webhook commits")
	blocked := receiverEnvelope(`[{"type":"future","webhookEventId":"overflow","timestamp":123}]`)
	response = serveSignedReceiver(limited, blocked)
	checks.Equal(http.StatusServiceUnavailable, response.Code)
	close(releaseAuthenticated)
	authenticated.Wait()
	for range 8 {
		checks.Equal(http.StatusOK, <-responses)
	}
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
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{b, a}, nil)
	must.NoError(err)
	body := func(bot string) string {
		return `{"destination":"` + bot + `","events":[{"type":"future","webhookEventId":"same-event","timestamp":123,"source_id":` + "999" + `}]}`
	}
	send := func(channel, bot, secret string) int {
		raw := body(bot)
		request := httptest.NewRequest(http.MethodPost, "/webhooks/line/"+channel, strings.NewReader(raw))
		request.Header.Set("X-Line-Signature", lineapi.Sign(secret, []byte(raw)))
		response := httptest.NewRecorder()
		receiver.ServeHTTP(response, request)
		return response.Code
	}
	checks.Equal(401, send(b.ChannelID, b.BotUserID, a.Secret))
	checks.Equal(401, send(b.ChannelID, a.BotUserID, b.Secret))
	for _, channel := range []line.ReceiverChannel{a, b} {
		checks.Equal(200, send(channel.ChannelID, channel.BotUserID, channel.Secret))
		rows, err := st.DueLineEventsContext(context.Background(), channel.SourceID, time.Now(), 10)
		must.NoError(err)
		must.Len(rows, 1)
		checks.Equal(channel.SourceID, rows[0].SourceID)
	}
}

func TestReceiverStorageFailuresDoNotWakeOrLeavePendingRows(t *testing.T) {
	t.Run("stale channel cannot enqueue into a replacement source", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)

		st := testutil.NewTestStore(t)
		_, err := st.GetOrCreateSource("line", "2000000000")
		must.NoError(err)
		replacement, err := st.GetOrCreateSource("line", "2000000001")
		must.NoError(err)
		must.NoError(st.SetLineBotContext(t.Context(), replacement.ID, receiverBot))
		stale := receiverChannel(replacement.ID)
		stale.ChannelID = "2000000000"
		wakeCalls := 0
		receiver, err := line.NewReceiver(st, []line.ReceiverChannel{stale}, func(string) { wakeCalls++ })
		must.NoError(err)
		body := receiverEnvelope(`[{"type":"message","webhookEventId":"stale-channel","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}]`)
		response := serveSignedReceiver(receiver, body)
		checks.Equal(http.StatusServiceUnavailable, response.Code)
		status, err := st.LineInboxStatusContext(t.Context(), replacement.ID)
		must.NoError(err)
		checks.Zero(status.Pending)
		checks.Zero(wakeCalls)
	})
	t.Run("registered bot changed after receiver mount", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)

		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource("line", "2000000000")
		must.NoError(err)
		must.NoError(st.SetLineBotContext(t.Context(), source.ID, "U00000000000000000000000000000003"))
		wakeCalls := 0
		receiver, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) { wakeCalls++ })
		must.NoError(err)
		body := receiverEnvelope(`[{"type":"message","webhookEventId":"stale-bot","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}]`)
		response := serveSignedReceiver(receiver, body)
		checks.Equal(http.StatusServiceUnavailable, response.Code)
		status, err := st.LineInboxStatusContext(t.Context(), source.ID)
		must.NoError(err)
		checks.Zero(status.Pending)
		checks.Zero(wakeCalls)
	})

	t.Run("retired source", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)

		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource("line", "2000000000")
		must.NoError(err)
		must.NoError(st.SetLineBotContext(t.Context(), source.ID, receiverBot))
		into, err := st.GetOrCreateSource("line", "2000000001")
		must.NoError(err)
		wakeCalls := 0
		receiver, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) { wakeCalls++ })
		must.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: into.ID})
		must.NoError(err)
		body := receiverEnvelope(`[{"type":"message","webhookEventId":"after-retirement","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}]`)
		response := serveSignedReceiver(receiver, body)
		checks.Equal(http.StatusServiceUnavailable, response.Code)
		status, err := st.LineInboxStatusContext(t.Context(), source.ID)
		must.NoError(err)
		checks.Zero(status.Pending)
		checks.Zero(wakeCalls)
	})
	t.Run("batch transaction rolls back", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)

		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource("line", "2000000000")
		must.NoError(err)
		must.NoError(st.SetLineBotContext(t.Context(), source.ID, receiverBot))
		wakeCalls := 0
		receiver, err := line.NewReceiver(st, []line.ReceiverChannel{receiverChannel(source.ID)}, func(string) { wakeCalls++ })
		must.NoError(err)
		if st.IsPostgreSQL() {
			_, err = st.DB().Exec(`CREATE OR REPLACE FUNCTION msgvault_test_reject_line_event() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
	IF NEW.webhook_event_id = 'reject-second' THEN RAISE EXCEPTION 'synthetic failure'; END IF;
	RETURN NEW;
END; $$`)
			must.NoError(err)
			_, err = st.DB().Exec(`CREATE TRIGGER line_receiver_fail_second BEFORE INSERT ON line_webhook_events
FOR EACH ROW EXECUTE FUNCTION msgvault_test_reject_line_event()`)
		} else {
			_, err = st.DB().Exec(`CREATE TRIGGER line_receiver_fail_second BEFORE INSERT ON line_webhook_events
 WHEN NEW.webhook_event_id='reject-second' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
		}
		must.NoError(err)
		first := `{"type":"message","webhookEventId":"accepted-first","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"text","text":"synthetic"}}`
		second := `{"type":"message","webhookEventId":"reject-second","timestamp":124,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"2","type":"text","text":"synthetic"}}`
		response := serveSignedReceiver(receiver, receiverEnvelope("["+first+","+second+"]"))
		checks.Equal(http.StatusServiceUnavailable, response.Code)
		rows, err := st.DueLineEventsContext(t.Context(), source.ID, time.Now(), 10)
		must.NoError(err)
		checks.Empty(rows)
		checks.Zero(wakeCalls)
	})
}
