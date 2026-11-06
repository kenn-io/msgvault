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
	"go.kenn.io/msgvault/internal/testutil/storetest"
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

func TestLogicalCallsSameNativeCallIDIsOneCall(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newLogicalCallFixture(t)
	first := f.put(t, "one", nil)
	f.put(t, "one", nil)
	calls, next, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
	requirements.NoError(err)
	requirements.Len(calls, 1)
	assertions.Empty(next)
	assertions.Equal(first, calls[0].MessageID)
	assertions.Equal([]store.CallParticipant{{ParticipantID: f.peer, Participation: "joined", Credited: true}}, calls[0].Participants)
	q := store.CallContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}}, Now: f.at}
	aggregate, err := f.st.CallContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(1, aggregate.Count)
}

func TestLogicalCallsImportedPhoneContact(t *testing.T) {
	for _, messageType := range []string{store.MessageTypeSynctechCall, store.MessageTypeGoogleVoiceCall} {
		for _, outgoing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/outgoing=%t", messageType, outgoing), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := storetest.New(t)
				peer, err := f.Store.EnsureParticipantByPhone("+12025550101", "Example", "phone")
				require.NoError(err)
				person, _, err := f.Store.CreatePersonFromParticipant(peer)
				require.NoError(err)
				owner, err := f.Store.EnsureParticipantByPhone("+12025550102", "Owner", "phone")
				require.NoError(err)
				at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
				message := f.NewMessage().WithSentAt(at).Build()
				message.MessageType, message.IsFromMe = messageType, outgoing
				id, err := f.Store.UpsertMessage(message)
				require.NoError(err)
				from, to := peer, owner
				if outgoing {
					from, to = owner, peer
				}
				require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
				require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
				require.NoError(f.Store.SetMessageMetadata(id, store.CallConnectedMetadata(60)))
				query := store.LogicalCallQuery{After: at, Before: at.Add(time.Hour), ParticipantIDs: []int64{peer}, Limit: 10}
				calls, next, err := f.Store.ListLogicalCallsContext(t.Context(), query)
				require.NoError(err)
				require.Len(calls, 1)
				assert.Empty(next)
				assert.Equal(fmt.Sprintf("m:%d", id), calls[0].IdentityKey)
				assert.Equal("answered", calls[0].Outcome)
				assert.Equal([]store.CallParticipant{{ParticipantID: peer, PersonID: person.ID, Participation: "joined", Credited: true}}, calls[0].Participants)
				aggregate, err := f.Store.CallContactAggregateContext(t.Context(), store.CallContactQuery{Subject: store.ActivitySubject{PersonID: person.ID}, Now: at})
				require.NoError(err)
				assert.Equal(1, aggregate.Count)
				if outgoing {
					assert.Equal(&at, aggregate.LastOutboundAt)
					assert.Nil(aggregate.LastInboundAt)
				} else {
					assert.Equal(&at, aggregate.LastInboundAt)
					assert.Nil(aggregate.LastOutboundAt)
				}
			})
		}
	}
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
	assertions.Equal(f.at, calls[0].OccurredAt)
	assertions.Equal(first, calls[0].MessageID)
	assertions.Equal("answered", calls[0].Outcome, "one whole observation establishes the answer")
	assertions.True(calls[0].Participants[0].Credited)
}

