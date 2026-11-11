package wakeups

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestQualifiesRequiresRecentMessageDateAndLiveRow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := now.Add(-365 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		row  store.ChangedMessage
		want bool
	}{
		{"undated", store.ChangedMessage{ContentChangedAt: now}, false},
		{"old sent overrides recent received", store.ChangedMessage{SentAt: &old, ReceivedAt: &recent}, false},
		{"recent received", store.ChangedMessage{ReceivedAt: &recent}, true},
		{"deleted", store.ChangedMessage{SentAt: &recent, DeletedAt: &recent}, false},
		{"removed from source", store.ChangedMessage{SentAt: &recent, DeletedFromSourceAt: &recent}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { Assert.Equal(t, tc.want, qualifies(tc.row, now, 72*time.Hour)) })
	}
}
func TestRetryDelayIsCapped(t *testing.T) {
	Assert.Equal(t, 30*time.Second, retryDelay(1))
	Assert.Equal(t, time.Minute, retryDelay(2))
	Assert.Equal(t, time.Hour, retryDelay(100))
}
func TestEngineRetriesFailedDeliveryAndAdvancesAfterSuccess(t *testing.T) {
	t.Run("same range", func(t *testing.T) {
		assert := Assert.New(t)
		require := Require.New(t)

		st := testutil.NewTestStore(t)
		ctx := t.Context()
		now := time.Now().UTC()
		start := now.Add(-time.Hour)
		id := seedWakeup(t, st, "wakeup@example.com", 0, 1, now)
		initializedWakeup(t, st, "inbox", start)
		attemptAt := now
		var bodies []payload
		status := http.StatusInternalServerError
		var decodeErr error
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var p payload
			decodeErr = json.NewDecoder(r.Body).Decode(&p)
			bodies = append(bodies, p)
			if len(bodies) == 1 {
				now = now.Add(5 * time.Second)
			}
			w.WriteHeader(status)
		}))
		defer server.Close()
		sub := Subscription{Name: "inbox", StateKey: store.WakeupStateKey("inbox"), URL: "https://example.com/hook", Secret: []byte(strings.Repeat("a", 32)), Filter: changefeed.Filter{IncomingOnly: true}, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(t, server)}
		engine := Engine{Store: &observedStore{Store: st, fixedBound: now.Add(-time.Second)}, Subscriptions: []Subscription{sub}, Now: func() time.Time { return now }}
		require.NoError(engine.RunPass(ctx))
		require.Len(bodies, 1)
		require.NoError(decodeErr)
		assert.Equal([]int64{id}, bodies[0].Matched.MessageIDs)
		state, _, err := st.LoadWakeupStateKey(ctx, store.WakeupStateKey("inbox"))
		require.NoError(err)
		assert.Equal(start, state.At)
		assert.Equal(1, state.Failures)
		assert.Equal(attemptAt.Add(30*time.Second), state.NextAttemptAt)
		require.NoError(engine.RunPass(ctx))
		assert.Len(bodies, 1)
		status = http.StatusNoContent
		now = now.Add(31 * time.Second)
		require.NoError(engine.RunPass(ctx))
		require.Len(bodies, 2)
		assert.Equal(bodies[0].CursorFrom, bodies[1].CursorFrom)
		assert.Equal(bodies[0].CursorTo, bodies[1].CursorTo)
		assert.Equal(bodies[0].DeliveryID, bodies[1].DeliveryID)
		state, _, err = st.LoadWakeupStateKey(ctx, store.WakeupStateKey("inbox"))
		require.NoError(err)
		assert.True(state.At.After(start))
		assert.Zero(state.Failures)
	})
	t.Run("range changed", func(t *testing.T) {
		assert := Assert.New(t)
		require := Require.New(t)

		st := testutil.NewTestStore(t)
		now := time.Now().UTC()
		first, err := st.EnsureParticipant("first@example.com", "First", "example.com")
		require.NoError(err)
		second, err := st.EnsureParticipant("second@example.com", "Second", "example.com")
		require.NoError(err)
		person, _, err := st.CreatePersonFromParticipant(first)
		require.NoError(err)
		incoming := true
		filter := FilterFromConfig(config.WakeupConfig{PersonID: &person.ID, IncomingOnly: &incoming})
		initializedWakeup(t, st, "person", now.Add(-time.Hour))
		firstID := seedWakeup(t, st, "first-source@example.com", first, 1, now)
		var deliveries []payload
		status := http.StatusInternalServerError
		var decodeErr error
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var p payload
			decodeErr = json.NewDecoder(r.Body).Decode(&p)
			deliveries = append(deliveries, p)
			w.WriteHeader(status)
		}))
		defer server.Close()
		observed := &observedStore{Store: st, fixedBound: now.Add(-30 * time.Second)}
		engine := Engine{Store: observed, Subscriptions: []Subscription{testSubscription(t, "person", server, filter)}, Now: func() time.Time { return now }}
		require.NoError(engine.RunPass(t.Context()))
		require.NoError(decodeErr)
		require.Len(deliveries, 1)
		assert.Equal([]int64{firstID}, deliveries[0].Matched.MessageIDs)
		_, err = st.LinkParticipants(first, second)
		require.NoError(err)
		secondID := seedWakeup(t, st, "second-source@example.com", second, 2, now)
		status = http.StatusNoContent
		observed.fixedBound = now.Add(-time.Second)
		now = now.Add(31 * time.Second)
		require.NoError(engine.RunPass(t.Context()))
		require.NoError(decodeErr)
		require.Len(deliveries, 2)
		assert.ElementsMatch([]int64{firstID, secondID}, deliveries[1].Matched.MessageIDs)
		assert.NotEqual(deliveries[0].CursorTo, deliveries[1].CursorTo)
		assert.NotEqual(deliveries[0].DeliveryID, deliveries[1].DeliveryID)
	})
}

