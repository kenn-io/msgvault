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
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportedCallReadableAndSearchable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	duration := 600.0
	o := callhistory.Observation{RecordID: "call", Version: 1, Provider: "signal", Account: "example",
		Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant",
		DurationSeconds: &duration, Raw: []byte(`{}`),
	}
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(err)
	for _, tc := range []struct{ direction, text, stale string }{
		{"incoming", "Call incoming, 600 seconds", "outgoing"},
		{"outgoing", "Call outgoing, 600 seconds", "incoming"},
	} {
		o.Direction = tc.direction
		id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
		require.NoError(err)
		var subject, title string
		var snippet sql.NullString
		var count int
		var latest sql.NullTime
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT m.subject, m.snippet, c.title, c.message_count, c.last_message_at FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE m.id = ?`), id).Scan(&subject, &snippet, &title, &count, &latest))
		assert.Equal(1, count)
		assert.True(latest.Valid)
		assert.True(o.OccurredAt.Equal(latest.Time))
		assert.Equal(tc.text, subject)
		assert.Equal(tc.text, snippet.String)
		assert.Equal(tc.text, title)
		body, err := st.GetMessageBodyTextContext(t.Context(), id)
		require.NoError(err)
		assert.Equal(tc.text, body)
		matches, total, err := st.SearchMessagesQueryContext(t.Context(), search.Parse(tc.direction+" seconds"), 0, 10)
		require.NoError(err)
		assert.EqualValues(1, total)
		if assert.Len(matches, 1) {
			assert.Equal(id, matches[0].ID)
		}
		for _, term := range []string{tc.stale, "evidence"} {
			_, total, err = st.SearchMessagesQueryContext(t.Context(), search.Parse(term), 0, 10)
			require.NoError(err)
			assert.Zero(total, term)
		}
	}
	o.DurationSeconds = nil
	o.RecordID = "outgoing"
	for _, direction := range []string{"outgoing", "incoming", "unknown"} {
		o.Direction = direction
		id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
		require.NoError(err)
		var isFromMe bool
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_from_me FROM messages WHERE id = ?`), id).Scan(&isFromMe))
		assert.Equal(direction == "outgoing", isFromMe, direction)
	}
	o.RecordID, o.Direction = "call", "incoming"
	for range 2 {
		id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
		require.NoError(err)
		var count int
		var latest sql.NullTime
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT c.message_count, c.last_message_at FROM conversations c JOIN messages m ON m.conversation_id = c.id WHERE m.id = ?`), id).Scan(&count, &latest))
		assert.Equal(1, count)
		assert.True(latest.Valid)
		assert.True(o.OccurredAt.Equal(latest.Time))
		o.OccurredAt = o.OccurredAt.Add(time.Hour)
	}
	o.OccurredAt = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(err)
	matches, total, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("incoming"), 0, 10)
	require.NoError(err)
	require.EqualValues(1, total)
	require.Len(matches, 1)
	require.Equal(id, matches[0].ID)
	o.Withdrawn = true
	again, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(err)
	require.Equal(id, again)
	matches, total, err = st.SearchMessagesQueryContext(t.Context(), search.Parse("incoming"), 0, 10)
	require.NoError(err)
	assert.Zero(total, "withdrawn calls leave archive search")
	assert.Empty(matches)
	matches, total, err = st.SearchMessagesQueryContext(t.Context(), search.Parse("subject:incoming"), 0, 10)
	require.NoError(err)
	assert.Zero(total, "withdrawn calls leave subject search")
	assert.Empty(matches)
	_, err = st.RebuildFTSContext(t.Context(), nil)
	require.NoError(err)
	matches, total, err = st.SearchMessagesQueryContext(t.Context(), search.Parse("incoming"), 0, 10)
	require.NoError(err)
	assert.Zero(total, "rebuilding search keeps withdrawn calls absent")
	assert.Empty(matches)
}

func TestWithdrawnCallAbsentFromMetadataSearch(t *testing.T) {
	requirements := require.New(t)
	st := storetest.New(t).Store
	o, err := (callhistory.ImportRequest{Format: "signal_desktop", Account: "example",
		Record: []byte(`{"callId":"withdrawn","timestamp":1790845200000,"direction":"Incoming","mode":"Direct","status":"Accepted"}`),
	}).Observation()
	requirements.NoError(err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	requirements.NoError(err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	queries := []string{"message_type:call_record", "after:2026-10-01 before:2026-10-02"}
	for _, query := range queries {
		matches, total, err := st.SearchMessagesQueryContext(t.Context(), search.Parse(query), 0, 10)
		requirements.NoError(err)
		requirements.EqualValues(1, total, query)
		requirements.Len(matches, 1, query)
		requirements.Equal(id, matches[0].ID, query)
	}
	o, err = (callhistory.ImportRequest{Format: "signal_desktop", Account: "example",
		Record: []byte(`{"callId":"withdrawn","timestamp":1790845200000,"direction":"Incoming","mode":"Direct","status":"Deleted"}`),
	}).Observation()
	requirements.NoError(err)
	again, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	requirements.Equal(id, again)
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			matches, total, err := st.SearchMessagesQueryContext(t.Context(), search.Parse(query), 0, 10)
			require.NoError(t, err)
			assert.Zero(t, total)
			assert.Empty(t, matches)
		})
	}
}

func TestListCallObservationsRejectsUnsupportedEvidenceVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	o := callhistory.Observation{RecordID: "call", Version: 1, Provider: "signal", Account: "example",
		Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant", Raw: []byte(`{}`),
	}
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(err)
	// Simulate evidence saved by a newer writer, bypassing current write validation.
	o.Version = 2
	metadata, err := json.Marshal(o.Evidence)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET metadata = ? WHERE id = ?`), string(metadata), id)
	require.NoError(err)
	_, err = st.ListCallObservationsContext(t.Context(), store.CallQuery{})
	assert.ErrorIs(err, callhistory.ErrInvalid)
}

func TestCallReimportPreservesLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	o := callhistory.Observation{RecordID: "call", Version: 1, Provider: "signal", Account: "example",
		Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant", Raw: []byte(`{}`),
	}
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(err)
	labelID, err := st.EnsureLabel(src.ID, "important", "Important", "user")
	require.NoError(err)
	require.NoError(st.AddMessageLabels(id, []int64{labelID}))

	again, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(err)
	assert.Equal(id, again)
	labels, err := st.MessageLabelIDsContext(t.Context(), again)
	require.NoError(err)
	assert.Equal([]int64{labelID}, labels)
}

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

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	o.RecordID = "cancelled-write"
	_, err = st.PutCallObservationContext(ctx, src.ID, o)
	requirements.ErrorIs(err, context.Canceled)
}

func TestCompleteCalendarCancellationReplacesEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(err)
	o, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "example",
		Record: []byte(`{"id":"e1","start":{"date":"2026-10-01"},"attendees":[{"email":"peer@example.com"}]}`), Bindings: map[string]int64{"peer@example.com": peer},
	}).Observation()
	require.NoError(err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(err)
	cancelled, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "example",
		Record: []byte(`{"id":"e1","status":"cancelled","start":{"date":"2026-10-02"}}`),
	}).Observation()
	require.NoError(err)
	again, err := st.PutCallObservationContext(t.Context(), src.ID, cancelled)
	require.NoError(err)
	assert.Equal(id, again)
	items, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
	require.NoError(err)
	require.Len(items, 1)
	assert.True(cancelled.OccurredAt.Equal(items[0].OccurredAt))
	assert.Empty(items[0].Participants)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.JSONEq(string(cancelled.Raw), string(raw))
	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(err)
	assert.Empty(summary)
}

func TestNormalizedCancelledObservationWithdrawal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(err)
	other, err := st.EnsureParticipant("other@example.com", "Other", "example.com")
	require.NoError(err)
	o, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "example",
		Record: []byte(`{"id":"e1","start":{"date":"2026-10-01"},"attendees":[{"email":"peer@example.com"}]}`), Bindings: map[string]int64{"peer@example.com": peer},
	}).Observation()
	require.NoError(err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(err)
	stale := o
	o.Outcome, o.Withdrawn = "cancelled", true
	o.Participants = []callhistory.Participant{{ID: other, Participation: "invited"}}
	o.Raw = []byte(`{"replacement":true}`)
	record, err := json.Marshal(o)
	require.NoError(err)
	o, err = (callhistory.ImportRequest{Format: "normalized", Account: "example", Record: record}).Observation()
	require.NoError(err)
	again, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	require.NoError(err)
	assert.Equal(id, again)
	items, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
	require.NoError(err)
	require.Len(items, 1)
	assert.True(items[0].Withdrawn)
	assert.Equal("cancelled", items[0].Outcome)
	assert.Equal(o.Participants, items[0].Participants)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.JSONEq(string(o.Raw), string(raw))
	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	require.NoError(err)
	assert.Empty(summary)
	_, err = st.PutCallObservationContext(t.Context(), src.ID, stale)
	require.ErrorIs(err, store.ErrCallObservationWithdrawn)
	cancelled, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "example", Record: []byte(`{"id":"e1","status":"cancelled"}`)}).Observation()
	require.NoError(err)
	_, err = st.PutCallObservationContext(t.Context(), src.ID, cancelled)
	require.ErrorIs(err, store.ErrCallObservationWithdrawn)
}

