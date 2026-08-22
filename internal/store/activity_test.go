package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

var activityTableColumns = map[string][]string{
	"activity_events": {
		"message_id", "ref_kind", "source_id", "conversation_id", "channel",
		"occurred_at", "date_origin", "date_precision", "timezone",
		"utc_offset_minutes", "local_date", "direction", "owner_source_id",
		"owner_address", "projected_last_modified", "projected_identity_revision",
		"projected_account_identity_revision", "created_at",
	},
	"activity_event_persons": {
		"message_id", "person_id", "role", "evidence", "local_date",
	},
	"person_contact_state": {
		"person_id", "first_contact_at", "first_contact_message_id",
		"last_contact_at", "last_contact_message_id", "last_contact_channel",
		"last_contact_source_id", "last_contact_owner", "last_inbound_at",
		"last_inbound_message_id", "last_outbound_at", "last_outbound_message_id",
		"interaction_count", "identity_revision", "account_identity_revision",
		"dirty_at", "computed_at",
	},
	"activity_projection_queue": {
		"message_id", "revision", "processed_revision", "queued_at",
	},
}

func TestActivitySchemaHasPortableColumns(t *testing.T) {
	st := testutil.NewTestStore(t)

	for table, want := range activityTableColumns {
		t.Run(table, func(t *testing.T) {
			got := activityColumns(t, st, table)
			assert.Equal(t, want, got)
		})
	}
}

func TestActivityEventRefIsStableAndNative(t *testing.T) {
	event := store.ActivityEvent{MessageID: 4242, RefKind: store.RefKindMeeting}
	assert.Equal(t, "meeting:4242", event.Ref())
}

func TestActivityValueValidationRejectsUnknownValues(t *testing.T) {
	assert := assert.New(t)
	assert.True(store.DirectionInbound.Valid())
	assert.True(store.ChannelMeeting.Valid())
	assert.True(store.RoleAttendee.Valid())
	assert.True(store.EvidenceDirect.Valid())
	assert.True(store.RefKindMessage.Valid())

	assert.False(store.ActivityDirection("sideways").Valid())
	assert.False(store.ActivityChannel("fax").Valid())
	assert.False(store.ActivityRole("spectator").Valid())
	assert.False(store.ActivityEvidence("hearsay").Valid())
	assert.False(store.ActivityRefKind("note").Valid())
}

func TestActivityMetadataProtocolsRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	watermark, err := st.ActivityWatermarkContext(ctx)
	require.NoError(err)
	assert.Zero(watermark)

	require.NoError(st.SetActivityWatermarkContext(ctx, 99))
	watermark, err = st.ActivityWatermarkContext(ctx)
	require.NoError(err)
	assert.Equal(int64(99), watermark)
	require.NoError(st.SetActivityWatermarkContext(ctx, 42))
	watermark, err = st.ActivityWatermarkContext(ctx)
	require.NoError(err)
	assert.Equal(int64(99), watermark,
		"repairing work below the frontier cannot lower the durable watermark")

	timezone, err := st.ActivityTimezoneTransitionContext(ctx)
	require.NoError(err)
	assert.Equal(store.ActivityTimezoneTransition{}, timezone)

	timezone, err = st.ClaimActivityTimezoneTransitionContext(
		ctx, "America/New_York")
	require.NoError(err)
	assert.Equal(store.ActivityTimezoneTransition{
		Active: true, Target: "America/New_York", Generation: 1,
	}, timezone)

	require.NoError(st.CompleteActivityTimezoneTransitionContext(ctx, timezone))
	timezone, err = st.ActivityTimezoneTransitionContext(ctx)
	require.NoError(err)
	assert.Equal(store.ActivityTimezoneTransition{
		Target: "America/New_York", Generation: 1,
	}, timezone)

	directLimit, err := st.ActivityDirectLimitTransitionContext(ctx)
	require.NoError(err)
	assert.Equal(store.ActivityDirectLimitTransition{}, directLimit)
	directLimit, err = st.ClaimActivityDirectLimitTransitionContext(ctx, 25)
	require.NoError(err)
	assert.Equal(store.ActivityDirectLimitTransition{
		Active: true, Target: 25, Generation: 1,
	}, directLimit)
	require.NoError(st.CompleteActivityDirectLimitTransitionContext(
		ctx, directLimit))
	directLimit, err = st.ActivityDirectLimitTransitionContext(ctx)
	require.NoError(err)
	assert.Equal(store.ActivityDirectLimitTransition{
		Target: 25, Generation: 1,
	}, directLimit)
}

func TestActivityCandidateLoadingPreservesExactTimezoneAndQueueObservations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	first := f.NewMessage().
		WithSourceMessageID("activity-exact-first").
		WithSentAt(time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	second := f.NewMessage().
		WithSourceMessageID("activity-exact-second").
		WithSentAt(time.Date(2026, 7, 31, 13, 0, 0, 0, time.UTC)).
		Create(t, f.Store)

	_, err := activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id = ?
	`, first)
	require.NoError(err)
	_, err = activityExec(f.Store,
		`DELETE FROM activity_projection_queue WHERE message_id = ?`, second)
	require.NoError(err)

	transition, err := f.Store.ClaimActivityTimezoneTransitionContext(
		t.Context(), "America/New_York")
	require.NoError(err)
	require.True(transition.Active)
	assert.Equal(int64(1), transition.Generation)
	directLimit, err := f.Store.ClaimActivityDirectLimitTransitionContext(
		t.Context(), 25)
	require.NoError(err)
	require.True(directLimit.Active)

	candidates, err := f.Store.LoadActivityCandidatesByIDContext(
		t.Context(), []int64{second, first, second})
	require.NoError(err)
	require.Len(candidates, 2)
	assert.Equal([]int64{first, second},
		[]int64{candidates[0].MessageID, candidates[1].MessageID})
	assert.True(candidates[0].Queue.Exists)
	assert.Equal(candidates[0].Queue.Revision, candidates[0].Queue.ProcessedRevision)
	assert.False(candidates[1].Queue.Exists)
	for _, candidate := range candidates {
		assert.True(candidate.TimezoneTransition.Active)
		assert.Equal("America/New_York", candidate.TimezoneTransition.Target)
		assert.Equal(int64(1), candidate.TimezoneTransition.Generation)
		assert.Equal(directLimit, candidate.DirectLimitTransition)
	}

	all, err := f.Store.ScanAllActivityCandidatesContext(t.Context(), 0, 1)
	require.NoError(err)
	require.Len(all, 1)
	assert.Equal(first, all[0].MessageID)
}

func TestActivityWatermarkRejectsMalformedMetadata(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	for _, value := range []string{"not-an-integer", "-1"} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
			INSERT INTO archive_metadata (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value
		`), "activity_spine_watermark", value)
		require.NoError(err)

		_, err = st.ActivityWatermarkContext(t.Context())
		require.ErrorContains(err, "invalid activity watermark")
	}
}

func TestActivityTimezoneTransitionRejectsOlderProjectionAndCompletesExactly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("activity-timezone-generation").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	stale := activityProjectionForMessage(t, f, messageID, personID, occurredAt)

	transition, err := f.Store.ClaimActivityTimezoneTransitionContext(
		t.Context(), "America/New_York")
	require.NoError(err)
	assert.True(transition.Active)

	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{stale})
	var staleErr *store.ErrActivityProjectionStale
	require.ErrorAs(err, &staleErr)
	assert.Contains(staleErr.Reason, "timezone transition")

	candidate, err := f.Store.LoadActivityCandidatesByIDContext(
		t.Context(), []int64{messageID})
	require.NoError(err)
	require.Len(candidate, 1)
	fresh := activityProjectionFromCandidate(t, candidate[0], personID, occurredAt)
	fresh.Event.Timezone = "UTC"
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{fresh})
	require.ErrorIs(err, store.ErrInvalidActivity)
	fresh = activityProjectionFromCandidate(t, candidate[0], personID, occurredAt)
	fresh.Event.Timezone = "America/New_York"
	fresh.Event.UTCOffsetMinutes = -240
	fresh.Event.LocalDate = "2026-07-31"
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{fresh})
	require.NoError(err)

	require.NoError(f.Store.CompleteActivityTimezoneTransitionContext(
		t.Context(), transition))
	completed, err := f.Store.ActivityTimezoneTransitionContext(t.Context())
	require.NoError(err)
	assert.False(completed.Active)
	assert.Equal("America/New_York", completed.Target)
	assert.Equal(transition.Generation, completed.Generation)

	err = f.Store.CompleteActivityTimezoneTransitionContext(t.Context(), transition)
	require.ErrorAs(err, &staleErr)
}

func TestActivityTimezoneTransitionRejectsCorruptActiveMetadata(t *testing.T) {
	f := storetest.New(t)
	_, err := activityExec(f.Store, `
		INSERT INTO archive_metadata (key, value) VALUES
			('activity_spine_timezone_active', 'Not/AZone'),
			('activity_spine_timezone_generation', '0')
	`)
	require.NoError(t, err)

	_, err = f.Store.ActivityTimezoneTransitionContext(t.Context())
	assert.Error(t, err)
}

func TestActivityReconciledRevisionCASRequiresCurrentEpoch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)

	got, reconciled, err := f.Store.ActivityReconciledRevisionsContext(t.Context())
	require.NoError(err)
	assert.False(reconciled)
	assert.Zero(got)

	require.NoError(f.Store.CompareAndSetActivityReconciledRevisionsContext(
		t.Context(), revisions))
	got, reconciled, err = f.Store.ActivityReconciledRevisionsContext(t.Context())
	require.NoError(err)
	assert.True(reconciled)
	assert.Equal(revisions, got)

	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "reconciled-cas@example.com", "manual"))
	err = f.Store.CompareAndSetActivityReconciledRevisionsContext(
		t.Context(), revisions)
	var staleErr *store.ErrActivityProjectionStale
	require.ErrorAs(err, &staleErr)
}

func TestMessageMutationsDurablyQueueProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("queue-message")
	initial := activityQueueRevision(t, f.Store, messageID)
	require.Positive(initial)

	_, err := activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("changed"), messageID)
	require.NoError(err)
	assert.Greater(activityQueueRevision(t, f.Store, messageID), initial)

	items, err := f.Store.ListActivityProjectionQueueContext(context.Background(), 10)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(messageID, items[0].MessageID)
	assert.Equal(activityQueueRevision(t, f.Store, messageID), items[0].Revision)
}

func TestExistingMessageMutationQueuesBelowWatermark(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("queue-below-watermark")
	require.NoError(t, f.Store.SetActivityWatermarkContext(context.Background(), messageID+1000))

	_, err := activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("repaired"), messageID)
	require.NoError(t, err)
	assert.Positive(t, activityQueueRevision(t, f.Store, messageID))
}

