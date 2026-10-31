package store_test

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type logicalCallFixture struct {
	st     *store.Store
	peer   int64
	source int64
	at     time.Time
	serial int
}

func newLogicalCallFixture(t *testing.T) *logicalCallFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	peer, err := st.EnsureParticipantByPhone("+12025550101", "Alice", "signal")
	require.NoError(t, err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, (callhistory.Evidence{Provider: "signal", Account: "a"}).SourceIdentifier())
	require.NoError(t, err)
	return &logicalCallFixture{st: st, peer: peer, source: src.ID, at: time.Date(2026, 10, 1, 23, 30, 0, 0, time.UTC)}
}

func (f *logicalCallFixture) put(t *testing.T, native string, change func(*callhistory.Observation)) int64 {
	t.Helper()
	f.serial++
	o := callhistory.Observation{RecordID: strconv.Itoa(f.serial), Version: 1, Provider: "signal", Account: "a", NativeCallID: native,
		Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined", OccurredAt: f.at, Precision: "instant",
		Participants: []callhistory.Participant{{ID: f.peer, Participation: "joined"}}, Raw: []byte(`{}`)}
	if change != nil {
		change(&o)
	}
	src, err := f.st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	id, err := f.st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(t, err)
	return id
}

func (f *logicalCallFixture) query() store.LogicalCallQuery {
	return store.LogicalCallQuery{After: f.at.Add(-time.Hour), Before: f.at.Add(time.Hour), Limit: 500}
}

func TestLogicalCallsEmptyKeysStaySeparate(t *testing.T) {
	f := newLogicalCallFixture(t)
	a, b := f.put(t, "", nil), f.put(t, "", nil)
	calls, next, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
	require.NoError(t, err)
	require.Len(t, calls, 2)
	assert.Equal(t, fmt.Sprintf("m:%d", a), calls[0].IdentityKey)
	assert.Equal(t, fmt.Sprintf("m:%d", b), calls[1].IdentityKey)
	assert.Empty(t, next)
}

func TestLogicalCallsTimestampIsEarliestObservation(t *testing.T) {
	f := newLogicalCallFixture(t)
	first := f.put(t, "one", nil)
	f.put(t, "one", func(o *callhistory.Observation) {
		o.OccurredAt = f.at.Add(2 * time.Hour)
		o.Outcome = "missed"
		o.OwnerParticipation = "not_joined"
	})
	q := f.query()
	q.After, q.Before = f.at.Add(time.Hour), f.at.Add(24*time.Hour)
	calls, _, err := f.st.ListLogicalCallsContext(t.Context(), q)
	require.NoError(t, err)
	assert.Empty(t, calls, "later evidence must not move the logical call into day two")
	calls, _, err = f.st.ListLogicalCallsContext(t.Context(), f.query())
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.True(t, f.at.Equal(calls[0].OccurredAt))
	assert.Equal(t, first, calls[0].MessageID)
	assert.Equal(t, "conflicting", calls[0].Outcome, "classification must include evidence outside the window")
	assert.False(t, calls[0].Participants[0].Credited)
}

func TestLogicalCallsPerParticipantCredit(t *testing.T) {
	f := newLogicalCallFixture(t)
	person, _, err := f.st.CreatePersonFromParticipant(f.peer)
	require.NoError(t, err)
	bob, err := f.st.EnsureParticipantByPhone("+12025550102", "Bob", "signal")
	require.NoError(t, err)
	f.put(t, "one", func(o *callhistory.Observation) {
		o.Participants = append(o.Participants, callhistory.Participant{ID: bob, Participation: "invited"})
	})
	calls, _, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "answered", calls[0].Outcome)
	assert.Equal(t, []store.CallParticipant{{ParticipantID: f.peer, PersonID: person.ID, Participation: "joined", Credited: true}, {ParticipantID: bob, Participation: "invited"}}, calls[0].Participants)
}

