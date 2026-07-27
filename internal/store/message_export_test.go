package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type collectingMessageExportSink struct {
	phases        []string
	sources       []store.MessageExportSource
	conversations []store.MessageExportConversation
	messages      []store.MessageExportMessage
}

func (s *collectingMessageExportSink) Source(source store.MessageExportSource) error {
	s.phases = append(s.phases, "source")
	s.sources = append(s.sources, source)
	return nil
}

func (s *collectingMessageExportSink) Conversation(
	conversation store.MessageExportConversation,
) error {
	s.phases = append(s.phases, "conversation")
	s.conversations = append(s.conversations, conversation)
	return nil
}

func (s *collectingMessageExportSink) Message(message store.MessageExportMessage) error {
	s.phases = append(s.phases, "message")
	s.messages = append(s.messages, message)
	return nil
}

func TestExportMessagesStreamsDeterministicPhases(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	gmail, err := st.GetOrCreateSource("gmail", "person@example.test")
	requirements.NoError(err)
	discord, err := st.GetOrCreateSource("discord", "guild-1")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(discord.ID, "Example Guild"))

	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO sync_runs (
			source_id, started_at, completed_at, status
		) VALUES (?, ?, ?, 'completed')
	`), discord.ID, start.Add(-time.Hour), start)
	requirements.NoError(err)
	discordConversation := insertMessageExportConversation(
		t, st, discord.ID, "200", "general", "channel", `{}`,
	)
	gmailConversation := insertMessageExportConversation(
		t, st, gmail.ID, "100", "Subject", "email_thread", `{}`,
	)
	insertMessageExportMessage(
		t, st, gmail.ID, gmailConversation, "m-2", "email",
		start.Add(2*time.Hour), "Mail body", "Mail subject", 0, false, false,
	)
	insertMessageExportMessage(
		t, st, discord.ID, discordConversation, "m-1", "discord",
		start.Add(time.Hour), "Chat body", "", 0, false, false,
	)

	sink := &collectingMessageExportSink{}
	counts, err := st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(24 * time.Hour),
	}, sink)
	requirements.NoError(err)

	assertions.Equal(store.MessageExportCounts{
		Sources: 2, Conversations: 2, Messages: 2,
	}, counts)
	assertions.Equal([]string{"source", "source", "conversation", "conversation", "message", "message"},
		sink.phases,
	)
	requirements.Len(sink.sources, 2)
	assertions.Equal("discord", sink.sources[0].SourceType)
	assertions.Equal("guild-1", sink.sources[0].Identifier)
	assertions.Equal("Example Guild", sink.sources[0].DisplayName)
	requirements.NotNil(sink.sources[0].LastSuccessfulSyncAt)
	assertions.Equal(start, *sink.sources[0].LastSuccessfulSyncAt)
	assertions.Equal("gmail", sink.sources[1].SourceType)
	assertions.Nil(sink.sources[1].LastSuccessfulSyncAt)

	requirements.Len(sink.conversations, 2)
	assertions.Equal("discord", sink.conversations[0].SourceType)
	assertions.Equal("200", sink.conversations[0].ID)
	assertions.Equal(store.MessageExportConversationChannel, sink.conversations[0].ConversationType)
	assertions.Equal("gmail", sink.conversations[1].SourceType)

	requirements.Len(sink.messages, 2)
	assertions.Equal("discord", sink.messages[0].SourceType)
	assertions.Equal("m-1", sink.messages[0].ID)
	assertions.Equal("Chat body", sink.messages[0].Text)
	assertions.Equal(start.Add(time.Hour), sink.messages[0].OccurredAt)
	assertions.Equal("gmail", sink.messages[1].SourceType)
	assertions.Equal("Mail subject", sink.messages[1].Subject)
}

func TestExportMessagesUsesHalfOpenEffectiveTimestampWindow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "fixture")
	requirements.NoError(err)
	conversationID := insertMessageExportConversation(
		t, st, source.ID, "conversation", "", "direct_chat", `{}`,
	)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	sentID := insertMessageExportMessage(
		t, st, source.ID, conversationID, "sent", "text",
		start, "sent", "", 0, false, false,
	)
	receivedID := insertMessageExportMessage(
		t, st, source.ID, conversationID, "received", "text",
		start.Add(time.Hour), "received", "", 0, false, false,
	)
	internalID := insertMessageExportMessage(
		t, st, source.ID, conversationID, "internal", "text",
		start.Add(2*time.Hour), "internal", "", 0, false, false,
	)
	endID := insertMessageExportMessage(
		t, st, source.ID, conversationID, "end", "text",
		start.Add(3*time.Hour), "end", "", 0, false, false,
	)
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET sent_at = NULL WHERE id IN (?, ?)
	`), receivedID, internalID)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET sent_at = NULL, received_at = NULL, internal_date = ?
		WHERE id = ?
	`), start.Add(2*time.Hour), internalID)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET sent_at = NULL, received_at = NULL, internal_date = NULL
		WHERE id = ?
	`), sentID)
	requirements.NoError(err)

	sink := &collectingMessageExportSink{}
	counts, err := st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(3 * time.Hour),
	}, sink)
	requirements.NoError(err)
	assertions.Equal(2, counts.Messages)
	requirements.Len(sink.messages, 2)
	assertions.Equal([]string{"received", "internal"}, []string{
		sink.messages[0].ID, sink.messages[1].ID,
	})
	assertions.Equal(start.Add(time.Hour), sink.messages[0].OccurredAt)
	assertions.Equal(start.Add(2*time.Hour), sink.messages[1].OccurredAt)

	var endCount int
	requirements.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE id = ?`), endID,
	).Scan(&endCount))
	assertions.Equal(1, endCount)
}

func TestExportMessagesFiltersSourcesAndMessageTypes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource("text", "first")
	requirements.NoError(err)
	second, err := st.GetOrCreateSource("text", "second")
	requirements.NoError(err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	firstConversation := insertMessageExportConversation(
		t, st, first.ID, "first-conversation", "", "direct_chat", `{}`,
	)
	secondConversation := insertMessageExportConversation(
		t, st, second.ID, "second-conversation", "", "direct_chat", `{}`,
	)
	insertMessageExportMessage(
		t, st, first.ID, firstConversation, "first-text", "text",
		start, "included", "", 0, false, false,
	)
	insertMessageExportMessage(
		t, st, first.ID, firstConversation, "first-sms", "sms",
		start, "wrong type", "", 0, false, false,
	)
	insertMessageExportMessage(
		t, st, second.ID, secondConversation, "second-text", "text",
		start, "wrong source", "", 0, false, false,
	)

	sink := &collectingMessageExportSink{}
	counts, err := st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start:        start,
		End:          start.Add(time.Hour),
		SourceIDs:    []int64{first.ID},
		MessageTypes: []string{"text"},
	}, sink)
	requirements.NoError(err)
	assertions.Equal(store.MessageExportCounts{
		Sources: 1, Conversations: 1, Messages: 1,
	}, counts)
	requirements.Len(sink.messages, 1)
	assertions.Equal("first-text", sink.messages[0].ID)
}

func TestExportMessagesEmitsExplicitEmptySources(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "empty")
	requirements.NoError(err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	sink := &collectingMessageExportSink{}
	counts, err := st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start:     start,
		End:       start.Add(time.Hour),
		SourceIDs: []int64{source.ID},
	}, sink)
	requirements.NoError(err)
	assertions.Equal(store.MessageExportCounts{Sources: 1}, counts)
	requirements.Len(sink.sources, 1)
	assertions.Equal("empty", sink.sources[0].Identifier)
	assertions.Empty(sink.conversations)
	assertions.Empty(sink.messages)
}

func TestExportMessagesNormalizesConversationsAuthorsAndDeletion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild")
	requirements.NoError(err)
	threadID := insertMessageExportConversation(
		t, st, source.ID, "thread", "A thread", "channel",
		`{"discord_channel_type":11,"parent_channel_id":"parent"}`,
	)
	channelID := insertMessageExportConversation(
		t, st, source.ID, "channel", "A channel", "channel",
		`{"discord_channel_type":0}`,
	)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	threadMessageID := insertMessageExportMessage(
		t, st, source.ID, threadID, "thread-message", "discord",
		start, "thread body", "", 0, true, false,
	)
	insertMessageExportMessage(
		t, st, source.ID, channelID, "hidden", "discord",
		start, "hidden body", "", 0, false, true,
	)
	requirements.NoError(st.SetMessageMetadata(
		threadMessageID,
		sql.NullString{String: `{"author_display_name":"Example User"}`, Valid: true},
	))

	sink := &collectingMessageExportSink{}
	counts, err := st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(time.Hour),
	}, sink)
	requirements.NoError(err)
	assertions.Equal(store.MessageExportCounts{
		Sources: 1, Conversations: 1, Messages: 1,
	}, counts)
	requirements.Len(sink.conversations, 1)
	assertions.Equal(store.MessageExportConversationThread, sink.conversations[0].ConversationType)
	requirements.NotNil(sink.conversations[0].ParentID)
	assertions.Equal("parent", *sink.conversations[0].ParentID)
	requirements.Len(sink.messages, 1)
	assertions.True(sink.messages[0].DeletedFromSource)
	requirements.NotNil(sink.messages[0].Author)
	assertions.Equal("Example User", sink.messages[0].Author.DisplayName)
	assertions.Empty(sink.messages[0].Author.Address)
}

func TestExportMessagesUsesClosedConversationVocabulary(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("fixture", "source")
	requirements.NoError(err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		id      string
		rawType string
		want    store.MessageExportConversationType
	}{
		{"calendar", "calendar_event", store.MessageExportConversationCalendar},
		{"direct", "oneOnOne", store.MessageExportConversationDirectChat},
		{"email", "email_thread", store.MessageExportConversationEmailThread},
		{"group", "group_chat", store.MessageExportConversationGroupChat},
		{"meeting", "meeting", store.MessageExportConversationMeeting},
		{"other", "provider_specific", store.MessageExportConversationOther},
	}
	for i, tc := range cases {
		conversationID := insertMessageExportConversation(
			t, st, source.ID, tc.id, "", tc.rawType, `{}`,
		)
		insertMessageExportMessage(
			t, st, source.ID, conversationID, "message-"+tc.id, "fixture",
			start.Add(time.Duration(i)*time.Minute), "", "", 0, false, false,
		)
	}

	sink := &collectingMessageExportSink{}
	_, err = st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(time.Hour),
	}, sink)
	requirements.NoError(err)
	requirements.Len(sink.conversations, len(cases))
	for i, tc := range cases {
		assertions.Equal(tc.id, sink.conversations[i].ID)
		assertions.Equal(tc.want, sink.conversations[i].ConversationType)
	}
}

func TestExportMessagesNormalizesParticipantAuthor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "person@example.test")
	requirements.NoError(err)
	conversationID := insertMessageExportConversation(
		t, st, source.ID, "conversation", "", "email_thread", `{}`,
	)
	var participantID int64
	err = st.DB().QueryRow(st.Rebind(`
		INSERT INTO participants (email_address, display_name)
		VALUES (?, ?)
		RETURNING id
	`), "author@example.test", "Example Author").Scan(&participantID)
	requirements.NoError(err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	insertMessageExportMessage(
		t, st, source.ID, conversationID, "message", "email",
		start, "", "", participantID, false, false,
	)

	sink := &collectingMessageExportSink{}
	_, err = st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(time.Hour),
	}, sink)
	requirements.NoError(err)
	requirements.Len(sink.messages, 1)
	requirements.NotNil(sink.messages[0].Author)
	assertions.Equal("Example Author", sink.messages[0].Author.DisplayName)
	assertions.Equal("author@example.test", sink.messages[0].Author.Address)
}

func TestExportMessagesPrefersDiscordMessageAuthorName(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild")
	requirements.NoError(err)
	conversationID := insertMessageExportConversation(
		t, st, source.ID, "channel", "", "channel", `{}`,
	)
	participantID := insertMessageExportParticipant(
		t, st, "user@example.test", "Stored Participant Name",
	)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	messageID := insertMessageExportMessage(
		t, st, source.ID, conversationID, "message", "discord",
		start, "", "", participantID, false, false,
	)
	requirements.NoError(st.SetMessageMetadata(
		messageID,
		sql.NullString{
			String: `{"author_display_name":"Message Author Name"}`,
			Valid:  true,
		},
	))

	sink := &collectingMessageExportSink{}
	_, err = st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(time.Hour),
	}, sink)
	requirements.NoError(err)
	requirements.Len(sink.messages, 1)
	requirements.NotNil(sink.messages[0].Author)
	assertions.Equal("Message Author Name", sink.messages[0].Author.DisplayName)
	assertions.Equal("user@example.test", sink.messages[0].Author.Address)
}

func TestExportMessagesUsesLegacyFromRecipientAuthor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "person@example.test")
	requirements.NoError(err)
	conversationID := insertMessageExportConversation(
		t, st, source.ID, "conversation", "", "email_thread", `{}`,
	)
	participantID := insertMessageExportParticipant(
		t, st, "author@example.test", "Stored Participant Name",
	)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	messageID := insertMessageExportMessage(
		t, st, source.ID, conversationID, "message", "email",
		start, "", "", 0, false, false,
	)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO message_recipients (
			message_id, participant_id, recipient_type, display_name
		) VALUES (?, ?, 'from', ?)
	`), messageID, participantID, "Message Author Name")
	requirements.NoError(err)

	sink := &collectingMessageExportSink{}
	_, err = st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(time.Hour),
	}, sink)
	requirements.NoError(err)
	requirements.Len(sink.messages, 1)
	requirements.NotNil(sink.messages[0].Author)
	assertions.Equal("Message Author Name", sink.messages[0].Author.DisplayName)
	assertions.Equal("author@example.test", sink.messages[0].Author.Address)
}

