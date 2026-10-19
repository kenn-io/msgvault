package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
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
	assertions.False(view.HistoryComplete, "the bounded window reports older messages outside its limit")
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
	requirements.ErrorIs(err, store.ErrInvalidLoop)
	completeView, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, loops.MaxConversationMessageLimit)
	requirements.NoError(err)
	assertions.True(completeView.HistoryComplete, "the full archive fits in the requested window")
	_, err = f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, loops.MaxConversationMessageLimit+1)
	assertions.ErrorIs(err, store.ErrInvalidLoop)
}

func TestLoopLoaderExcludesLegacyUnsentSynctechFromActivityTime(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("synctech_sms", "+15550000001")
	requirements.NoError(err)
	conversationID, err := f.Store.EnsureConversation(source.ID, "legacy-unsent", "Legacy SMS thread")
	requirements.NoError(err)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	oldAt := now.Add(-400 * 24 * time.Hour)
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	requirements.NoError(f.Store.AddAccountIdentity(source.ID, "owner@example.test", "manual"))
	loopExec(t, f.Store, "UPDATE conversations SET conversation_type='direct_chat' WHERE id=?", conversationID)
	for _, participant := range []int64{owner, peer} {
		loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", conversationID, participant)
	}
	var deliveredID int64
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO messages(
		source_id,conversation_id,source_message_id,message_type,sent_at,source_is_from_me,is_sent,sender_id,subject,snippet
	) VALUES(?,?,?,'sms',?,FALSE,TRUE,?,'Old question','Could you help?') RETURNING id`),
		source.ID, conversationID, "delivered-old", loopTestStamp(f.Store, oldAt), peer).Scan(&deliveredID))
	loopExec(t, f.Store, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", deliveredID, owner)
	loopExec(t, f.Store, `INSERT INTO messages(
		source_id,conversation_id,source_message_id,message_type,sent_at,source_is_from_me,is_sent,sender_id,subject,snippet
	) VALUES(?,?,?,'sms',?,TRUE,FALSE,?,'Pending text','Not yet sent')`,
		source.ID, conversationID, "legacy-unsent", loopTestStamp(f.Store, now.Add(-time.Hour)), owner)

	loaded, err := f.Store.LoadLoopConversationContext(t.Context(), conversationID, 10)
	requirements.NoError(err)
	assertions.True(oldAt.Equal(loaded.LastMessageAt))
	requirements.Len(loaded.Messages, 1)
	assertions.Equal(deliveredID, loaded.Messages[0].ID)
	verdict := loops.Detect(loopViewFromLoadedConversation(t, loaded, f.Store), nil, now, loops.DefaultThresholds(), loops.Suppressions{})
	requirements.NoError(verdict.Err)
	requirements.Nil(verdict.Open, "the old delivered message stays outside the lookback window")
}

func TestLoopLoaderExcludesCurrentProviderDrafts(t *testing.T) {
	for _, provider := range []string{"gmail", "imap"} {
		t.Run(provider, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := storetest.New(t)
			sourceID, err := f.Store.GetOrCreateSource(provider, "owner@example.test")
			requirements.NoError(err)
			conversationID, err := f.Store.EnsureConversation(sourceID.ID, "draft-thread", "Draft thread")
			requirements.NoError(err)

			participants := []store.ParticipantPersistData{
				{EmailAddress: "owner@example.test", Domain: "example.test"},
				{EmailAddress: "peer@example.test", Domain: "example.test"},
			}
			build := func(sourceMessageID, threadID string) func([]int64) *store.MessagePersistData {
				return func(ids []int64) *store.MessagePersistData {
					return &store.MessagePersistData{
						Message: &store.Message{
							SourceID: sourceID.ID, SourceMessageID: sourceMessageID,
							ConversationID: conversationID, MessageType: store.MessageTypeEmail,
							SenderID: sql.NullInt64{Int64: ids[0], Valid: true},
							SentAt:   sql.NullTime{Time: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Valid: true},
							IsFromMe: true,
						},
						Conversation: &store.ConversationPersistData{
							SourceConversationID: threadID,
							ConversationType:     "email_thread",
							Title:                "Draft thread",
						},
						Recipients: []store.RecipientSet{
							{Type: "from", ParticipantIDs: ids[:1], EmailAddresses: []string{"owner@example.test"}},
							{Type: "to", ParticipantIDs: ids[1:], EmailAddresses: []string{"peer@example.test"}},
						},
					}
				}
			}

			var messageID int64
			switch provider {
			case "gmail":
				receipt := store.GmailDraftReceipt{
					SourceID: sourceID.ID, GmailDraftID: "gmail-draft-1",
					GmailMessageID: "gmail-message-1", ThreadID: "draft-thread",
				}
				draft, persistErr := f.Store.PersistGmailDraftContext(t.Context(), receipt, participants, build(receipt.GmailMessageID, receipt.ThreadID))
				requirements.NoError(persistErr)
				messageID = draft.CurrentMessageID
			case "imap":
				receipt := store.IMAPDraftReceipt{SourceID: sourceID.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
				draft, persistErr := f.Store.PersistIMAPDraftContext(t.Context(), receipt, participants, build(store.IMAPDraftSourceMessageID(receipt), "draft-thread"))
				requirements.NoError(persistErr)
				messageID = draft.CurrentMessageID
			}
			ownerID, err := f.Store.EnsureParticipant("owner@example.test", "", "example.test")
			requirements.NoError(err)
			peerID, err := f.Store.EnsureParticipant("peer@example.test", "", "example.test")
			requirements.NoError(err)
			controlID, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message: &store.Message{
					SourceID: sourceID.ID, SourceMessageID: "sent-control",
					ConversationID: conversationID, MessageType: store.MessageTypeEmail,
					SenderID: sql.NullInt64{Int64: ownerID, Valid: true},
					SentAt:   sql.NullTime{Time: time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC), Valid: true},
					IsFromMe: true,
				},
				Recipients: []store.RecipientSet{
					{Type: "from", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{"owner@example.test"}},
					{Type: "to", ParticipantIDs: []int64{peerID}, EmailAddresses: []string{"peer@example.test"}},
				},
			})
			requirements.NoError(err)

			view, err := f.Store.LoadLoopConversationContext(t.Context(), conversationID, 10)
			requirements.NoError(err)
			requirements.Len(view.Messages, 1)
			assertions.Equal(controlID, view.Messages[0].ID)
			assertions.NotEqual(messageID, view.Messages[0].ID)
			assertions.Equal(store.DirectionOutbound, view.Messages[0].Direction)
			assertions.False(view.Messages[0].Draft)
		})
	}
}

func TestLoopLoaderExcludesUntimedMessages(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	ids := f.CreateMessages(3)
	dated := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	loopExec(t, f.Store, "UPDATE messages SET sent_at=?,source_is_from_me=TRUE WHERE id=?", loopTestStamp(f.Store, dated), ids[1])

	view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 3)
	requirements.NoError(err)
	requirements.Len(view.Messages, 1)
	assertions.Equal(ids[1], view.Messages[0].ID, "the dated sent control must be returned")
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

func TestLoopListIncludesSnoozedClosedHistory(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	id := insertSchemaLoop(t, f, "snoozed-closed-history", "reply_owed", "turn_state")
	loopExec(t, f.Store, "UPDATE open_loops SET snoozed_until=? WHERE id=?", loopTestStamp(f.Store, now.Add(24*time.Hour)), id)
	closed, err := f.Store.CloseLoopContext(t.Context(), id, "handled", now)
	requirements.NoError(err)
	assertions.Equal("closed", closed.Status)

	rows, err := f.Store.ListOpenLoopsContext(t.Context(), store.LoopListFilter{Status: "all", Limit: 20, Now: now})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(id, rows[0].ID)
	assertions.Equal("closed", rows[0].Status)
}

func TestLoopPersonFilterResolvesPromotionAfterClosure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	counterparty := f.EnsureParticipant("promoted-after-close@example.test", "Promoted Counterparty", "example.test")
	messageID := f.CreateMessage("closed-before-promotion")
	loopExec(t, f.Store, "UPDATE messages SET sender_id=?,sent_at=? WHERE id=?", counterparty, loopTestStamp(f.Store, now), messageID)
	open := loopOpen(t, f, messageID, messageID, now)
	open.CounterpartyParticipantID = &counterparty
	applyLoop(t, f, open, now)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	closed, err := f.Store.CloseLoopContext(t.Context(), live[0].ID, "handled", now)
	requirements.NoError(err)
	requirements.NotNil(closed.ClosedReason)
	assertions.Equal("done", *closed.ClosedReason)

	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), counterparty)
	requirements.NoError(err)
	rows, err := f.Store.ListOpenLoopsContext(t.Context(), store.LoopListFilter{
		Status: "all", PersonID: person.ID, IncludeSnoozed: true, Limit: 20, Now: now,
	})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(live[0].ID, rows[0].ID)
	assertions.Equal("closed", rows[0].Status)
	requirements.NotNil(rows[0].CounterpartyPersonID)
	assertions.Equal(person.ID, *rows[0].CounterpartyPersonID)
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
	requirements.Len(view.Participants, 2)
	assertions.Equal("Owner", view.Participants[0].DisplayName)
	assertions.True(view.Participants[0].IsOwner)
	assertions.Equal("Peer", view.Participants[1].DisplayName)
	assertions.False(view.Participants[1].IsOwner)
	exists, err := f.Store.LoopConversationAnchorExistsContext(t.Context(), f.ConvID, 0)
	requirements.NoError(err)
	assertions.True(exists)
}

func TestLoopLoaderSkipsNullProviderLabelID(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	message := f.CreateMessage("nullable-provider-label")
	loopExec(t, f.Store, "UPDATE messages SET sent_at=? WHERE id=?", loopTestStamp(f.Store, time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)), message)
	var localLabel int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("INSERT INTO labels(source_id,name,label_type) VALUES(?,?,'user') RETURNING id"), f.Source.ID, "Local label").Scan(&localLabel))
	providerLabel, err := f.Store.EnsureLabel(f.Source.ID, "provider-label", "Provider label", "user")
	requirements.NoError(err)
	for _, label := range []int64{localLabel, providerLabel} {
		loopExec(t, f.Store, "INSERT INTO message_labels(message_id,label_id) VALUES(?,?)", message, label)
	}
	var storedProviderLabelID sql.NullString
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT source_label_id FROM labels WHERE id=?"), providerLabel).Scan(&storedProviderLabelID))
	requirements.True(storedProviderLabelID.Valid)
	requirements.Equal("provider-label", storedProviderLabelID.String)
	view, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 10)
	requirements.NoError(err)
	requirements.Len(view.Messages, 1)
	assert.Equal(t, []string{"provider-label"}, view.Messages[0].Labels)
}

func TestLoopLoaderPreservesGmailCategoryEvidenceAfterSourceMerge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	from, err := f.Store.GetOrCreateSource("gmail", "history-account@example.test")
	requirements.NoError(err)
	into, err := f.Store.GetOrCreateSource("gmail", "live-account@example.test")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(from.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	conversationID, err := f.Store.EnsureConversationWithType(from.ID, "category-thread", "email_thread", "Promotional email")
	requirements.NoError(err)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	messageID, err := f.Store.UpsertMessage(&store.Message{
		SourceID: from.ID, ConversationID: conversationID, SourceMessageID: "promotions-only-inbound",
		MessageType: "email", SentAt: sql.NullTime{Time: now.Add(-48 * time.Hour), Valid: true},
		SenderID: sql.NullInt64{Int64: peer, Valid: true}, Subject: sql.NullString{String: "Weekly update", Valid: true},
		Snippet: sql.NullString{String: "An update for subscribers", Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(f.Store.UpsertMessageBody(messageID, sql.NullString{String: "An update for subscribers", Valid: true}, sql.NullString{}))
	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "from", []int64{peer}, []string{"Peer"}))
	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", []int64{owner}, []string{"Owner"}))
	categoryLabels := []string{"CATEGORY_PROMOTIONS", "CATEGORY_SOCIAL", "CATEGORY_FORUMS"}
	descriptors := make(map[string]store.LabelInfo, len(categoryLabels))
	for _, category := range categoryLabels {
		descriptors[category] = store.LabelInfo{Name: category, Type: "system"}
	}
	labelIDs, err := f.Store.EnsureLabelsBatch(from.ID, descriptors)
	requirements.NoError(err)
	for _, category := range categoryLabels {
		requirements.NoError(f.Store.AddMessageLabels(messageID, []int64{labelIDs[category]}))
	}

	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	requirements.NoError(err)
	loaded, err := f.Store.LoadLoopConversationContext(t.Context(), conversationID, 10)
	requirements.NoError(err)
	requirements.Len(loaded.Messages, 1)
	assertions.ElementsMatch(categoryLabels, loaded.Messages[0].Labels)
	verdict := loops.Detect(loopViewFromLoadedConversation(t, loaded, f.Store), nil, now, loops.DefaultThresholds(), loops.Suppressions{})
	requirements.NoError(verdict.Err)
	requirements.Nil(verdict.Open, "a message excluded by Gmail's Promotions category must not open a reply loop")
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

func TestLoopLoaderGroupParticipantsExcludeDepartedSenders(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	source, err := f.Store.GetOrCreateSource("beeper", "synthetic-group-source")
	requirements.NoError(err)
	conversation, err := f.Store.EnsureConversation(source.ID, "group-membership", "Group membership")
	requirements.NoError(err)
	loopExec(t, f.Store, "UPDATE conversations SET conversation_type='group_chat' WHERE id=?", conversation)
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	current := f.EnsureParticipant("current@example.test", "Current member", "example.test")
	departed := f.EnsureParticipant("former@example.test", "Former member", "example.test")
	loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?),(?,?)", conversation, owner, conversation, current)
	loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id,left_at) VALUES(?,?,?)", conversation, departed, loopTestStamp(f.Store, now.Add(-time.Hour)))
	for _, message := range []struct {
		id     string
		sender int64
		at     time.Time
	}{
		{id: "old-departed-message", sender: departed, at: now.Add(-30 * 24 * time.Hour)},
		{id: "current-member-message", sender: current, at: now},
	} {
		_, err := f.Store.UpsertMessage(&store.Message{
			ConversationID:  conversation,
			SourceID:        source.ID,
			SourceMessageID: message.id,
			MessageType:     "chat",
			SentAt:          sql.NullTime{Time: message.at, Valid: true},
			SenderID:        sql.NullInt64{Int64: message.sender, Valid: true},
		})
		requirements.NoError(err)
	}

	view, err := f.Store.LoadLoopConversationContext(t.Context(), conversation, 1)
	requirements.NoError(err)
	requirements.Len(view.Messages, 1)
	assertions.Equal(current, view.Messages[0].Sender.ParticipantID)
	participantIDs := make([]int64, 0, len(view.Participants))
	for _, participant := range view.Participants {
		participantIDs = append(participantIDs, participant.ParticipantID)
	}
	assertions.ElementsMatch([]int64{owner, current}, participantIDs, "group eligibility includes current members, not departed historical senders")
}