func TestEngineTruncatesIDsWithoutLosingCount(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	var ids []int64
	for i := range 60 {
		ids = append(ids, seedWakeup(t, st, "bulk@example.com", 0, i, now))
	}
	initializedWakeup(t, st, "bulk", start)
	var got payload
	var decodeErr error
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeErr = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	engine := Engine{Store: st, Subscriptions: []Subscription{{Name: "bulk", StateKey: store.WakeupStateKey("bulk"), URL: "https://example.com/hook", Secret: []byte(strings.Repeat("a", 32)), Filter: changefeed.Filter{IncomingOnly: true}, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(t, server)}}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(ctx))
	require.NoError(decodeErr)
	assert.Equal(60, got.Matched.Count)
	assert.True(got.Matched.Truncated)
	assert.Equal(ids[10:], got.Matched.MessageIDs)
}

func TestNativeDeliveryDropsPermanentAndEventuallyAdvances(t *testing.T) {
	for _, status := range []int{http.StatusGone, http.StatusRequestEntityTooLarge, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			now := time.Now().UTC()
			start := now.Add(-time.Hour)
			src, err := st.GetOrCreateSource("gmail", "events@example.com")
			require.NoError(err)
			seedWakeup(t, st, "events@example.com", 0, 1, now)
			key := store.MCPEventsPositionKey("sub-x")
			ok, err := st.AdvanceWakeupStateKey(ctx, key, nil, store.WakeupState{At: start})
			require.NoError(err)
			require.True(ok)
			var bodies []map[string]any
			var ids []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				bodies = append(bodies, body)
				ids = append(ids, r.Header.Get("Webhook-Id"))
				assert.Equal("sub-x", r.Header.Get("X-Mcp-Subscription-Id"))
				w.WriteHeader(status)
			}))
			defer server.Close()
			sub := Subscription{Name: "sub-x", StateKey: key, Native: true, URL: "https://example.com/hook", Secret: []byte(strings.Repeat("a", 32)), Filter: changefeed.Filter{SourceIDs: []int64{src.ID}, IncomingOnly: true}, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(t, server)}
			nativeCalls := 0
			engine := Engine{Store: st, Native: func(context.Context) ([]Subscription, error) {
				nativeCalls++
				return []Subscription{sub}, nil
			}, Now: func() time.Time { return now }}
			attempts := 1
			if status == 500 {
				attempts = 5
			}
			for range attempts {
				require.NoError(engine.RunPass(ctx))
				now = now.Add(time.Hour)
			}
			assert.Equal(attempts, nativeCalls)
			require.Len(bodies, attempts)
			assert.Equal("msgvault.messages.changed", bodies[0]["name"])
			cursor, present := bodies[0]["cursor"]
			assert.True(present)
			assert.Nil(cursor)
			assert.Equal(ids[0], bodies[0]["eventId"])
			state, _, err := st.LoadWakeupStateKey(ctx, key)
			require.NoError(err)
			assert.True(state.At.After(start))
			assert.Zero(state.Failures)
			assert.NotEmpty(state.LastError)
			assert.NotNil(state.FailedSince)
		})
	}
}

func TestNativeInterruptedFinalAttempt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		wantHits int
	}{
		{"fifth reservation", 5, 1},
		{"sixth reservation", 6, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)
			st := testutil.NewTestStore(t)
			now := time.Now().UTC()
			start := now.Add(-time.Hour)
			seedWakeup(t, st, "exhausted@example.com", 0, 1, now)
			key := store.MCPEventsPositionKey("exhausted")
			ok, err := st.AdvanceWakeupStateKey(t.Context(), key, nil, store.WakeupState{At: start, Failures: tc.failures})
			require.NoError(err)
			require.True(ok)
			hits := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits++
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			sub := testSubscription(t, "exhausted", server, changefeed.Filter{IncomingOnly: true})
			sub.StateKey, sub.Native = key, true
			engine := Engine{Store: st, Subscriptions: []Subscription{sub}, Now: func() time.Time { return now }}
			require.NoError(engine.RunPass(t.Context()))
			assert.Equal(tc.wantHits, hits)
			state, _, err := st.LoadWakeupStateKey(t.Context(), key)
			require.NoError(err)
			assert.True(state.At.After(start))
			assert.Zero(state.Failures)
		})
	}
}

func TestNativeSuccessfulDeliveryClearsFailureStatus(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	src, err := st.GetOrCreateSource("gmail", "downtime@example.com")
	require.NoError(err)
	key := store.MCPEventsPositionKey("restart")
	ok, err := st.AdvanceWakeupStateKey(ctx, key, nil, store.WakeupState{At: start, LastError: "http_5xx", FailedSince: &start})
	require.NoError(err)
	require.True(ok)
	seedWakeup(t, st, "downtime@example.com", 0, 1, now)
	var bodies []map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sub := Subscription{Name: "restart", Native: true, StateKey: key, URL: "https://example.com/hook", Secret: []byte(strings.Repeat("a", 32)), Filter: changefeed.Filter{SourceIDs: []int64{src.ID}, IncomingOnly: true}, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(t, server)}
	engine := Engine{Store: st, Subscriptions: []Subscription{sub}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(ctx))
	require.Len(bodies, 1)
	assert.Equal("msgvault.messages.changed", bodies[0]["name"])
	state, _, err := st.LoadWakeupStateKey(ctx, key)
	require.NoError(err)
	assert.NotNil(state.LastDeliveryAt)
	assert.Empty(state.LastError)
	assert.Nil(state.FailedSince)
}
