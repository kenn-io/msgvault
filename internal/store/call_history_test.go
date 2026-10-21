package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
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

func TestCallObservationDoesNotProjectMessageContact(t *testing.T) {
	assertions := assert.New(t)
	personID := int64(7)
	ownerPersonID := int64(8)
	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		SourceID: 9, MessageType: callhistory.MessageType, ConversationType: "direct_chat",
		SourceIsFromMe: true,
		Counterparts: []store.ActivityCounterpart{
			{ParticipantID: 1, PersonID: &personID, RecipientType: "call_joined"},
			{ParticipantID: 2, PersonID: &ownerPersonID, RecipientType: "to",
				IsOwner: true, OwnerAddress: "owner@example.test"},
		},
	}, 10)
	assertions.Equal(store.RefKindMessage, got.RefKind)
	assertions.Equal(store.ChannelChat, got.Channel)
	assertions.Equal(store.DirectionObserved, got.Direction)
	assertions.Nil(got.OwnerSourceID)
	assertions.Empty(got.OwnerAddress)
	assertions.Empty(got.Persons)
}

func TestCallObservationClassificationUsesConversationChannel(t *testing.T) {
	tests := []struct {
		conversationType string
		wantChannel      store.ActivityChannel
	}{
		{conversationType: "email_thread", wantChannel: store.ChannelEmail},
		{conversationType: "chat", wantChannel: store.ChannelChat},
		{conversationType: "group_chat", wantChannel: store.ChannelChat},
		{conversationType: "direct_chat", wantChannel: store.ChannelChat},
		{conversationType: "channel", wantChannel: store.ChannelChat},
		{conversationType: "call_history", wantChannel: store.ChannelOther},
		{conversationType: "meeting", wantChannel: store.ChannelOther},
		{conversationType: "future_type", wantChannel: store.ChannelOther},
	}
	for _, test := range tests {
		t.Run(test.conversationType, func(t *testing.T) {
			assertions := assert.New(t)
			got := store.ClassifyActivityCandidate(store.ActivityCandidate{
				MessageType: callhistory.MessageType, ConversationType: test.conversationType,
			}, 25)
			assertions.Equal(test.wantChannel, got.Channel)
		})
	}
}

func FuzzCallObservationIgnoresOwnerAndRosterMetadata(f *testing.F) {
	f.Add("call_history", false, int64(1), "call_joined", false, "owner@example.test", 25)
	f.Add("direct_chat", true, int64(9), "from", true, "owner@example.test", -1)
	f.Fuzz(func(
		t *testing.T,
		conversationType string,
		sourceIsFromMe bool,
		sourceID int64,
		recipientType string,
		isOwner bool,
		ownerAddress string,
		maxDirect int,
	) {
		assertions := assert.New(t)
		personID := int64(42)
		ownerPersonID := int64(43)
		candidate := store.ActivityCandidate{
			MessageType: callhistory.MessageType, ConversationType: conversationType,
			SourceIsFromMe: sourceIsFromMe, SourceID: sourceID,
			Counterparts: []store.ActivityCounterpart{
				{ParticipantID: 1, PersonID: &personID, RecipientType: recipientType,
					IsOwner: isOwner, OwnerAddress: ownerAddress},
				{ParticipantID: 2, PersonID: &ownerPersonID, RecipientType: "call_unknown",
					IsOwner: !isOwner, OwnerAddress: ownerAddress},
			},
		}
		got := store.ClassifyActivityCandidate(candidate, maxDirect)
		assertions.Equal(store.RefKindMessage, got.RefKind)
		assertions.Equal(store.DirectionObserved, got.Direction)
		assertions.Nil(got.OwnerSourceID)
		assertions.Empty(got.OwnerAddress)
		assertions.Empty(got.Persons)
	})
}

func TestPersonMergeReclassifiesCallWithoutContactLinks(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ctx := t.Context()
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource(
		callhistory.SourceType,
		(callhistory.Evidence{Provider: "teams", Account: "example"}).SourceIdentifier(),
	)
	requirements.NoError(err)
	peer, err := f.Store.EnsureParticipantByIdentifier("teams", "peer", "Example Peer")
	requirements.NoError(err)
	other, err := f.Store.EnsureParticipantByIdentifier("teams", "other", "Example Other")
	requirements.NoError(err)
	peerPerson, _, err := f.Store.CreatePersonFromParticipantContext(ctx, peer)
	requirements.NoError(err)
	otherPerson, _, err := f.Store.CreatePersonFromParticipantContext(ctx, other)
	requirements.NoError(err)
	messageID, err := f.Store.PutCallObservationContext(ctx, source.ID, callhistory.Observation{
		RecordID: "event-merge",
		Version:  1, Provider: "teams", Account: "example", NativeCallID: "call-merge",
		Kind: "call", Direction: "unknown", Outcome: "unknown", OwnerParticipation: "unknown",
		OccurredAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), Precision: "instant",
		Participants: []callhistory.Participant{{ID: peer, Participation: "joined"}},
		Raw:          []byte(`{"id":"event-merge"}`),
	})
	requirements.NoError(err)
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 1})
	requirements.NoError(err)
	_, err = projector.RunOnce(ctx)
	requirements.NoError(err)

	// A stale link models a projection written before call records were
	// excluded from ordinary contact activity. The merge repair must clear it.
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
		INSERT INTO activity_event_persons(message_id, person_id, role, evidence, local_date)
		VALUES (?, ?, 'addressed', 'direct', '2026-10-01')
	`), messageID, peerPerson.ID)
	requirements.NoError(err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: peerPerson.ID, AbsorbedID: otherPerson.ID,
		ExpectedSurvivorRevision: peerPerson.Revision,
		ExpectedAbsorbedRevision: otherPerson.Revision,
		IdempotencyKey:           "call-record-identity-repair", Actor: "test",
	})
	requirements.NoError(err)
	requirements.Equal(peerPerson.ID, merged.Person.ID)
	var links int
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`
		SELECT COUNT(*) FROM activity_event_persons WHERE message_id = ?
	`), messageID).Scan(&links))
	assertions.Zero(links)
	var channel, direction string
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`
		SELECT channel, direction FROM activity_events WHERE message_id = ?
	`), messageID).Scan(&channel, &direction))
	assertions.Equal(string(store.ChannelOther), channel)
	assertions.Equal(string(store.DirectionObserved), direction)
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