func TestExportMessagesPrefersCanonicalSenderOverStaleFromRecipient(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "person@example.test")
	requirements.NoError(err)
	conversationID := insertMessageExportConversation(
		t, st, source.ID, "conversation", "", "email_thread", `{}`,
	)
	canonicalID := insertMessageExportParticipant(
		t, st, "canonical@example.test", "Canonical Author",
	)
	staleID := insertMessageExportParticipant(
		t, st, "stale@example.test", "Stale Participant",
	)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	messageID := insertMessageExportMessage(
		t, st, source.ID, conversationID, "message", "email",
		start, "", "", canonicalID, false, false,
	)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO message_recipients (
			message_id, participant_id, recipient_type, display_name
		) VALUES (?, ?, 'from', ?)
	`), messageID, staleID, "Stale Message Author")
	requirements.NoError(err)

	sink := &collectingMessageExportSink{}
	_, err = st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(time.Hour),
	}, sink)
	requirements.NoError(err)
	requirements.Len(sink.messages, 1)
	requirements.NotNil(sink.messages[0].Author)
	assertions.Equal("Canonical Author", sink.messages[0].Author.DisplayName)
	assertions.Equal("canonical@example.test", sink.messages[0].Author.Address)
}

type hidingMessageExportSink struct {
	collectingMessageExportSink

	st             *store.Store
	sourceID       int64
	sourceMessage  string
	hideAfterCount int
}

func (s *hidingMessageExportSink) Message(message store.MessageExportMessage) error {
	if err := s.collectingMessageExportSink.Message(message); err != nil {
		return err
	}
	if len(s.messages) != s.hideAfterCount {
		return nil
	}
	_, err := s.st.DB().Exec(s.st.Rebind(`
		UPDATE messages
		SET deleted_at = ?
		WHERE source_id = ? AND source_message_id = ?
	`), time.Now().UTC(), s.sourceID, s.sourceMessage)
	return err
}

func TestExportMessagesKeysetPagingDoesNotSkipAfterPrefixMutation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "source")
	requirements.NoError(err)
	conversationID := insertMessageExportConversation(
		t, st, source.ID, "conversation", "", "direct_chat", `{}`,
	)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	const pageSize = 256
	for i := range pageSize + 1 {
		insertMessageExportMessage(
			t, st, source.ID, conversationID, fmt.Sprintf("message-%03d", i), "text",
			start, "", "", 0, false, false,
		)
	}

	sink := &hidingMessageExportSink{
		st:             st,
		sourceID:       source.ID,
		sourceMessage:  "message-000",
		hideAfterCount: pageSize,
	}
	counts, err := st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start,
		End:   start.Add(time.Hour),
	}, sink)
	requirements.NoError(err)
	assertions.Equal(pageSize+1, counts.Messages)
	requirements.Len(sink.messages, pageSize+1)
	assertions.Equal("message-256", sink.messages[pageSize].ID)
}

type failingMessageExportSink struct {
	err error
}

func (s failingMessageExportSink) Source(store.MessageExportSource) error {
	return s.err
}

func (s failingMessageExportSink) Conversation(store.MessageExportConversation) error {
	return s.err
}

func (s failingMessageExportSink) Message(store.MessageExportMessage) error {
	return s.err
}

func TestExportMessagesStopsWhenSinkFails(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "source")
	requirements.NoError(err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	sentinel := errors.New("write stopped")
	_, err = st.ExportMessages(context.Background(), store.MessageExportFilter{
		Start: start, End: start.Add(time.Hour), SourceIDs: []int64{source.ID},
	}, failingMessageExportSink{err: sentinel})
	requirements.ErrorIs(err, sentinel)
}

func TestExportMessagesRejectsInvalidFilter(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	tests := []store.MessageExportFilter{
		{Start: start, End: start},
		{Start: start, End: start.Add(time.Hour), SourceIDs: []int64{0}},
		{Start: start, End: start.Add(time.Hour), MessageTypes: []string{""}},
	}
	for _, filter := range tests {
		_, err := st.ExportMessages(
			context.Background(), filter, &collectingMessageExportSink{},
		)
		requirements.Error(err)
	}
}

func insertMessageExportConversation(
	t *testing.T,
	st *store.Store,
	sourceID int64,
	sourceConversationID, title, conversationType, metadata string,
) int64 {
	t.Helper()
	requirements := require.New(t)
	var id int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO conversations (
			source_id, source_conversation_id, conversation_type, title
		) VALUES (?, ?, ?, ?)
		RETURNING id
	`), sourceID, sourceConversationID, conversationType, title).Scan(&id)
	requirements.NoError(err)
	requirements.NoError(st.SetConversationMetadata(
		id, sql.NullString{String: metadata, Valid: true},
	))
	return id
}

func insertMessageExportParticipant(
	t *testing.T, st *store.Store, address, displayName string,
) int64 {
	t.Helper()
	var id int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO participants (email_address, display_name)
		VALUES (?, ?)
		RETURNING id
	`), address, displayName).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertMessageExportMessage(
	t *testing.T,
	st *store.Store,
	sourceID, conversationID int64,
	sourceMessageID, messageType string,
	sentAt time.Time,
	body, subject string,
	senderID int64,
	sourceDeleted, hidden bool,
) int64 {
	t.Helper()
	requirements := require.New(t)
	var sender any
	if senderID != 0 {
		sender = senderID
	}
	var deletedFromSource any
	if sourceDeleted {
		deletedFromSource = sentAt.Add(time.Hour)
	}
	var deletedAt any
	if hidden {
		deletedAt = sentAt.Add(time.Hour)
	}
	var messageID int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO messages (
			conversation_id, source_id, source_message_id, message_type,
			sent_at, received_at, sender_id, subject,
			deleted_from_source_at, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id
	`), conversationID, sourceID, sourceMessageID, messageType,
		sentAt, sentAt, sender, subject, deletedFromSource, deletedAt).Scan(&messageID)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)
	`), messageID, body)
	requirements.NoError(err)
	return messageID
}