func TestActivitySchemaReinitKeepsQueueTriggers(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("queue-after-reinit")
	before := activityQueueRevision(t, f.Store, messageID)

	require.NoError(t, f.Store.InitSchema())
	_, err := activityExec(
		f.Store, `UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("after reinit"), messageID)
	require.NoError(t, err)
	assert.Greater(t, activityQueueRevision(t, f.Store, messageID), before)
}

func TestRecipientMutationsQueueOldAndNewMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	firstMessageID := f.CreateMessage("queue-recipient-first")
	secondMessageID := f.CreateMessage("queue-recipient-second")
	participantID := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	firstBefore := activityQueueRevision(t, f.Store, firstMessageID)
	secondBefore := activityQueueRevision(t, f.Store, secondMessageID)

	require.NoError(f.Store.ReplaceMessageRecipients(
		firstMessageID, "to", []int64{participantID}, []string{"Alice"}))
	firstAfterInsert := activityQueueRevision(t, f.Store, firstMessageID)
	assert.Greater(firstAfterInsert, firstBefore)

	_, err := activityExec(f.Store, `
		UPDATE message_recipients SET message_id = ? WHERE message_id = ? AND participant_id = ?
	`, secondMessageID, firstMessageID, participantID)
	require.NoError(err)
	assert.Greater(activityQueueRevision(t, f.Store, firstMessageID), firstAfterInsert)
	assert.Greater(activityQueueRevision(t, f.Store, secondMessageID), secondBefore)

	secondAfterMove := activityQueueRevision(t, f.Store, secondMessageID)
	require.NoError(f.Store.ReplaceMessageRecipients(secondMessageID, "to", nil, nil))
	assert.Greater(activityQueueRevision(t, f.Store, secondMessageID), secondAfterMove)
}

func TestConversationMemberMutationsQueueEveryAffectedMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	firstMessageID := f.CreateMessage("queue-member-first")
	secondMessageID := f.CreateMessage("queue-member-second")
	otherConversationID, err := f.Store.EnsureConversation(
		f.Source.ID, "other-thread", "Other Thread")
	require.NoError(err)
	otherMessageID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  otherConversationID,
		SourceID:        f.Source.ID,
		SourceMessageID: "queue-member-other",
		MessageType:     "email",
	})
	require.NoError(err)
	participantID := f.EnsureParticipant("bob@example.com", "Bob", "example.com")

	firstBefore := activityQueueRevision(t, f.Store, firstMessageID)
	secondBefore := activityQueueRevision(t, f.Store, secondMessageID)
	otherBefore := activityQueueRevision(t, f.Store, otherMessageID)
	require.NoError(f.Store.EnsureConversationParticipant(
		f.ConvID, participantID, "member"))
	firstAfterInsert := activityQueueRevision(t, f.Store, firstMessageID)
	secondAfterInsert := activityQueueRevision(t, f.Store, secondMessageID)
	assert.Greater(firstAfterInsert, firstBefore)
	assert.Greater(secondAfterInsert, secondBefore)

	_, err = activityExec(f.Store, `
		UPDATE conversation_participants SET conversation_id = ?
		WHERE conversation_id = ? AND participant_id = ?
	`, otherConversationID, f.ConvID, participantID)
	require.NoError(err)
	assert.Greater(activityQueueRevision(t, f.Store, firstMessageID), firstAfterInsert)
	assert.Greater(activityQueueRevision(t, f.Store, secondMessageID), secondAfterInsert)
	assert.Greater(activityQueueRevision(t, f.Store, otherMessageID), otherBefore)

	otherAfterMove := activityQueueRevision(t, f.Store, otherMessageID)
	_, err = activityExec(f.Store, `
		DELETE FROM conversation_participants
		WHERE conversation_id = ? AND participant_id = ?
	`, otherConversationID, participantID)
	require.NoError(err)
	assert.Greater(activityQueueRevision(t, f.Store, otherMessageID), otherAfterMove)
}

func TestConversationTypeMutationReopensEveryAffectedActivityCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	first := f.NewMessage().
		WithSourceMessageID("queue-conversation-type-first").
		WithSentAt(sent).
		Create(t, f.Store)
	second := f.NewMessage().
		WithSourceMessageID("queue-conversation-type-second").
		WithSentAt(sent).
		Create(t, f.Store)
	for _, messageID := range []int64{first, second} {
		_, err := activityExec(f.Store, `
			UPDATE activity_projection_queue SET processed_revision = revision
			WHERE message_id = ?
		`, messageID)
		require.NoError(err)
	}

	conversationID, err := f.Store.EnsureConversationWithType(
		f.Source.ID, "default-thread", "group_chat", "Default Thread")
	require.NoError(err)
	assert.Equal(f.ConvID, conversationID)

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.Equal([]int64{first, second},
		[]int64{candidates[0].MessageID, candidates[1].MessageID})
	for _, candidate := range candidates {
		require.NotNil(candidate.ConversationID)
		assert.Equal(f.ConvID, *candidate.ConversationID)
		assert.Equal("group_chat", candidate.ConversationType)
		assert.Equal(store.ChannelChat, activity.Classify(candidate, 25).Channel)
		assert.Greater(candidate.Queue.Revision, candidate.Queue.ProcessedRevision)
	}
}

func TestProcessedActivityQueueRowsReopenWithoutRevisionABA(t *testing.T) {
	t.Run("message mutation", func(t *testing.T) {
		f := storetest.New(t)
		messageID := f.CreateMessage("queue-processed-message")
		assertActivityQueueReopensAfterProcessed(t, f.Store, messageID, func() error {
			_, err := activityExec(f.Store,
				`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("changed"), messageID)
			return err
		})
	})

	t.Run("recipient mutation", func(t *testing.T) {
		f := storetest.New(t)
		messageID := f.CreateMessage("queue-processed-recipient")
		participantID := f.EnsureParticipant(
			"processed-recipient@example.com", "Processed Recipient", "example.com")
		assertActivityQueueReopensAfterProcessed(t, f.Store, messageID, func() error {
			return f.Store.ReplaceMessageRecipients(
				messageID, "to", []int64{participantID}, []string{"Processed Recipient"})
		})
	})

	t.Run("conversation member mutation", func(t *testing.T) {
		f := storetest.New(t)
		messageID := f.CreateMessage("queue-processed-member")
		participantID := f.EnsureParticipant(
			"processed-member@example.com", "Processed Member", "example.com")
		assertActivityQueueReopensAfterProcessed(t, f.Store, messageID, func() error {
			return f.Store.EnsureConversationParticipant(f.ConvID, participantID, "member")
		})
	})
}

func TestActivityProjectionQueueRejectsInvalidProcessedRevision(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("queue-invalid-processed-revision")

	_, err := activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = -1
		WHERE message_id = ?
	`, messageID)
	require.Error(t, err)

	_, err = activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = revision + 1
		WHERE message_id = ?
	`, messageID)
	require.Error(t, err)
}

func TestHardMessageDeleteCascadesProjectionQueue(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("queue-cascade")
	require.Positive(t, activityQueueRevision(t, f.Store, messageID))

	_, err := activityExec(f.Store, `DELETE FROM messages WHERE id = ?`, messageID)
	require.NoError(t, err)
	assert.Zero(t, activityQueueRows(t, f.Store, messageID))
}

func TestDirectActivityLinkHardCascadeMarksExistingContactStateDirty(t *testing.T) {
	f, personID, messageID := activityLinkFixture(t, store.EvidenceDirect)

	_, err := activityExec(f.Store,
		`DELETE FROM messages WHERE id = ?`, messageID)
	require.NoError(t, err)

	var dirtyAt sql.NullTime
	require.NoError(t, activityQueryRow(f.Store,
		`SELECT dirty_at FROM person_contact_state WHERE person_id = ?`, personID,
	).Scan(&dirtyAt))
	assert.True(t, dirtyAt.Valid)
}

func TestCoPresenceActivityLinkDeleteDoesNotDirtyContactState(t *testing.T) {
	f, personID, messageID := activityLinkFixture(t, store.EvidenceCoPresence)

	_, err := activityExec(f.Store,
		`DELETE FROM activity_event_persons WHERE message_id = ? AND person_id = ?`,
		messageID, personID)
	require.NoError(t, err)

	var dirtyAt sql.NullTime
	require.NoError(t, activityQueryRow(f.Store,
		`SELECT dirty_at FROM person_contact_state WHERE person_id = ?`, personID,
	).Scan(&dirtyAt))
	assert.False(t, dirtyAt.Valid)
}

func TestDirectLinkCascadeDoesNotResurrectContactStateDuringPersonDelete(t *testing.T) {
	require := require.New(t)
	f, personID, _ := activityLinkFixture(t, store.EvidenceDirect)
	_, err := activityExec(
		f.Store, `DELETE FROM person_contact_state WHERE person_id = ?`, personID)
	require.NoError(err)

	_, err = activityExec(f.Store, `DELETE FROM persons WHERE id = ?`, personID)
	require.NoError(err)

	var rows int
	require.NoError(activityQueryRow(f.Store,
		`SELECT COUNT(*) FROM person_contact_state WHERE person_id = ?`, personID,
	).Scan(&rows))
	assert.Zero(t, rows)
}

func TestActivityEventPersonAllowsOnlyOneStablePersonReference(t *testing.T) {
	f, personID, messageID := activityLinkFixture(t, store.EvidenceDirect)

	_, err := activityExec(f.Store, `
		INSERT INTO activity_event_persons (message_id, person_id, role, evidence, local_date)
		VALUES (?, ?, ?, ?, ?)
	`, messageID, personID, store.RoleAddressed, store.EvidenceDirect, "2026-07-30")
	require.Error(t, err)
}

func TestActivitySchemaRejectsInvalidEnumAndDateValues(t *testing.T) {
	tests := []struct {
		name      string
		refKind   string
		channel   string
		origin    string
		precision string
		timezone  string
		offset    int
		localDate string
		direction string
	}{
		{
			name: "ref kind", refKind: "note", channel: "email",
			origin: "sent_at", precision: "timestamp", timezone: "UTC",
			localDate: "2026-07-30", direction: "inbound",
		},
		{
			name: "channel", refKind: "message", channel: "fax",
			origin: "sent_at", precision: "timestamp", timezone: "UTC",
			localDate: "2026-07-30", direction: "inbound",
		},
		{
			name: "origin", refKind: "message", channel: "email",
			origin: "edited_at", precision: "timestamp", timezone: "UTC",
			localDate: "2026-07-30", direction: "inbound",
		},
		{
			name: "precision", refKind: "message", channel: "email",
			origin: "sent_at", precision: "minute", timezone: "UTC",
			localDate: "2026-07-30", direction: "inbound",
		},
		{
			name: "empty timezone", refKind: "message", channel: "email",
			origin: "sent_at", precision: "timestamp",
			localDate: "2026-07-30", direction: "inbound",
		},
		{
			name: "offset", refKind: "message", channel: "email",
			origin: "sent_at", precision: "timestamp", timezone: "UTC",
			offset: 841, localDate: "2026-07-30", direction: "inbound",
		},
		{
			name: "date shape", refKind: "message", channel: "email",
			origin: "sent_at", precision: "timestamp", timezone: "UTC",
			localDate: "20260730", direction: "inbound",
		},
		{
			name: "direction", refKind: "message", channel: "email",
			origin: "sent_at", precision: "timestamp", timezone: "UTC",
			localDate: "2026-07-30", direction: "sideways",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := storetest.New(t)
			messageID := f.CreateMessage("invalid-" + test.name)
			_, err := activityExec(f.Store, `
				INSERT INTO activity_events (
					message_id, ref_kind, source_id, channel, occurred_at,
					date_origin, date_precision, timezone, utc_offset_minutes,
					local_date, direction, owner_address, projected_last_modified,
					projected_identity_revision, projected_account_identity_revision
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, 1, 1)
			`, messageID, test.refKind, f.Source.ID, test.channel,
				time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
				test.origin, test.precision, test.timezone, test.offset,
				test.localDate, test.direction,
				time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC))
			require.Error(t, err)
		})
	}
}

func TestActivitySchemaRejectsInvalidRoleAndEvidence(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		evidence string
	}{
		{name: "role", role: "spectator", evidence: "direct"},
		{name: "evidence", role: "sender", evidence: "hearsay"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, personID, messageID := activityLinkFixture(t, store.EvidenceDirect)
			_, err := activityExec(f.Store, `
				UPDATE activity_event_persons SET role = ?, evidence = ?
				WHERE message_id = ? AND person_id = ?
			`, test.role, test.evidence, messageID, personID)
			require.Error(t, err)
		})
	}
}

func TestActivitySchemaRejectsNonDigitLocalDates(t *testing.T) {
	t.Run("activity event", func(t *testing.T) {
		f := storetest.New(t)
		messageID := f.CreateMessage("invalid-event-date-digits")
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		_, err := activityExec(f.Store, `
			INSERT INTO activity_events (
				message_id, ref_kind, source_id, channel, occurred_at, date_origin,
				date_precision, timezone, utc_offset_minutes, local_date, direction,
				owner_address, projected_last_modified, projected_identity_revision,
				projected_account_identity_revision
			) VALUES (?, 'message', ?, 'email', ?, 'sent_at', 'timestamp',
				'UTC', 0, 'nope-no-pe', 'inbound', '', ?, 1, 1)
		`, messageID, f.Source.ID, now, now)
		require.Error(t, err)
	})

	t.Run("activity event person", func(t *testing.T) {
		f, personID, messageID := activityLinkFixture(t, store.EvidenceDirect)

		_, err := activityExec(f.Store, `
			UPDATE activity_event_persons SET local_date = ?
			WHERE message_id = ? AND person_id = ?
		`, "nope-no-pe", messageID, personID)
		require.Error(t, err)
	})
}

