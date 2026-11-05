package store_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCallObservationReplacementAndWithdrawal(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	src, err := st.GetOrCreateSource(callhistory.SourceType, (callhistory.Evidence{Provider: "signal", Account: "example"}).SourceIdentifier())
	requirements.NoError(err)
	peer, err := st.EnsureParticipantByPhone("+12025550101", "Example", "signal")
	requirements.NoError(err)
	o := callhistory.Observation{
		RecordID: "native-1",

		Version: 1, Provider: "signal", Account: "example", NativeCallID: "native-1",
		Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant",

		Participants: []callhistory.Participant{{ID: peer, Participation: "joined"}},
		Raw:          []byte(`{"callId":"native-1","status":"Accepted"}`),
	}
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	again, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	assertions.Equal(id, again)
	var count int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM call_details WHERE message_id = ?`), id).Scan(&count))
	assertions.Equal(1, count)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	requirements.NoError(err)
	assertions.JSONEq(string(o.Raw), string(raw))
	var duration sql.NullFloat64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT duration_seconds FROM call_details WHERE message_id = ?`), id).Scan(&duration))
	assertions.False(duration.Valid, "unknown duration is not zero")

	o.Participants[0].Participation = "unknown"
	o.Outcome, o.OwnerParticipation = "unknown", "unknown"
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'call_joined'`), id).Scan(&count))
	assertions.Zero(count, "reimport clears stale joined attendance")

	o.Participants[0].ID = 999999
	o.Outcome = "missed"
	o.Raw = []byte(`{"status":"Missed"}`)
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.ErrorIs(err, callhistory.ErrInvalid)
	var outcome string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT outcome FROM call_details WHERE message_id = ?`), id).Scan(&outcome))
	assertions.Equal("unknown", outcome, "invalid participant rolls back scalar changes")
	unchangedRaw, err := st.GetMessageRawContext(t.Context(), id)
	requirements.NoError(err)
	assertions.JSONEq(string(raw), string(unchangedRaw))

	o.Participants = nil
	o.Withdrawn = true
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	o.Withdrawn = false
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.ErrorIs(err, store.ErrCallObservationWithdrawn)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	o.RecordID = "cancelled-write"
	_, err = st.PutCallObservationContext(ctx, src.ID, o)
	requirements.ErrorIs(err, context.Canceled)
}

func TestImportedCallMessageDirection(t *testing.T) {
	st := storetest.New(t).Store
	request := callhistory.ImportRequest{Format: "signal_desktop", Account: "example",
		Record: []byte(`{"callId":"outgoing","direction":"Outgoing","mode":"Direct","status":"Accepted","timestamp":1790841600000}`),
	}
	o, err := request.Observation()
	require.NoError(t, err)
	require.Equal(t, "outgoing", o.Direction)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	for _, direction := range []string{"outgoing", "incoming", "unknown"} {
		o.Direction = direction
		id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
		require.NoError(t, err)
		var isFromMe bool
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT is_from_me FROM messages WHERE id = ?`), id).Scan(&isFromMe))
		assert.Equal(t, direction == "outgoing", isFromMe, direction)
	}
}

func TestImportedCallConversationStats(t *testing.T) {
	st := storetest.New(t).Store
	o := callhistory.Observation{RecordID: "call", Version: 1, Provider: "signal", Account: "example",
		Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant", Raw: []byte(`{}`),
	}
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	for range 2 {
		id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
		require.NoError(t, err)
		var count int
		var latest sql.NullTime
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT c.message_count, c.last_message_at FROM conversations c JOIN messages m ON m.conversation_id = c.id WHERE m.id = ?`), id).Scan(&count, &latest))
		assert.Equal(t, 1, count)
		assert.True(t, latest.Valid)
		assert.True(t, o.OccurredAt.Equal(latest.Time))
		o.OccurredAt = o.OccurredAt.Add(time.Hour)
	}
}

func TestSparseCalendarCancellationKeepsEvent(t *testing.T) {
	testCalendarCancellationKeepsEvent(t, `{"id":"e1","status":"cancelled"}`)
}

func TestCompleteCalendarCancellationReplacesEvent(t *testing.T) {
	st := storetest.New(t).Store
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(t, err)
	o, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "example",
		Record: []byte(`{"id":"e1","start":{"date":"2026-10-01"},"attendees":[{"email":"peer@example.com"}]}`), Bindings: map[string]int64{"peer@example.com": peer},
	}).Observation()
	require.NoError(t, err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(t, err)
	cancelled, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "example",
		Record: []byte(`{"id":"e1","status":"cancelled","start":{"date":"2026-10-02"}}`),
	}).Observation()
	require.NoError(t, err)
	again, err := st.PutCallObservationContext(t.Context(), src.ID, cancelled)
	require.NoError(t, err)
	assert.Equal(t, id, again)
	items, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.True(t, cancelled.OccurredAt.Equal(items[0].OccurredAt))
	assert.Empty(t, items[0].Participants)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.JSONEq(t, string(cancelled.Raw), string(raw))
	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(t, err)
	assert.Empty(t, summary)
}

