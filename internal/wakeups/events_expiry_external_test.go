package wakeups_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/wakeups"
)

func TestOwnerEventExpiresBehindConfiguredCallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "events@example.test")
	require.NoError(err)
	var mu sync.Mutex
	now := time.Now().UTC()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	service := mcpevents.Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: clock}
	in, err := mcpevents.DecodeSubscribe(fmt.Appendf(nil, `{"name":"msgvault.messages.changed","arguments":{"source_id":%d},"delivery":{"mode":"webhook","url":"https://example.com/native","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"},"ttlMs":60000}`, source.ID))
	require.NoError(err)
	created, err := service.Subscribe(ctx, in, func(context.Context, string, string, []byte) error { return nil })
	require.NoError(err)
	start := clock().Add(-time.Hour)
	key := store.MCPEventsPositionKey(created.ID)
	_, raw, err := st.LoadWakeupStateKey(ctx, key)
	require.NoError(err)
	ok, err := st.AdvanceWakeupStateKey(ctx, key, raw, store.WakeupState{At: start})
	require.NoError(err)
	require.True(ok)
	ok, err = st.AdvanceWakeupStateKey(ctx, store.WakeupStateKey("configured"), nil, store.WakeupState{At: start})
	require.NoError(err)
	require.True(ok)
	conv, err := st.EnsureConversationWithType(source.ID, "expiry", "email_thread", "Fixture")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "expiry", ConversationID: conv, MessageType: "email", SentAt: sql.NullTime{Time: clock(), Valid: true}})
	require.NoError(err)
	var stamp any = clock().Add(-time.Minute)
	if !st.IsPostgreSQL() {
		stamp = clock().Add(-time.Minute).Format(store.SQLiteTimestampLayout)
	}
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE messages SET content_changed_at=? WHERE id=?`), stamp, id)
	require.NoError(err)
	_, before, err := st.LoadWakeupStateKey(ctx, key)
	require.NoError(err)
	var requests []string
	var nativeCount int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		if r.URL.Path == "/configured" {
			now = created.RefreshBefore
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	transport := wakeups.FixtureWebhookSubscription(t, server)
	configured := wakeups.NewSubscription("configured", "https://example.com/configured", []byte(strings.Repeat("a", 32)), changefeed.Filter{}, 72*time.Hour, time.Minute, nil, nil)
	configured.Client = transport.Client
	engine := wakeups.Engine{Store: st, Subscriptions: []wakeups.Subscription{configured}, Now: clock, Native: func(ctx context.Context) ([]wakeups.Subscription, error) {
		subs, err := service.Native(ctx)
		nativeCount = len(subs)
		for i := range subs {
			subs[i].Client = transport.Client
		}
		return subs, err
	}}
	require.NoError(engine.RunPass(ctx))
	assert.Equal(1, nativeCount)
	_, after, err := st.LoadWakeupStateKey(ctx, key)
	require.NoError(err)
	assert.Equal(before, after)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal([]string{"/configured"}, requests)
}
