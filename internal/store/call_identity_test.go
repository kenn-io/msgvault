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
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	a, b := f.put(t, "", nil), f.put(t, "", nil)
	calls, next, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
	requirements.NoError(err)
	requirements.Len(calls, 2)
	assertions.Equal(fmt.Sprintf("m:%d", a), calls[0].IdentityKey)
	assertions.Equal(fmt.Sprintf("m:%d", b), calls[1].IdentityKey)
	assertions.Empty(next)
}

func TestLogicalCallsTimestampIsEarliestObservation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
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
	requirements.NoError(err)
	assertions.Empty(calls, "later evidence must not move the logical call into day two")
	calls, _, err = f.st.ListLogicalCallsContext(t.Context(), f.query())
	requirements.NoError(err)
	requirements.Len(calls, 1)
	assertions.True(f.at.Equal(calls[0].OccurredAt))
	assertions.Equal(first, calls[0].MessageID)
	assertions.Equal("conflicting", calls[0].Outcome, "classification must include evidence outside the window")
	assertions.False(calls[0].Participants[0].Credited)
}

func TestLogicalCallsPerParticipantCredit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	person, _, err := f.st.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	bob, err := f.st.EnsureParticipantByPhone("+12025550102", "Bob", "signal")
	requirements.NoError(err)
	f.put(t, "one", func(o *callhistory.Observation) {
		o.Participants = append(o.Participants, callhistory.Participant{ID: bob, Participation: "invited"})
	})
	calls, _, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
	requirements.NoError(err)
	requirements.Len(calls, 1)
	assertions.Equal("answered", calls[0].Outcome)
	assertions.Equal([]store.CallParticipant{{ParticipantID: f.peer, PersonID: person.ID, Participation: "joined", Credited: true}, {ParticipantID: bob, Participation: "invited"}}, calls[0].Participants)
}

func TestLogicalCallsDurationIsMax(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	f.put(t, "one", func(o *callhistory.Observation) { v := 60.; o.DurationSeconds = &v })
	f.put(t, "one", func(o *callhistory.Observation) { v := 75.; o.DurationSeconds = &v })
	f.put(t, "two", nil)
	calls, _, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
	requirements.NoError(err)
	requirements.Len(calls, 2)
	for _, c := range calls {
		if c.DurationSeconds != nil {
			assertions.InDelta(75., *c.DurationSeconds, 1e-9)
		} else {
			assertions.Equal([]store.CallParticipant{{ParticipantID: f.peer, Participation: "joined", Credited: true}}, c.Participants)
		}
	}
	requirements.NotEqual(calls[0].DurationSeconds == nil, calls[1].DurationSeconds == nil)
}

func TestLogicalCallsCreditUnsavedParticipant(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	f.put(t, "one", nil)
	q := f.query()
	q.ParticipantIDs = []int64{f.peer}
	calls, _, err := f.st.ListLogicalCallsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(calls, 1)
	assertions.Equal([]store.CallParticipant{{ParticipantID: f.peer, Participation: "joined", Credited: true}}, calls[0].Participants)
	q.ParticipantIDs = []int64{f.peer + 1}
	calls, _, err = f.st.ListLogicalCallsContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Empty(calls)
}

func TestLogicalCallsScopeAndLifecycle(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	first := f.put(t, "one", nil)
	hidden := f.put(t, "one", func(o *callhistory.Observation) {
		o.Account = "b"
		o.Outcome = "missed"
		o.OwnerParticipation = "not_joined"
	})
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.source})
	calls, _, err := f.st.ListLogicalCallsContext(ctx, f.query())
	requirements.NoError(err)
	requirements.Len(calls, 1)
	assertions.Equal("answered", calls[0].Outcome)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`), f.at, hidden)
	requirements.NoError(err)
	again, _, err := f.st.ListLogicalCallsContext(ctx, f.query())
	requirements.NoError(err)
	assertions.Equal(calls, again)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`), f.at, first)
	requirements.NoError(err)
	calls, _, err = f.st.ListLogicalCallsContext(t.Context(), f.query())
	requirements.NoError(err)
	assertions.Empty(calls)
	calls, _, err = f.st.ListLogicalCallsContext(sourcescope.WithSourceIDs(t.Context(), nil), f.query())
	requirements.NoError(err)
	assertions.Empty(calls)
}

