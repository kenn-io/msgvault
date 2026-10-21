package store_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCallSummaryRequiresWholeObservationAndDeduplicates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	peer, err := st.EnsureParticipantByPhone("+12025550101", "Example", "signal")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	serial := 0
	put := func(native, account, outcome, owner, role, kind string) int64 {
		t.Helper()
		serial++
		o := callhistory.Observation{
			RecordID: strconv.Itoa(serial),

			Version: 1, Provider: "signal", Account: account, NativeCallID: native,
			Kind: kind, Direction: "incoming", Outcome: outcome, OwnerParticipation: owner,
			OccurredAt: at, Precision: "instant",

			Participants: []callhistory.Participant{{ID: peer, Participation: role}},
			Raw:          []byte(`{}`),
		}
		src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
		requirements.NoError(err)
		id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
		requirements.NoError(err)
		return id
	}
	first := put("one", "a", "answered", "joined", "joined", "call")
	put("one", "a", "answered", "joined", "joined", "call")
	put("one", "b", "answered", "joined", "joined", "call")
	put("partial", "a", "answered", "unknown", "joined", "call")
	put("partial", "a", "unknown", "joined", "unknown", "call")
	put("conflict", "a", "answered", "joined", "joined", "call")
	put("conflict", "a", "missed", "not_joined", "unknown", "call")
	put("", "a", "unknown", "unknown", "invited", "scheduled_meeting")
	put("", "a", "cancelled", "unknown", "invited", "scheduled_meeting")
	put("missed", "a", "missed", "not_joined", "unknown", "call")
	got, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(person.ID, got[0].PersonID)
	assertions.Equal(2, got[0].Answered)
	assertions.Equal(1, got[0].Unknown)
	assertions.Equal(1, got[0].Conflicting)
	assertions.Equal(2, got[0].Scheduled)
	assertions.Equal(1, got[0].ScheduledCancelled)
	assertions.Equal(1, got[0].Missed)
	assertions.Equal(first, got[0].ExampleMessageID)

	list, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{PersonID: person.ID, Limit: 2})
	requirements.NoError(err)
	requirements.Len(list, 2)
	assertions.Equal("joined", list[0].Participants[0].Participation)
	assertions.Equal(first, list[0].MessageID)
	next, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{AfterID: list[1].MessageID, Limit: 2})
	requirements.NoError(err)
	requirements.Len(next, 2)
	assertions.Greater(next[0].MessageID, list[1].MessageID)

	before := at
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{Before: &before})
	requirements.NoError(err)
	assertions.Empty(got)
	after := at.Add(time.Second)
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{After: &after})
	requirements.NoError(err)
	assertions.Empty(got)
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{After: &at})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(2, got[0].Answered)
}

func TestCallSummaryGroupsBeforeWindowAndUsesCurrentBindings(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(callhistory.SourceType, (callhistory.Evidence{Provider: "signal", Account: "a"}).SourceIdentifier())
	requirements.NoError(err)
	a, err := st.EnsureParticipantByPhone("+12025550101", "Example", "signal")
	requirements.NoError(err)
	b, err := st.EnsureParticipantByPhone("+12025550102", "Example", "signal")
	requirements.NoError(err)
	_, err = st.LinkParticipants(a, b)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := callhistory.Observation{
		RecordID: "one",

		Version: 1, Provider: "signal", Account: "a", NativeCallID: "native",
		Kind: "call", Direction: "unknown", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: at, Precision: "instant",

		Participants: []callhistory.Participant{{ID: a, Participation: "joined"}, {ID: b, Participation: "joined"}},
		Raw:          []byte(`{}`),
	}
	first, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	o.RecordID, o.OccurredAt = "two", at.Add(time.Hour)
	second, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	got, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(person.ID, got[0].PersonID)
	assertions.Equal(1, got[0].Answered, "aliases and observations each deduplicate")
	window := at.Add(time.Minute)
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{After: &window})
	requirements.NoError(err)
	assertions.Empty(got, "later duplicate cannot move a call into the window")
	requirements.NoError(st.DeletePersonContext(t.Context(), person.ID, person.Revision))
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	assertions.Empty(got, "unbound participants are not guessed into a person")
	rebound, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(rebound.ID, got[0].PersonID)
	o.Withdrawn = true
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(1, got[0].Answered, "one withdrawn observation does not erase other evidence")
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`), at, first)
	requirements.NoError(err)
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	assertions.Empty(got)
	list, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	requirements.Len(list, 1, "withdrawn evidence remains inspectable")
	assertions.Equal(second, list[0].MessageID)
	assertions.True(list[0].Withdrawn)
}

func TestCallQueriesRejectInvalidBounds(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	zero := time.Time{}
	for _, query := range []store.CallQuery{
		{Limit: 101}, {Limit: -1}, {PersonID: -1}, {AfterID: -1},
		{Offset: -1}, {After: &zero},
	} {
		_, err := st.SummarizeCallsContext(t.Context(), query)
		requirements.ErrorIs(err, callhistory.ErrInvalid)
		_, err = st.ListCallObservationsContext(t.Context(), query)
		requirements.ErrorIs(err, callhistory.ErrInvalid)
	}
}