func TestSparseCalendarCancellationPreservesDirection(t *testing.T) {
	for _, direction := range []string{"outgoing", "incoming", "unknown"} {
		t.Run(direction, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := storetest.New(t).Store
			o := callhistory.Observation{
				RecordID: "e1", Version: 1, Provider: "google_calendar", Account: "calendar@example.com",
				Kind: "scheduled_meeting", Direction: direction, Outcome: "unknown", OwnerParticipation: "unknown",
				OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant", Raw: []byte(`{}`),
			}
			src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
			require.NoError(err)
			id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
			require.NoError(err)
			var fromMe bool
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_from_me FROM messages WHERE id = ?`), id).Scan(&fromMe))
			require.Equal(direction == "outgoing", fromMe)
			cancelled, err := (callhistory.ImportRequest{
				Format: "google_calendar", Account: "calendar@example.com", Record: []byte(`{"id":"e1","status":"cancelled"}`),
			}).Observation()
			require.NoError(err)
			again, err := st.PutCallObservationContext(t.Context(), src.ID, cancelled)
			require.NoError(err)
			assert.Equal(id, again)
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_from_me FROM messages WHERE id = ?`), id).Scan(&fromMe))
			assert.Equal(direction == "outgoing", fromMe)
			items, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
			require.NoError(err)
			require.Len(items, 1)
			assert.Equal(direction, items[0].Direction)
			assert.Equal("cancelled", items[0].Outcome)
		})
	}
}

func TestSparseCalendarCancellationKeepsEvent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	peer, err := st.EnsureParticipant("attendee@example.com", "Example", "example.com")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	o, err := (callhistory.ImportRequest{
		Format: "google_calendar", Account: "calendar@example.com",
		Record:   []byte(`{"id":"e1","start":{"dateTime":"2026-10-01T09:00:00Z"},"attendees":[{"email":"attendee@example.com"}]}`),
		Bindings: map[string]int64{"attendee@example.com": peer},
	}).Observation()
	requirements.NoError(err)
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	requirements.NoError(err)
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	cancelled, err := (callhistory.ImportRequest{
		Format: "google_calendar", Account: "calendar@example.com", Record: []byte(`{"id":"e1","status":"cancelled"}`),
	}).Observation()
	requirements.NoError(err)
	cancelled.RecordID = "missing"
	_, err = st.PutCallObservationContext(t.Context(), src.ID, cancelled)
	requirements.ErrorIs(err, callhistory.ErrInvalid)
	requirements.ErrorContains(err, "calendar event to cancel does not exist")
	t.Run("non-scheduled record", func(t *testing.T) {
		requirements := require.New(t)
		st := storetest.New(t).Store
		o := callhistory.Observation{RecordID: "7", Version: 1, Provider: "google_calendar", Account: "a", Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined", OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant", Raw: []byte(`{}`)}
		src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
		requirements.NoError(err)
		id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
		requirements.NoError(err)
		cancelled, err := (callhistory.ImportRequest{Format: "google_calendar", Account: "a", Record: []byte(`{"id":"7","status":"cancelled"}`)}).Observation()
		requirements.NoError(err)
		_, err = st.PutCallObservationContext(t.Context(), src.ID, cancelled)
		requirements.ErrorIs(err, callhistory.ErrInvalid)
		var outcome string
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT outcome FROM call_details WHERE message_id = ?`), id).Scan(&outcome))
		assert.Equal(t, "answered", outcome)
	})
	t.Run("non-calendar record", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		requirements := require.New(t)
		o := callhistory.Observation{RecordID: "same-record", Version: 1, Provider: "signal", Account: "a", Kind: "call", Direction: "unknown", Outcome: "unknown", OwnerParticipation: "unknown", OccurredAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Precision: "instant", Raw: []byte(`{}`)}
		src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
		requirements.NoError(err)
		_, err = st.PutCallObservationContext(t.Context(), src.ID, o)
		requirements.NoError(err)
		cancelled, err := (callhistory.ImportRequest{
			Format: "google_calendar", Account: "a", Record: []byte(`{"id":"same-record","status":"cancelled"}`),
		}).Observation()
		requirements.NoError(err)
		_, err = st.PutCallObservationContext(t.Context(), src.ID, cancelled)
		requirements.ErrorIs(err, callhistory.ErrInvalid)
	})
	cancelled.RecordID = "e1"
	again, err := st.PutCallObservationContext(t.Context(), src.ID, cancelled)
	requirements.NoError(err)
	assertions.Equal(id, again)
	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	requirements.NoError(err)
	requirements.Len(summary, 1)
	assertions.Equal(1, summary[0].Scheduled)
	assertions.Equal(1, summary[0].ScheduledCancelled)
	items, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.True(o.OccurredAt.Equal(items[0].OccurredAt))
	assertions.Equal("cancelled", items[0].Outcome)
	assertions.Equal([]callhistory.Participant{{ID: peer, Participation: "invited"}}, items[0].Participants)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	requirements.NoError(err)
	assertions.JSONEq(string(o.Raw), string(raw))
}

func TestCallObservationDoesNotProjectMessageContact(t *testing.T) {
	assertions := assert.New(t)
	personID := int64(7)
	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		MessageType: callhistory.MessageType, ConversationType: "call_history",
		SourceIsFromMe: true,
		Counterparts:   []store.ActivityCounterpart{{ParticipantID: 1, PersonID: &personID, RecipientType: "call_joined"}},
	}, 10)
	assertions.Empty(got.Persons)
	assertions.Equal(store.DirectionObserved, got.Direction)
	assertions.Equal(store.ChannelChat, got.Channel)
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
