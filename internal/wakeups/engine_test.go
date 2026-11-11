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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, qualifies(tc.row, now, 72*time.Hour)) })
	}
}
func TestRetryDelayIsCapped(t *testing.T) {
	assert.Equal(t, 30*time.Second, retryDelay(1))
	assert.Equal(t, time.Minute, retryDelay(2))
	assert.Equal(t, time.Hour, retryDelay(100))
}
func TestEngineRetriesFailedDeliveryAndAdvancesAfterSuccess(t *testing.T) {
	t.Run("same range", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		ctx := t.Context()
		now := time.Now().UTC()
		start := now.Add(-time.Hour)
		src, err := st.GetOrCreateSource("gmail", "wakeup@example.com")
		require.NoError(t, err)
		conv, err := st.EnsureConversationWithType(src.ID, "wakeup-conversation", "email_thread", "Wakeup")
		require.NoError(t, err)
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, SourceMessageID: "message-1", ConversationID: conv, MessageType: "email", SentAt: sql.NullTime{Time: now, Valid: true}})
		require.NoError(t, err)
		stamp := now.Add(-time.Minute)
		var value any = stamp
		if !st.IsPostgreSQL() {
			value = stamp.Format(store.SQLiteTimestampLayout)
		}
		_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE messages SET content_changed_at=? WHERE id=?`), value, id)
		require.NoError(t, err)
		ok, err := st.AdvanceWakeupState(ctx, "inbox", nil, store.WakeupState{At: start})
		require.NoError(t, err)
		require.True(t, ok)
		var bodies []payload
		status := http.StatusInternalServerError
		var decodeErr error
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var p payload
			decodeErr = json.NewDecoder(r.Body).Decode(&p)
			bodies = append(bodies, p)
			w.WriteHeader(status)
		}))
		defer server.Close()
		sub := Subscription{Name: "inbox", URL: "https://example.com/hook", Secret: []byte(strings.Repeat("a", 32)), Filter: changefeed.Filter{IncomingOnly: true}, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(server)}
		engine := Engine{Store: &observedStore{Store: st, fixedBound: now.Add(-time.Second)}, Subscriptions: []Subscription{sub}, Now: func() time.Time { return now }}
		require.NoError(t, engine.RunPass(ctx))
		require.Len(t, bodies, 1)
		require.NoError(t, decodeErr)
		assert.Equal(t, []int64{id}, bodies[0].Matched.MessageIDs)
		state, _, err := st.LoadWakeupState(ctx, "inbox")
		require.NoError(t, err)
		assert.Equal(t, start, state.At)
		assert.Equal(t, 1, state.Failures)
		require.NoError(t, engine.RunPass(ctx))
		assert.Len(t, bodies, 1)
		status = http.StatusNoContent
		now = now.Add(31 * time.Second)
		require.NoError(t, engine.RunPass(ctx))
		require.Len(t, bodies, 2)
		assert.Equal(t, bodies[0].CursorFrom, bodies[1].CursorFrom)
		assert.Equal(t, bodies[0].CursorTo, bodies[1].CursorTo)
		assert.Equal(t, bodies[0].DeliveryID, bodies[1].DeliveryID)
		state, _, err = st.LoadWakeupState(ctx, "inbox")
		require.NoError(t, err)
		assert.True(t, state.At.After(start))
		assert.Zero(t, state.Failures)
	})
	t.Run("empty rescan", func(t *testing.T) {
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
		engine := Engine{Store: st, Subscriptions: []Subscription{testSubscription("empty-retry", server, changefeed.Filter{IncomingOnly: true})}, Now: func() time.Time { return now }}
		require.NoError(t, engine.RunPass(t.Context()))
		require.Len(t, bodies, 1)
		now = now.Add(73 * time.Hour)
		require.NoError(t, engine.RunPass(t.Context()))
		require.Len(t, bodies, 2)
		assert.Equal(t, bodies[0].CursorFrom, bodies[1].CursorFrom)
		assert.Zero(t, bodies[1].Matched.Count)
		assert.Empty(t, bodies[1].Matched.MessageIDs)
		state, _, err := st.LoadWakeupState(t.Context(), "empty-retry")
		require.NoError(t, err)
		assert.Equal(t, start, state.At)
		status = http.StatusNoContent
		now = now.Add(time.Minute)
		require.NoError(t, engine.RunPass(t.Context()))
		require.Len(t, bodies, 3)
		state, _, err = st.LoadWakeupState(t.Context(), "empty-retry")
		require.NoError(t, err)
		assert.True(t, state.At.After(start))
		assert.Zero(t, state.Failures)
	})
}

func TestEngineTruncatesIDsWithoutLosingCount(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	stamp := now.Add(-time.Minute)
	src, err := st.GetOrCreateSource("gmail", "bulk@example.com")
	require.NoError(t, err)
	conv, err := st.EnsureConversationWithType(src.ID, "bulk-conversation", "email_thread", "Bulk")
	require.NoError(t, err)
	var ids []int64
	for i := range 60 {
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, SourceMessageID: fmt.Sprintf("bulk-%d", i), ConversationID: conv, MessageType: "email", SentAt: sql.NullTime{Time: now, Valid: true}})
		require.NoError(t, err)
		ids = append(ids, id)
		var value any = stamp
		if !st.IsPostgreSQL() {
			value = stamp.Format(store.SQLiteTimestampLayout)
		}
		_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE messages SET content_changed_at=? WHERE id=?`), value, id)
		require.NoError(t, err)
	}
	ok, err := st.AdvanceWakeupState(ctx, "bulk", nil, store.WakeupState{At: start})
	require.NoError(t, err)
	require.True(t, ok)
	var got payload
	var decodeErr error
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeErr = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	engine := Engine{Store: st, Subscriptions: []Subscription{{Name: "bulk", URL: "https://example.com/hook", Secret: []byte(strings.Repeat("a", 32)), Filter: changefeed.Filter{IncomingOnly: true}, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(server)}}, Now: func() time.Time { return now }}
	require.NoError(t, engine.RunPass(ctx))
	require.NoError(t, decodeErr)
	assert.Equal(t, 60, got.Matched.Count)
	assert.True(t, got.Matched.Truncated)
	assert.Equal(t, ids[10:], got.Matched.MessageIDs)
}
