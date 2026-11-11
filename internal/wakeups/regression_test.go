package wakeups

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type observedStore struct {
	*store.Store

	reads      []int
	zeroBound  bool
	fixedBound time.Time
	personErr  error
	person     *store.Person
}

func (s *observedStore) ListChangedMessages(ctx context.Context, cursor store.ChangedMessagesCursor, limit int) (store.ChangedMessagePage, error) {
	s.reads = append(s.reads, limit)
	page, err := s.Store.ListChangedMessages(ctx, cursor, limit)
	if s.zeroBound {
		page.CompleteThrough = time.Time{}
		page.Messages = nil
	}
	if !s.fixedBound.IsZero() {
		page.CompleteThrough = s.fixedBound
	}
	return page, err
}
func (s *observedStore) GetPersonContext(ctx context.Context, id int64) (*store.Person, error) {
	if s.personErr != nil {
		return nil, s.personErr
	}
	if s.person != nil {
		return s.person, nil
	}
	return s.Store.GetPersonContext(ctx, id)
}
func seedWakeup(t *testing.T, st *store.Store, source string, sender int64, n int, now time.Time) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", source)
	Require.NoError(t, err)
	conv, err := st.EnsureConversationWithType(src.ID, fmt.Sprintf("conv-%d", n), "email_thread", "Fixture")
	Require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, SourceMessageID: fmt.Sprintf("msg-%d", n), ConversationID: conv, MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, SentAt: sql.NullTime{Time: now, Valid: true}})
	Require.NoError(t, err)
	var stamp any = now.Add(-time.Minute)
	if !st.IsPostgreSQL() {
		stamp = now.Add(-time.Minute).Format(store.SQLiteTimestampLayout)
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET content_changed_at=? WHERE id=?`), stamp, id)
	Require.NoError(t, err)
	return id
}
func initializedWakeup(t *testing.T, st *store.Store, name string, start time.Time) {
	t.Helper()
	ok, err := st.AdvanceWakeupState(t.Context(), name, nil, store.WakeupState{At: start})
	Require.NoError(t, err)
	Require.True(t, ok)
}
func testSubscription(t *testing.T, name string, server *httptest.Server, filter changefeed.Filter) Subscription {
	t.Helper()
	return Subscription{Name: name, URL: "https://example.com/hook", Secret: []byte(strings.Repeat("k", 32)), Filter: filter, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(t, server)}
}

func TestZeroBoundDefersAndSharedPositionReadsOnce(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	base := testutil.NewTestStore(t)
	st := &observedStore{Store: base, zeroBound: true}
	now := time.Now().UTC()
	engine := Engine{Store: st, Subscriptions: []Subscription{{Name: "a"}, {Name: "b"}}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(t.Context()))
	assert.Equal([]int{1}, st.reads)
	for _, name := range []string{"a", "b"} {
		_, raw, err := base.LoadWakeupState(t.Context(), name)
		require.NoError(err)
		assert.Nil(raw)
	}
	st.zeroBound = false
	st.reads = nil
	start := now.Add(-time.Hour)
	initializedWakeup(t, base, "a", start)
	initializedWakeup(t, base, "b", start)
	_, before, err := base.LoadWakeupState(t.Context(), "a")
	require.NoError(err)
	require.NoError(engine.RunPass(t.Context()))
	assert.Equal([]int{2001}, st.reads)
	_, after, err := base.LoadWakeupState(t.Context(), "a")
	require.NoError(err)
	assert.Equal(before, after)
}
func TestHeadReadIsNotReusedForWalkFromZero(t *testing.T) {
	base := testutil.NewTestStore(t)
	st := &observedStore{Store: base}
	initializedWakeup(t, base, "walk", time.Time{})
	engine := Engine{Store: st, Subscriptions: []Subscription{{Name: "new"}, {Name: "walk"}}}
	Require.NoError(t, engine.RunPass(t.Context()))
	Assert.Equal(t, []int{1, 2001}, st.reads)
}
func TestPersonResolutionBackoffAndStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failure  error
		person   *store.Person
		category string
		failures int
	}{
		{"missing", store.ErrPersonNotFound, nil, "person_unavailable", 1},
		{"empty", nil, &store.Person{ID: 1}, "person_unavailable", 1},
		{"store", errors.New("database unavailable"), nil, "store_error", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)

			base := testutil.NewTestStore(t)
			st := &observedStore{Store: base, personErr: tc.failure, person: tc.person}
			now := time.Now().UTC()
			start := now.Add(-time.Hour)
			initializedWakeup(t, base, "person", start)
			var logs bytes.Buffer
			engine := Engine{Store: st, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Now: func() time.Time { return now }, Subscriptions: []Subscription{{Name: "person", Filter: changefeed.Filter{PersonID: 1}}}}
			require.NoError(engine.RunPass(t.Context()))
			assert.Contains(logs.String(), tc.category)
			state, _, err := base.LoadWakeupState(t.Context(), "person")
			require.NoError(err)
			assert.Equal(start, state.At)
			assert.Equal(tc.failures, state.Failures)
			if tc.failures > 0 {
				assert.Equal(now.Add(30*time.Second), state.NextAttemptAt)
			} else {
				assert.True(state.NextAttemptAt.IsZero())
			}
			require.NoError(engine.RunPass(t.Context()))
			state, _, err = base.LoadWakeupState(t.Context(), "person")
			require.NoError(err)
			assert.Equal(tc.failures, state.Failures)
		})
	}
}
func TestConfigPersonAcrossSourcesAndLinkAfterStartup(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(fmt.Sprintf("[[wakeups]]\nname='person'\nurl='https://example.com/hook'\nsecret_env='EXAMPLE_SECRET'\nperson_id=%d\n", person.ID)), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(err)
	require.Len(cfg.Wakeups, 1)
	filter := FilterFromConfig(cfg.Wakeups[0])
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
}
func TestLostCASRedelivers(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	seedWakeup(t, st, "cas@example.com", 0, 1, now)
	initializedWakeup(t, st, "cas", start)
	var count int
	var mutateErr error
	var changed bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if count == 1 {
			var state store.WakeupState
			var raw []byte
			state, raw, mutateErr = st.LoadWakeupState(r.Context(), "cas")
			if mutateErr == nil {
				state.Failures = 1
				changed, mutateErr = st.AdvanceWakeupState(r.Context(), "cas", raw, state)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	var logs bytes.Buffer
	engine := Engine{Store: st, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Subscriptions: []Subscription{testSubscription(t, "cas", server, changefeed.Filter{IncomingOnly: true})}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(t.Context()))
	require.NoError(mutateErr)
	require.True(changed)
	assert.Contains(logs.String(), "state_conflict")
	state, _, err := st.LoadWakeupState(t.Context(), "cas")
	require.NoError(err)
	assert.Equal(start, state.At)
	require.NoError(engine.RunPass(t.Context()))
	assert.Equal(2, count)
	state, _, err = st.LoadWakeupState(t.Context(), "cas")
	require.NoError(err)
	assert.True(state.At.After(start))
}
func TestEndpointFailuresAreIndependent(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	seedWakeup(t, st, "endpoints@example.com", 0, 1, now)
	initializedWakeup(t, st, "a", start)
	initializedWakeup(t, st, "z", start)
	var failed, succeeded int
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { failed++; w.WriteHeader(http.StatusInternalServerError) }))
	defer first.Close()
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { succeeded++; w.WriteHeader(http.StatusNoContent) }))
	defer second.Close()
	filter := changefeed.Filter{IncomingOnly: true}
	engine := Engine{Store: st, Subscriptions: []Subscription{testSubscription(t, "a", first, filter), testSubscription(t, "z", second, filter)}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(t.Context()))
	assert.Equal(1, failed)
	assert.Equal(1, succeeded)
	state, _, err := st.LoadWakeupState(t.Context(), "a")
	require.NoError(err)
	assert.Equal(start, state.At)
	assert.Equal(1, state.Failures)
	state, _, err = st.LoadWakeupState(t.Context(), "z")
	require.NoError(err)
	assert.True(state.At.After(start))
}
func TestOwnerCursorReplaysDeliveredIDs(t *testing.T) {
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	id := seedWakeup(t, st, "replay@example.com", 0, 1, now)
	initializedWakeup(t, st, "replay", now.Add(-time.Hour))
	var sent payload
	var decodeErr error
	status := http.StatusNoContent
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeErr = json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(status)
		_, _ = w.Write([]byte("sensitive-response-body"))
	}))
	defer server.Close()
	var logs bytes.Buffer
	sub := testSubscription(t, "replay", server, changefeed.Filter{IncomingOnly: true})
	sub.URL += "?token=private-query"
	engine := Engine{Store: st, Subscriptions: []Subscription{sub}, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(t.Context()))
	require.NoError(decodeErr)
	require.Equal([]int64{id}, sent.Matched.MessageIDs)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	t.Cleanup(func() { _ = srv.Shutdown(context.WithoutCancel(t.Context())) })
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/messages/changes?incoming_only=true&cursor="+url.QueryEscape(sent.CursorFrom), nil)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var feed api.ChangesResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &feed))
	require.NotEmpty(feed.Messages)
	Assert.Equal(t, id, feed.Messages[0].ID)
}
