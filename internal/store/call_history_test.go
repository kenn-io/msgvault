package store_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
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

func validCallObservation(t *testing.T, st *store.Store) (callhistory.Observation, int64) {
	t.Helper()
	o := callhistory.Observation{RecordID: "call", Version: 1, Provider: "signal", Account: "example",
		Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Raw: []byte(`{}`),
	}
	src, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(t, err)
	return o, src.ID
}

func TestCallObservationSearchAndMessageProjections(t *testing.T) {
	t.Run("normalized timezone offsets", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := storetest.New(t).Store
		o, sourceID := validCallObservation(t, st)
		var ids []int64
		for i, timestamp := range []string{"2026-10-01T00:30:00-04:00", "2026-10-01T01:30:00Z"} {
			o.RecordID = fmt.Sprintf("offset-call-%d", i)
			var err error
			o.OccurredAt, err = time.Parse(time.RFC3339, timestamp)
			require.NoError(err)
			o.EndedAt = new(o.OccurredAt.Add(time.Minute))
			o.Raw = []byte(fmt.Sprintf(`{"occurred_at":%q}`, timestamp))
			record, err := json.Marshal(o)
			require.NoError(err)
			imported, err := (callhistory.ImportRequest{Format: "normalized", Account: o.Account, Record: record}).Observation()
			require.NoError(err)
			id, err := st.PutCallObservationContext(t.Context(), sourceID, imported)
			require.NoError(err)
			ids = append(ids, id)
			raw, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(string(o.Raw), string(raw))
		}
		matches, total, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("message_type:call_record"), 0, 10)
		require.NoError(err)
		require.EqualValues(2, total)
		require.Len(matches, 2)
		assert.Equal([]int64{ids[0], ids[1]}, []int64{matches[0].ID, matches[1].ID})
		after := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
		before := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)
		query := search.Parse("message_type:call_record")
		query.AfterDate, query.BeforeDate = &after, &before
		matches, total, err = st.SearchMessagesQueryContext(t.Context(), query, 0, 10)
		require.NoError(err)
		assert.EqualValues(1, total)
		if assert.Len(matches, 1) {
			assert.Equal(ids[0], matches[0].ID)
		}
		calls, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{After: &after, Before: &before})
		require.NoError(err)
		require.Len(calls, 1)
		assert.Equal(ids[0], calls[0].MessageID)
		assert.Equal("2026-10-01T04:30:00Z", calls[0].OccurredAt.Format(time.RFC3339))
		require.NotNil(calls[0].EndedAt)
		assert.Equal("2026-10-01T04:31:00Z", calls[0].EndedAt.Format(time.RFC3339))
	})
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	duration := 600.0
	o, sourceID := validCallObservation(t, st)
	o.DurationSeconds = &duration
	for _, tc := range []struct{ direction, text, stale string }{
		{"incoming", "Call incoming, 600 seconds", "outgoing"},
		{"outgoing", "Call outgoing, 600 seconds", "incoming"},
	} {
		o.Direction = tc.direction
		id, err := st.PutCallObservationContext(t.Context(), sourceID, o)
		require.NoError(err)
		var title string
		var subject, snippet sql.NullString
		var count int
		var latest sql.NullTime
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT m.subject, m.snippet, c.title, c.message_count, c.last_message_at FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE m.id = ?`), id).Scan(&subject, &snippet, &title, &count, &latest))
		assert.Equal(1, count)
		assert.True(latest.Valid)
		assert.True(o.OccurredAt.Equal(latest.Time))
		assert.Equal(tc.text, subject.String)
		assert.False(snippet.Valid)
		assert.Equal(tc.text, title)
		body, err := st.GetMessageBodyTextContext(t.Context(), id)
		require.NoError(err)
		assert.Empty(body)
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
	for _, direction := range []string{"outgoing", "incoming", "unknown"} {
		o.Direction = direction
		id, err := st.PutCallObservationContext(t.Context(), sourceID, o)
		require.NoError(err)
		var isFromMe bool
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_from_me FROM messages WHERE id = ?`), id).Scan(&isFromMe))
		assert.Equal(direction == "outgoing", isFromMe, direction)
	}
	o.RecordID, o.Direction = "call", "incoming"
	for range 2 {
		id, err := st.PutCallObservationContext(t.Context(), sourceID, o)
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
	id, err := st.PutCallObservationContext(t.Context(), sourceID, o)
	require.NoError(err)
	matches, total, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("incoming"), 0, 10)
	require.NoError(err)
	require.EqualValues(1, total)
	require.Len(matches, 1)
	require.Equal(id, matches[0].ID)
	queries := []string{"message_type:call_record", "after:2026-10-01 before:2026-10-02"}
	for _, query := range queries {
		matches, total, err := st.SearchMessagesQueryContext(t.Context(), search.Parse(query), 0, 10)
		require.NoError(err)
		require.EqualValues(1, total, query)
		require.Len(matches, 1, query)
		assert.Equal(id, matches[0].ID, query)
	}
	peerID := f.NewMessage().WithSubject("Call incoming").WithSentAt(o.OccurredAt.Add(time.Hour)).Create(t, st)
	require.NoError(st.UpsertFTS(peerID, "Call incoming", "", "", "", ""))
	matches, total, err = st.SearchMessagesQueryContext(t.Context(), search.Parse("incoming"), 0, 10)
	require.NoError(err)
	require.EqualValues(2, total)
	var importedOrder []int64
	for _, match := range matches {
		importedOrder = append(importedOrder, match.ID)
	}
	assert.Equal([]int64{peerID, id}, importedOrder)
	_, err = st.RebuildFTSContext(t.Context(), nil)
	require.NoError(err)
	matches, total, err = st.SearchMessagesQueryContext(t.Context(), search.Parse("incoming"), 0, 10)
	require.NoError(err)
	assert.EqualValues(2, total)
	var rebuiltOrder []int64
	for _, match := range matches {
		rebuiltOrder = append(rebuiltOrder, match.ID)
	}
	assert.Equal(importedOrder, rebuiltOrder)
}