func activityColumns(t *testing.T, st *store.Store, table string) []string {
	t.Helper()
	var (
		rows *sql.Rows
		err  error
	)
	if st.IsPostgreSQL() {
		rows, err = st.DB().Query(st.Rebind(`
			SELECT column_name
			FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ?
			ORDER BY ordinal_position
		`), table)
	} else {
		rows, err = st.DB().Query(
			fmt.Sprintf(`SELECT name FROM pragma_table_info('%s') ORDER BY cid`, table))
	}
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(t, rows.Err())
	return columns
}

func activityQueueRevision(t *testing.T, st *store.Store, messageID int64) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, activityQueryRow(st,
		`SELECT revision FROM activity_projection_queue WHERE message_id = ?`, messageID,
	).Scan(&revision))
	return revision
}

func activityQueueRows(t *testing.T, st *store.Store, messageID int64) int {
	t.Helper()
	var count int
	require.NoError(t, activityQueryRow(st,
		`SELECT COUNT(*) FROM activity_projection_queue WHERE message_id = ?`, messageID,
	).Scan(&count))
	return count
}

func activityQueueState(
	t *testing.T, st *store.Store, messageID int64,
) (revision int64, processedRevision int64) {
	t.Helper()
	require.NoError(t, activityQueryRow(st, `
		SELECT revision, processed_revision
		FROM activity_projection_queue
		WHERE message_id = ?
	`, messageID).Scan(&revision, &processedRevision))
	return revision, processedRevision
}

func assertActivityQueueReopensAfterProcessed(
	t *testing.T, st *store.Store, messageID int64, mutate func() error,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()

	oldToken := activityQueueRevision(t, st, messageID)
	_, err := activityExec(st, `
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id = ?
	`, messageID)
	require.NoError(err)

	items, err := st.ListActivityProjectionQueueContext(ctx, 10)
	require.NoError(err)
	assert.Empty(items)

	require.NoError(mutate())
	revision, processedRevision := activityQueueState(t, st, messageID)
	assert.Equal(oldToken, processedRevision)
	assert.Greater(revision, processedRevision)
	assert.NotEqual(oldToken, revision)

	items, err = st.ListActivityProjectionQueueContext(ctx, 10)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(messageID, items[0].MessageID)
	assert.Equal(revision, items[0].Revision)
}

func activityLinkFixture(
	t *testing.T, evidence store.ActivityEvidence,
) (*storetest.Fixture, int64, int64) {
	t.Helper()
	f := storetest.New(t)
	participantID := f.EnsureParticipant("carol@example.com", "Carol", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	require.True(t, created)
	messageID := f.CreateMessage("activity-link")
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	_, err = activityExec(f.Store, `
		INSERT INTO activity_events (
			message_id, ref_kind, source_id, channel, occurred_at, date_origin,
			date_precision, timezone, utc_offset_minutes, local_date, direction,
			owner_address, projected_last_modified, projected_identity_revision,
			projected_account_identity_revision
		) VALUES (?, 'message', ?, 'email', ?, 'sent_at', 'timestamp',
			'UTC', 0, '2026-07-30', 'inbound', '', ?, 1, 1)
	`, messageID, f.Source.ID, now, now)
	require.NoError(t, err)
	_, err = activityExec(f.Store, `
		INSERT INTO activity_event_persons (message_id, person_id, role, evidence, local_date)
		VALUES (?, ?, 'sender', ?, '2026-07-30')
	`, messageID, person.ID, evidence)
	require.NoError(t, err)
	_, err = activityExec(f.Store, `
		INSERT INTO person_contact_state (person_id, interaction_count)
		VALUES (?, 1)
	`, person.ID)
	require.NoError(t, err)
	return f, person.ID, messageID
}

// activityTouch renders a metadata payload that requeues a message without
// changing its derived event. metadata is an activity input (meeting
// all_day/status), but the plain fixtures here carry neither key, so the touch
// advances the queue revision and last_modified and nothing else. Columns the
// projector never reads (subject, snippet) do not requeue at all -- see
// MessagesActivityColumns.
func activityTouch(marker string) string {
	return `{"touch":` + strconv.Quote(marker) + `}`
}

func activityExec(st *store.Store, query string, args ...any) (sql.Result, error) {
	return st.DB().Exec(st.Rebind(query), args...)
}

func activityQueryRow(st *store.Store, query string, args ...any) *sql.Row {
	return st.DB().QueryRow(st.Rebind(query), args...)
}

func TestListActivityProjectionQueueIsOrderedAndLimited(t *testing.T) {
	f := storetest.New(t)
	messageIDs := []int64{
		f.CreateMessage("queue-order-first"),
		f.CreateMessage("queue-order-second"),
		f.CreateMessage("queue-order-third"),
	}
	slices.Sort(messageIDs)

	items, err := f.Store.ListActivityProjectionQueueContext(context.Background(), 2)
	require.NoError(t, err)
	require.Len(t, items, 2)
	gotMessageIDs := []int64{items[0].MessageID, items[1].MessageID}
	assert.Equal(t, messageIDs[:2], gotMessageIDs)
}

func TestLoadQueuedActivityCandidatesPreservesRevisionOrderAndLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageIDs := []int64{
		f.NewMessage().WithSourceMessageID("candidate-first").WithSentAt(sent).Create(t, f.Store),
		f.NewMessage().WithSourceMessageID("candidate-second").WithSentAt(sent).Create(t, f.Store),
		f.NewMessage().WithSourceMessageID("candidate-third").WithSentAt(sent).Create(t, f.Store),
	}
	slices.Sort(messageIDs)
	_, err := activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id = ?
	`, messageIDs[0])
	require.NoError(err)

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 2)
	require.NoError(err)
	require.Len(candidates, 2)
	gotMessageIDs := []int64{candidates[0].MessageID, candidates[1].MessageID}
	assert.Equal(messageIDs[1:], gotMessageIDs)
	for _, candidate := range candidates {
		revision, processedRevision := activityQueueState(t, f.Store, candidate.MessageID)
		assert.True(candidate.Queue.Exists)
		assert.Equal(revision, candidate.Queue.Revision)
		assert.Equal(processedRevision, candidate.Queue.ProcessedRevision)
		assert.Greater(candidate.Queue.Revision, candidate.Queue.ProcessedRevision)
	}
}

func TestLoadActivityCandidateResolvesSourceOwnersWithoutDuplicateCounterparts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	sender := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(sender)
	require.NoError(err)
	require.True(created)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(f.Store.SetParticipantIdentifier(owner, "email", "owner-alias@example.com"))
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner-alias@example.com", "manual"))

	otherSource, err := f.Store.GetOrCreateSource("gmail", "other-source@example.com")
	require.NoError(err)
	otherSourceOwner := f.EnsureParticipant("other-owner@example.com", "Other", "example.com")
	require.NoError(f.Store.AddAccountIdentity(otherSource.ID, "other-owner@example.com", "manual"))

	messageID := f.NewMessage().
		WithSourceMessageID("candidate-owner-resolution").
		WithSentAt(sent).
		Create(t, f.Store)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{sender}, []string{"Sender"}))
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{owner, otherSourceOwner}, []string{"Owner", "Other"}))

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(candidates, 1)
	got := candidates[0]
	require.Len(got.Counterparts, 3, "multiple matching account identities must not duplicate a participant")

	byParticipant := make(map[int64]store.ActivityCounterpart, len(got.Counterparts))
	for _, counterpart := range got.Counterparts {
		byParticipant[counterpart.ParticipantID] = counterpart
	}
	require.NotNil(byParticipant[sender].PersonID)
	assert.Equal(person.ID, *byParticipant[sender].PersonID)
	assert.True(byParticipant[owner].IsOwner)
	assert.Equal("owner@example.com", byParticipant[owner].OwnerAddress,
		"fallback attribution uses the participant's primary email when present")
	assert.False(byParticipant[otherSourceOwner].IsOwner,
		"an identity confirmed for another source cannot make this message outbound")

	identityRevision, err := f.Store.IdentityRevision()
	require.NoError(err)
	accountIdentityRevision, err := f.Store.AccountIdentityRevision()
	require.NoError(err)
	assert.Equal(identityRevision, got.IdentityRevision)
	assert.Equal(accountIdentityRevision, got.AccountIdentityRevision)
}

func TestActivityAttributionMatchesCanonicalMessageIdentityRules(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	persist := func(sourceMessageID string, sender, recipient int64, envelopes []string) int64 {
		t.Helper()
		message := f.NewMessage().
			WithSourceMessageID(sourceMessageID).
			WithSentAt(sent).
			Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		fromNames := make([]string, len(envelopes))
		messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
			Message: message,
			Recipients: []store.RecipientSet{
				{
					Type:           "from",
					ParticipantIDs: slices.Repeat([]int64{sender}, len(envelopes)),
					DisplayNames:   fromNames,
					EmailAddresses: envelopes,
				},
				{
					Type:           "to",
					ParticipantIDs: []int64{recipient},
					DisplayNames:   []string{"Recipient"},
				},
			},
		})
		require.NoError(err)
		return messageID
	}
	counterpart := func(candidate store.ActivityCandidate, recipientType string, participantID int64) store.ActivityCounterpart {
		t.Helper()
		for _, value := range candidate.Counterparts {
			if value.RecipientType == recipientType && value.ParticipantID == participantID {
				return value
			}
		}
		require.Failf("missing counterpart", "missing %s counterpart for participant %d",
			recipientType, participantID)
		return store.ActivityCounterpart{}
	}

	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	recipient := f.EnsureParticipant("recipient@example.test", "Recipient", "example.test")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))

	// A non-empty envelope is authoritative. The current participant email
	// must not turn a message sent from another address into an outbound event.
	envelopeID := persist("activity-envelope-authority", owner, recipient,
		[]string{"outside@example.test"})
	candidates, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{envelopeID})
	require.NoError(err)
	require.Len(candidates, 1)
	sender := counterpart(candidates[0], "from", owner)
	assert.False(sender.IsOwner)
	assert.Equal(store.DirectionObserved,
		activity.Classify(candidates[0], 25).Direction)

	// An email-typed identifier is a legacy fallback only when the
	// participant has no primary email address.
	emailOnly, err := f.Store.EnsureParticipantByIdentifier(
		"email", "send-as@example.test", "Send As")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "SEND-AS@EXAMPLE.TEST", "manual"))
	emailID := persist("activity-email-identifier", emailOnly, recipient, []string{""})
	candidates, err = f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{emailID})
	require.NoError(err)
	require.Len(candidates, 1)
	assert.True(counterpart(candidates[0], "from", emailOnly).IsOwner)

	// Non-email identifiers keep exact comparison semantics, including their
	// service/scope-shaped values.
	matrix, err := f.Store.EnsureParticipantByIdentifier(
		"matrix", "@owner:example.test", "Matrix Owner")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "@owner:example.test", "manual"))
	matrixID := persist("activity-scoped-identifier", matrix, recipient, []string{""})
	candidates, err = f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{matrixID})
	require.NoError(err)
	require.Len(candidates, 1)
	assert.True(counterpart(candidates[0], "from", matrix).IsOwner)

	// Multiple envelope aliases for one participant collapse before the
	// activity classifier sees them. Matching one alias still gives one owner
	// counterpart and therefore cannot reverse direction or inflate audience.
	duplicateID := persist("activity-duplicate-envelope", owner, recipient,
		[]string{"outside@example.test", "owner@example.test"})
	candidates, err = f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{duplicateID})
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Len(candidates[0].Counterparts, 2)
	assert.True(counterpart(candidates[0], "from", owner).IsOwner)
	assert.Equal(store.DirectionOutbound,
		activity.Classify(candidates[0], 1).Direction)
}

func TestLoadActivityCandidateUsesRecipientsBeforeConversationMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	recipient := f.EnsureParticipant("recipient@example.com", "Recipient", "example.com")
	member := f.EnsureParticipant("member@example.com", "Member", "example.com")
	messageID := f.NewMessage().WithSentAt(sent).Create(t, f.Store)
	require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, member, "member"))
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{recipient}, []string{"Recipient"}))

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(candidates, 1)
	require.Len(candidates[0].Counterparts, 1)
	assert.Equal(recipient, candidates[0].Counterparts[0].ParticipantID)
	assert.Equal("to", candidates[0].Counterparts[0].RecipientType)
}

func TestLoadActivityCandidateFallsBackToConversationMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	member := f.EnsureParticipant("member@example.com", "Member", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(member)
	require.NoError(err)
	require.True(created)
	messageID := f.NewMessage().WithSentAt(sent).Create(t, f.Store)
	require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, member, "member"))

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(candidates, 1)
	require.Len(candidates[0].Counterparts, 1)
	got := candidates[0].Counterparts[0]
	assert.Equal(messageID, candidates[0].MessageID)
	assert.Equal(member, got.ParticipantID)
	assert.Equal("member", got.RecipientType)
	require.NotNil(got.PersonID)
	assert.Equal(person.ID, *got.PersonID)
}

func TestLoadActivityCandidateSynthesizesSenderWithoutFromRecipient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	sender := f.EnsureParticipant("sender-only@example.com", "Sender Only", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(sender)
	require.NoError(err)
	require.True(created)

	message := f.NewMessage().WithSentAt(sent).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: message,
	})
	require.NoError(err)

	candidates, err := f.Store.LoadActivityCandidatesByIDContext(
		t.Context(), []int64{messageID})
	require.NoError(err)
	require.Len(candidates, 1)
	require.Len(candidates[0].Counterparts, 1)
	got := candidates[0].Counterparts[0]
	assert.Equal(sender, got.ParticipantID)
	assert.Equal("from", got.RecipientType)
	require.NotNil(got.PersonID)
	assert.Equal(person.ID, *got.PersonID)
}

func TestLoadActivityCandidateKeepsConversationFallbackWithSenderMention(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	sender := f.EnsureParticipant("sender-mention@example.com", "Sender Mention", "example.com")
	member := f.EnsureParticipant("conversation-member@example.com", "Conversation Member", "example.com")
	memberPerson, created, err := f.Store.CreatePersonFromParticipant(member)
	require.NoError(err)
	require.True(created)
	mention := f.EnsureParticipant("mentioned@example.com", "Mentioned", "example.com")
	require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, member, "member"))

	message := f.NewMessage().WithSentAt(sent).Build()
	message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: message,
		Recipients: []store.RecipientSet{
			{Type: "mention", ParticipantIDs: []int64{mention}},
		},
	})
	require.NoError(err)

	candidates, err := f.Store.LoadActivityCandidatesByIDContext(
		t.Context(), []int64{messageID})
	require.NoError(err)
	require.Len(candidates, 1)
	byType := make(map[string]store.ActivityCounterpart, len(candidates[0].Counterparts))
	for _, counterpart := range candidates[0].Counterparts {
		byType[counterpart.RecipientType] = counterpart
	}
	require.Contains(byType, "from")
	assert.Equal(sender, byType["from"].ParticipantID)
	require.Contains(byType, "mention")
	assert.Equal(mention, byType["mention"].ParticipantID)
	require.Contains(byType, "member")
	assert.Equal(member, byType["member"].ParticipantID)
	require.NotNil(byType["member"].PersonID)
	assert.Equal(memberPerson.ID, *byType["member"].PersonID)
}

func TestLoadActivityCandidatePreservesBakedSenderOwnerFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	sender := f.EnsureParticipant("legacy-owner@example.com", "Owner", "example.com")
	messageID := f.NewMessage().
		WithSentAt(sent).
		WithIsFromMe(true).
		Create(t, f.Store)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{sender}, []string{"Owner"}))

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(candidates, 1)
	require.Len(candidates[0].Counterparts, 1)
	assert.True(candidates[0].Counterparts[0].IsOwner,
		"is_from_me remains an owner signal when no account identity row exists")
}

func TestQueuedActivityCandidateEligibilitySupportsRetractionAndRestoration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, f *storetest.Fixture, messageID int64)
	}{
		{
			name: "dedup soft delete",
			mutate: func(t *testing.T, f *storetest.Fixture, messageID int64) {
				t.Helper()
				_, err := activityExec(f.Store,
					`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, messageID)
				require.NoError(t, err)
			},
		},
		{
			name: "source soft delete",
			mutate: func(t *testing.T, f *storetest.Fixture, messageID int64) {
				t.Helper()
				_, err := activityExec(f.Store,
					`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, messageID)
				require.NoError(t, err)
			},
		},
		{
			name: "cancelled calendar event",
			mutate: func(t *testing.T, f *storetest.Fixture, messageID int64) {
				t.Helper()
				_, err := activityExec(f.Store,
					`UPDATE messages SET message_type = 'calendar_event' WHERE id = ?`, messageID)
				require.NoError(t, err)
				require.NoError(t, f.Store.SetMessageMetadata(messageID,
					sql.NullString{String: `{"status":"cancelled"}`, Valid: true}))
			},
		},
		{
			name: "missing timestamp",
			mutate: func(t *testing.T, f *storetest.Fixture, messageID int64) {
				t.Helper()
				_, err := activityExec(f.Store, `
					UPDATE messages SET sent_at = NULL, received_at = NULL, internal_date = NULL
					WHERE id = ?
				`, messageID)
				require.NoError(t, err)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			messageID := f.NewMessage().WithSentAt(sent).Create(t, f.Store)
			_, err := activityExec(f.Store, `
				UPDATE activity_projection_queue SET processed_revision = revision
				WHERE message_id = ?
			`, messageID)
			require.NoError(err)

			test.mutate(t, f, messageID)
			retraction, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
			require.NoError(err)
			require.Len(retraction, 1)
			assert.False(retraction[0].Eligible)
			deletedRevision := retraction[0].Queue.Revision

			_, err = activityExec(f.Store, `
				UPDATE messages
				SET deleted_at = NULL, deleted_from_source_at = NULL,
				    sent_at = ?, message_type = 'email', metadata = NULL
				WHERE id = ?
			`, sent, messageID)
			require.NoError(err)
			restored, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
			require.NoError(err)
			require.Len(restored, 1)
			assert.True(restored[0].Eligible)
			assert.Greater(restored[0].Queue.Revision, deletedRevision)
		})
	}
}

func TestActivityCandidateDateOnlyRequiresExplicitAllDayMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	midnight := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	ordinary := f.NewMessage().
		WithSourceMessageID("candidate-midnight").
		WithSentAt(midnight).
		Create(t, f.Store)
	allDayMessage := f.NewMessage().
		WithSourceMessageID("candidate-all-day").
		WithSentAt(midnight).
		Build()
	allDayMessage.MessageType = "calendar_event"
	allDay, err := f.Store.UpsertMessage(allDayMessage)
	require.NoError(err)
	require.NoError(f.Store.SetMessageMetadata(allDay,
		sql.NullString{String: `{"all_day":true}`, Valid: true}))

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(candidates, 2)
	byID := make(map[int64]store.ActivityCandidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.MessageID] = candidate
	}
	assert.False(byID[ordinary].DateOnly, "midnight alone is still timestamp precision")
	assert.True(byID[allDay].DateOnly)
}

func TestScanForActivityProjectionFindsForwardAndRevisionMismatchBelowWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	staleID := f.NewMessage().
		WithSourceMessageID("candidate-stale").
		WithSentAt(sent).
		Create(t, f.Store)
	forwardID := f.NewMessage().
		WithSourceMessageID("candidate-forward").
		WithSentAt(sent).
		Create(t, f.Store)
	identityRevision, err := f.Store.IdentityRevision()
	require.NoError(err)
	accountRevision, err := f.Store.AccountIdentityRevision()
	require.NoError(err)
	_, err = activityExec(f.Store, `
		INSERT INTO activity_events (
			message_id, ref_kind, source_id, channel, occurred_at, date_origin,
			date_precision, timezone, utc_offset_minutes, local_date, direction,
			owner_address, projected_last_modified, projected_identity_revision,
			projected_account_identity_revision
		)
		SELECT id, 'message', source_id, 'email', sent_at, 'sent_at', 'timestamp',
		       'UTC', 0, '2026-07-30', 'observed', '', last_modified, ?, ?
		FROM messages WHERE id = ?
	`, identityRevision, accountRevision, staleID)
	require.NoError(err)
	_, err = activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id = ?
	`, staleID)
	require.NoError(err)
	_, err = activityExec(f.Store,
		`DELETE FROM activity_projection_queue WHERE message_id = ?`, forwardID)
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "new-owner@example.com", "manual"))
	require.NoError(f.Store.SetActivityWatermarkContext(t.Context(), forwardID+100))

	candidates, err := f.Store.ScanForActivityProjectionContext(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.Equal([]int64{staleID, forwardID},
		[]int64{candidates[0].MessageID, candidates[1].MessageID})
	assert.True(candidates[0].Queue.Exists, "backstop observes retained queue rows")
	assert.Equal(candidates[0].Queue.Revision, candidates[0].Queue.ProcessedRevision,
		"the exact drained generation is part of the historical token")
	assert.False(candidates[1].Queue.Exists,
		"an absent legacy queue row remains distinguishable from revision zero")
	assert.Zero(candidates[1].Queue.Revision)
	assert.Zero(candidates[1].Queue.ProcessedRevision)
	assert.Greater(candidates[0].AccountIdentityRevision, accountRevision)

	afterStale, err := f.Store.ScanForActivityProjectionContext(t.Context(), staleID, 1)
	require.NoError(err)
	require.Len(afterStale, 1)
	assert.Equal(forwardID, afterStale[0].MessageID)
}