func TestLogicalCallsDurationIsMax(t *testing.T) {
	f := newLogicalCallFixture(t)
	f.put(t, "one", func(o *callhistory.Observation) { v := 60.; o.DurationSeconds = &v })
	f.put(t, "one", func(o *callhistory.Observation) { v := 75.; o.DurationSeconds = &v })
	f.put(t, "two", nil)
	calls, _, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
	require.NoError(t, err)
	require.Len(t, calls, 2)
	for _, c := range calls {
		if c.DurationSeconds != nil {
			assert.InDelta(t, 75., *c.DurationSeconds, 1e-9)
		} else {
			assert.Equal(t, []store.CallParticipant{{ParticipantID: f.peer, Participation: "joined", Credited: true}}, c.Participants)
		}
	}
	require.NotEqual(t, calls[0].DurationSeconds == nil, calls[1].DurationSeconds == nil)
}

func TestLogicalCallsCreditUnsavedParticipant(t *testing.T) {
	f := newLogicalCallFixture(t)
	f.put(t, "one", nil)
	q := f.query()
	q.ParticipantIDs = []int64{f.peer}
	calls, _, err := f.st.ListLogicalCallsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, []store.CallParticipant{{ParticipantID: f.peer, Participation: "joined", Credited: true}}, calls[0].Participants)
	q.ParticipantIDs = []int64{f.peer + 1}
	calls, _, err = f.st.ListLogicalCallsContext(t.Context(), q)
	require.NoError(t, err)
	assert.Empty(t, calls)
}

func TestLogicalCallsScopeAndLifecycle(t *testing.T) {
	f := newLogicalCallFixture(t)
	first := f.put(t, "one", nil)
	hidden := f.put(t, "one", func(o *callhistory.Observation) {
		o.Account = "b"
		o.Outcome = "missed"
		o.OwnerParticipation = "not_joined"
	})
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.source})
	calls, _, err := f.st.ListLogicalCallsContext(ctx, f.query())
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "answered", calls[0].Outcome)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`), f.at, hidden)
	require.NoError(t, err)
	again, _, err := f.st.ListLogicalCallsContext(ctx, f.query())
	require.NoError(t, err)
	assert.Equal(t, calls, again)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`), f.at, first)
	require.NoError(t, err)
	calls, _, err = f.st.ListLogicalCallsContext(t.Context(), f.query())
	require.NoError(t, err)
	assert.Empty(t, calls)
	calls, _, err = f.st.ListLogicalCallsContext(sourcescope.WithSourceIDs(t.Context(), nil), f.query())
	require.NoError(t, err)
	assert.Empty(t, calls)
}

func TestLogicalCallsPaginationAndValidation(t *testing.T) {
	f := newLogicalCallFixture(t)
	for range 3 {
		f.put(t, "", nil)
	}
	q := f.query()
	q.Limit = 1
	var keys []string
	for range 3 {
		calls, next, err := f.st.ListLogicalCallsContext(t.Context(), q)
		require.NoError(t, err)
		require.Len(t, calls, 1)
		keys = append(keys, calls[0].IdentityKey)
		q.AfterKey = next
	}
	assert.Empty(t, q.AfterKey)
	assert.Len(t, map[string]bool{keys[0]: true, keys[1]: true, keys[2]: true}, 3)
	for _, change := range []func(*store.LogicalCallQuery){
		func(q *store.LogicalCallQuery) { q.Limit = 501 }, func(q *store.LogicalCallQuery) { q.Limit = 0 },
		func(q *store.LogicalCallQuery) { q.Before = q.After }, func(q *store.LogicalCallQuery) { q.AfterKey = "invalid" },
		func(q *store.LogicalCallQuery) { q.ParticipantIDs = []int64{-1} },
	} {
		q := f.query()
		change(&q)
		_, _, err := f.st.ListLogicalCallsContext(t.Context(), q)
		assert.ErrorIs(t, err, callhistory.ErrInvalid)
	}
}

