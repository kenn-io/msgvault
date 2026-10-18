package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopLoaderOrderAndNativeDirection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(3)
	for _, id := range ids {
		loopExec(t, f.Store, "UPDATE messages SET sent_at=?,source_is_from_me=TRUE WHERE id=?", now, id)
	}
	loopExec(t, f.Store, "INSERT INTO message_bodies(message_id,body_text) VALUES(?,?)", ids[2], "direct body")
	view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 2)
	requirements.NoError(err)
	requirements.Len(view.Messages, 2)
	assertions.Equal(ids[1], view.Messages[0].ID)
	assertions.Equal(ids[2], view.Messages[1].ID)
	assertions.Equal(store.DirectionOutbound, view.Messages[0].Direction)
	body, err := f.Store.LoopMessageBodyContext(t.Context(), ids[2])
	requirements.NoError(err)
	assertions.Equal("direct body", body)
	exists, err := f.Store.LoopConversationAnchorExistsContext(t.Context(), f.ConvID, ids[2])
	requirements.NoError(err)
	assertions.True(exists)
	otherConversation, err := f.Store.EnsureConversation(f.Source.ID, "other-anchor-thread", "Other anchor thread")
	requirements.NoError(err)
	exists, err = f.Store.LoopConversationAnchorExistsContext(t.Context(), otherConversation, ids[2])
	requirements.NoError(err)
	assertions.False(exists, "an anchor in another conversation is not a window gap")
	loopExec(t, f.Store, "UPDATE messages SET deleted_at=? WHERE id=?", now, ids[2])
	exists, err = f.Store.LoopConversationAnchorExistsContext(t.Context(), f.ConvID, ids[2])
	requirements.NoError(err)
	assertions.False(exists)
	_, err = f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 0)
	assertions.ErrorIs(err, store.ErrInvalidLoop)
}

func TestLoopLoaderKeepsUndatedMessagesOutOfBoundedWindow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	base := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(4)
	for _, id := range ids {
		loopExec(t, f.Store, "UPDATE messages SET sent_at=NULL,received_at=NULL,internal_date=NULL WHERE id=?", id)
	}
	for i, id := range ids[:3] {
		loopExec(t, f.Store, "UPDATE messages SET sent_at=? WHERE id=?", loopTestStamp(f.Store, base.Add(time.Duration(i)*time.Hour)), id)
	}

	view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 2)
	requirements.NoError(err)
	requirements.Len(view.Messages, 2)
	assertions.Equal(ids[1], view.Messages[0].ID)
	assertions.Equal(ids[2], view.Messages[1].ID)
	assertions.True(view.WindowTruncated)
}

func TestLoopListFiltersAndCursor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	first := insertSchemaLoop(t, f, "first", "reply_owed", "turn_state")
	second := insertSchemaLoop(t, f, "second", "awaiting_reply", "turn_state")
	loopExec(t, f.Store, "UPDATE open_loops SET owed_since_at=?,opens_at=?", loopTestStamp(f.Store, now), loopTestStamp(f.Store, now))
	loopExec(t, f.Store, "UPDATE open_loops SET snoozed_until=? WHERE id=?", now.Add(time.Hour), second)
	rows, err := f.Store.ListOpenLoopsContext(t.Context(), store.LoopListFilter{Status: "open", Limit: 201, Now: now})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(first, rows[0].ID)
	rows, err = f.Store.ListOpenLoopsContext(t.Context(), store.LoopListFilter{Status: "all", IncludeSnoozed: true, HasCursor: true, CursorAt: now, CursorID: first, Limit: 201, Now: now})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(second, rows[0].ID)
	_, err = f.Store.GetOpenLoopContext(t.Context(), 999999)
	assertions.ErrorIs(err, store.ErrLoopNotFound)
}

func loopTestStamp(s *store.Store, at time.Time) any {
	if s.IsPostgreSQL() {
		return at
	}
	return at.UTC().Format(store.SQLiteTimestampLayout)
}

