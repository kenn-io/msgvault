package wakeups

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/changefeed"
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
		src, err := st.GetOrCreateSource("gmail", "wakeup@example.com")
		require.NoError(err)
		conv, err := st.EnsureConversationWithType(src.ID, "wakeup-conversation", "email_thread", "Wakeup")
		require.NoError(err)
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, SourceMessageID: "message-1", ConversationID: conv, MessageType: "email", SentAt: sql.NullTime{Time: now, Valid: true}})
		require.NoError(err)
		stamp := now.Add(-time.Minute)
		var value any = stamp
		if !st.IsPostgreSQL() {
			value = stamp.Format(store.SQLiteTimestampLayout)
		}
		_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE messages SET content_changed_at=? WHERE id=?`), value, id)
		require.NoError(err)
		ok, err := st.AdvanceWakeupState(ctx, "inbox", nil, store.WakeupState{At: start})
		require.NoError(err)
		require.True(ok)
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
		assert.Equal(now.Add(30*time.Second), state.NextAttemptAt)
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
	t.Run("empty rescan", func(t *testing.T) {
		assert := Assert.New(t)
		require := Require.New(t)

		st := testutil.NewTestStore(t)
		now := time.Now().UTC()
		start := now.Add(-time.Hour)
		seedWakeup(t, st, "empty-retry@example.com", 0, 1, now)
		initializedWakeup(t, st, "empty-retry", start)
		var bodies []payload
		status := http.StatusInternalServerError
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var p payload
			if err := json.NewDecoder(r.Body).Decode(&p); err == nil {
				bodies = append(bodies, p)
			}
			w.WriteHeader(status)
		}))
		defer server.Close()
		engine := Engine{Store: st, Subscriptions: []Subscription{testSubscription(t, "empty-retry", server, changefeed.Filter{IncomingOnly: true})}, Now: func() time.Time { return now }}
		require.NoError(engine.RunPass(t.Context()))
		require.Len(bodies, 1)
		now = now.Add(73 * time.Hour)
		require.NoError(engine.RunPass(t.Context()))
		require.Len(bodies, 2)
		assert.Equal(bodies[0].CursorFrom, bodies[1].CursorFrom)
		assert.Zero(bodies[1].Matched.Count)
		assert.Empty(bodies[1].Matched.MessageIDs)
		state, _, err := st.LoadWakeupState(t.Context(), "empty-retry")
		require.NoError(err)
		assert.Equal(start, state.At)
		status = http.StatusNoContent
		now = now.Add(time.Minute)
		require.NoError(engine.RunPass(t.Context()))
		require.Len(bodies, 3)
		state, _, err = st.LoadWakeupState(t.Context(), "empty-retry")
		require.NoError(err)
		assert.True(state.At.After(start))
		assert.Zero(state.Failures)
	})
}

func TestEngineTruncatesIDsWithoutLosingCount(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	stamp := now.Add(-time.Minute)
	src, err := st.GetOrCreateSource("gmail", "bulk@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "bulk-conversation", "email_thread", "Bulk")
	require.NoError(err)
	var ids []int64
	for i := range 60 {
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, SourceMessageID: fmt.Sprintf("bulk-%d", i), ConversationID: conv, MessageType: "email", SentAt: sql.NullTime{Time: now, Valid: true}})
		require.NoError(err)
		ids = append(ids, id)
		var value any = stamp
		if !st.IsPostgreSQL() {
			value = stamp.Format(store.SQLiteTimestampLayout)
		}
		_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE messages SET content_changed_at=? WHERE id=?`), value, id)
		require.NoError(err)
	}
	ok, err := st.AdvanceWakeupState(ctx, "bulk", nil, store.WakeupState{At: start})
	require.NoError(err)
	require.True(ok)
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
