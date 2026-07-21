package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationWindowHydratesParticipantsLabelsBodiesAndAttachments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "portable-thread", "Portable thread")
	require.NoError(err)
	senderID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	toID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	ccID, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err)
	bccID, err := st.EnsureParticipant("dave@example.com", "Dave", "example.com")
	require.NoError(err)

	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "portable-message",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC), Valid: true},
		Subject:         sql.NullString{String: "Portable details", Valid: true},
		Snippet:         sql.NullString{String: "Complete portable fixture", Valid: true},
		SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
		SizeEstimate:    128,
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(messageID,
		sql.NullString{String: "Portable body", Valid: true},
		sql.NullString{String: "<p>Portable body</p>", Valid: true}))
	require.NoError(st.ReplaceMessageRecipients(messageID, "from", []int64{senderID}, []string{"Alice"}))
	require.NoError(st.ReplaceMessageRecipients(messageID, "to", []int64{toID}, []string{"Bob"}))
	require.NoError(st.ReplaceMessageRecipients(messageID, "cc", []int64{ccID}, []string{"Carol"}))
	require.NoError(st.ReplaceMessageRecipients(messageID, "bcc", []int64{bccID}, []string{"Dave"}))
	labelID, err := st.EnsureLabel(source.ID, "STARRED", "Starred", "system")
	require.NoError(err)
	require.NoError(st.ReplaceMessageLabels(messageID, []int64{labelID}))
	require.NoError(st.UpsertAttachment(messageID, "report.pdf", "application/pdf",
		"", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 42))
	require.NoError(st.RecomputeMessageAttachmentStats(messageID))

	window, err := st.GetConversationWindow(conversationID, messageID, 1, 1)
	require.NoError(err)
	require.Len(window.Messages, 1)
	message := window.Messages[0]
	assert.Equal("Alice <alice@example.com>", message.From)
	assert.Equal([]string{"Bob <bob@example.com>"}, message.To)
	assert.Equal([]string{"Carol <carol@example.com>"}, message.Cc)
	assert.Equal([]string{"Dave <dave@example.com>"}, message.Bcc)
	assert.Equal([]string{"Starred"}, message.Labels)
	assert.Equal("Portable body", message.BodyText)
	assert.Equal("<p>Portable body</p>", message.BodyHTML)
	require.Len(message.Attachments, 1)
	assert.Equal("report.pdf", message.Attachments[0].Filename)
}

func TestConversationWindowIncludesSourceDeletedArchiveEntries(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "retained-thread", "Retained thread")
	requirements.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID, SourceMessageID: "retained-message",
		MessageType: "email", SentAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
	})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), messageID)
	requirements.NoError(err)

	window, err := st.GetConversationWindow(conversationID, messageID, 1, 1)
	requirements.NoError(err)
	requirements.NotNil(window)
	requirements.Len(window.Messages, 1)
	assertions.Equal(messageID, window.Messages[0].ID)
}

func TestConversationWindowContextScopesToTimeRange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "ranged-thread", "Ranged thread")
	require.NoError(err)

	day1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	sentTimes := []time.Time{
		day1, day1.Add(time.Minute), day1.Add(2 * time.Minute),
		day2, day2.Add(time.Minute), day2.Add(2 * time.Minute),
	}
	ids := make([]int64, 0, len(sentTimes))
	for i, sentAt := range sentTimes {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: fmt.Sprintf("ranged-message-%d", i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: sentAt, Valid: true},
		})
		require.NoError(err)
		ids = append(ids, id)
	}

	start := day2.Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 1)

	window, err := st.GetConversationWindowContext(context.Background(), conversationID, ids[4], 25, 25, &start, &end)
	require.NoError(err)
	require.NotNil(window)
	require.Len(window.Messages, 3)
	assert.Equal(int64(3), window.Total)
	assert.Equal([]int64{ids[3], ids[4], ids[5]}, []int64{
		window.Messages[0].ID, window.Messages[1].ID, window.Messages[2].ID,
	})
	assert.Equal(int64(2), window.AnchorPosition)

	_, err = st.GetConversationWindowContext(context.Background(), conversationID, ids[0], 25, 25, &start, &end)
	require.ErrorIs(err, store.ErrConversationAnchorOutsideRange)
}

func TestConversationMetadataGetSetAndClear(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "123456789012345678")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)

	metadata, err := st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.False(metadata.Valid)

	want := sql.NullString{
		String: `{"guild_id":"123456789012345678","nsfw":false}`,
		Valid:  true,
	}
	require.NoError(st.SetConversationMetadata(conversationID, want))

	metadata, err = st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.True(metadata.Valid)
	assert.JSONEq(want.String, metadata.String)

	require.NoError(st.SetConversationMetadata(conversationID, sql.NullString{}))
	metadata, err = st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.False(metadata.Valid)
}

func TestConversationMetadataBatchScopesSourceAndPreservesMissingMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-1")
	require.NoError(err)
	otherSource, err := st.GetOrCreateSource("discord", "guild-2")
	require.NoError(err)
	withMetadata, err := st.EnsureConversationWithType(source.ID, "thread-1", "channel", "Thread")
	require.NoError(err)
	_, err = st.EnsureConversationWithType(source.ID, "channel-1", "channel", "Channel")
	require.NoError(err)
	otherConversation, err := st.EnsureConversationWithType(otherSource.ID, "thread-1", "channel", "Other")
	require.NoError(err)
	require.NoError(st.SetConversationMetadata(withMetadata, sql.NullString{
		String: `{"parent_channel_id":"parent-1","discord_channel_type":11}`, Valid: true,
	}))
	require.NoError(st.SetConversationMetadata(otherConversation, sql.NullString{
		String: `{"parent_channel_id":"wrong-source","discord_channel_type":11}`, Valid: true,
	}))

	metadata, err := st.ConversationMetadataBatch(source.ID, []string{"thread-1", "channel-1", "missing"})
	require.NoError(err)
	require.Len(metadata, 2)
	assert.JSONEq(`{"parent_channel_id":"parent-1","discord_channel_type":11}`, metadata["thread-1"].String)
	assert.False(metadata["channel-1"].Valid)
	assert.NotContains(metadata, "missing")
}
