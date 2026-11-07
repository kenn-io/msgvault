package store_test

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCallReadersExcludeUndatedLegacyCalls(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	peer, err := f.Store.EnsureParticipantByPhone(t.Context(), "+12025550101", "Example", "google_voice")
	requirements.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var datedID int64
	for _, tc := range []struct {
		messageType string
		dated       bool
	}{
		{store.MessageTypeGoogleVoiceCall, false},
		{store.MessageTypeSynctechCall, false},
		{store.MessageTypeGoogleVoiceCall, true},
	} {
		message := f.NewMessage().Build()
		message.MessageType = tc.messageType
		message.SentAt = sql.NullTime{Time: at, Valid: tc.dated}
		id, err := f.Store.UpsertMessage(message)
		requirements.NoError(err)
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
		requirements.NoError(f.Store.SetMessageMetadata(id, store.CallConnectedMetadata(600)))
		if tc.dated {
			datedID = id
		}
	}
	t.Run("list", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		items, err := f.Store.ListCallObservationsContext(t.Context(), store.CallQuery{})
		require.NoError(err)
		require.Len(items, 1)
		assert.Equal(datedID, items[0].MessageID)
		assert.True(at.Equal(items[0].OccurredAt))
	})
	t.Run("summary", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		summary, err := f.Store.SummarizeCallsContext(t.Context(), store.CallQuery{})
		require.NoError(err)
		require.Len(summary, 1)
		assert.Equal([]store.PersonCallSummary{{PersonID: person.ID, Answered: 1, ExampleMessageID: datedID}}, summary)
	})
}

func TestLegacyCallsExcludeArchiveOwner(t *testing.T) {
	for _, messageType := range []string{store.MessageTypeGoogleVoiceCall, store.MessageTypeSynctechCall, "call_record"} {
		for _, outgoing := range []bool{false, true} {
			t.Run(messageType+"/outgoing="+strconv.FormatBool(outgoing), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := storetest.New(t)
				owner, err := f.Store.EnsureParticipantByPhone(t.Context(), "+12025550102", "Owner", "manual")
				require.NoError(err)
				ownerPerson, _, err := f.Store.CreatePersonFromParticipant(owner)
				require.NoError(err)
				alias := f.EnsureParticipant("owner-alias@example.com", "Owner alias", "example.com")
				_, err = f.Store.LinkParticipants(owner, alias)
				require.NoError(err)
				require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "+12025550102", "manual"))
				peer, err := f.Store.EnsureParticipantByPhone(t.Context(), "+12025550101", "Peer", "manual")
				require.NoError(err)
				peerPerson, _, err := f.Store.CreatePersonFromParticipant(peer)
				require.NoError(err)
				role := "from"
				if outgoing {
					role = "to"
				}
				var id int64
				if messageType == "call_record" {
					o, sourceID := validCallObservation(t, f.Store)
					if outgoing {
						o.Direction = "outgoing"
					}
					o.Participants = []callhistory.Participant{{ID: owner, Participation: "joined"}, {ID: alias, Participation: "joined"}, {ID: peer, Participation: "joined"}}
					id, err = f.Store.PutCallObservationContext(t.Context(), sourceID, o)
					require.NoError(err)
				} else {
					message := f.NewMessage().Build()
					message.MessageType, message.IsFromMe = messageType, outgoing
					message.SentAt = sql.NullTime{Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Valid: true}
					id, err = f.Store.UpsertMessage(message)
					require.NoError(err)
					counterparties := []int64{peer}
					if outgoing {
						counterparties = append(counterparties, owner, alias)
					} else {
						require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, alias}, nil))
					}
					require.NoError(f.Store.ReplaceMessageRecipients(id, role, counterparties, nil))
					require.NoError(f.Store.SetMessageMetadata(id, store.CallConnectedMetadata(600)))
				}

				items, err := f.Store.ListCallObservationsContext(t.Context(), store.CallQuery{})
				require.NoError(err)
				require.Len(items, 1)
				assert.Equal([]callhistory.Participant{{ID: peer, Participation: "joined"}}, items[0].Participants)
				items, err = f.Store.ListCallObservationsContext(t.Context(), store.CallQuery{PersonID: ownerPerson.ID})
				require.NoError(err)
				assert.Empty(items, "calls with another owned number do not credit the owner")
				summary, err := f.Store.SummarizeCallsContext(t.Context(), store.CallQuery{})
				require.NoError(err)
				assert.Equal([]store.PersonCallSummary{{PersonID: peerPerson.ID, Answered: 1, ExampleMessageID: id}}, summary)
			})
		}
	}
}

