package beeper

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func eventTestServer(t *testing.T, handle func(context.Context, *websocket.Conn)) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			_, _ = w.Write([]byte(`{"endpoints":{"ws_events":"/events"}}`))
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		handle(ctx, conn)
	}))
	t.Cleanup(s.Close)
	return s
}
func eventWrite(ctx context.Context, t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	assert.NoError(t, c.Write(ctx, websocket.MessageText, []byte(s)))
}
func eventSubscribe(ctx context.Context, t *testing.T, c *websocket.Conn) {
	t.Helper()
	eventWrite(ctx, t, c, `{"type":"ready","version":1}`)
	_, b, err := c.Read(ctx)
	if !assert.NoError(t, err) {
		return
	}
	var command struct {
		Type      string   `json:"type"`
		RequestID string   `json:"requestID"`
		ChatIDs   []string `json:"chatIDs"`
	}
	require.NoError(t, json.Unmarshal(b, &command))
	assert.Equal(t, "subscriptions.set", command.Type)
	assert.Equal(t, []string{"*"}, command.ChatIDs)
	eventWrite(ctx, t, c, fmt.Sprintf(`{"type":"subscriptions.updated","requestID":%q,"chatIDs":["*"]}`, command.RequestID))
}
func fastEventOptions() eventWatchOptions {
	o := defaultEventWatchOptions()
	o.minRetry = time.Millisecond
	o.maxRetry = 10 * time.Millisecond
	return o
}
func startTestWatch(t *testing.T, c *Client, wake func(), report func(error), o eventWatchOptions) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.watchEvents(ctx, wake, report, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			assert.Fail(t, "event watcher did not stop")
		}
	})
	return cancel
}
func TestEventsWaitsForMatchingAck(t *testing.T) {
	for _, ack := range []string{`{"type":"subscriptions.updated","requestID":"wrong","chatIDs":["*"]}`, `{"type":"subscriptions.updated","requestID":"msgvault","chatIDs":[]}`, `{"type":"error","requestID":"msgvault","message":"private peer text"}`} {
		t.Run(ack, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			var wakes atomic.Int32
			server := eventTestServer(t, func(ctx context.Context, c *websocket.Conn) {
				eventWrite(ctx, t, c, `{"type":"ready","version":1}`)
				_, _, err := c.Read(ctx)
				if err != nil {
					assertions.Fail("read subscription command", "%v", err)
					return
				}
				eventWrite(ctx, t, c, `{"type":"message.upserted","seq":1,"chatID":"chat-example","ids":["message-example"]}`)
				eventWrite(ctx, t, c, ack)
				_, _, _ = c.Read(ctx)
			})
			reported := make(chan error, 1)
			c := NewClient(server.URL, testToken, 100)
			o := fastEventOptions()
			o.minRetry = time.Hour
			o.maxRetry = time.Hour
			cancel := startTestWatch(t, c, func() { wakes.Add(1) }, func(err error) {
				select {
				case reported <- err:
				default:
				}
			}, o)
			select {
			case err := <-reported:
				requirements.Error(err)
				assertions.NotContains(err.Error(), "private peer text")
			case <-time.After(5 * time.Second):
				requirements.FailNow("missing protocol error")
			}
			cancel()
			assertions.EqualValues(0, wakes.Load())
		})
	}
}
func TestEventsIDOnlyAndUnknownFrames(t *testing.T) {
	sent := make(chan struct{})
	release := make(chan struct{})
	server := eventTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		eventSubscribe(ctx, t, c)
		for _, s := range []string{`{"type":"future.event","seq":1}`, `{"type":"message.upserted","seq":2,"chatID":"chat-example","ids":["one","two"]}`, `{"type":"message.deleted","seq":2,"chatID":"chat-example","ids":["one"]}`, `{"type":"chat.upserted","seq":9,"chatID":"chat-example","ids":["chat-example"]}`, `{"type":"chat.deleted","seq":10,"chatID":"chat-example","ids":["chat-example"]}`} {
			eventWrite(ctx, t, c, s)
		}
		close(sent)
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	var wakes atomic.Int32
	c := NewClient(server.URL, testToken, 100)
	cancel := startTestWatch(t, c, func() { wakes.Add(1) }, nil, fastEventOptions())
	<-sent
	assert.Eventually(t, func() bool { return wakes.Load() == 5 }, 5*time.Second, time.Millisecond)
	cancel()
	close(release)
}
func TestEventsUpgradeRejectsRedirect(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			_, _ = w.Write([]byte(`{"endpoints":{"ws_events":"/events"}}`))
			return
		}
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer server.Close()
	c := NewClient(server.URL, testToken, 100)
	reported := make(chan error, 1)
	o := fastEventOptions()
	o.minRetry = time.Hour
	o.maxRetry = time.Hour
	cancel := startTestWatch(t, c, func() { assert.Fail(t, "unsubscribed wake") }, func(err error) { reported <- err }, o)
	select {
	case err := <-reported:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no upgrade rejection")
	}
	cancel()
	assert.False(t, leaked.Load())
}
func TestEventsReloadsToken(t *testing.T) {
	headers := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	var calls atomic.Int32
	c := NewClient(server.URL, func(context.Context) (string, error) { return strconv.Itoa(int(calls.Add(1))), nil }, 100)
	cancel := startTestWatch(t, c, func() { assert.Fail(t, "failed dial wake") }, nil, fastEventOptions())
	for _, want := range []string{"Bearer 1", "Bearer 2"} {
		select {
		case got := <-headers:
			assert.Equal(t, want, got)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "no reconnect")
		}
	}
	cancel()
}
func TestEventsBackoffSurvivesFlapping(t *testing.T) {
	o := defaultEventWatchOptions()
	delay := o.minRetry
	for _, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second} {
		delay = nextEventRetry(delay, 0, o)
		assert.Equal(t, want, delay)
	}
	assert.Equal(t, time.Second, nextEventRetry(delay, time.Minute, o))
}
func TestEventsCancelSilentPeer(t *testing.T) {
	entered := make(chan struct{})
	server := eventTestServer(t, func(ctx context.Context, c *websocket.Conn) { close(entered); _, _, _ = c.Read(ctx) })
	c := NewClient(server.URL, testToken, 100)
	cancel := startTestWatch(t, c, func() { assert.Fail(t, "silent peer wake") }, nil, fastEventOptions())
	<-entered
	cancel()
}
func TestEventsSilentPeerReconnects(t *testing.T) {
	connected := make(chan struct{}, 10)
	server := eventTestServer(t, func(ctx context.Context, c *websocket.Conn) { connected <- struct{}{}; _, _, _ = c.Read(ctx) })
	c := NewClient(server.URL, testToken, 100)
	o := fastEventOptions()
	o.controlTimeout = time.Second
	cancel := startTestWatch(t, c, func() { assert.Fail(t, "unsubscribed wake") }, nil, o)
	for range 2 {
		select {
		case <-connected:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "no finite liveness")
		}
	}
	cancel()
}
func FuzzEventEnvelope(f *testing.F) {
	for _, s := range []string{`{"type":"message.upserted","seq":1,"chatID":"chat-example","ids":["one","two"]}`, `{"type":"message.deleted","seq":0,"chatID":"chat-example","ids":["one"]}`, `{"type":"future.event"}`, `null`, `[]`, `{"type":"message.upserted"}`, `{"type":"message.deleted","seq":-1,"chatID":"x","ids":["y"]}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := decodeEventEnvelope([]byte(s))
		if err != nil {
			return
		}
		if e.isHint() {
			assert.Contains(t, []string{"chat.upserted", "chat.deleted", "message.upserted", "message.deleted"}, e.Type)
			assert.NotNil(t, e.Seq)
			assert.NotEmpty(t, e.ChatID)
			assert.NotEmpty(t, e.IDs)
			for _, id := range e.IDs {
				assert.NotEmpty(t, id)
			}
		}
	})
}

func TestEventsCancelDiscoveryAndUpgrade(t *testing.T) {
	for _, phase := range []string{"discovery", "upgrade"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{})
			released := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "upgrade" && r.URL.Path == "/v1/info" {
					_, _ = w.Write([]byte(`{"endpoints":{"ws_events":"/events"}}`))
					return
				}
				close(entered)
				<-r.Context().Done()
				close(released)
			}))
			t.Cleanup(server.Close)
			c := NewClient(server.URL, testToken, 100)
			cancel := startTestWatch(t, c, func() { assert.Fail(t, "unsubscribed wake") }, nil, fastEventOptions())
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "request not started")
			}
			cancel()
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "request not canceled")
			}
		})
	}
}
func TestEventsCancelBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	reported := make(chan struct{}, 1)
	c := NewClient(server.URL, testToken, 100)
	o := fastEventOptions()
	o.minRetry = time.Hour
	o.maxRetry = time.Hour
	cancel := startTestWatch(t, c, func() { assert.Fail(t, "failed connection wake") }, func(error) { reported <- struct{}{} }, o)
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no backoff")
	}
	cancel()
}
func TestEventsCancelSubscribedIdle(t *testing.T) {
	subscribed := make(chan struct{})
	server := eventTestServer(t, func(ctx context.Context, c *websocket.Conn) { eventSubscribe(ctx, t, c); _, _, _ = c.Read(ctx) })
	c := NewClient(server.URL, testToken, 100)
	cancel := startTestWatch(t, c, func() {}, func(err error) {
		if err == nil {
			close(subscribed)
		}
	}, fastEventOptions())
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "not subscribed")
	}
	cancel()
}

func TestEventsSubscribedSilentPeerReconnects(t *testing.T) {
	subscribed := make(chan struct{}, 10)
	server := eventTestServer(t, func(ctx context.Context, c *websocket.Conn) { eventSubscribe(ctx, t, c); _, _, _ = c.Read(ctx) })
	c := NewClient(server.URL, testToken, 100)
	o := fastEventOptions()
	o.readTimeout = time.Second
	cancel := startTestWatch(t, c, func() {}, func(err error) {
		if err == nil {
			subscribed <- struct{}{}
		}
	}, o)
	for range 2 {
		select {
		case <-subscribed:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "subscribed silent connection never recovered")
		}
	}
	cancel()
}

func TestEventsHealthySessionResetsActualReconnectWait(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	observedHint := make(chan struct{})
	subscriptionAcknowledged := make(chan struct{}, 1)
	var attempts atomic.Int32
	const healthyThreshold = 200 * time.Millisecond
	const healthySessionHold = 500 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			if attempts.Add(1) <= 4 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"endpoints":{"ws_events":"/events"}}`))
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if !assertions.NoError(err) {
			return
		}
		defer func() { _ = c.CloseNow() }()
		eventSubscribe(r.Context(), t, c)
		eventWrite(r.Context(), t, c, `{"type":"message.upserted","seq":1,"chatID":"chat-example","ids":["message-example"]}`)
		select {
		case <-observedHint:
			// Keep the acknowledged session alive well past the threshold so
			// clock granularity cannot classify it as an unhealthy flap.
			time.Sleep(healthySessionHold)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	c := NewClient(server.URL, testToken, 100)
	o := defaultEventWatchOptions()
	o.maxRetry = 8 * time.Second
	o.healthyAfter = healthyThreshold
	var waits []time.Duration
	o.wait = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 5 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	wakes := 0
	done := make(chan error, 1)
	watcherFinished := false
	var watcherErr error
	t.Cleanup(func() {
		cancel()
		if watcherFinished {
			return
		}
		select {
		case err := <-done:
			assertions.ErrorIs(err, context.Canceled)
		case <-time.After(5 * time.Second):
			assertions.Fail("event watcher did not stop")
		}
	})
	go func() {
		done <- c.watchEvents(ctx, func() {
			wakes++
			if wakes == 2 {
				close(observedHint)
			}
		}, func(err error) {
			if err == nil {
				select {
				case subscriptionAcknowledged <- struct{}{}:
				default:
				}
			}
		}, o)
	}()
	waitFor := func(observed <-chan struct{}, what string) {
		t.Helper()
		if watcherFinished {
			select {
			case <-observed:
				return
			default:
				requirements.FailNow("watcher stopped before "+what, "%v", watcherErr)
			}
		}
		select {
		case <-observed:
		case watcherErr = <-done:
			watcherFinished = true
			select {
			case <-observed:
			default:
				requirements.FailNow("watcher stopped before "+what, "%v", watcherErr)
			}
		case <-time.After(5 * time.Second):
			cancel()
			requirements.FailNow(what + " was not observed")
		}
	}
	waitFor(subscriptionAcknowledged, "subscription acknowledgement")
	waitFor(observedHint, "domain hint")
	if !watcherFinished {
		watcherErr = <-done
		watcherFinished = true
	}
	requirements.ErrorIs(watcherErr, context.Canceled)
	requirements.Len(waits, 5)
	assertions.GreaterOrEqual(waits[3], 4*time.Second, "unestablished failures must reach capped backoff")
	assertions.GreaterOrEqual(waits[4], 500*time.Millisecond)
	assertions.Less(waits[4], time.Second, "a healthy session resets the next timer, not the following retry")
}