func TestScanForActivityProjectionSkipsDrainedEventlessIneligibleRows(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	missingTimestamp := f.CreateMessage("candidate-drained-missing-timestamp")
	cancelledMessage := f.NewMessage().
		WithSourceMessageID("candidate-drained-cancelled").
		WithSentAt(sent).
		Build()
	cancelledMessage.MessageType = "calendar_event"
	cancelled, err := f.Store.UpsertMessage(cancelledMessage)
	require.NoError(err)
	require.NoError(f.Store.SetMessageMetadata(cancelled,
		sql.NullString{String: `{"status":"cancelled"}`, Valid: true}))
	deleted := f.NewMessage().
		WithSourceMessageID("candidate-drained-deleted").
		WithSentAt(sent).
		Create(t, f.Store)
	_, err = activityExec(f.Store,
		`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, deleted)
	require.NoError(err)

	for _, messageID := range []int64{missingTimestamp, cancelled, deleted} {
		_, err = activityExec(f.Store, `
			UPDATE activity_projection_queue SET processed_revision = revision
			WHERE message_id = ?
		`, messageID)
		require.NoError(err)
	}

	candidates, err := f.Store.ScanForActivityProjectionContext(t.Context(), 0, 1)
	require.NoError(err)
	require.Empty(candidates,
		"drained eventless retractions are reconciled absence and cannot starve later work")

	later := f.NewMessage().
		WithSourceMessageID("candidate-after-drained-ineligible").
		WithSentAt(sent).
		Create(t, f.Store)
	candidates, err = f.Store.ScanForActivityProjectionContext(t.Context(), 0, 1)
	require.NoError(err)
	require.Len(candidates, 1)
	require.Equal(later, candidates[0].MessageID,
		"SQL must discard reconciled absences before applying the batch limit")
}

func TestScanForActivityProjectionReopensEventlessCandidateAfterRestoration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("candidate-restored-eventless")

	pending, err := f.Store.ScanForActivityProjectionContext(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(messageID, pending[0].MessageID)
	assert.False(pending[0].Eligible)

	_, err = activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id = ?
	`, messageID)
	require.NoError(err)
	drained, err := f.Store.ScanForActivityProjectionContext(t.Context(), 0, 10)
	require.NoError(err)
	assert.Empty(drained)

	sent := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	_, err = activityExec(f.Store,
		`UPDATE messages SET sent_at = ? WHERE id = ?`, sent, messageID)
	require.NoError(err)
	restored, err := f.Store.ScanForActivityProjectionContext(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(restored, 1)
	assert.Equal(messageID, restored[0].MessageID)
	assert.True(restored[0].Eligible)
	assert.Greater(restored[0].Queue.Revision, restored[0].Queue.ProcessedRevision)
}

func TestLoadQueuedActivityCandidatesKeepsProjectedIneligibleRetractionDiscoverable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	sent := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("candidate-projected-retraction").
		WithSentAt(sent).
		Create(t, f.Store)
	identityRevision, err := f.Store.IdentityRevision()
	require.NoError(err)
	accountRevision, err := f.Store.AccountIdentityRevision()
	require.NoError(err)
	_, err = activityExec(f.Store, `
		INSERT INTO activity_events (
			message_id, ref_kind, source_id, channel, occurred_at, date_origin,
			date_precision, timezone, utc_offset_minutes, local_date, direction,
			owner_address, projected_last_modified, projected_identity_revision,
			projected_account_identity_revision
		)
		SELECT id, 'message', source_id, 'email', sent_at, 'sent_at', 'timestamp',
		       'UTC', 0, '2026-07-30', 'observed', '', last_modified, ?, ?
		FROM messages WHERE id = ?
	`, identityRevision, accountRevision, messageID)
	require.NoError(err)
	_, err = activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id = ?
	`, messageID)
	require.NoError(err)
	_, err = activityExec(f.Store,
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, messageID)
	require.NoError(err)

	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(messageID, candidates[0].MessageID)
	assert.False(candidates[0].Eligible)
	assert.Greater(candidates[0].Queue.Revision, candidates[0].Queue.ProcessedRevision)
}

func activityProjectionFixture(
	t *testing.T,
) (*storetest.Fixture, int64) {
	t.Helper()
	f := storetest.New(t)
	participantID := f.EnsureParticipant(
		"activity-contact@example.com", "Activity Contact", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	require.True(t, created)
	return f, person.ID
}

func activityProjectionForMessage(
	t *testing.T,
	f *storetest.Fixture,
	messageID int64,
	personID int64,
	occurredAt time.Time,
) store.ActivityProjection {
	t.Helper()
	candidates, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 100)
	require.NoError(t, err)
	var candidate *store.ActivityCandidate
	for index := range candidates {
		if candidates[index].MessageID == messageID {
			candidate = &candidates[index]
			break
		}
	}
	require.NotNil(t, candidate, "message %d must have a pending candidate", messageID)
	return activityProjectionFromCandidate(t, *candidate, personID, occurredAt)
}

func activityProjectionFromCandidate(
	t *testing.T,
	candidate store.ActivityCandidate,
	personID int64,
	occurredAt time.Time,
) store.ActivityProjection {
	t.Helper()
	refKind := store.RefKindMessage
	channel := store.ChannelOther
	if store.IsMeetingMessageType(candidate.MessageType) {
		refKind = store.RefKindMeeting
		channel = store.ChannelMeeting
	} else {
		switch candidate.ConversationType {
		case "email_thread":
			channel = store.ChannelEmail
		case "group_chat", "direct_chat", "channel":
			channel = store.ChannelChat
		}
	}
	sourceID := candidate.SourceID
	event := &store.ActivityEvent{
		MessageID:                        candidate.MessageID,
		RefKind:                          refKind,
		SourceID:                         sourceID,
		ConversationID:                   candidate.ConversationID,
		Channel:                          channel,
		OccurredAt:                       occurredAt,
		DateOrigin:                       "sent_at",
		DatePrecision:                    "timestamp",
		Timezone:                         "UTC",
		UTCOffsetMinutes:                 0,
		LocalDate:                        occurredAt.UTC().Format(time.DateOnly),
		Direction:                        store.DirectionInbound,
		OwnerSourceID:                    &sourceID,
		OwnerAddress:                     "owner@example.com",
		ProjectedLastModified:            candidate.LastModified,
		ProjectedIdentityRevision:        candidate.IdentityRevision,
		ProjectedAccountIdentityRevision: candidate.AccountIdentityRevision,
		Persons: []store.ActivityEventPerson{{
			PersonID: personID,
			Role:     store.RoleSender,
			Evidence: store.EvidenceDirect,
		}},
	}
	return store.ActivityProjection{
		Token: store.ActivityProjectionToken{
			MessageID:               candidate.MessageID,
			SourceID:                candidate.SourceID,
			LastModified:            candidate.LastModified,
			Queue:                   candidate.Queue,
			TimezoneTransition:      candidate.TimezoneTransition,
			DirectLimitTransition:   candidate.DirectLimitTransition,
			IdentityRevision:        candidate.IdentityRevision,
			AccountIdentityRevision: candidate.AccountIdentityRevision,
			ConversationID:          candidate.ConversationID,
			ConversationType:        candidate.ConversationType,
			MessageType:             candidate.MessageType,
		},
		Event: event,
	}
}

func cleanContactStates(
	t *testing.T,
	st *store.Store,
	personIDs ...int64,
) store.ContactRevisions {
	t.Helper()
	revisions, err := st.ContactRevisionsContext(t.Context())
	require.NoError(t, err)
	seedActivityReconciledEpoch(t, st, revisions)
	require.NoError(t, st.RecomputeContactStateContext(
		t.Context(), personIDs, revisions))
	return revisions
}

func contactDirtyAt(t *testing.T, st *store.Store, personID int64) sql.NullTime {
	t.Helper()
	var dirty sql.NullTime
	require.NoError(t, activityQueryRow(st,
		`SELECT dirty_at FROM person_contact_state WHERE person_id = ?`,
		personID,
	).Scan(&dirty))
	return dirty
}

func TestContactStateDirtySelectionIsAtomicOrderedAndBounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	personIDs := make([]int64, 0, 3)
	for index := range 3 {
		participantID := f.EnsureParticipant(
			fmt.Sprintf("dirty-%d@example.com", index),
			fmt.Sprintf("Dirty %d", index),
			"example.com",
		)
		person, created, err := f.Store.CreatePersonFromParticipant(participantID)
		require.NoError(err)
		require.True(created)
		personIDs = append(personIDs, person.ID)
	}
	cleanContactStates(t, f.Store, personIDs...)

	err := f.Store.MarkContactStateDirtyContext(
		t.Context(), personIDs[1], 0, personIDs[1])
	require.Error(err)
	for _, personID := range personIDs {
		assert.False(contactDirtyAt(t, f.Store, personID).Valid,
			"invalid input must not partially dirty person %d", personID)
	}

	require.NoError(f.Store.MarkContactStateDirtyContext(
		t.Context(), personIDs[2], personIDs[0], personIDs[2]))
	stale, err := f.Store.StaleContactStatePersonsContext(t.Context(), 1)
	require.NoError(err)
	assert.Equal([]int64{personIDs[0]}, stale)
	stale, err = f.Store.StaleContactStatePersonsContext(t.Context(), 10)
	require.NoError(err)
	assert.Equal([]int64{personIDs[0], personIDs[2]}, stale)

	require.NoError(f.Store.MarkContactStateDirtyContext(
		t.Context(), personIDs[2]+1000))
	stale, err = f.Store.StaleContactStatePersonsContext(t.Context(), 10)
	require.NoError(err)
	assert.Equal([]int64{personIDs[0], personIDs[2]}, stale,
		"persons without contact state are a no-op")

	require.NoError(f.Store.MarkAllContactStateDirtyContext(t.Context()))
	stale, err = f.Store.StaleContactStatePersonsContext(t.Context(), 10)
	require.NoError(err)
	assert.Equal(personIDs, stale)
}

func TestContactStateDirtyEntrypointsLockIdentityBeforeContactRowsOnPostgreSQL(
	t *testing.T,
) {
	for _, test := range []struct {
		name string
		run  func(context.Context, *store.Store, int64) error
	}{
		{
			name: "targeted",
			run: func(ctx context.Context, st *store.Store, personID int64) error {
				return st.MarkContactStateDirtyContext(ctx, personID)
			},
		},
		{
			name: "all",
			run: func(ctx context.Context, st *store.Store, _ int64) error {
				return st.MarkAllContactStateDirtyContext(ctx)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			if !f.Store.IsPostgreSQL() {
				t.Skip("PostgreSQL identity-row lock barrier")
			}
			participantID := f.EnsureParticipant(
				"dirty-lock-"+test.name+"@example.com",
				"Dirty Lock "+test.name,
				"example.com",
			)
			person, created, err := f.Store.CreatePersonFromParticipant(participantID)
			require.NoError(err)
			require.True(created)
			cleanContactStates(t, f.Store, person.ID)

			blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
			require.NoError(err)
			blockerDone := false
			t.Cleanup(func() {
				if !blockerDone {
					_ = blocker.Rollback()
				}
			})
			_, err = blocker.ExecContext(t.Context(), f.Store.Rebind(`
				UPDATE archive_metadata SET value = value WHERE key = ?
			`), "identity_revision")
			require.NoError(err)

			dirtyResult := make(chan error, 1)
			go func() {
				dirtyResult <- test.run(t.Context(), f.Store, person.ID)
			}()
			select {
			case err := <-dirtyResult:
				require.NoError(err)
				require.Fail(
					"dirty entrypoint bypassed identity lock",
					"contact state changed while identity mutation was locked",
				)
			case <-time.After(100 * time.Millisecond):
			}
			waitForPostgreSQLLockWait(
				t, f.Store, "%archive_metadata%")
			assert.False(contactDirtyAt(t, f.Store, person.ID).Valid,
				"contact row must remain untouched behind the identity lock")

			require.NoError(blocker.Commit())
			blockerDone = true
			require.NoError(<-dirtyResult)
			assert.True(contactDirtyAt(t, f.Store, person.ID).Valid)
		})
	}
}

func TestStaleContactStateSelectionIncludesRevisionMismatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	var personIDs []int64
	for index := range 2 {
		participantID := f.EnsureParticipant(
			fmt.Sprintf("revision-stale-%d@example.com", index),
			fmt.Sprintf("Revision Stale %d", index),
			"example.com",
		)
		person, created, err := f.Store.CreatePersonFromParticipant(participantID)
		require.NoError(err)
		require.True(created)
		personIDs = append(personIDs, person.ID)
	}
	cleanContactStates(t, f.Store, personIDs...)

	fresh, err := f.Store.StaleContactStatePersonsContext(t.Context(), 0)
	require.NoError(err)
	assert.Empty(fresh)

	newParticipant := f.EnsureParticipant(
		"revision-stale-new@example.com", "Revision Stale New", "example.com")
	_, created, err := f.Store.CreatePersonFromParticipant(newParticipant)
	require.NoError(err)
	require.True(created)

	stale, err := f.Store.StaleContactStatePersonsContext(t.Context(), 0)
	require.NoError(err)
	assert.Equal(personIDs, stale)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	require.NoError(f.Store.RecomputeContactStateContext(
		t.Context(), personIDs, revisions))
	stillStale, err := f.Store.StaleContactStatePersonsContext(t.Context(), 0)
	require.NoError(err)
	assert.Equal(personIDs, stillStale,
		"recomputing from unreconciled historical links must not clear revision staleness")
}

func TestPersonBindingChangesMarkExistingContactStateDirty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Run("link adds binding", func(t *testing.T) {
		f := storetest.New(t)
		participantID := f.EnsureParticipant(
			"dirty-link@example.com", "Dirty Link", "example.com")
		aliasID := f.EnsureParticipant(
			"dirty-link-alias@example.com", "Dirty Link Alias", "example.com")
		person, created, err := f.Store.CreatePersonFromParticipant(participantID)
		require.NoError(err)
		require.True(created)
		cleanContactStates(t, f.Store, person.ID)

		_, err = f.Store.LinkParticipants(participantID, aliasID)
		require.NoError(err)
		assert.True(contactDirtyAt(t, f.Store, person.ID).Valid)
	})

	t.Run("merge removes absorbed binding", func(t *testing.T) {
		f := storetest.New(t)
		winnerID := f.EnsureParticipant(
			"dirty-merge-winner@example.com", "Dirty Merge Winner", "example.com")
		absorbedID := f.EnsureParticipant(
			"dirty-merge-absorbed@example.com", "Dirty Merge Absorbed", "example.com")
		_, err := f.Store.LinkParticipants(winnerID, absorbedID)
		require.NoError(err)
		person, created, err := f.Store.CreatePersonFromParticipant(absorbedID)
		require.NoError(err)
		require.True(created)
		cleanContactStates(t, f.Store, person.ID)

		require.NoError(f.Store.MergeParticipants(absorbedID, winnerID))
		assert.True(contactDirtyAt(t, f.Store, person.ID).Valid)
	})
}

func TestDeletePersonCascadesContactLinksButPreservesActivityEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	person, err := f.Store.GetPerson(personID)
	require.NoError(err)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("delete-person-activity").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{projection})
	require.NoError(err)
	revisionBefore, err := f.Store.IdentityRevision()
	require.NoError(err)

	require.NoError(f.Store.DeletePersonContext(
		t.Context(), personID, person.Revision))
	revisionAfter, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revisionBefore+1, revisionAfter)

	for _, check := range []struct {
		query string
		arg   int64
		want  int64
	}{
		{`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`, messageID, 1},
		{`SELECT COUNT(*) FROM activity_event_persons WHERE message_id = ?`, messageID, 0},
		{`SELECT COUNT(*) FROM person_contact_state WHERE person_id = ?`, personID, 0},
	} {
		var got int64
		require.NoError(activityQueryRow(f.Store, check.query, check.arg).Scan(&got))
		assert.Equal(check.want, got, check.query)
	}
}

func TestProjectActivityBatchMaintainsMonotoneExtremaAndReplayCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	older := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	newerMessage := f.NewMessage().
		WithSourceMessageID("projection-newer").
		WithSentAt(newer).
		Create(t, f.Store)
	olderMessage := f.NewMessage().
		WithSourceMessageID("projection-older").
		WithSentAt(older).
		Create(t, f.Store)

	_, err := f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
		activityProjectionForMessage(t, f, newerMessage, personID, newer),
	})
	require.NoError(err)
	_, err = f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
		activityProjectionForMessage(t, f, olderMessage, personID, older),
	})
	require.NoError(err)

	state, err := f.Store.ContactStateContext(t.Context(), personID, newer)
	require.NoError(err)
	require.NotNil(state.FirstContactAt)
	require.NotNil(state.LastContactAt)
	assert.Equal(older, state.FirstContactAt.UTC())
	assert.Equal(newer, state.LastContactAt.UTC())
	assert.Equal(fmt.Sprintf("message:%d", olderMessage), state.FirstContactRef)
	assert.Equal(fmt.Sprintf("message:%d", newerMessage), state.LastContactRef)
	assert.Equal(int64(2), state.InteractionCount)

	_, err = activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("replayed"), newerMessage)
	require.NoError(err)
	replay := activityProjectionForMessage(t, f, newerMessage, personID, newer)
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{replay})
	require.NoError(err)
	state, err = f.Store.ContactStateContext(t.Context(), personID, newer)
	require.NoError(err)
	assert.Equal(int64(2), state.InteractionCount,
		"reprojecting one native event cannot increment its interaction twice")
}

func TestProjectActivityBatchExactQueueReplayReportsNoContactWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-exact-replay").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	initial := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)
	first, err := f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{initial})
	require.NoError(err)
	assert.Equal(1, first.ContactPersons)

	_, err = activityExec(f.Store, `
		UPDATE activity_projection_queue
		SET revision = revision + 1
		WHERE message_id = ?
	`, messageID)
	require.NoError(err)
	replay := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)
	result, err := f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{replay})
	require.NoError(err)
	assert.Zero(result.EventsWritten)
	assert.Zero(result.EventsRetracted)
	assert.Zero(result.ContactPersons,
		"queue-only exact replay must not report or lock contact work")
	state, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.Equal(int64(1), state.InteractionCount)
}

func TestProjectActivityBatchBreaksTimestampTiesByMessageID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	same := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	firstMessage := f.NewMessage().
		WithSourceMessageID("projection-tie-first").
		WithSentAt(same).
		Create(t, f.Store)
	secondMessage := f.NewMessage().
		WithSourceMessageID("projection-tie-second").
		WithSentAt(same).
		Create(t, f.Store)
	lowID, highID := firstMessage, secondMessage
	if lowID > highID {
		lowID, highID = highID, lowID
	}

	_, err := f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
		activityProjectionForMessage(t, f, highID, personID, same),
		activityProjectionForMessage(t, f, lowID, personID, same),
	})
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), personID, same)
	require.NoError(err)
	assert.Equal(fmt.Sprintf("message:%d", lowID), state.FirstContactRef)
	assert.Equal(fmt.Sprintf("message:%d", highID), state.LastContactRef)
}

func TestProjectActivityBatchPreservesMeetingReferenceKindInContactState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	message := f.NewMessage().
		WithSourceMessageID("projection-meeting-ref").
		WithSentAt(occurredAt).
		Build()
	message.MessageType = "calendar_event"
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)

	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{projection})
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.Equal(fmt.Sprintf("meeting:%d", messageID), state.FirstContactRef)
	assert.Equal(fmt.Sprintf("meeting:%d", messageID), state.LastContactRef)
	assert.Equal(fmt.Sprintf("meeting:%d", messageID), state.LastInboundRef)
}

func TestProjectActivityBatchRetractsAndMovesDirectEvidenceAuthoritatively(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, oldPersonID := activityProjectionFixture(t)
	newParticipant := f.EnsureParticipant(
		"moved-contact@example.com", "Moved Contact", "example.com")
	newPerson, created, err := f.Store.CreatePersonFromParticipant(newParticipant)
	require.NoError(err)
	require.True(created)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-move").
		WithSentAt(occurredAt).
		Create(t, f.Store)

	initial := activityProjectionForMessage(t, f, messageID, oldPersonID, occurredAt)
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{initial})
	require.NoError(err)

	_, err = activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("move"), messageID)
	require.NoError(err)
	moved := activityProjectionForMessage(t, f, messageID, newPerson.ID, occurredAt)
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{moved})
	require.NoError(err)

	oldState, err := f.Store.ContactStateContext(t.Context(), oldPersonID, occurredAt)
	require.NoError(err)
	newState, err := f.Store.ContactStateContext(t.Context(), newPerson.ID, occurredAt)
	require.NoError(err)
	assert.Zero(oldState.InteractionCount)
	assert.Nil(oldState.LastContactAt)
	assert.Equal(int64(1), newState.InteractionCount)
	require.NotNil(newState.LastContactAt)
	assert.Equal(occurredAt, newState.LastContactAt.UTC())

	_, err = activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("retract"), messageID)
	require.NoError(err)
	retractionCandidate, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(retractionCandidate, 1)
	candidate := retractionCandidate[0]
	retraction := store.ActivityProjection{
		Token: store.ActivityProjectionToken{
			MessageID:               candidate.MessageID,
			SourceID:                candidate.SourceID,
			LastModified:            candidate.LastModified,
			Queue:                   candidate.Queue,
			TimezoneTransition:      candidate.TimezoneTransition,
			DirectLimitTransition:   candidate.DirectLimitTransition,
			IdentityRevision:        candidate.IdentityRevision,
			AccountIdentityRevision: candidate.AccountIdentityRevision,
			ConversationID:          candidate.ConversationID,
			ConversationType:        candidate.ConversationType,
			MessageType:             candidate.MessageType,
		},
	}
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{retraction})
	require.NoError(err)
	newState, err = f.Store.ContactStateContext(t.Context(), newPerson.ID, occurredAt)
	require.NoError(err)
	assert.Zero(newState.InteractionCount)
	assert.Nil(newState.LastContactAt)
}

func TestProjectActivityBatchRollsBackEveryItemOnLaterFailure(t *testing.T) {
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	firstMessage := f.NewMessage().
		WithSourceMessageID("projection-rollback-first").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	secondMessage := f.NewMessage().
		WithSourceMessageID("projection-rollback-second").
		WithSentAt(occurredAt.Add(time.Hour)).
		Create(t, f.Store)
	first := activityProjectionForMessage(t, f, firstMessage, personID, occurredAt)
	second := activityProjectionForMessage(
		t, f, secondMessage, personID, occurredAt.Add(time.Hour))
	second.Event.Persons[0].PersonID = 9223372036854775000

	_, err := f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{first, second})
	require.Error(err)

	for _, messageID := range []int64{firstMessage, secondMessage} {
		var eventCount int
		require.NoError(activityQueryRow(f.Store,
			`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`, messageID,
		).Scan(&eventCount))
		assert.Zero(t, eventCount)
		revision, processed := activityQueueState(t, f.Store, messageID)
		assert.Less(t, processed, revision)
	}
}

func TestProjectActivityBatchRestoresContactAfterAuthoritativeRetraction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-restore").
		WithSentAt(occurredAt).
		Create(t, f.Store)

	initial := activityProjectionForMessage(t, f, messageID, personID, occurredAt)
	_, err := f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{initial})
	require.NoError(err)

	for _, subject := range []string{"retract", "restore"} {
		_, err = activityExec(f.Store,
			`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch(subject), messageID)
		require.NoError(err)
		if subject == "retract" {
			candidates, loadErr := f.Store.LoadQueuedActivityCandidatesContext(
				t.Context(), 10)
			require.NoError(loadErr)
			require.Len(candidates, 1)
			candidate := candidates[0]
			_, err = f.Store.ProjectActivityBatchContext(
				t.Context(), []store.ActivityProjection{{
					Token: activityProjectionForMessage(
						t, f, messageID, personID, occurredAt).Token,
				}})
			require.NoError(err)
			assert.Equal(messageID, candidate.MessageID)
			continue
		}
		restored := activityProjectionForMessage(
			t, f, messageID, personID, occurredAt)
		_, err = f.Store.ProjectActivityBatchContext(
			t.Context(), []store.ActivityProjection{restored})
		require.NoError(err)
	}

	state, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.Equal(int64(1), state.InteractionCount)
	require.NotNil(state.FirstContactAt)
	require.NotNil(state.LastContactAt)
	assert.Equal(occurredAt, state.FirstContactAt.UTC())
	assert.Equal(occurredAt, state.LastContactAt.UTC())
}