func TestLogicalCallsPaginationAndValidation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	for range 3 {
		f.put(t, "", nil)
	}
	q := f.query()
	q.Limit = 1
	var keys []string
	for range 3 {
		calls, next, err := f.st.ListLogicalCallsContext(t.Context(), q)
		requirements.NoError(err)
		requirements.Len(calls, 1)
		keys = append(keys, calls[0].IdentityKey)
		q.AfterKey = next
	}
	assertions.Empty(q.AfterKey)
	assertions.Len(map[string]bool{keys[0]: true, keys[1]: true, keys[2]: true}, 3)
	for _, change := range []func(*store.LogicalCallQuery){
		func(q *store.LogicalCallQuery) { q.Limit = 501 }, func(q *store.LogicalCallQuery) { q.Limit = 0 },
		func(q *store.LogicalCallQuery) { q.Before = q.After }, func(q *store.LogicalCallQuery) { q.AfterKey = "invalid" },
		func(q *store.LogicalCallQuery) { q.ParticipantIDs = []int64{-1} },
		func(q *store.LogicalCallQuery) { q.DateAfter = "2026-10-01" },
		func(q *store.LogicalCallQuery) { q.DateAfter, q.DateBefore = "bad", "2026-10-02" },
		func(q *store.LogicalCallQuery) { q.DateAfter, q.DateBefore = "2026-10-02", "2026-10-02" },
	} {
		q := f.query()
		change(&q)
		_, _, err := f.st.ListLogicalCallsContext(t.Context(), q)
		assertions.ErrorIs(err, callhistory.ErrInvalid)
	}
}

func TestLogicalCallsDatePrecisionUsesCalendarDateBounds(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newLogicalCallFixture(t)
	outsideDate := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	f.put(t, "outside-date", func(o *callhistory.Observation) {
		o.OccurredAt = outsideDate
		o.Precision = "day"
	})
	// An instant alias makes this identity a candidate through the instant path;
	// its earliest day-precision evidence must still be filtered by civil date.
	f.put(t, "outside-date", func(o *callhistory.Observation) {
		o.OccurredAt = outsideDate.Add(time.Hour)
		o.Precision = "instant"
	})
	wantDate := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	f.put(t, "inside-date", func(o *callhistory.Observation) {
		o.OccurredAt = wantDate
		o.Precision = "day"
	})
	q := f.query()
	q.After = time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)
	q.Before = time.Date(2026, 10, 23, 7, 0, 0, 0, time.UTC)
	q.DateAfter = "2026-10-22"
	q.DateBefore = "2026-10-23"

	calls, _, err := f.st.ListLogicalCallsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(calls, 1)
	assertions.True(calls[0].OccurredAt.Equal(wantDate))
	assertions.Equal("day", calls[0].Precision)
}

func TestCallContactAggregateCountsCreditedLogicalCalls(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	alias, err := f.st.EnsureParticipantByPhone("+12025550102", "Alice", "signal")
	requirements.NoError(err)
	_, err = f.st.LinkParticipants(f.peer, alias)
	requirements.NoError(err)
	person, _, err := f.st.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
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
	requirements.NoError(err)
	expectedMonthly := [12]int{}
	expectedMonthly[1] = 1
	expectedMonthly[11] = 2
	want := store.ContactAggregate{FirstAt: &old, LastAt: &outbound, LastInboundAt: &f.at, LastOutboundAt: &outbound,
		Count: 4, Channels: []store.ActivityChannel{store.ChannelOther}, Monthly: expectedMonthly}
	assertions.Equal(want, got, "copies, aliases and partial attendance must not inflate counts")
	q.Subject = store.ActivitySubject{ParticipantIDs: []int64{f.peer, alias}}
	unsaved, err := f.st.CallContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(got, unsaved)
	q.Filters.IncludeNoise = true
	again, err := f.st.CallContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(got, again, "header semantics do not depend on display noise toggles")
}

