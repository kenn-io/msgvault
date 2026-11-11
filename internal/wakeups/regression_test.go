package wakeups

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

type observedStore struct {
	*store.Store

	reads         []int
	zeroBound     bool
	fixedBound    time.Time
	personErr     error
	listErr       error
	person        *store.Person
	rejectAdvance bool
}

func (s *observedStore) ListChangedMessages(ctx context.Context, cursor store.ChangedMessagesCursor, limit int) (store.ChangedMessagePage, error) {
	s.reads = append(s.reads, limit)
	if s.listErr != nil {
		return store.ChangedMessagePage{}, s.listErr
	}
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
func (s *observedStore) AdvanceWakeupStateKey(ctx context.Context, name string, before []byte, after store.WakeupState) (bool, error) {
	if s.rejectAdvance {
		return false, nil
	}
	return s.Store.AdvanceWakeupStateKey(ctx, name, before, after)
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
	ok, err := st.AdvanceWakeupStateKey(t.Context(), store.WakeupStateKey(name), nil, store.WakeupState{At: start})
	Require.NoError(t, err)
	Require.True(t, ok)
}
func testSubscription(t *testing.T, name string, server *httptest.Server, filter changefeed.Filter) Subscription {
	t.Helper()
	return Subscription{Name: name, StateKey: store.WakeupStateKey(name), URL: "https://example.com/hook", Secret: []byte(strings.Repeat("k", 32)), Filter: filter, MaxMessageAge: 72 * time.Hour, Client: testWebhookClient(t, server)}
}

func TestZeroBoundDefersAndSharedPositionReadsOnce(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	base := testutil.NewTestStore(t)
	st := &observedStore{Store: base, zeroBound: true}
	now := time.Now().UTC()
	engine := Engine{Store: st, Subscriptions: []Subscription{{Name: "a", StateKey: store.WakeupStateKey("a")}, {Name: "b", StateKey: store.WakeupStateKey("b")}}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(t.Context()))
	assert.Equal([]int{1}, st.reads)
	for _, name := range []string{"a", "b"} {
		_, raw, err := base.LoadWakeupStateKey(t.Context(), store.WakeupStateKey(name))
		require.NoError(err)
		assert.Nil(raw)
	}
	st.zeroBound = false
	st.reads = nil
	start := now.Add(-time.Hour)
	initializedWakeup(t, base, "a", start)
	initializedWakeup(t, base, "b", start)
	_, before, err := base.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("a"))
	require.NoError(err)
	require.NoError(engine.RunPass(t.Context()))
	assert.Equal([]int{changefeed.ScanBudget + 1}, st.reads)
	_, after, err := base.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("a"))
	require.NoError(err)
	assert.Equal(before, after)
	t.Run("walk after head", func(t *testing.T) {
		base := testutil.NewTestStore(t)
		st := &observedStore{Store: base}
		initializedWakeup(t, base, "walk", time.Now().UTC().Add(-time.Hour))
		engine := Engine{Store: st, Subscriptions: []Subscription{{Name: "new", StateKey: store.WakeupStateKey("new")}, {Name: "walk", StateKey: store.WakeupStateKey("walk")}}}
		Require.NoError(t, engine.RunPass(t.Context()))
		Assert.Equal(t, []int{1, changefeed.ScanBudget + 1}, st.reads)
	})
}
func TestPersonResolutionBackoffAndStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failure  error
		person   *store.Person
		category string
		failures int
		native   bool
	}{
		{"missing", store.ErrPersonNotFound, nil, "person_unavailable", 0, false},
		{"empty", nil, &store.Person{ID: 1}, "person_unavailable", 0, false},
		{"held delivery", store.ErrPersonNotFound, nil, "person_unavailable", 2, false},
		{"store", errors.New("database unavailable"), nil, "store_error", 0, false},
		{"native missing", store.ErrPersonNotFound, nil, "person_unavailable", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)

			base := testutil.NewTestStore(t)
			st := &observedStore{Store: base, personErr: tc.failure, person: tc.person}
			now := time.Now().UTC()
			start := now.Add(-time.Hour)
			key := store.WakeupStateKey("person")
			if tc.native {
				key = store.MCPEventsPositionKey("person")
			}
			initial := store.WakeupState{At: start, Failures: tc.failures}
			if tc.failures > 0 {
				initial.NextAttemptAt = now.Add(-time.Minute)
			}
			ok, err := base.AdvanceWakeupStateKey(t.Context(), key, nil, initial)
			require.NoError(err)
			require.True(ok)
			var logs bytes.Buffer
			engine := Engine{Store: st, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Now: func() time.Time { return now }, Subscriptions: []Subscription{{Name: "person", StateKey: key, Native: tc.native, Filter: changefeed.Filter{PersonID: 1}}}}
			require.NoError(engine.RunPass(t.Context()))
			assert.Contains(logs.String(), tc.category)
			state, _, err := base.LoadWakeupStateKey(t.Context(), key)
			require.NoError(err)
			assert.Equal(start, state.At)
			assert.Equal(tc.failures, state.Failures)
			if tc.native {
				assert.Equal("person_unavailable", state.LastError)
				assert.NotNil(state.FailedSince)
			}
			if tc.category == "store_error" {
				assert.True(state.NextAttemptAt.IsZero())
			} else {
				assert.Equal(now.Add(30*time.Second), state.NextAttemptAt)
			}
		})
	}
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
			state, raw, mutateErr = st.LoadWakeupStateKey(r.Context(), store.WakeupStateKey("cas"))
			if mutateErr == nil {
				state.Failures++
				changed, mutateErr = st.AdvanceWakeupStateKey(r.Context(), store.WakeupStateKey("cas"), raw, state)
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
	state, _, err := st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("cas"))
	require.NoError(err)
	assert.Equal(start, state.At)
	now = now.Add(31 * time.Second)
	require.NoError(engine.RunPass(t.Context()))
	assert.Equal(2, count)
	state, _, err = st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("cas"))
	require.NoError(err)
	assert.True(state.At.After(start))
}