func TestNormalizedCancelledObservationWithdrawal(t *testing.T) {
	st := storetest.New(t).Store
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(t, err)
	other, err := st.EnsureParticipant("other@example.com", "Other", "example.com")
	require.NoError(t, err)
	o, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "example",
		Record: []byte(`{"id":"e1","start":{"date":"2026-10-01"},"attendees":[{"email":"peer@example.com"}]}`), Bindings: map[string]int64{"peer@example.com": peer},
	}).Observation()
	require.NoError(t, err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(t, err)
	stale := o
	o.Outcome, o.Withdrawn = "cancelled", true
	o.Participants = []callhistory.Participant{{ID: other, Participation: "invited"}}
	o.Raw = []byte(`{"replacement":true}`)
	record, err := json.Marshal(o)
	require.NoError(t, err)
	o, err = (callhistory.ImportRequest{Format: "normalized", Account: "example", Record: record}).Observation()
	require.NoError(t, err)
	again, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(t, err)
	assert.Equal(t, id, again)
	items, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.True(t, items[0].Withdrawn)
	assert.Equal(t, "cancelled", items[0].Outcome)
	assert.Equal(t, o.Participants, items[0].Participants)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.JSONEq(t, string(o.Raw), string(raw))
	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	require.NoError(t, err)
	assert.Empty(t, summary)
	_, err = st.PutCallObservationContext(t.Context(), src.ID, stale)
	assert.ErrorIs(t, err, store.ErrCallObservationWithdrawn)
}

func testCalendarCancellationKeepsEvent(t *testing.T, cancellation string) {
	st := storetest.New(t).Store
	peer, err := st.EnsureParticipant("attendee@example.com", "Example", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(t, err)
	o, err := (callhistory.ImportRequest{
		Format: "google_calendar", Account: "calendar@example.com",
		Record:   []byte(`{"id":"e1","start":{"dateTime":"2026-10-01T09:00:00Z"},"attendees":[{"email":"attendee@example.com"}]}`),
		Bindings: map[string]int64{"attendee@example.com": peer},
	}).Observation()
	require.NoError(t, err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(t, err)
	cancelled, err := (callhistory.ImportRequest{
		Format: "google_calendar", Account: "calendar@example.com", Record: []byte(cancellation),
	}).Observation()
	require.NoError(t, err)
	cancelled.RecordID = "missing"
	_, err = st.PutCallObservationContext(t.Context(), src.ID, cancelled)
	require.ErrorIs(t, err, callhistory.ErrInvalid)
	assert.ErrorContains(t, err, "calendar event to cancel does not exist")
	cancelled.RecordID = "e1"
	again, err := st.PutCallObservationContext(t.Context(), src.ID, cancelled)
	require.NoError(t, err)
	assert.Equal(t, id, again)
	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(t, err)
	require.Len(t, summary, 1)
	assert.Equal(t, 1, summary[0].Scheduled)
	assert.Equal(t, 1, summary[0].ScheduledCancelled)
	items, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.True(t, o.OccurredAt.Equal(items[0].OccurredAt))
	assert.Equal(t, "cancelled", items[0].Outcome)
	assert.Equal(t, []callhistory.Participant{{ID: peer, Participation: "invited"}}, items[0].Participants)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.JSONEq(t, string(o.Raw), string(raw))
}

func TestCallObservationDoesNotProjectMessageContact(t *testing.T) {
	assertions := assert.New(t)
	personID := int64(7)
	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		MessageType: callhistory.MessageType, ConversationType: "direct_chat",
		SourceIsFromMe: true,
		Counterparts:   []store.ActivityCounterpart{{ParticipantID: 1, PersonID: &personID, RecipientType: "call_joined"}},
	}, 10)
	assertions.Empty(got.Persons)
}

func TestCallObservationRejectsCrossAccountSource(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	o := callhistory.Observation{
		RecordID: "same-record",

		Version: 1, Provider: "signal", Account: "a", Kind: "call",
		Direction: "unknown", Outcome: "unknown", OwnerParticipation: "unknown",
		OccurredAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Precision: "instant",

		Raw: []byte(`{}`),
	}
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	requirements.NoError(err)
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	o.Account = "b"
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.ErrorIs(err, callhistory.ErrInvalid)
}