func TestEventsRejectsWrongReadyVersion(t *testing.T) {
	for _, frame := range []string{`{"type":"ready","version":0}`, `{"type":"ready","version":2}`, `{"type":"ready","version":null}`, `{"type":"ready","version":"1"}`, `{"type":"future.control"}`} {
		t.Run(frame, func(t *testing.T) {
			server := eventTestServer(t, func(ctx context.Context, c *websocket.Conn) {
				eventWrite(ctx, t, c, frame)
				_, b, err := c.Read(ctx)
				if err != nil {
					return
				}
				var command struct {
					Type      string   `json:"type"`
					RequestID string   `json:"requestID"`
					ChatIDs   []string `json:"chatIDs"`
				}
				require.NoError(t, json.Unmarshal(b, &command))
				require.Equal(t, "subscriptions.set", command.Type)
				require.Equal(t, "msgvault", command.RequestID)
				require.Equal(t, []string{"*"}, command.ChatIDs)
				eventWrite(ctx, t, c, fmt.Sprintf(`{"type":"subscriptions.updated","requestID":%q,"chatIDs":["*"]}`, command.RequestID))
				eventWrite(ctx, t, c, `{"type":"message.upserted","seq":1,"chatID":"chat-example","ids":["message-example"]}`)
				<-ctx.Done()
			})
			c := NewClient(server.URL, testToken, 100)
			o := fastEventOptions()
			o.minRetry = time.Hour
			o.maxRetry = time.Hour
			reported := make(chan error, 1)
			cancel := startTestWatch(t, c, func() { assert.Fail(t, "invalid handshake wake") }, func(err error) {
				select {
				case reported <- err:
				default:
				}
			}, o)
			select {
			case err := <-reported:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "invalid ready not rejected")
			}
			cancel()
		})
	}
}
func TestEventsCancelAckWait(t *testing.T) {
	waiting := make(chan struct{})
	server := eventTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		eventWrite(ctx, t, c, `{"type":"ready","version":1}`)
		_, _, err := c.Read(ctx)
		if err != nil {
			assert.Fail(t, "read subscription command", "%v", err)
			return
		}
		close(waiting)
		_, _, _ = c.Read(ctx)
	})
	c := NewClient(server.URL, testToken, 100)
	cancel := startTestWatch(t, c, func() { assert.Fail(t, "unacknowledged wake") }, nil, fastEventOptions())
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no subscription command")
	}
	cancel()
}