func TestCancelledAttemptSurvivesRestartAndEmptyRescan(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	seedWakeup(t, st, "cancel@example.com", 0, 1, now)
	initializedWakeup(t, st, "cancel", start)
	var bodies []payload
	status := http.StatusInternalServerError
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body payload
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			bodies = append(bodies, body)
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	filter := changefeed.Filter{IncomingOnly: true}
	sub := testSubscription(t, "cancel", server, filter)
	ctx, cancel := context.WithCancel(t.Context())
	sub.Client.dial = func(context.Context, string, string) (net.Conn, error) { cancel(); return nil, context.Canceled }
	engine := Engine{Store: st, Subscriptions: []Subscription{sub}, Now: func() time.Time { return now }}
	require.ErrorIs(engine.RunPass(ctx), context.Canceled)
	state, _, err := st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("cancel"))
	require.NoError(err)
	assert.Equal(start, state.At)
	assert.Equal(1, state.Failures)
	assert.Equal(now.Add(30*time.Second), state.NextAttemptAt)
	assert.Empty(bodies)
	uid, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	from := changefeed.EncodeCursor(uid, store.ChangedMessagesFrom(start), filter.Digest())
	now = now.Add(73 * time.Hour)
	engine = Engine{Store: st, Subscriptions: []Subscription{testSubscription(t, "cancel", server, filter)}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(t.Context()))
	require.Len(bodies, 1)
	assert.Equal(from, bodies[0].CursorFrom)
	assert.Zero(bodies[0].Matched.Count)
	state, _, err = st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("cancel"))
	require.NoError(err)
	assert.Equal(start, state.At)
	assert.Equal(2, state.Failures)
	assert.Equal(now.Add(time.Minute), state.NextAttemptAt)
	status = http.StatusNoContent
	now = now.Add(time.Minute)
	require.NoError(engine.RunPass(t.Context()))
	require.Len(bodies, 2)
	state, _, err = st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("cancel"))
	require.NoError(err)
	assert.True(state.At.After(start))
	assert.Zero(state.Failures)
}

func TestPreSendConflictDoesNotDeliver(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	base := testutil.NewTestStore(t)
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	seedWakeup(t, base, "conflict@example.com", 0, 1, now)
	initializedWakeup(t, base, "conflict", start)
	st := &observedStore{Store: base, rejectAdvance: true}
	hits := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	var logs bytes.Buffer
	engine := Engine{Store: st, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Subscriptions: []Subscription{testSubscription(t, "conflict", server, changefeed.Filter{IncomingOnly: true})}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(t.Context()))
	assert.Zero(hits)
	assert.Contains(logs.String(), "state_conflict")
}