func TestCallObservationSubsetAndParticipantMerge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(callhistory.SourceType, (callhistory.Evidence{Provider: "signal", Account: "example"}).SourceIdentifier())
	requirements.NoError(err)
	old, err := st.EnsureParticipantByPhone("+12025550101", "Example", "signal")
	requirements.NoError(err)
	survivor, err := st.EnsureParticipantByPhone("+12025550102", "Example Two", "signal")
	requirements.NoError(err)
	o := callhistory.Observation{
		RecordID: "record",

		Version: 1, Provider: "signal", Account: "example", Kind: "call",
		Direction: "unknown", Outcome: "unknown", OwnerParticipation: "unknown",
		OccurredAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Precision: "instant",

		Participants: []callhistory.Participant{{ID: old, Participation: "joined"}},
		Raw:          []byte(`{"id":"record"}`),
	}
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	requirements.NoError(st.MergeParticipants(old, survivor))
	var participant int64
	requirements.NoError(st.DB().QueryRow(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'call_joined'`, id).Scan(&participant))
	assertions.Equal(survivor, participant)
	dstDir := t.TempDir()
	_, err = store.CopySubset(store.DBPathForTest(st), dstDir, 10, true)
	requirements.NoError(err)
	dst, err := store.Open(filepath.Join(dstDir, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(dst.Close()) })
	var outcome string
	requirements.NoError(dst.DB().QueryRow(`SELECT outcome FROM call_details WHERE message_id = ?`, id).Scan(&outcome))
	assertions.Equal("unknown", outcome)
	requirements.NoError(dst.DB().QueryRow(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'call_joined'`, id).Scan(&participant))
	assertions.Equal(survivor, participant)
	_, err = dst.DB().Exec(`DELETE FROM messages WHERE id = ?`, id)
	requirements.NoError(err)
	var count int
	requirements.NoError(dst.DB().QueryRow(`SELECT COUNT(*) FROM call_details`).Scan(&count))
	assertions.Zero(count, "archive deletion removes the projection through its foreign key")
}

func TestCalendarCancellationRejectsCallAndWrongSource(t *testing.T) {
	for _, provider := range []string{"signal", "google_calendar"} {
		t.Run(provider, func(t *testing.T) {
			st := storetest.New(t).Store
			o := callhistory.Observation{RecordID: "7", Version: 1, Provider: provider, Account: "a", Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined", OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant", Raw: []byte(`{}`)}
			src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
			require.NoError(t, err)
			id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
			require.NoError(t, err)
			cancelled, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "a", Record: []byte(`{"id":"7","status":"cancelled"}`)}).Observation()
			require.NoError(t, err)
			_, err = st.PutCallObservationContext(t.Context(), src.ID, cancelled)
			require.ErrorIs(t, err, callhistory.ErrInvalid)
			var outcome string
			require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT outcome FROM call_details WHERE message_id = ?`), id).Scan(&outcome))
			assert.Equal(t, "answered", outcome)
		})
	}
}

func TestCalendarCancellationRejectsWithdrawn(t *testing.T) {
	st := storetest.New(t).Store
	o, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "a", Record: []byte(`{"id":"e1","start":{"date":"2026-10-01"}}`)}).Observation()
	require.NoError(t, err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	o.Withdrawn = true
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(t, err)
	cancelled, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "a", Record: []byte(`{"id":"e1","status":"cancelled"}`)}).Observation()
	require.NoError(t, err)
	_, err = st.PutCallObservationContext(t.Context(), src.ID, cancelled)
	require.ErrorIs(t, err, store.ErrCallObservationWithdrawn)
}

func TestSignalDeletedRemovesAnsweredSummary(t *testing.T) {
	st := storetest.New(t).Store
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(t, err)
	request := callhistory.ImportRequest{Format: "signal_desktop", Account: "a", Bindings: map[string]int64{"peer": peer}, Record: []byte(`{"callId":"7","peerId":"peer","mode":"Direct","status":"Accepted","timestamp":1790841600000,"endedTimestamp":1790841660000}`)}
	o, err := request.Observation()
	require.NoError(t, err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(t, err)
	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(t, err)
	require.Len(t, summary, 1)
	require.Equal(t, 1, summary[0].Answered)
	request.Record = []byte(`{"callId":"7","peerId":"peer","mode":"Direct","status":"Deleted","timestamp":1790841720000,"endedTimestamp":1790841660000}`)
	o, err = request.Observation()
	require.NoError(t, err)
	_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(t, err)
	summary, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(t, err)
	for _, row := range summary {
		assert.Zero(t, row.Answered)
	}
}