func TestLoopLoaderParticipantParityAndLegalNonpositiveMessageIDs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	override := ""
	if f.Store.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	for _, id := range []int64{-1, 0} {
		loopExec(t, f.Store, "INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,sender_id)"+override+" VALUES(?,?,?,?,?,?,?)", id, f.ConvID, f.Source.ID, fmt.Sprintf("legal-%d", id), "email", loopTestStamp(f.Store, now), peer)
		loopExec(t, f.Store, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", id, owner)
	}
	loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", f.ConvID, peer)
	view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 2)
	requirements.NoError(err)
	requirements.Len(view.Messages, 2)
	assertions.Equal(int64(-1), view.Messages[0].ID)
	assertions.Equal(int64(0), view.Messages[1].ID)
	for _, message := range view.Messages {
		assertions.Equal(store.DirectionInbound, message.Direction)
		assertions.Equal("owner@example.test", message.OwnerAddress)
		assertions.Equal(peer, message.Sender.ParticipantID)
		requirements.NotNil(message.Sender.PersonID)
		assertions.Equal(person.ID, *message.Sender.PersonID)
		requirements.Len(message.Recipients, 1)
		assertions.True(message.Recipients[0].IsOwner)
	}
	participantsByEmail := make(map[string]store.LoopParticipant, len(view.Participants))
	for _, participant := range view.Participants {
		participantsByEmail[participant.EmailAddress] = participant
	}
	requirements.Len(participantsByEmail, 2)
	assertions.True(participantsByEmail["owner@example.test"].IsOwner)
	assertions.Equal("Peer", participantsByEmail["peer@example.test"].DisplayName)
	exists, err := f.Store.LoopConversationAnchorExistsContext(t.Context(), f.ConvID, 0)
	requirements.NoError(err)
	assertions.True(exists)
}

func TestLoopLoaderSkipsNullProviderLabelID(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	message := f.CreateMessage("nullable-provider-label")
	var localLabel int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("INSERT INTO labels(source_id,name,label_type) VALUES(?,?,'user') RETURNING id"), f.Source.ID, "Local label").Scan(&localLabel))
	providerLabel, err := f.Store.EnsureLabel(f.Source.ID, "provider-label", "Provider label", "user")
	requirements.NoError(err)
	for _, label := range []int64{localLabel, providerLabel} {
		loopExec(t, f.Store, "INSERT INTO message_labels(message_id,label_id) VALUES(?,?)", message, label)
	}
	view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 10)
	requirements.NoError(err)
	requirements.Len(view.Messages, 1)
	assert.Equal(t, []string{"provider-label"}, view.Messages[0].Labels)
}

func TestLoopLoaderHydratesDepartedRecipientMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	source, err := f.Store.GetOrCreateSource("beeper", "synthetic-chat-source")
	requirements.NoError(err)
	conversation, err := f.Store.EnsureConversation(source.ID, "departed-member-chat", "Departed member chat")
	requirements.NoError(err)
	loopExec(t, f.Store, "UPDATE conversations SET conversation_type='direct_chat' WHERE id=?", conversation)
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	departed := f.EnsureParticipant("former@example.test", "Former member", "example.test")
	loopExec(t, f.Store, "UPDATE participants SET phone_number=?,canonical_id=? WHERE id=?", "+12025550188", "synthetic-former", departed)
	person, _, err := f.Store.CreatePersonFromParticipant(departed)
	requirements.NoError(err)
	loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", conversation, owner)
	loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id,left_at) VALUES(?,?,?)", conversation, departed, loopTestStamp(f.Store, now.Add(-time.Hour)))
	_, err = f.Store.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: source.ID, SourceMessageID: "native-member-message", MessageType: "chat", SentAt: sql.NullTime{Time: now, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}, IsFromMe: true})
	requirements.NoError(err)
	view, err := f.Store.LoadLoopConversationContext(t.Context(), conversation, 10)
	requirements.NoError(err)
	requirements.Len(view.Messages, 1)
	assertions.Equal(store.DirectionOutbound, view.Messages[0].Direction)
	recipients := map[int64]store.LoopRecipient{}
	for _, recipient := range view.Messages[0].Recipients {
		recipients[recipient.ParticipantID] = recipient
		assertions.NotZero(recipient.ParticipantID)
	}
	expected := store.LoopParticipant{ParticipantID: departed, PersonID: &person.ID, EmailAddress: "former@example.test", PhoneNumber: "+12025550188", DisplayName: "Former member", CanonicalID: "synthetic-former"}
	assertions.Equal(expected, recipients[departed].LoopParticipant)
	assertions.Equal("member", recipients[departed].RecipientType)
	requirements.Len(view.Participants, 1)
	assertions.Equal(owner, view.Participants[0].ParticipantID)
	assertions.True(view.Participants[0].IsOwner)
}