func TestStoreFailureCategoriesDoNotExposeErrors(t *testing.T) {
	Assert.Empty(t, StoreErrorCategory(context.Canceled))
	for _, tc := range []struct {
		name     string
		err      error
		gate     bool
		category string
	}{
		{"gate", nil, true, "gate_busy"},
		{"null watermark", fmt.Errorf("message PRIVATE_WATERMARK_SENTINEL: %w", store.ErrNullChangeWatermark), false, "null_watermark"},
		{"other", errors.New("private detail PRIVATE_WATERMARK_SENTINEL"), false, "store_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			base := testutil.NewTestStore(t)
			var logs bytes.Buffer
			engine := Engine{Store: &observedStore{Store: base, listErr: tc.err}, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Subscriptions: []Subscription{{Name: "fixture", StateKey: store.WakeupStateKey("fixture")}}}
			if tc.gate {
				engine.Gate = func(context.Context) (func(), bool) { return nil, false }
			}
			require.NoError(engine.RunPass(t.Context()))
			assert.Contains(logs.String(), tc.category)
			assert.NotContains(logs.String(), "PRIVATE_WATERMARK_SENTINEL")
			assert.NotContains(logs.String(), "private detail")
		})
	}
}

func TestEndpointFailuresAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		firstStatus, secondStatus int
		advanceClock              bool
	}{
		{"earlier failure", http.StatusInternalServerError, http.StatusNoContent, false},
		{"later attempt time", http.StatusNoContent, http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st := testutil.NewTestStore(t)
			now := time.Now().UTC()
			start := now.Add(-time.Hour)
			seedWakeup(t, st, "endpoints@example.com", 0, 1, now)
			initializedWakeup(t, st, "a", start)
			initializedWakeup(t, st, "z", start)
			var firstHits, secondHits int
			first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				firstHits++
				if tc.advanceClock {
					now = now.Add(time.Minute)
				}
				w.WriteHeader(tc.firstStatus)
			}))
			defer first.Close()
			second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { secondHits++; w.WriteHeader(tc.secondStatus) }))
			defer second.Close()
			filter := changefeed.Filter{IncomingOnly: true}
			engine := Engine{Store: st, Subscriptions: []Subscription{testSubscription(t, "a", first, filter), testSubscription(t, "z", second, filter)}, Now: func() time.Time { return now }}
			require.NoError(engine.RunPass(t.Context()))
			assert.Equal(1, firstHits)
			assert.Equal(1, secondHits)
			firstState, _, err := st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("a"))
			require.NoError(err)
			secondState, _, err := st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("z"))
			require.NoError(err)
			if tc.advanceClock {
				assert.Equal(1, secondState.Failures)
				assert.Equal(now.Add(30*time.Second), secondState.NextAttemptAt)
			} else {
				assert.Equal(start, firstState.At)
				assert.Equal(1, firstState.Failures)
				assert.True(secondState.At.After(start))
			}
		})
	}
}

func TestEmptyPageCorrectsFutureCursorAfterClockRollback(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	base := testutil.NewTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	bound := now.Add(-10 * time.Minute)
	future := now.Add(time.Hour)
	initializedWakeup(t, base, "rollback", future)
	var deliveries []payload
	var decodeErr error
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body payload
		decodeErr = json.NewDecoder(r.Body).Decode(&body)
		deliveries = append(deliveries, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	observed := &observedStore{Store: base, fixedBound: bound}
	engine := Engine{Store: observed, Subscriptions: []Subscription{testSubscription(t, "rollback", server, changefeed.Filter{IncomingOnly: true})}, Now: func() time.Time { return now }}

	require.NoError(engine.RunPass(t.Context()))
	state, _, err := base.LoadWakeupState(t.Context(), "rollback")
	require.NoError(err)
	assert.Equal(bound, state.At)
	assert.Empty(deliveries)

	id := seedWakeup(t, base, "rollback@example.com", 0, 1, now)
	observed.fixedBound = now.Add(time.Minute)
	require.NoError(engine.RunPass(t.Context()))
	require.NoError(decodeErr)
	require.Len(deliveries, 1)
	assert.Equal([]int64{id}, deliveries[0].Matched.MessageIDs)
}

func TestSetupInitializesBeforeFirstPass(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	base := testutil.NewTestStore(t)
	now := time.Now().UTC()
	seedWakeup(t, base, "old@example.com", 0, 1, now)
	existing := store.WakeupState{At: now.Add(-time.Hour), Failures: 2, NextAttemptAt: now.Add(time.Hour)}
	ok, err := base.AdvanceWakeupState(t.Context(), "existing", nil, existing)
	require.NoError(err)
	require.True(ok)
	var deliveries []payload
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body payload
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			deliveries = append(deliveries, body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	st := &observedStore{Store: base, fixedBound: now.Add(-30 * time.Second)}
	filter := changefeed.Filter{IncomingOnly: true}
	engine := Engine{Store: st, Subscriptions: []Subscription{testSubscription(t, "new", server, filter), testSubscription(t, "existing", server, filter)}, Now: func() time.Time { return now }}
	require.NoError(engine.GatedSetup(t.Context(), func() error { return engine.InitializeMissing(t.Context()) }))
	state, _, err := base.LoadWakeupState(t.Context(), "existing")
	require.NoError(err)
	assert.Equal(existing, state)
	id := seedWakeup(t, base, "new@example.com", 0, 2, now.Add(45*time.Second))
	st.fixedBound = now
	require.NoError(engine.RunPass(t.Context()))
	require.Len(deliveries, 1)
	assert.Equal([]int64{id}, deliveries[0].Matched.MessageIDs)
}

func TestSetupInitializationFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		zero    bool
		failure error
	}{
		{name: "zero bound", zero: true},
		{name: "store error", failure: errors.New("private store detail")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			base := testutil.NewTestStore(t)
			st := &observedStore{Store: base, zeroBound: tc.zero, listErr: tc.failure}
			engine := Engine{Store: st, Subscriptions: []Subscription{{Name: "new"}}}
			err := engine.InitializeMissing(t.Context())
			if tc.failure != nil {
				require.ErrorIs(err, tc.failure)
			} else {
				require.NoError(err)
			}
			_, raw, err := base.LoadWakeupState(t.Context(), "new")
			require.NoError(err)
			assert.Nil(raw)
			st.zeroBound, st.listErr = false, nil
			st.fixedBound = time.Now().UTC()
			require.NoError(engine.RunPass(t.Context()))
			_, raw, err = base.LoadWakeupState(t.Context(), "new")
			require.NoError(err)
			assert.NotNil(raw)
		})
	}
}