func TestCallContactAggregateCountsCreditedLogicalCalls(t *testing.T) {
	f := newLogicalCallFixture(t)
	alias, err := f.st.EnsureParticipantByPhone("+12025550102", "Alice", "signal")
	require.NoError(t, err)
	_, err = f.st.LinkParticipants(f.peer, alias)
	require.NoError(t, err)
	person, _, err := f.st.CreatePersonFromParticipant(f.peer)
	require.NoError(t, err)
	old := time.Date(2019, 3, 4, 12, 0, 0, 0, time.UTC)
	monthEdge := time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)
	outbound := f.at.Add(4 * 24 * time.Hour)
	f.put(t, "old", func(o *callhistory.Observation) { o.OccurredAt = old })
	f.put(t, "edge", func(o *callhistory.Observation) { o.OccurredAt = monthEdge })
	f.put(t, "one", func(o *callhistory.Observation) {
		o.Participants = append(o.Participants, callhistory.Participant{ID: alias, Participation: "joined"})
	})
	f.put(t, "one", func(o *callhistory.Observation) { o.OccurredAt = f.at.Add(time.Hour) })
	f.put(t, "outbound", func(o *callhistory.Observation) { o.OccurredAt = outbound; o.Direction = "outgoing" })
	f.put(t, "uncredited", func(o *callhistory.Observation) { o.Participants[0].Participation = "invited" })
	f.put(t, "partial", func(o *callhistory.Observation) { o.OwnerParticipation = "unknown" })
	f.put(t, "partial", func(o *callhistory.Observation) { o.Outcome = "unknown"; o.Participants[0].Participation = "unknown" })
	f.put(t, "conflicting", nil)
	f.put(t, "conflicting", func(o *callhistory.Observation) { o.Outcome = "missed"; o.OwnerParticipation = "not_joined" })
	f.put(t, "withdrawn", func(o *callhistory.Observation) { o.Withdrawn = true })
	q := store.ContactQuery{Subject: store.ActivitySubject{PersonID: person.ID}, Now: f.at.Add(20 * 24 * time.Hour), Location: time.FixedZone("archive", -7*60*60)}
	got, err := f.st.CallContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	expectedMonthly := [12]int{}
	expectedMonthly[1] = 1
	expectedMonthly[11] = 2
	want := store.ContactAggregate{FirstAt: &old, LastAt: &outbound, LastInboundAt: &f.at, LastOutboundAt: &outbound,
		Count: 4, Channels: []store.ActivityChannel{store.ChannelOther}, Monthly: expectedMonthly}
	assert.Equal(t, want, got, "copies, aliases and partial attendance must not inflate counts")
	q.Subject = store.ActivitySubject{ParticipantIDs: []int64{f.peer, alias}}
	unsaved, err := f.st.CallContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, got, unsaved)
	q.Filters.IncludeNoise = true
	again, err := f.st.CallContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, got, again, "header semantics do not depend on display noise toggles")
}

func TestCallContactAggregateScope(t *testing.T) {
	f := newLogicalCallFixture(t)
	f.put(t, "one", nil)
	hidden := f.put(t, "two", func(o *callhistory.Observation) {
		o.Account = "b"
		o.Direction = "outgoing"
		o.OccurredAt = f.at.Add(time.Hour)
	})
	q := store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}}, Now: f.at.Add(24 * time.Hour)}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.source})
	got, err := f.st.CallContactAggregateContext(ctx, q)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Count)
	assert.Nil(t, got.LastOutboundAt)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`), f.at, hidden)
	require.NoError(t, err)
	again, err := f.st.CallContactAggregateContext(ctx, q)
	require.NoError(t, err)
	assert.Equal(t, got, again)
	empty, err := f.st.CallContactAggregateContext(sourcescope.WithSourceIDs(t.Context(), nil), q)
	require.NoError(t, err)
	assert.Equal(t, store.ContactAggregate{}, empty)
	q.Subject = store.ActivitySubject{ParticipantIDs: []int64{f.peer + 1}}
	empty, err = f.st.CallContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, store.ContactAggregate{}, empty)
}

func TestCallContactAggregateRejectsInvalidSubject(t *testing.T) {
	f := newLogicalCallFixture(t)
	for _, subject := range []store.ActivitySubject{{}, {PersonID: -1}, {ParticipantIDs: []int64{0}}, {PersonID: 1, ParticipantIDs: []int64{f.peer}}} {
		_, err := f.st.CallContactAggregateContext(t.Context(), store.ContactQuery{Subject: subject, Now: f.at})
		assert.ErrorIs(t, err, callhistory.ErrInvalid)
	}
}