func TestProjectActivityBatchStaleItemRollsBackWholeBatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	firstMessage := f.NewMessage().
		WithSourceMessageID("projection-stale-first").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	secondMessage := f.NewMessage().
		WithSourceMessageID("projection-stale-second").
		WithSentAt(occurredAt.Add(time.Hour)).
		Create(t, f.Store)
	first := activityProjectionForMessage(t, f, firstMessage, personID, occurredAt)
	second := activityProjectionForMessage(
		t, f, secondMessage, personID, occurredAt.Add(time.Hour))

	_, err := activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("changed"), secondMessage)
	require.NoError(err)
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{first, second})
	var stale *store.ErrActivityProjectionStale
	require.ErrorAs(err, &stale)
	assert.Equal(secondMessage, stale.MessageID)

	for _, messageID := range []int64{firstMessage, secondMessage} {
		var count int
		require.NoError(activityQueryRow(f.Store,
			`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`, messageID,
		).Scan(&count))
		assert.Zero(count)
		revision, processed := activityQueueState(t, f.Store, messageID)
		assert.Less(processed, revision)
	}
}

func TestProjectActivityBatchRejectsIdentityEpochMismatchWithoutAcknowledging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-identity-stale").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "new-owner@example.com", "manual"))

	_, err := f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{projection})
	var stale *store.ErrActivityProjectionStale
	require.ErrorAs(err, &stale)
	assert.Zero(stale.MessageID, "the whole identity epoch is stale")
	var eventCount int
	require.NoError(activityQueryRow(f.Store,
		`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`, messageID,
	).Scan(&eventCount))
	assert.Zero(eventCount)
	revision, processed := activityQueueState(t, f.Store, messageID)
	assert.Less(processed, revision)
}