func TestSetupInitializesOtherEntriesAfterStateLoadError(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	base := testutil.NewTestStore(t)
	const invalid = "not valid wakeup state"
	_, err := base.DB().ExecContext(t.Context(), base.Rebind(`INSERT INTO archive_metadata (key, value) VALUES (?, ?)`), "wakeup:v1:a", invalid)
	require.NoError(err)
	bound := time.Now().UTC().Add(-30 * time.Second)
	engine := Engine{Store: &observedStore{Store: base, fixedBound: bound}, Subscriptions: []Subscription{{Name: "a"}, {Name: "b"}}}
	require.Error(engine.InitializeMissing(t.Context()))
	state, raw, err := base.LoadWakeupState(t.Context(), "b")
	require.NoError(err)
	require.NotNil(raw)
	assert.Equal(bound, state.At)
	var after string
	err = base.DB().QueryRowContext(t.Context(), base.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), "wakeup:v1:a").Scan(&after)
	require.NoError(err)
	assert.Equal(invalid, after)
}

// linkBeforeWakeupPage commits the link and arrival at the real page boundary.
type linkBeforeWakeupPage struct {
	*store.Store

	before func()
}

func (s *linkBeforeWakeupPage) ListChangedMessages(ctx context.Context, cursor store.ChangedMessagesCursor, limit int) (store.ChangedMessagePage, error) {
	if limit == changefeed.ScanBudget+1 && s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.Store.ListChangedMessages(ctx, cursor, limit)
}

func TestWakeupPersonIncludesIdentityLinkedBeforePage(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	first, err := st.EnsureParticipant("first@example.test", "First", "example.test")
	require.NoError(err)
	second, err := st.EnsureParticipant("second@example.test", "Second", "example.test")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(err)
	initializedWakeup(t, st, "person", now.Add(-time.Hour))
	var id int64
	wrapped := &linkBeforeWakeupPage{Store: st, before: func() {
		_, err := st.LinkParticipants(first, second)
		require.NoError(err)
		id = seedWakeup(t, st, "linked@example.test", second, 1, now)
	}}
	var deliveries []payload
	var decodeErr error
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body payload
		decodeErr = json.NewDecoder(r.Body).Decode(&body)
		deliveries = append(deliveries, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	engine := Engine{Store: wrapped, Subscriptions: []Subscription{testSubscription(t, "person", server, changefeed.Filter{PersonID: person.ID})}, Now: func() time.Time { return now }}
	require.NoError(engine.RunPass(t.Context()))
	require.Len(deliveries, 1)
	require.NoError(decodeErr)
	assert.Equal([]int64{id}, deliveries[0].Matched.MessageIDs)
	require.NoError(engine.RunPass(t.Context()))
	assert.Len(deliveries, 1)
}