func TestCallSummaryRequiresWholeObservationPerRecord(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	peer, err := st.EnsureParticipantByPhone(t.Context(), "+12025550101", "Example", "signal")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	put := func(native, account, outcome, owner, role string) int64 {
		t.Helper()
		o := callhistory.Observation{
			RecordID: native,

			Version: 1, Provider: "signal", Account: account,
			Direction: "incoming", Outcome: outcome, OwnerParticipation: owner,
			OccurredAt: at,

			Participants: []callhistory.Participant{{ID: peer, Participation: role}},
			Raw:          []byte(`{}`),
		}
		src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
		requirements.NoError(err)
		id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
		requirements.NoError(err)
		return id
	}
	first := put("one", "a", "answered", "joined", "joined")
	put("one", "a", "answered", "joined", "joined")
	put("one", "b", "answered", "joined", "joined")
	put("partial-1", "a", "answered", "unknown", "joined")
	put("partial-2", "a", "answered", "joined", "invited")
	put("separate-1", "a", "answered", "joined", "joined")
	put("separate-2", "a", "missed", "not_joined", "unknown")
	put("missed", "a", "missed", "not_joined", "unknown")
	got, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(person.ID, got[0].PersonID)
	assertions.Equal(3, got[0].Answered)
	assertions.Equal(2, got[0].Unknown)
	assertions.Equal(2, got[0].Missed)
	assertions.Equal(first, got[0].ExampleMessageID)
}

func TestCallSummaryWindowAndCurrentBindings(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(callhistory.SourceType, (callhistory.Evidence{Provider: "signal", Account: "a"}).SourceIdentifier())
	requirements.NoError(err)
	a, err := st.EnsureParticipantByPhone(t.Context(), "+12025550101", "Example", "signal")
	requirements.NoError(err)
	b, err := st.EnsureParticipantByPhone(t.Context(), "+12025550102", "Example", "signal")
	requirements.NoError(err)
	_, err = st.LinkParticipants(a, b)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := callhistory.Observation{
		RecordID: "one",

		Version: 1, Provider: "signal", Account: "a",
		Direction: "unknown", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: at,

		Participants: []callhistory.Participant{{ID: a, Participation: "joined"}, {ID: b, Participation: "joined"}},
		Raw:          []byte(`{}`),
	}
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	o.RecordID, o.OccurredAt = "two", at.Add(time.Hour)
	secondID, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	got, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(person.ID, got[0].PersonID)
	assertions.Equal(2, got[0].Answered, "aliases count once per record")
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{Before: &at})
	requirements.NoError(err)
	assertions.Empty(got, "before excludes the record at the boundary")
	end := at.Add(time.Hour)
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{After: &end})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(1, got[0].Answered, "after includes the record at the boundary")
	pastEnd := end.Add(time.Second)
	got, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{After: &pastEnd})
	requirements.NoError(err)
	assertions.Empty(got)
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

	t.Run("reclassified message", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var conversationID int64
		requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), secondID).Scan(&conversationID))
		_, err := st.UpsertMessage(&store.Message{
			ConversationID: conversationID,
			SourceID:       src.ID, SourceMessageID: "two", MessageType: "email",
			SentAt: sql.NullTime{Time: end, Valid: true},
		})
		requirements.NoError(err)
		calls, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
		requirements.NoError(err)
		requirements.Len(calls, 1)
		assertions.NotEqual(secondID, calls[0].MessageID)
		summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{})
		requirements.NoError(err)
		requirements.Len(summary, 1)
		assertions.Equal(1, summary[0].Answered)
	})
}

