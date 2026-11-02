package query_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicyRecipientHydrationUsesEligibleValues(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	engine, messageID := coldNameSharedRecipientFixture(t)
	rows, err := engine.ListMessages(t.Context(), query.MessageFilter{})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	requirements.Equal(messageID, rows[0].ID)

	for _, recipients := range [][]query.Address{rows[0].To, rows[0].Cc, rows[0].Bcc} {
		requirements.Len(recipients, 2)
		var visible, redacted bool
		for _, recipient := range recipients {
			assertions.NotEqual("cold-only@hidden.test", recipient.Email)
			assertions.NotEqual("Cold Only Contact", recipient.Name)
			if recipient.Email == "visible@example.test" {
				visible = true
				assertions.Equal("visible@example.test", recipient.Name)
			}
			if recipient.Email == "" && recipient.Name == "" {
				redacted = true
			}
		}
		requirements.True(visible, "Full-supported recipient values should remain visible")
		requirements.True(redacted, "Cold-only recipient values should be redacted")
	}
}

func TestConversationPolicyRecipientNameAggregateSearchUsesEligibleNames(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	engine, _ := coldNameSharedRecipientFixture(t)

	byColdName := query.DefaultAggregateOptions()
	byColdName.SearchQuery = "message_type:slack Cold Only Contact"
	rows, err := engine.Aggregate(t.Context(), query.ViewRecipientNames, byColdName)
	requirements.NoError(err)
	assertions.Empty(rows, "a name supported only by a Cold room must not select a Full-room message")

	byEligibleAddress := query.DefaultAggregateOptions()
	byEligibleAddress.SearchQuery = "message_type:slack visible"
	rows, err = engine.Aggregate(t.Context(), query.ViewRecipientNames, byEligibleAddress)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	requirements.Equal("visible@example.test", rows[0].Key)
	requirements.Equal(int64(3), rows[0].Count)

	stats, err := engine.GetTotalStats(t.Context(), query.StatsOptions{
		SearchQuery: "message_type:slack Cold Only Contact",
		SearchScope: true,
		GroupBy:     query.ViewRecipientNames,
	})
	requirements.NoError(err)
	assertions.Zero(stats.MessageCount, "aggregate stats must use the same eligible recipient name")

	stats, err = engine.GetTotalStats(t.Context(), query.StatsOptions{
		SearchQuery: "message_type:slack visible",
		SearchScope: true,
		GroupBy:     query.ViewRecipientNames,
	})
	requirements.NoError(err)
	assertions.Equal(int64(1), stats.MessageCount, "aggregate stats must fall back to the eligible recipient address")
}

func coldNameSharedRecipientFixture(t *testing.T) (*query.SQLiteEngine, int64) {
	t.Helper()
	requirements := require.New(t)
	s := testutil.NewSQLiteTestStore(t)
	source, err := s.GetOrCreateSource("slack", "synthetic:shared-recipient-policy")
	requirements.NoError(err)
	supportRoom, err := s.EnsureConversation(source.ID, "support", "Synthetic support room")
	requirements.NoError(err)
	targetRoom, err := s.EnsureConversation(source.ID, "target", "Synthetic target room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "support", conversationpolicy.Full))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "target", conversationpolicy.Full))

	support, err := s.ResolveConversationParticipantContext(t.Context(), supportRoom, store.ConversationParticipantIdentity{
		IdentifierType: "slack", IdentifierValue: "SYNTHETIC:CONTACT",
		Email: "cold-only@hidden.test", DisplayName: "Cold Only Contact",
	})
	requirements.NoError(err)
	target, err := s.ResolveConversationParticipantContext(t.Context(), targetRoom, store.ConversationParticipantIdentity{
		IdentifierType: "slack", IdentifierValue: "SYNTHETIC:CONTACT",
	})
	requirements.NoError(err)
	requirements.Equal(support.ParticipantID, target.ParticipantID, "the fixture requires one participant shared across rooms")
	visible, err := s.ResolveConversationParticipantContext(t.Context(), targetRoom, store.ConversationParticipantIdentity{
		IdentifierType: "slack", IdentifierValue: "SYNTHETIC:VISIBLE",
		Email: "visible@example.test",
	})
	requirements.NoError(err)

	messageID, err := s.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: targetRoom, SourceMessageID: "target-message",
		MessageType: "slack", Subject: sql.NullString{String: "synthetic message", Valid: true},
		SentAt: sql.NullTime{Time: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), Valid: true},
	})
	requirements.NoError(err)
	for _, recipientType := range []string{"to", "cc", "bcc"} {
		requirements.NoError(s.ReplaceMessageRecipientsContext(t.Context(), messageID, recipientType,
			[]int64{target.ParticipantID, visible.ParticipantID}, []string{"", ""}))
	}
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "support", conversationpolicy.Cold))

	return query.NewSQLiteEngine(s.DB()), messageID
}

// Deletion staging must select the same messages the displayed sender-name
// group does. A name supported only by a cold room is not displayed, so it
// must not stage another source's messages either.
func TestConversationPolicyDeletionStagingUsesEligibleSenderNames(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	ctx := t.Context()
	_, err := st.ConfigureConversationPolicy(ctx, conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	chat, err := st.GetOrCreateSource("slack", "T0SYNTHETIC")
	requirements.NoError(err)
	coldRoom, err := st.EnsureConversation(chat.ID, "cold-room", "Cold room")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(ctx, chat.ID, "cold-room", conversationpolicy.Cold))
	sender, err := st.EnsureParticipant("staged@example.test", "Cold Only Name", "example.test")
	requirements.NoError(err)
	chatMessage, err := st.UpsertMessage(&store.Message{SourceID: chat.ID, ConversationID: coldRoom, SourceMessageID: "cold-name", MessageType: "slack"})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(chatMessage, "from", []int64{sender}, []string{"Cold Only Name"}))

	mail, err := st.GetOrCreateSource("gmail", "reader@example.test")
	requirements.NoError(err)
	thread, err := st.EnsureConversation(mail.ID, "thread", "Thread")
	requirements.NoError(err)
	mailMessage, err := st.UpsertMessage(&store.Message{SourceID: mail.ID, ConversationID: thread, SourceMessageID: "mail", MessageType: "email", SentAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(mailMessage, "from", []int64{sender}, []string{""}))

	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	rows, err := engine.Aggregate(ctx, query.ViewSenderNames, query.DefaultAggregateOptions())
	requirements.NoError(err)
	for _, row := range rows {
		assertions.NotEqual("Cold Only Name", row.Key, "the cold-only name is not displayed")
	}
	targets, err := engine.GetDeletionTargetsByFilter(ctx, query.MessageFilter{SenderName: "Cold Only Name"})
	requirements.NoError(err)
	assertions.Empty(targets, "staging must not select messages the displayed group excludes")
}
