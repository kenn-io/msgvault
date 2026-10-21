package store_test

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type modeledCall struct {
	callhistory.Observation

	messageID int64
}

// The oracle groups explicit tuples into sets. It neither hashes identities nor
// repeats the production SQL/CTEs. Every participant in this fixture is bound to
// one person, so aliases must not multiply that person's count.
func modeledSummary(records []modeledCall, personID int64) []store.PersonCallSummary {
	type identity struct{ provider, account, native, fallback string }
	groups := map[identity][]modeledCall{}
	for _, r := range records {
		if r.Withdrawn {
			continue
		}
		key := identity{provider: r.Provider, account: r.Account, native: r.NativeCallID}
		if key.native == "" {
			key.fallback = r.RecordID
		}
		groups[key] = append(groups[key], r)
	}
	result := store.PersonCallSummary{PersonID: personID}
	for _, observations := range groups {
		outcomes := map[string]bool{}
		negativeOutcomes := map[string]bool{}
		associated, confirmed, notJoined := false, false, false
		for _, o := range observations {
			outcomes[o.Outcome] = true
			if o.Outcome == "missed" || o.Outcome == "declined" || o.Outcome == "cancelled" {
				negativeOutcomes[o.Outcome] = true
			}
			notJoined = notJoined || o.OwnerParticipation == "not_joined"
			for _, p := range o.Participants {
				associated = true
				if result.ExampleMessageID == 0 || o.messageID < result.ExampleMessageID {
					result.ExampleMessageID = o.messageID
				}
				confirmed = confirmed || (o.Outcome == "answered" && o.OwnerParticipation == "joined" && p.Participation == "joined")
			}
		}
		if !associated {
			continue
		}
		switch {
		case observations[0].Kind == "scheduled_meeting":
			result.Scheduled++
			if outcomes["cancelled"] {
				result.ScheduledCancelled++
			}
		case (outcomes["answered"] && (notJoined || len(negativeOutcomes) > 0)) || len(negativeOutcomes) > 1:
			result.Conflicting++
		case confirmed:
			result.Answered++
		case outcomes["missed"]:
			result.Missed++
		case outcomes["declined"]:
			result.Declined++
		case outcomes["cancelled"]:
			result.Cancelled++
		default:
			result.Unknown++
		}
	}
	if result.ExampleMessageID == 0 {
		return []store.PersonCallSummary{}
	}
	return []store.PersonCallSummary{result}
}

func FuzzCallStoreSequences(f *testing.F) {
	for _, seed := range []uint64{0, 1, 42, 0xffffffffffffffff} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) {
		st := testutil.NewTestStore(t)
		a, err := st.EnsureParticipantByPhone("+12025550101", "Example", "signal")
		require.NoError(t, err)
		b, err := st.EnsureParticipantByPhone("+12025550102", "Example", "signal")
		require.NoError(t, err)
		_, err = st.LinkParticipants(a, b)
		require.NoError(t, err)
		person, _, err := st.CreatePersonFromParticipant(a)
		require.NoError(t, err)
		random := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // Replayable synthetic sequences need deterministic randomness.
		// Bound only materialization/work: the generated seed domain is intact.
		records := make([]modeledCall, 6)
		change := func(o callhistory.Observation) callhistory.Observation {
			o.Outcome = []string{"answered", "missed", "declined", "cancelled", "unknown"}[random.IntN(5)]
			o.OwnerParticipation = []string{"joined", "not_joined", "unknown"}[random.IntN(3)]
			role := []string{"joined", "invited", "unknown"}[random.IntN(3)]
			if o.Kind == "scheduled_meeting" {
				o.OwnerParticipation, role = "unknown", "invited"
				o.Outcome = []string{"unknown", "cancelled"}[random.IntN(2)]
			}
			o.Participants = []callhistory.Participant{{ID: a, Participation: role}}
			switch random.IntN(3) {
			case 0:
				o.Participants = nil
			case 1:
				o.Participants = append(o.Participants, callhistory.Participant{ID: b, Participation: role})
			}
			o.Withdrawn = random.IntN(5) == 0
			return o
		}
		put := func(index int, o callhistory.Observation) {
			t.Helper()
			src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
			require.NoError(t, err)
			id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
			if records[index].Withdrawn && !o.Withdrawn {
				require.ErrorIs(t, err, store.ErrCallObservationWithdrawn)
				return
			}
			require.NoError(t, err)
			if records[index].messageID != 0 {
				assert.Equal(t, records[index].messageID, id)
			}
			records[index] = modeledCall{Observation: o, messageID: id}
		}
		check := func() {
			t.Helper()
			got, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{})
			require.NoError(t, err)
			assert.Equal(t, modeledSummary(records, person.ID), got)
			evidence, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{Limit: 100})
			require.NoError(t, err)
			require.Len(t, evidence, len(records))
			for _, row := range evidence {
				found := false
				for _, expected := range records {
					if expected.messageID == row.MessageID {
						found = true
						assert.Equal(t, expected.Evidence, row.Evidence)
						assert.ElementsMatch(t, expected.Participants, row.Participants)
					}
				}
				assert.True(t, found)
			}
		}
		for _, index := range random.Perm(len(records)) {
			o := callhistory.Observation{
				RecordID: strconv.Itoa(index),

				Version: 1, Provider: fmt.Sprintf("network-%d", random.IntN(2)), Account: fmt.Sprintf("account-%d", random.IntN(2)),
				NativeCallID: strconv.Itoa(random.IntN(2)), Kind: "call", Direction: "unknown",
				OccurredAt: time.Date(2026, 10, 1, index, 0, 0, 0, time.UTC), Precision: "instant",

				Raw: []byte(`{}`),
			}
			if random.IntN(4) == 0 {
				o.Kind, o.NativeCallID = "scheduled_meeting", ""
			}
			put(index, change(o))
		}
		check()
		for _, index := range random.Perm(len(records)) {
			put(index, records[index].Observation)
		}
		check()
		for range 8 {
			index := random.IntN(len(records))
			put(index, change(records[index].Observation))
			check()
		}
	})
}