func TestProjectActivityBatchRejectsRetainedDrainedGenerationABA(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-drained-aba").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	initial := activityProjectionForMessage(t, f, messageID, personID, occurredAt)
	_, err := f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{initial})
	require.NoError(err)

	_, err = activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("generation-2"), messageID)
	require.NoError(err)
	staleProjection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)
	_, err = activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("generation-3"), messageID)
	require.NoError(err)
	_, err = activityExec(f.Store, `
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id = ?
	`, messageID)
	require.NoError(err)

	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{staleProjection})
	var stale *store.ErrActivityProjectionStale
	require.ErrorAs(err, &stale)
	assert.Equal(messageID, stale.MessageID)
	revision, processed := activityQueueState(t, f.Store, messageID)
	assert.Equal(revision, processed,
		"the later drained generation must remain untouched")
}

func TestProjectActivityBatchReservesAbsentLegacyQueueAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-legacy-queue").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	_, err := activityExec(f.Store,
		`DELETE FROM activity_projection_queue WHERE message_id = ?`, messageID)
	require.NoError(err)
	candidates, err := f.Store.ScanForActivityProjectionContext(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.False(candidates[0].Queue.Exists)
	projection := activityProjectionFromCandidate(
		t, candidates[0], personID, occurredAt)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, projectErr := f.Store.ProjectActivityBatchContext(
				t.Context(), []store.ActivityProjection{projection})
			results <- projectErr
		}()
	}
	close(start)
	var successes, staleErrors int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		if _, ok := errors.AsType[*store.ErrActivityProjectionStale](err); ok {
			staleErrors++
			continue
		}
		require.NoError(err)
	}
	assert.Equal(1, successes)
	assert.Equal(1, staleErrors)
	revision, processed := activityQueueState(t, f.Store, messageID)
	assert.Equal(int64(1), revision)
	assert.Equal(revision, processed)
	state, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.Equal(int64(1), state.InteractionCount)
}