func TestCallReadersRejectUnsupportedEvidenceVersion(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	o, sourceID := validCallObservation(t, st)
	id, err := st.PutCallObservationContext(t.Context(), sourceID, o)
	require.NoError(err)
	// Simulate unsupported persisted evidence, bypassing current write validation.
	o.Version = 2
	metadata, err := json.Marshal(o.Evidence)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET metadata = ? WHERE id = ?`), string(metadata), id)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE call_details SET evidence_version = ? WHERE message_id = ?`), o.Version, id)
	require.NoError(err)
	_, err = st.ListCallObservationsContext(t.Context(), store.CallQuery{})
	require.ErrorIs(err, callhistory.ErrInvalid)
	_, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	require.ErrorIs(err, callhistory.ErrInvalid)
}

func TestCallReimportPreservesLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	o, sourceID := validCallObservation(t, st)
	id, err := st.PutCallObservationContext(t.Context(), sourceID, o)
	require.NoError(err)
	labelID, err := st.EnsureLabel(sourceID, "important", "Important", "user")
	require.NoError(err)
	require.NoError(st.AddMessageLabels(id, []int64{labelID}))

	again, err := st.PutCallObservationContext(t.Context(), sourceID, o)
	require.NoError(err)
	assert.Equal(id, again)
	labels, err := st.MessageLabelIDsContext(t.Context(), again)
	require.NoError(err)
	assert.Equal([]int64{labelID}, labels)
}

func TestCallObservationReplacement(t *testing.T) {
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
		Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),

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
	assertions.Equal("unknown", outcome, "participant validation rejects the update before scalar writes")
	unchangedRaw, err := st.GetMessageRawContext(t.Context(), id)
	requirements.NoError(err)
	assertions.JSONEq(string(raw), string(unchangedRaw))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	o.RecordID = "cancelled-write"
	_, err = st.PutCallObservationContext(ctx, src.ID, o)
	requirements.ErrorIs(err, context.Canceled)
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
	assertions.Equal(store.ChannelOther, got.Channel)
}

func TestCallObservationParticipantMergeSubsetCopyAndDeletion(t *testing.T) {
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

		Version: 1, Provider: "signal", Account: "example",
		Direction: "unknown", Outcome: "unknown", OwnerParticipation: "unknown",
		OccurredAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),

		Participants: []callhistory.Participant{{ID: old, Participation: "joined"}},
		Raw:          []byte(`{"id":"record"}`),
	}
	id, err := st.PutCallObservationContext(t.Context(), src.ID, o)
	requirements.NoError(err)
	t.Run("participant merge", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		requirements.NoError(st.MergeParticipants(old, survivor))
		var participant int64
		requirements.NoError(st.DB().QueryRow(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'call_joined'`, id).Scan(&participant))
		assertions.Equal(survivor, participant)
	})
	dstDir := t.TempDir()
	_, err = store.CopySubset(store.DBPathForTest(st), dstDir, 10, true)
	requirements.NoError(err)
	dst, err := store.Open(filepath.Join(dstDir, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(dst.Close()) })
	t.Run("subset copy", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var participant int64
		var outcome string
		requirements.NoError(dst.DB().QueryRow(`SELECT outcome FROM call_details WHERE message_id = ?`, id).Scan(&outcome))
		assertions.Equal("unknown", outcome)
		requirements.NoError(dst.DB().QueryRow(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'call_joined'`, id).Scan(&participant))
		assertions.Equal(survivor, participant)
	})
	t.Run("message deletion cascades to call projection", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		_, err = dst.DB().Exec(`DELETE FROM messages WHERE id = ?`, id)
		requirements.NoError(err)
		var count int
		requirements.NoError(dst.DB().QueryRow(`SELECT COUNT(*) FROM call_details`).Scan(&count))
		assertions.Zero(count, "archive deletion removes the projection through its foreign key")
	})
}
