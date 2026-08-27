package query

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonInboxesListsDeduplicatedChatSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b := NewTestDataBuilder(t)
	whatsAppSource := b.AddSourceWithType("whatsapp", "beeper")
	signalSource := b.AddSourceWithType("signal", "beeper")
	unrelatedSource := b.AddSourceWithType("unrelated-chat", "beeper")
	gmailSource := b.AddSourceWithType("owner@example.com", "gmail")

	owner := b.AddParticipant("owner@example.com", "example.com", "Owner")
	subject := b.AddPhoneParticipant("+15550000001", "Subject")
	alias := b.AddParticipant("subject@chat.example", "chat.example", "Subject Chat")
	other := b.AddPhoneParticipant("+15550000002", "Other")
	b.LinkCluster(subject, alias)
	for _, sourceID := range []int64{whatsAppSource, signalSource, unrelatedSource, gmailSource} {
		b.AddOwnerParticipant(sourceID, owner)
	}

	whatsAppConversation := int64(500)
	for _, participantID := range []int64{owner, subject, alias, other} {
		b.AddConversationParticipant(whatsAppConversation, participantID)
	}
	receivedOld := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	receivedNew := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	sentWhatsApp := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	for _, message := range []struct {
		at       time.Time
		from, to int64
		fromMe   bool
	}{
		{at: receivedOld, from: subject, to: owner},
		{at: receivedNew, from: other, to: owner},
		{at: sentWhatsApp, from: owner, to: subject, fromMe: true},
	} {
		messageID := b.AddMessage(MessageOpt{
			SourceID: whatsAppSource, ConversationID: whatsAppConversation,
			MessageType: "whatsapp", ConversationType: "group_chat",
			SentAt: message.at, IsFromMe: message.fromMe,
		})
		b.AddFrom(messageID, message.from, "")
		b.AddTo(messageID, message.to, "")
	}

	signalConversation := int64(600)
	for _, participantID := range []int64{owner, alias} {
		b.AddConversationParticipant(signalConversation, participantID)
	}
	receivedSignal := time.Date(2026, 8, 16, 7, 0, 0, 0, time.UTC)
	signalMessage := b.AddMessage(MessageOpt{
		SourceID: signalSource, ConversationID: signalConversation,
		MessageType: "beeper", ConversationType: "direct_chat", SentAt: receivedSignal,
	})
	b.AddFrom(signalMessage, alias, "Subject Chat")
	b.AddTo(signalMessage, owner, "Owner")

	b.AddConversationParticipant(650, owner)
	b.AddConversationParticipant(650, other)
	unrelatedMessage := b.AddMessage(MessageOpt{
		SourceID: unrelatedSource, ConversationID: 650,
		MessageType: "beeper", ConversationType: "direct_chat", SentAt: receivedSignal,
	})
	b.AddFrom(unrelatedMessage, other, "Other")
	b.AddTo(unrelatedMessage, owner, "Owner")

	emailMessage := b.AddMessage(MessageOpt{
		SourceID: gmailSource, ConversationID: 700,
		MessageType: "email", ConversationType: "email",
		SentAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
	})
	b.AddFrom(emailMessage, subject, "Subject")
	b.AddTo(emailMessage, owner, "Owner")

	engine := b.BuildEngine()
	state, err := ReadCacheSyncState(engine.analyticsDir)
	require.NoError(err)
	state.IdentityRevision = 9
	stateData, err := json.Marshal(state)
	require.NoError(err)
	require.NoError(os.WriteFile(CacheStatePath(engine.analyticsDir), stateData, 0o600))

	result, err := engine.ListPersonInboxes(context.Background(), PersonInboxRequest{CanonicalID: subject})
	require.NoError(err)
	require.Len(result.Rows, 2)

	assert.Equal(state.Revision(), result.CacheRevision)
	assert.Equal(int64(9), result.IdentityRevision)
	assert.Equal(PersonInboxRow{
		SourceID: whatsAppSource, SourceType: "beeper", SourceIdentifier: "whatsapp",
		ConversationCount: 1, ReceivedCount: 2, SentCount: 1,
		LatestReceivedAt: &receivedNew, LatestSentAt: &sentWhatsApp, LatestAt: sentWhatsApp,
	}, result.Rows[0])
	assert.Equal(PersonInboxRow{
		SourceID: signalSource, SourceType: "beeper", SourceIdentifier: "signal",
		ConversationCount: 1, ReceivedCount: 1, SentCount: 0,
		LatestReceivedAt: &receivedSignal, LatestAt: receivedSignal,
	}, result.Rows[1])
}

func TestPersonInboxesRejectsNonPositiveCanonicalID(t *testing.T) {
	engine := NewTestDataBuilder(t).BuildEngine()
	_, err := engine.ListPersonInboxes(context.Background(), PersonInboxRequest{})
	require.ErrorIs(t, err, ErrInvalidExploreRequest)
}