func TestCallQueriesRejectInvalidBounds(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	zero := time.Time{}
	for _, query := range []store.CallQuery{
		{Limit: 101}, {Limit: -1}, {PersonID: -1}, {After: &zero},
	} {
		_, err := st.SummarizeCallsContext(t.Context(), query)
		requirements.ErrorIs(err, callhistory.ErrInvalid)
		_, err = st.ListCallObservationsContext(t.Context(), query)
		requirements.ErrorIs(err, callhistory.ErrInvalid)
	}
	_, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{AfterID: -1})
	requirements.ErrorIs(err, callhistory.ErrInvalid)
	_, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{Offset: -1})
	requirements.ErrorIs(err, callhistory.ErrInvalid)
	_, err = st.ListCallObservationsContext(t.Context(), store.CallQuery{Offset: 1})
	requirements.ErrorIs(err, callhistory.ErrInvalid)
	_, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{AfterID: 1})
	requirements.ErrorIs(err, callhistory.ErrInvalid)
}

func TestCallsReadImportedPhoneCalls(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	remote, err := f.Store.EnsureParticipantByPhone(t.Context(), "+12025550101", "Example", "synctech_sms")
	requirements.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(remote)
	requirements.NoError(err)
	owner, err := f.Store.EnsureParticipantByPhone(t.Context(), "+12025550102", "Owner", "synctech_sms")
	requirements.NoError(err)
	_, _, err = f.Store.CreatePersonFromParticipant(owner)
	requirements.NoError(err)
	for i, seconds := range []int{600, 0} {
		message := f.NewMessage().WithSentAt(time.Date(2026, 10, 2-i, 12, 0, 0, 0, time.UTC)).Build()
		message.MessageType = store.MessageTypeSynctechCall
		id, err := f.Store.UpsertMessage(message)
		requirements.NoError(err)
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{remote}, nil))
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
		requirements.NoError(f.Store.SetMessageMetadata(id, store.CallConnectedMetadata(seconds)))
	}

	summary, err := f.Store.SummarizeCallsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	if assertions.Len(summary, 1) {
		assertions.Equal(person.ID, summary[0].PersonID)
		assertions.Equal(1, summary[0].Answered)
		assertions.Equal(1, summary[0].Unknown)
	}
	items, err := f.Store.ListCallObservationsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	requirements.NoError(err)
	requirements.Len(items, 2)
	assertions.Equal("incoming", items[0].Direction)
	assertions.Equal("answered", items[0].Outcome)
	assertions.Equal([]callhistory.Participant{{ID: remote, Participation: "joined"}}, items[0].Participants)
	assertions.Equal("unknown", items[1].Outcome)
	assertions.Equal([]callhistory.Participant{{ID: remote, Participation: "unknown"}}, items[1].Participants)

	page, err := f.Store.ListCallObservationsContext(t.Context(), store.CallQuery{PersonID: person.ID, Limit: 1})
	requirements.NoError(err)
	requirements.Len(page, 1)
	assertions.Equal(items[0].MessageID, page[0].MessageID)
	next, err := f.Store.ListCallObservationsContext(t.Context(), store.CallQuery{PersonID: person.ID, AfterID: page[0].MessageID, Limit: 1})
	requirements.NoError(err)
	requirements.Len(next, 1)
	assertions.Equal(items[1].MessageID, next[0].MessageID)

	t.Run("constant query count", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		logs := captureAttachmentQueryLogs(t)
		for _, limit := range []int{1, 2} {
			logs.Reset()
			page, err := f.Store.ListCallObservationsContext(t.Context(), store.CallQuery{Limit: limit})
			require.NoError(err)
			require.Len(page, limit)
			for i := range page {
				assert.Equal(items[i].MessageID, page[i].MessageID)
				assert.Equal(items[i].Participants, page[i].Participants)
			}
			assert.Equal(2, strings.Count(logs.String(), `"kind":"query"`), "listing calls and participants must use two queries regardless of page size")
		}
	})
}