func TestProjectActivityBatchConcurrentSameTokenCommitsOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-same-token").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, projectErr := f.Store.ProjectActivityBatchContext(
				t.Context(), []store.ActivityProjection{projection})
			results <- projectErr
		}()
	}
	close(start)
	var successes, staleErrors int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		if _, ok := errors.AsType[*store.ErrActivityProjectionStale](err); ok {
			staleErrors++
			continue
		}
		require.NoError(err)
	}
	assert.Equal(1, successes)
	assert.Equal(1, staleErrors)
	state, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.Equal(int64(1), state.InteractionCount)
}

func TestProjectActivityBatchRacingMutationLeavesPendingGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-racing-mutation").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)

	start := make(chan struct{})
	projectResult := make(chan error, 1)
	mutationResult := make(chan error, 1)
	go func() {
		<-start
		_, err := f.Store.ProjectActivityBatchContext(
			t.Context(), []store.ActivityProjection{projection})
		projectResult <- err
	}()
	go func() {
		<-start
		_, err := activityExec(f.Store,
			`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("raced"), messageID)
		mutationResult <- err
	}()
	close(start)
	require.NoError(<-mutationResult)
	projectErr := <-projectResult
	if projectErr != nil {
		var stale *store.ErrActivityProjectionStale
		require.ErrorAs(projectErr, &stale)
	}
	revision, processed := activityQueueState(t, f.Store, messageID)
	assert.Greater(revision, processed,
		"a mutation before or behind projection must leave discoverable work")
}

func TestProjectActivityBatchLocksMessageBeforeQueueOnPostgreSQL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock barrier")
	}
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-lock-order").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)

	blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	blockerDone := false
	t.Cleanup(func() {
		if !blockerDone {
			_ = blocker.Rollback()
		}
	})
	var queueRevision int64
	require.NoError(blocker.QueryRowContext(t.Context(),
		f.Store.Rebind(`
			SELECT revision
			FROM activity_projection_queue
			WHERE message_id = ?
			FOR UPDATE
		`), messageID).Scan(&queueRevision))

	projectResult := make(chan error, 1)
	go func() {
		_, projectErr := f.Store.ProjectActivityBatchContext(
			t.Context(), []store.ActivityProjection{projection})
		projectResult <- projectErr
	}()
	waitForPostgreSQLLockWait(t, f.Store, "%activity_projection_queue%")

	mutationResult := make(chan error, 1)
	go func() {
		_, mutationErr := activityExec(f.Store,
			`UPDATE messages SET metadata = ? WHERE id = ?`,
			activityTouch("blocked-behind-projection"), messageID)
		mutationResult <- mutationErr
	}()
	waitForPostgreSQLLockWait(t, f.Store, "%UPDATE messages SET metadata%")

	require.NoError(blocker.Commit())
	blockerDone = true
	require.NoError(<-projectResult)
	require.NoError(<-mutationResult)
	revision, processed := activityQueueState(t, f.Store, messageID)
	assert.Greater(revision, processed,
		"the mutation unblocked after projection must leave a higher generation")
}

func TestProjectActivityBatchPureAdditionDoesNotLockHistoricalEvidence(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock barrier")
	}
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	historicalMessage := f.NewMessage().
		WithSourceMessageID("projection-addition-historical").
		WithSentAt(base).
		Create(t, f.Store)
	_, err := f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{
			activityProjectionForMessage(
				t, f, historicalMessage, personID, base),
		})
	require.NoError(err)

	newMessage := f.NewMessage().
		WithSourceMessageID("projection-addition-new").
		WithSentAt(base.Add(time.Hour)).
		Create(t, f.Store)
	addition := activityProjectionForMessage(
		t, f, newMessage, personID, base.Add(time.Hour))

	blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	blockerDone := false
	t.Cleanup(func() {
		if !blockerDone {
			_ = blocker.Rollback()
		}
	})
	var lockedMessageID int64
	require.NoError(blocker.QueryRowContext(t.Context(),
		f.Store.Rebind(`
			SELECT message_id
			FROM activity_event_persons
			WHERE message_id = ? AND person_id = ?
			FOR UPDATE
		`), historicalMessage, personID).Scan(&lockedMessageID))
	assert.Equal(historicalMessage, lockedMessageID)

	projectContext, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result, err := f.Store.ProjectActivityBatchContext(
		projectContext, []store.ActivityProjection{addition})
	require.NoError(err,
		"pure addition must not scan-lock the person's historical evidence")
	assert.Equal(1, result.ContactPersons)
	require.NoError(blocker.Rollback())
	blockerDone = true

	state, err := f.Store.ContactStateContext(t.Context(), personID, base)
	require.NoError(err)
	assert.Equal(int64(2), state.InteractionCount)
}

func TestRecomputeContactStateLocksDifferentMessageEvidenceBeforeClearingDirty(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock barrier")
	}
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	firstMessage := f.NewMessage().
		WithSourceMessageID("projection-recompute-lock-first").
		WithSentAt(base).
		Create(t, f.Store)
	secondMessage := f.NewMessage().
		WithSourceMessageID("projection-recompute-lock-second").
		WithSentAt(base.Add(time.Hour)).
		Create(t, f.Store)
	_, err = f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
		activityProjectionForMessage(t, f, firstMessage, personID, base),
		activityProjectionForMessage(
			t, f, secondMessage, personID, base.Add(time.Hour)),
	})
	require.NoError(err)

	blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	blockerDone := false
	t.Cleanup(func() {
		if !blockerDone {
			_ = blocker.Rollback()
		}
	})
	var lockedPersonID int64
	require.NoError(blocker.QueryRowContext(t.Context(),
		f.Store.Rebind(`
			SELECT person_id
			FROM person_contact_state
			WHERE person_id = ?
			FOR UPDATE
		`), personID).Scan(&lockedPersonID))
	assert.Equal(personID, lockedPersonID)

	deleteResult := make(chan error, 1)
	go func() {
		_, deleteErr := activityExec(f.Store,
			`DELETE FROM activity_event_persons
			 WHERE message_id = ? AND person_id = ?`, secondMessage, personID)
		deleteResult <- deleteErr
	}()
	// Delete the evidence row itself so this barrier tests evidence ordering
	// without also taking the queue freshness lock through a message cascade.
	waitForPostgreSQLLockWait(t, f.Store, "%DELETE FROM activity_event_persons%")

	recomputeResult := make(chan error, 1)
	go func() {
		recomputeResult <- f.Store.RecomputeContactStateContext(
			t.Context(), []int64{personID}, revisions)
	}()
	waitForPostgreSQLLockWait(
		t, f.Store, "%SELECT message_id, person_id%activity_event_persons%")

	require.NoError(blocker.Commit())
	blockerDone = true
	require.NoError(<-deleteResult)
	require.NoError(<-recomputeResult)

	state, err := f.Store.ContactStateContext(t.Context(), personID, base)
	require.NoError(err)
	assert.Equal(int64(1), state.InteractionCount,
		"recompute must not publish evidence deleted by the earlier transaction")
	assert.Equal(fmt.Sprintf("message:%d", firstMessage), state.LastContactRef)
	assert.False(state.Stale,
		"same-epoch recompute may clear dirty only after observing the deletion")
}

func TestRecomputeContactStateLocksEvidenceWhenContactRowIsMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	firstMessage := f.NewMessage().
		WithSourceMessageID("projection-missing-state-first").
		WithSentAt(base).
		Create(t, f.Store)
	secondMessage := f.NewMessage().
		WithSourceMessageID("projection-missing-state-second").
		WithSentAt(base.Add(time.Hour)).
		Create(t, f.Store)
	_, err = f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
		activityProjectionForMessage(t, f, firstMessage, personID, base),
		activityProjectionForMessage(
			t, f, secondMessage, personID, base.Add(time.Hour)),
	})
	require.NoError(err)
	_, err = activityExec(f.Store,
		`DELETE FROM person_contact_state WHERE person_id = ?`, personID)
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		deleting, beginErr := f.Store.DB().BeginTx(t.Context(), nil)
		require.NoError(beginErr)
		deletingDone := false
		t.Cleanup(func() {
			if !deletingDone {
				_ = deleting.Rollback()
			}
		})
		// Keep the queue freshness barrier out of this setup. The invariant under
		// test is that recompute waits on the uncommitted evidence deletion before
		// rebuilding the missing contact row.
		_, err = deleting.ExecContext(t.Context(),
			f.Store.Rebind(`DELETE FROM activity_event_persons
			WHERE message_id = ? AND person_id = ?`), secondMessage, personID)
		require.NoError(err)

		recomputeResult := make(chan error, 1)
		go func() {
			recomputeResult <- f.Store.RecomputeContactStateContext(
				t.Context(), []int64{personID}, revisions)
		}()
		waitForPostgreSQLLockWait(t, f.Store, "%activity_event_persons%")
		require.NoError(deleting.Commit())
		deletingDone = true
		require.NoError(<-recomputeResult)
	} else {
		_, err = activityExec(f.Store,
			`DELETE FROM messages WHERE id = ?`, secondMessage)
		require.NoError(err)
		require.NoError(f.Store.RecomputeContactStateContext(
			t.Context(), []int64{personID}, revisions))
	}
	state, err := f.Store.ContactStateContext(t.Context(), personID, base)
	require.NoError(err)
	assert.Equal(int64(1), state.InteractionCount)
	assert.Equal(fmt.Sprintf("message:%d", firstMessage), state.LastContactRef)
	assert.False(state.Stale)
}

func waitForPostgreSQLLockWait(t *testing.T, st *store.Store, pattern string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		err := st.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
			  AND query LIKE $1
		`, pattern).Scan(&count)
		require.NoError(t, err)
		if count > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.FailNow(t, "timed out waiting for PostgreSQL lock", pattern)
}

func TestProjectActivityBatchRacingHardDeleteLeavesNoFreshGhostState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-racing-delete").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)

	start := make(chan struct{})
	projectResult := make(chan error, 1)
	deleteResult := make(chan error, 1)
	go func() {
		<-start
		_, err := f.Store.ProjectActivityBatchContext(
			t.Context(), []store.ActivityProjection{projection})
		projectResult <- err
	}()
	go func() {
		<-start
		_, err := activityExec(f.Store,
			`DELETE FROM messages WHERE id = ?`, messageID)
		deleteResult <- err
	}()
	close(start)
	require.NoError(<-deleteResult)
	projectErr := <-projectResult
	if projectErr != nil {
		var stale *store.ErrActivityProjectionStale
		require.ErrorAs(projectErr, &stale)
	}

	var eventCount int
	require.NoError(activityQueryRow(f.Store,
		`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`, messageID,
	).Scan(&eventCount))
	assert.Zero(eventCount)
	var stateCount int
	// SQLite returns an aggregate over a declared DATETIME column as an
	// untyped string, so database/sql cannot scan MAX(dirty_at) directly into
	// sql.NullTime. The invariant here is only that a surviving state row has a
	// non-NULL dirty marker; cast the aggregate to the portable representation
	// and keep that assertion independent of driver type metadata.
	var dirty sql.NullString
	err := activityQueryRow(f.Store, `
		SELECT COUNT(*), CAST(MAX(dirty_at) AS TEXT)
		FROM person_contact_state
		WHERE person_id = ?
	`, personID).Scan(&stateCount, &dirty)
	require.NoError(err)
	if stateCount > 0 {
		assert.True(dirty.Valid,
			"a committed projection deleted by cascade must remain recoverably dirty")
	}
}