func TestLoopLoaderUsesMessageActivityWhenConversationTimestampIsNull(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	first := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	latest := first.Add(time.Hour)
	ids := f.CreateMessages(2)
	loopExec(t, f.Store, "UPDATE messages SET sent_at=? WHERE id=?", loopTestStamp(f.Store, first), ids[0])
	loopExec(t, f.Store, "UPDATE messages SET sent_at=? WHERE id=?", loopTestStamp(f.Store, latest), ids[1])
	loopExec(t, f.Store, "UPDATE conversations SET last_message_at=NULL WHERE id=?", f.ConvID)

	view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 10)
	requirements.NoError(err)
	assertions.True(latest.Equal(view.LastMessageAt))
}

func TestLoopLoaderIncludesEmailRecipientsButKeepsChatRosterSemantics(t *testing.T) {
	t.Run("sent-only email recipient is eligible", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		f := storetest.New(t)
		now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
		owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
		recipient := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
		requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
		message := f.CreateMessage("sent-only-email")
		loopExec(t, f.Store, "UPDATE messages SET sent_at=?,sender_id=?,source_is_from_me=TRUE WHERE id=?", loopTestStamp(f.Store, now), owner, message)
		loopExec(t, f.Store, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", message, recipient)

		view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 10)
		requirements.NoError(err)
		var participantIDs []int64
		for _, participant := range view.Participants {
			participantIDs = append(participantIDs, participant.ParticipantID)
		}
		assertions.Contains(participantIDs, recipient)
	})

	t.Run("chat recipient is not promoted to conversation member", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		f := storetest.New(t)
		now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
		source, err := f.Store.GetOrCreateSource("beeper", "synthetic-chat-source")
		requirements.NoError(err)
		conversation, err := f.Store.EnsureConversation(source.ID, "chat-roster", "Chat roster")
		requirements.NoError(err)
		loopExec(t, f.Store, "UPDATE conversations SET conversation_type='direct_chat' WHERE id=?", conversation)
		owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
		recipient := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
		loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", conversation, owner)
		message, err := f.Store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "chat-message", MessageType: "chat", SenderID: sql.NullInt64{Int64: owner, Valid: true}, SentAt: sql.NullTime{Time: now, Valid: true}, IsFromMe: true})
		requirements.NoError(err)
		loopExec(t, f.Store, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", message, recipient)

		view, err := f.Store.LoadLoopConversationContext(t.Context(), conversation, 10)
		requirements.NoError(err)
		var participantIDs []int64
		for _, participant := range view.Participants {
			participantIDs = append(participantIDs, participant.ParticipantID)
		}
		assertions.Contains(participantIDs, owner)
		assertions.NotContains(participantIDs, recipient)
	})
}

func TestLoopLoaderExcludesDraftsBeforeApplyingWindowLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(2)
	loopExec(t, f.Store, "UPDATE messages SET sent_at=? WHERE id=?", loopTestStamp(f.Store, now.Add(-time.Hour)), ids[0])
	loopExec(t, f.Store, "UPDATE messages SET sent_at=? WHERE id=?", loopTestStamp(f.Store, now), ids[1])
	labels, err := f.Store.EnsureLabelsBatch(f.Source.ID, map[string]store.LabelInfo{
		"DRAFT": {Name: "DRAFT", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
	})
	requirements.NoError(err)
	loopExec(t, f.Store, "INSERT INTO message_labels(message_id,label_id) VALUES(?,?)", ids[1], labels["DRAFT"])

	view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 1)
	requirements.NoError(err)
	requirements.Len(view.Messages, 1)
	assertions.Equal(ids[0], view.Messages[0].ID, "a draft must not consume the bounded evidence window")
}