func TestCallContactAggregateBoundsResolvedIdentityByRequestClock(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	person, _, err := f.st.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	now := f.at
	before := now.Add(-24 * time.Hour)
	earliest := before.Add(-time.Hour)
	f.put(t, "before", func(o *callhistory.Observation) { o.OccurredAt = before })
	f.put(t, "at-clock", func(o *callhistory.Observation) { o.Direction = "outgoing" })
	f.put(t, "future-incoming", func(o *callhistory.Observation) { o.OccurredAt = now.Add(time.Minute) })
	f.put(t, "future-outgoing", func(o *callhistory.Observation) {
		o.OccurredAt = now.AddDate(0, 1, 0)
		o.Direction = "outgoing"
	})
	// Later evidence still resolves an earlier identity's credit and conflicts.
	f.put(t, "later-credit", func(o *callhistory.Observation) {
		o.OccurredAt = earliest
		o.Outcome = "unknown"
		o.OwnerParticipation = "unknown"
		o.Participants[0].Participation = "unknown"
	})
	f.put(t, "later-credit", func(o *callhistory.Observation) { o.OccurredAt = now.Add(time.Hour) })
	f.put(t, "later-conflict", func(o *callhistory.Observation) { o.OccurredAt = before.Add(-2 * time.Hour) })
	f.put(t, "later-conflict", func(o *callhistory.Observation) {
		o.OccurredAt = now.Add(time.Hour)
		o.Outcome = "missed"
		o.OwnerParticipation = "not_joined"
	})
	want := store.ContactAggregate{
		FirstAt: &earliest, LastAt: &now, LastInboundAt: &before, LastOutboundAt: &now,
		Count: 3, Channels: []store.ActivityChannel{store.ChannelOther}, Monthly: [12]int{10: 2, 11: 1},
	}
	for _, subject := range []store.ActivitySubject{{PersonID: person.ID}, {ParticipantIDs: []int64{f.peer}}} {
		q := store.ContactQuery{Subject: subject, Now: now, Location: time.FixedZone("archive", -7*60*60)}
		got, err := f.st.CallContactAggregateContext(t.Context(), q)
		requirements.NoError(err)
		assertions.Equal(want, got, "bound resolved identities inclusively, without hiding later evidence")
		q.Now = earliest.Add(-24 * time.Hour)
		got, err = f.st.CallContactAggregateContext(t.Context(), q)
		requirements.NoError(err)
		assertions.Equal(store.ContactAggregate{}, got, "future-only identities must leave all extrema and buckets empty")
	}
}

func TestCallContactAggregateScope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
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
	requirements.NoError(err)
	assertions.Equal(1, got.Count)
	assertions.Nil(got.LastOutboundAt)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`), f.at, hidden)
	requirements.NoError(err)
	again, err := f.st.CallContactAggregateContext(ctx, q)
	requirements.NoError(err)
	assertions.Equal(got, again)
	empty, err := f.st.CallContactAggregateContext(sourcescope.WithSourceIDs(t.Context(), nil), q)
	requirements.NoError(err)
	assertions.Equal(store.ContactAggregate{}, empty)
	q.Subject = store.ActivitySubject{ParticipantIDs: []int64{f.peer + 1}}
	empty, err = f.st.CallContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(store.ContactAggregate{}, empty)
}

func TestCallContactAggregateRejectsInvalidSubject(t *testing.T) {
	f := newLogicalCallFixture(t)
	for _, subject := range []store.ActivitySubject{{}, {PersonID: -1}, {ParticipantIDs: []int64{0}}, {PersonID: 1, ParticipantIDs: []int64{f.peer}}} {
		_, err := f.st.CallContactAggregateContext(t.Context(), store.ContactQuery{Subject: subject, Now: f.at})
		assert.ErrorIs(t, err, callhistory.ErrInvalid)
	}
}