func TestProjectActivityBatchRecomputesEveryContactRelevantChange(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(
			t *testing.T,
			f *storetest.Fixture,
			projection *store.ActivityProjection,
			personID int64,
		)
		check func(
			t *testing.T,
			state store.ContactState,
			original time.Time,
		)
	}{
		{
			name: "direct to co-presence",
			mutate: func(
				_ *testing.T,
				_ *storetest.Fixture,
				projection *store.ActivityProjection,
				_ int64,
			) {
				projection.Event.Persons[0].Evidence = store.EvidenceCoPresence
			},
			check: func(t *testing.T, state store.ContactState, _ time.Time) {
				t.Helper()
				assert.Zero(t, state.InteractionCount)
				assert.Nil(t, state.LastContactAt)
			},
		},
		{
			name: "redating",
			mutate: func(
				_ *testing.T,
				_ *storetest.Fixture,
				projection *store.ActivityProjection,
				_ int64,
			) {
				redated := projection.Event.OccurredAt.Add(72 * time.Hour)
				projection.Event.OccurredAt = redated
				projection.Event.LocalDate = redated.Format(time.DateOnly)
			},
			check: func(t *testing.T, state store.ContactState, original time.Time) {
				t.Helper()
				require.NotNil(t, state.LastContactAt)
				assert.Equal(t, original.Add(72*time.Hour), state.LastContactAt.UTC())
			},
		},
		{
			name: "direction",
			mutate: func(
				_ *testing.T,
				_ *storetest.Fixture,
				projection *store.ActivityProjection,
				_ int64,
			) {
				projection.Event.Direction = store.DirectionOutbound
			},
			check: func(t *testing.T, state store.ContactState, original time.Time) {
				t.Helper()
				assert.Nil(t, state.LastInboundAt)
				require.NotNil(t, state.LastOutboundAt)
				assert.Equal(t, original, state.LastOutboundAt.UTC())
			},
		},
		{
			name: "channel source and owner",
			mutate: func(
				t *testing.T,
				f *storetest.Fixture,
				projection *store.ActivityProjection,
				_ int64,
			) {
				t.Helper()
				otherSource, err := f.Store.GetOrCreateSource(
					"gmail", "other-owner@example.com")
				require.NoError(t, err)
				_, err = activityExec(f.Store,
					`UPDATE messages SET source_id = ? WHERE id = ?`,
					otherSource.ID, projection.Token.MessageID)
				require.NoError(t, err)
				_, err = f.Store.EnsureConversationWithType(
					f.Source.ID, "default-thread", "direct_chat", "Default Thread")
				require.NoError(t, err)
				candidates, err := f.Store.LoadQueuedActivityCandidatesContext(
					t.Context(), 10)
				require.NoError(t, err)
				require.Len(t, candidates, 1)
				updated := activityProjectionFromCandidate(
					t, candidates[0], projection.Event.Persons[0].PersonID,
					projection.Event.OccurredAt)
				*projection = updated
				projection.Event.OwnerAddress = "other-owner@example.com"
			},
			check: func(t *testing.T, state store.ContactState, _ time.Time) {
				t.Helper()
				assert.Equal(t, store.ChannelChat, state.LastContactChannel)
				require.NotNil(t, state.LastContactSourceID)
				assert.Equal(t, "other-owner@example.com", state.LastContactOwner)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			f, personID := activityProjectionFixture(t)
			occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			messageID := f.NewMessage().
				WithSourceMessageID("projection-recompute-"+test.name).
				WithSentAt(occurredAt).
				Create(t, f.Store)
			initial := activityProjectionForMessage(
				t, f, messageID, personID, occurredAt)
			_, err := f.Store.ProjectActivityBatchContext(
				t.Context(), []store.ActivityProjection{initial})
			require.NoError(err)

			_, err = activityExec(f.Store,
				`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch(test.name), messageID)
			require.NoError(err)
			replacement := activityProjectionForMessage(
				t, f, messageID, personID, occurredAt)
			test.mutate(t, f, &replacement, personID)
			_, err = f.Store.ProjectActivityBatchContext(
				t.Context(), []store.ActivityProjection{replacement})
			require.NoError(err)
			state, err := f.Store.ContactStateContext(
				t.Context(), personID, occurredAt)
			require.NoError(err)
			test.check(t, state, occurredAt)
		})
	}
}

func seedActivityReconciledEpoch(
	t *testing.T,
	st *store.Store,
	revisions store.ContactRevisions,
) {
	t.Helper()
	for key, revision := range map[string]int64{
		"activity_spine_reconciled_identity_revision":         revisions.IdentityRevision,
		"activity_spine_reconciled_account_identity_revision": revisions.AccountIdentityRevision,
	} {
		_, err := activityExec(st, `
			INSERT INTO archive_metadata (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value
		`, key, strconv.FormatInt(revision, 10))
		require.NoError(t, err)
	}
}

func TestProjectActivityBatchPreservesDirtyStateUntilEpochReconciles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-dirty").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)

	_, err := f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{projection})
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.True(state.Stale, "an unreconciled global epoch cannot look fresh")

	revisions := store.ContactRevisions{
		IdentityRevision:        projection.Token.IdentityRevision,
		AccountIdentityRevision: projection.Token.AccountIdentityRevision,
	}
	seedActivityReconciledEpoch(t, f.Store, revisions)
	secondMessage := f.NewMessage().
		WithSourceMessageID("projection-dirty-addition").
		WithSentAt(occurredAt.Add(30*time.Minute)).
		Create(t, f.Store)
	addition := activityProjectionForMessage(
		t, f, secondMessage, personID, occurredAt.Add(30*time.Minute))
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{addition})
	require.NoError(err)
	state, err = f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.True(state.Stale, "a pure addition cannot clear pre-existing dirtiness")

	_, err = activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("still-dirty"), messageID)
	require.NoError(err)
	replacement := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt.Add(time.Hour))
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{replacement})
	require.NoError(err)
	state, err = f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.True(state.Stale,
		"a row dirty before this transaction cannot be declared reconciled")
	require.NotNil(state.LastContactAt)
	assert.Equal(occurredAt.Add(time.Hour), state.LastContactAt.UTC())
}

func TestProjectActivityBatchClearsOnlyTransactionLocalDirtyAtReconciledEpoch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	occurredAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("projection-local-dirty").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	initial := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{initial})
	require.NoError(err)

	_, err = activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`, activityTouch("redated"), messageID)
	require.NoError(err)
	replacement := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt.Add(time.Hour))
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{replacement})
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.False(state.Stale)
	var dirty sql.NullTime
	require.NoError(activityQueryRow(f.Store,
		`SELECT dirty_at FROM person_contact_state WHERE person_id = ?`, personID,
	).Scan(&dirty))
	assert.False(dirty.Valid)
}

func TestRecomputeContactStateMatchesIncrementalProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	base := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	for index := range 8 {
		occurredAt := base.Add(time.Duration(index) * 13 * time.Hour)
		messageID := f.NewMessage().
			WithSourceMessageID(fmt.Sprintf("projection-equivalent-%d", index)).
			WithSentAt(occurredAt).
			Create(t, f.Store)
		projection := activityProjectionForMessage(
			t, f, messageID, personID, occurredAt)
		if index%2 == 1 {
			projection.Event.Direction = store.DirectionOutbound
		}
		_, err = f.Store.ProjectActivityBatchContext(
			t.Context(), []store.ActivityProjection{projection})
		require.NoError(err)
	}
	incremental, err := f.Store.ContactStateContext(t.Context(), personID, base)
	require.NoError(err)
	require.NoError(f.Store.RecomputeContactStateContext(
		t.Context(), []int64{personID}, revisions))
	recomputed, err := f.Store.ContactStateContext(t.Context(), personID, base)
	require.NoError(err)
	incremental.ComputedAt = recomputed.ComputedAt
	assert.Equal(recomputed, incremental)
	assert.Equal(int64(8), recomputed.InteractionCount)
}

func TestRecomputeContactStateRejectsPendingProjectionQueue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)

	occurredAt := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("recompute-pending-projection").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{projection})
	require.NoError(err)
	require.NoError(f.Store.MarkAllContactStateDirtyContext(t.Context()))

	_, err = activityExec(f.Store,
		`UPDATE messages SET metadata = ? WHERE id = ?`,
		activityTouch("pending repair"), messageID)
	require.NoError(err)
	err = f.Store.RecomputeContactStateContext(
		t.Context(), []int64{personID}, revisions)
	var stale *store.ErrActivityProjectionStale
	require.ErrorAs(err, &stale)

	state, err := f.Store.ContactStateContext(
		t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.True(state.Stale,
		"recompute must not clear dirtiness while a projection repair is pending")
}

func TestRecomputeContactStateSerializesPostgreSQLQueueInsertBeforeFreshCommit(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := activityProjectionFixture(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL queue-table lock barrier")
	}
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	occurredAt := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().
		WithSourceMessageID("recompute-queue-lock-barrier").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	projection := activityProjectionForMessage(
		t, f, messageID, personID, occurredAt)
	_, err = f.Store.ProjectActivityBatchContext(
		t.Context(), []store.ActivityProjection{projection})
	require.NoError(err)
	require.NoError(f.Store.MarkAllContactStateDirtyContext(t.Context()))

	const advisoryLockKey int64 = 7_310_260_731
	_, err = f.Store.DB().ExecContext(t.Context(), fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION pause_activity_contact_recompute()
		RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(%d);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER pause_activity_contact_recompute
		BEFORE INSERT OR UPDATE ON person_contact_state
		FOR EACH ROW EXECUTE FUNCTION pause_activity_contact_recompute()
	`, advisoryLockKey))
	require.NoError(err)

	blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	blockerDone := false
	t.Cleanup(func() {
		if !blockerDone {
			_ = blocker.Rollback()
		}
	})
	_, err = blocker.ExecContext(t.Context(),
		`SELECT pg_advisory_xact_lock($1)`, advisoryLockKey)
	require.NoError(err)

	recomputeResult := make(chan error, 1)
	go func() {
		recomputeResult <- f.Store.RecomputeContactStateContext(
			t.Context(), []int64{personID}, revisions)
	}()
	waitForPostgreSQLLockWait(t, f.Store, "%person_contact_state%")

	mutationResult := make(chan error, 1)
	go func() {
		_, mutationErr := activityExec(f.Store,
			`UPDATE messages SET metadata = ? WHERE id = ?`,
			activityTouch("queued behind recompute"), messageID)
		mutationResult <- mutationErr
	}()
	mutationBlocked := false
	select {
	case mutationErr := <-mutationResult:
		require.NoError(mutationErr)
	case <-time.After(150 * time.Millisecond):
		mutationBlocked = true
	}
	assert.True(mutationBlocked,
		"trigger queue insert must wait until the freshness transaction commits")

	require.NoError(blocker.Commit())
	blockerDone = true
	require.NoError(<-recomputeResult)
	if mutationBlocked {
		require.NoError(<-mutationResult)
	}
	require.NoError(f.Store.MarkAllContactStateDirtyContext(t.Context()))
	pending, err := f.Store.ListActivityProjectionQueueContext(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(messageID, pending[0].MessageID)
	state, err := f.Store.ContactStateContext(
		t.Context(), personID, occurredAt)
	require.NoError(err)
	assert.True(state.Stale)
}

// TestClaimActivityTimezoneTransitionRejectsHostDependentZone guards the
// persistence boundary: time.LoadLocation accepts "Local", but replay keys on
// the persisted zone name, so a host TZ change would skew new projections
// without ever replaying existing rows.
func TestClaimActivityTimezoneTransitionRejectsHostDependentZone(t *testing.T) {
	f := storetest.New(t)
	_, err := f.Store.ClaimActivityTimezoneTransitionContext(t.Context(), "Local")
	require.ErrorIs(t, err, store.ErrInvalidActivity)
}