func TestLogicalCallsNegativeOutcomesMustAgree(t *testing.T) {
	for _, tc := range []struct{ first, second, want string }{
		{"missed", "missed", "missed"},
		{"missed", "declined", "unknown"},
		{"cancelled", "unknown", "cancelled"},
	} {
		t.Run(tc.first+"/"+tc.second, func(t *testing.T) {
			require := require.New(t)
			f := newLogicalCallFixture(t)
			for _, outcome := range []string{tc.first, tc.second} {
				f.put(t, "one", func(o *callhistory.Observation) { o.Outcome, o.OwnerParticipation = outcome, "unknown" })
			}
			calls, _, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
			require.NoError(err)
			require.Len(calls, 1)
			assert.Equal(t, tc.want, calls[0].Outcome)
		})
	}
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

func TestLogicalCallsOutcomeIndependentOfParticipantCredit(t *testing.T) {
	for _, tc := range []struct {
		name          string
		participation string
		owner         string
		want          string
	}{
		{name: "answered group without roster", owner: "joined", want: "answered"},
		{name: "answered with invited participant", participation: "invited", owner: "joined", want: "answered"},
		{name: "unknown owner attendance", owner: "unknown", want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newLogicalCallFixture(t)
			f.put(t, "one", func(o *callhistory.Observation) {
				o.OwnerParticipation = tc.owner
				if tc.participation == "" {
					o.Participants = nil
				} else {
					o.Participants[0].Participation = tc.participation
				}
			})
			calls, _, err := f.st.ListLogicalCallsContext(t.Context(), f.query())
			require.NoError(err)
			require.Len(calls, 1)
			assert.Equal(tc.want, calls[0].Outcome)
			if tc.participation == "" {
				assert.Empty(calls[0].Participants)
			} else {
				require.Len(calls[0].Participants, 1)
				assert.False(calls[0].Participants[0].Credited)
			}
		})
	}
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
	person, _, err := f.st.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
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
	summary, err := f.st.SummarizeCallsContext(ctx, store.CallQuery{})
	requirements.NoError(err)
	assertions.Equal([]store.PersonCallSummary{{PersonID: person.ID, Answered: 1, ExampleMessageID: first}}, summary)
	summary, err = f.st.SummarizeCallsContext(sourcescope.WithSourceIDs(t.Context(), nil), store.CallQuery{})
	requirements.NoError(err)
	assertions.Empty(summary)
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

func TestLogicalCallReadersRejectUnsupportedEvidenceVersion(t *testing.T) {
	require := require.New(t)
	f := newLogicalCallFixture(t)
	f.put(t, "visible", nil)
	hidden := f.put(t, "hidden", func(o *callhistory.Observation) { o.Account = "b" })
	_, err := f.st.DB().Exec(f.st.Rebind(`UPDATE call_details SET evidence_version = 2 WHERE message_id = ?`), hidden)
	require.NoError(err)
	_, _, err = f.st.ListLogicalCallsContext(t.Context(), f.query())
	require.ErrorIs(err, callhistory.ErrInvalid)
	q := store.CallContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}}, Now: f.at}
	_, err = f.st.CallContactAggregateContext(t.Context(), q)
	require.ErrorIs(err, callhistory.ErrInvalid)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.source})
	calls, _, err := f.st.ListLogicalCallsContext(ctx, f.query())
	require.NoError(err)
	require.Len(calls, 1)
	aggregate, err := f.st.CallContactAggregateContext(ctx, q)
	require.NoError(err)
	require.Equal(1, aggregate.Count)
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
	} {
		q := f.query()
		change(&q)
		_, _, err := f.st.ListLogicalCallsContext(t.Context(), q)
		assertions.ErrorIs(err, callhistory.ErrInvalid)
	}
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
	f.put(t, "answered-and-missed", nil)
	f.put(t, "answered-and-missed", func(o *callhistory.Observation) { o.Outcome = "missed"; o.OwnerParticipation = "not_joined" })
	f.put(t, "withdrawn", func(o *callhistory.Observation) { o.Withdrawn = true })
	q := store.CallContactQuery{Subject: store.ActivitySubject{PersonID: person.ID}, Now: f.at.Add(20 * 24 * time.Hour), Location: time.FixedZone("archive", -7*60*60)}
	got, err := f.st.CallContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	expectedMonthly := [12]int{}
	expectedMonthly[1] = 1
	expectedMonthly[11] = 3
	want := store.ContactAggregate{FirstAt: &old, LastAt: &outbound, LastInboundAt: &f.at, LastOutboundAt: &outbound,
		Count: 5, Channels: []store.ActivityChannel{store.ChannelOther}, Monthly: expectedMonthly}
	assertions.Equal(want, got, "each call counts once; aliases and partial attendance add no credit")
	q.Subject = store.ActivitySubject{ParticipantIDs: []int64{f.peer, alias}}
	unsaved, err := f.st.CallContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(got, unsaved)
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
	// Later records of a call still resolve the earlier call's credit.
	f.put(t, "later-credit", func(o *callhistory.Observation) {
		o.OccurredAt = earliest
		o.Outcome = "unknown"
		o.OwnerParticipation = "unknown"
		o.Participants[0].Participation = "unknown"
	})
	f.put(t, "later-credit", func(o *callhistory.Observation) { o.OccurredAt = now.Add(time.Hour) })
	f.put(t, "later-missed", func(o *callhistory.Observation) { o.OccurredAt = before.Add(-2 * time.Hour) })
	f.put(t, "later-missed", func(o *callhistory.Observation) {
		o.OccurredAt = now.Add(time.Hour)
		o.Outcome = "missed"
		o.OwnerParticipation = "not_joined"
	})
	want := store.ContactAggregate{
		FirstAt: new(before.Add(-2 * time.Hour)), LastAt: &now, LastInboundAt: &before, LastOutboundAt: &now,
		Count: 4, Channels: []store.ActivityChannel{store.ChannelOther}, Monthly: [12]int{10: 3, 11: 1},
	}
	for _, subject := range []store.ActivitySubject{{PersonID: person.ID}, {ParticipantIDs: []int64{f.peer}}} {
		q := store.CallContactQuery{Subject: subject, Now: now, Location: time.FixedZone("archive", -7*60*60)}
		got, err := f.st.CallContactAggregateContext(t.Context(), q)
		requirements.NoError(err)
		assertions.Equal(want, got, "bound calls by their earliest record, without hiding later evidence")
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
	q := store.CallContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}}, Now: f.at.Add(24 * time.Hour)}
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
		_, err := f.st.CallContactAggregateContext(t.Context(), store.CallContactQuery{Subject: subject, Now: f.at})
		assert.ErrorIs(t, err, callhistory.ErrInvalid)
	}
}
