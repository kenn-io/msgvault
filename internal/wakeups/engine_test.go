package wakeups

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		sub := Subscription{Name: "inbox", URL: "https://example.com/hook", Secret: []byte(strings.Repeat("a", 32)), Filter: changefeed.Filter{IncomingOnly: true}, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(t, server)}
		engine := Engine{Store: &observedStore{Store: st, fixedBound: now.Add(-time.Second)}, Subscriptions: []Subscription{sub}, Now: func() time.Time { return now }}
		require.NoError(engine.RunPass(ctx))
		require.Len(bodies, 1)
		require.NoError(decodeErr)
		assert.Equal([]int64{id}, bodies[0].Matched.MessageIDs)
		state, _, err := st.LoadWakeupState(ctx, "inbox")
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
		state, _, err = st.LoadWakeupState(ctx, "inbox")
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
	engine := Engine{Store: st, Subscriptions: []Subscription{{Name: "bulk", URL: "https://example.com/hook", Secret: []byte(strings.Repeat("a", 32)), Filter: changefeed.Filter{IncomingOnly: true}, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(t, server)}}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(ctx))
	require.NoError(decodeErr)
	assert.Equal(60, got.Matched.Count)
	assert.True(got.Matched.Truncated)
	assert.Equal(ids[10:], got.Matched.MessageIDs)
}
