package store_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChatDraftLifecyclePreservesNativeAddressing(t *testing.T) {
	st := testutil.NewTestStore(t)
	providers := []string{"slack", "teams", "discord"}
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			source, err := st.GetOrCreateSource(provider, provider+"-account")
			requirements.NoError(err)
			conversationID, err := st.EnsureConversationWithType(
				source.ID, provider+"-conversation", "channel", provider+" channel",
			)
			requirements.NoError(err)
			messageID, err := st.UpsertMessage(&store.Message{
				SourceID: source.ID, ConversationID: conversationID,
				SourceMessageID: provider + "-message", MessageType: "chat",
			})
			requirements.NoError(err)

			created, err := st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
				SourceID: source.ID, SourceType: provider, SourceIdentifier: source.Identifier,
				ConversationID: conversationID, ReplyToMessageID: messageID,
				Body: "hello " + provider,
			})
			requirements.NoError(err)
			assertions.Equal("msgvault", created.Location)
			assertions.Equal(int64(1), created.Revision)
			assertions.Equal(provider+"-conversation", created.SourceConversationID)
			assertions.Equal(provider+"-message", created.ReplyToSourceMessageID)

			loaded, err := st.GetChatDraftContext(t.Context(), created.DraftID)
			requirements.NoError(err)
			assertions.Equal(created, loaded)

			unicodeBody := "line one\nline two\nこんにちは 🌐"
			updated, err := st.UpdateChatDraftContext(t.Context(), created.DraftID, 1, unicodeBody)
			requirements.NoError(err)
			assertions.Equal(unicodeBody, updated.Body)
			assertions.Equal(int64(2), updated.Revision)
			_, err = st.UpdateChatDraftContext(t.Context(), created.DraftID, 1, "stale")
			requirements.ErrorIs(err, store.ErrChatDraftRevisionConflict)
			err = st.DeleteChatDraftContext(t.Context(), created.DraftID, 1)
			requirements.ErrorIs(err, store.ErrChatDraftRevisionConflict)

			updated, err = st.UpdateChatDraftContext(t.Context(), created.DraftID, 2, "")
			requirements.NoError(err)
			assertions.Empty(updated.Body)
			assertions.Equal(int64(3), updated.Revision)

			requirements.NoError(st.DeleteChatDraftContext(t.Context(), created.DraftID, 3))
			_, err = st.GetChatDraftContext(t.Context(), created.DraftID)
			requirements.ErrorIs(err, store.ErrChatDraftNotFound)
		})
	}
}

func TestChatDraftCreateRejectsWrongSourceAndReply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "source-one")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("teams", "source-two")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "channel-one", "channel", "one")
	requirements.NoError(err)
	otherConversationID, err := st.EnsureConversationWithType(other.ID, "chat-two", "direct_chat", "two")
	requirements.NoError(err)

	_, err = st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: 0, SourceType: source.SourceType, SourceIdentifier: source.Identifier,
		ConversationID: conversationID, Body: "body",
	})
	requirements.ErrorIs(err, store.ErrChatDraftInvalidInput)
	_, err = st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: source.ID, SourceType: source.SourceType, SourceIdentifier: source.Identifier,
		ConversationID: 0, Body: "body",
	})
	requirements.ErrorIs(err, store.ErrChatDraftInvalidInput)
	zeroReply, err := st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: source.ID, SourceType: source.SourceType, SourceIdentifier: source.Identifier,
		ConversationID: conversationID, ReplyToMessageID: 0, Body: "",
	})
	requirements.NoError(err)
	assertions.Empty(zeroReply.ReplyToSourceMessageID)
	requirements.NoError(st.DeleteChatDraftContext(t.Context(), zeroReply.DraftID, 1))
	otherMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: other.ID, ConversationID: otherConversationID,
		SourceMessageID: "other-message", MessageType: "chat",
	})
	requirements.NoError(err)

	_, err = st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier,
		ConversationID: conversationID, Body: "body",
	})
	requirements.ErrorIs(err, store.ErrChatDraftUnsupportedSource)

	_, err = st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: source.ID, SourceType: source.SourceType, SourceIdentifier: "wrong",
		ConversationID: conversationID, Body: "body",
	})
	requirements.ErrorIs(err, store.ErrChatDraftInvalidDestination)

	_, err = st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: source.ID, SourceType: source.SourceType, SourceIdentifier: source.Identifier,
		ConversationID: conversationID, ReplyToMessageID: otherMessageID, Body: "body",
	})
	requirements.ErrorIs(err, store.ErrChatDraftInvalidDestination)
	otherLocalConversation, err := st.EnsureConversationWithType(source.ID, "other-local-conversation", "channel", "Other")
	requirements.NoError(err)
	otherLocalMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: otherLocalConversation,
		SourceMessageID: "other-local-message", MessageType: "chat",
	})
	requirements.NoError(err)
	_, err = st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: source.ID, SourceType: source.SourceType, SourceIdentifier: source.Identifier,
		ConversationID: conversationID, ReplyToMessageID: otherLocalMessageID, Body: "body",
	})
	requirements.ErrorIs(err, store.ErrChatDraftInvalidDestination)
}

func TestChatDraftSchemaReinitializes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	requirements.NoError(st.InitSchema())
	var count int
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM chat_drafts")).Scan(&count))
	assertions.Equal(0, count)
}

func TestChatDraftCascadeAndReplacementSource(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("slack", "cascade-account")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "channel", "channel", "Channel")
	requirements.NoError(err)
	draft, err := st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: src.ID, SourceType: src.SourceType, SourceIdentifier: src.Identifier,
		ConversationID: conversationID, Body: "old text",
	})
	requirements.NoError(err)
	requirements.NoError(st.RemoveSource(src.ID))
	_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
	requirements.ErrorIs(err, store.ErrChatDraftNotFound)
	replacement, err := st.GetOrCreateSource("slack", "cascade-account")
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM chat_drafts WHERE source_id = ?"), replacement.ID).Scan(&count))
	requirements.Zero(count)

	conversationID, err = st.EnsureConversationWithType(replacement.ID, "other-channel", "channel", "Other")
	requirements.NoError(err)
	draft, err = st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: replacement.ID, SourceType: replacement.SourceType, SourceIdentifier: replacement.Identifier,
		ConversationID: conversationID, Body: "new text",
	})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind("DELETE FROM conversations WHERE id = ?"), conversationID)
	requirements.NoError(err)
	_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
	requirements.ErrorIs(err, store.ErrChatDraftNotFound)
}

func TestChatDraftConcurrentRevisionAndCancellation(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("discord", "concurrent-account")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "channel", "channel", "Channel")
	requirements.NoError(err)
	draft, err := st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: src.ID, SourceType: src.SourceType, SourceIdentifier: src.Identifier,
		ConversationID: conversationID, Body: "original",
	})
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.UpdateChatDraftContext(ctx, draft.DraftID, 1, "cancelled")
	requirements.ErrorIs(err, context.Canceled)
	unchanged, err := st.GetChatDraftContext(t.Context(), draft.DraftID)
	requirements.NoError(err)
	requirements.Equal(draft.Body, unchanged.Body)
	requirements.Equal(draft.Revision, unchanged.Revision)

	var wait sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wait.Go(func() {
			if i == 0 {
				_, updateErr := st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "updated")
				results <- updateErr
			} else {
				results <- st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1)
			}
		})
	}
	wait.Wait()
	close(results)
	var successes, conflicts int
	for result := range results {
		if result == nil {
			successes++
		} else if errors.Is(result, store.ErrChatDraftRevisionConflict) || errors.Is(result, store.ErrChatDraftNotFound) {
			conflicts++
		} else {
			requirements.ErrorIs(result, store.ErrChatDraftRevisionConflict)
			conflicts++
		}
	}
	requirements.Equal(1, successes)
	requirements.Equal(1, conflicts)
}

type chatDraftPreservationSnapshot struct {
	MessageCount        int
	BodyCount           int
	MessageSourceKey    string
	MessageType         string
	MessageSourceID     int64
	MessageConversation int64
	MessageSnippet      sql.NullString
	MessageRaw          []byte
	ParticipantCount    int
	ConversationCount   int
	UnreadCount         int
	LastPreview         sql.NullString
	SyncCursor          sql.NullString
	IMAPDraftID         string
	IMAPMessageID       int64
	IMAPMailbox         string
	IMAPUIDValidity     int64
	IMAPUID             int64
	IMAPRevision        int64
	GmailDraftID        string
	GmailMessageID      int64
	GmailProviderID     string
	GmailProviderMsgID  string
	GmailThreadID       string
	GmailRevision       int64
}

func readChatDraftPreservationSnapshot(
	st *store.Store,
	sourceID, conversationID, messageID int64,
) (chatDraftPreservationSnapshot, error) {
	var snapshot chatDraftPreservationSnapshot
	if err := st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages")).Scan(&snapshot.MessageCount); err != nil {
		return snapshot, err
	}
	if err := st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM message_bodies")).Scan(&snapshot.BodyCount); err != nil {
		return snapshot, err
	}
	if err := st.DB().QueryRow(st.Rebind(`
		SELECT source_message_id, message_type, source_id, conversation_id, snippet
		FROM messages WHERE id = ?
	`), messageID).Scan(
		&snapshot.MessageSourceKey, &snapshot.MessageType, &snapshot.MessageSourceID,
		&snapshot.MessageConversation, &snapshot.MessageSnippet,
	); err != nil {
		return snapshot, err
	}
	var err error
	snapshot.MessageRaw, err = st.GetMessageRaw(messageID)
	if err != nil {
		return snapshot, err
	}
	if err := st.DB().QueryRow(st.Rebind(`
		SELECT participant_count, message_count, unread_count, last_message_preview
		FROM conversations WHERE id = ?
	`), conversationID).Scan(
		&snapshot.ParticipantCount, &snapshot.ConversationCount,
		&snapshot.UnreadCount, &snapshot.LastPreview,
	); err != nil {
		return snapshot, err
	}
	source, err := st.GetSourceByID(sourceID)
	if err != nil {
		return snapshot, err
	}
	snapshot.SyncCursor = source.SyncCursor
	if err := st.DB().QueryRow(st.Rebind(`
		SELECT draft_id, current_message_id, current_mailbox,
		       current_uidvalidity, current_uid, revision
		FROM imap_drafts ORDER BY draft_id LIMIT 1
	`)).Scan(
		&snapshot.IMAPDraftID, &snapshot.IMAPMessageID, &snapshot.IMAPMailbox,
		&snapshot.IMAPUIDValidity, &snapshot.IMAPUID, &snapshot.IMAPRevision,
	); err != nil {
		return snapshot, err
	}
	if err := st.DB().QueryRow(st.Rebind(`
		SELECT draft_id, current_message_id, gmail_draft_id,
		       current_gmail_message_id, thread_id, revision
		FROM gmail_drafts ORDER BY draft_id LIMIT 1
	`)).Scan(
		&snapshot.GmailDraftID, &snapshot.GmailMessageID, &snapshot.GmailProviderID,
		&snapshot.GmailProviderMsgID, &snapshot.GmailThreadID, &snapshot.GmailRevision,
	); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func TestChatDraftPreservesArchiveAndExistingDraftRows(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	slack, err := st.GetOrCreateSource("slack", "preserve-slack")
	requirements.NoError(err)
	slackConversation, err := st.EnsureConversationWithType(slack.ID, "preserve-channel", "channel", "Preserve")
	requirements.NoError(err)
	archiveMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: slack.ID, ConversationID: slackConversation,
		SourceMessageID: "slack-archive-message", MessageType: "chat",
		Snippet: sql.NullString{String: "archive snippet", Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRaw(archiveMessageID, []byte("archive raw\nbody")))
	requirements.NoError(st.UpdateSourceSyncCursor(slack.ID, "slack-cursor"))

	imap, err := st.GetOrCreateSource("imap", "preserve-imap")
	requirements.NoError(err)
	imapConversation, err := st.EnsureConversationWithType(imap.ID, "imap-thread", "email_thread", "IMAP")
	requirements.NoError(err)
	imapMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: imap.ID, ConversationID: imapConversation,
		SourceMessageID: "Drafts|17", MessageType: store.MessageTypeEmail,
	})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO imap_drafts (
			draft_id, source_id, current_message_id, current_mailbox,
			current_uidvalidity, current_uid, revision
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`), "preserve-imap-draft", imap.ID, imapMessageID, "Drafts", 7, 17, 1)
	requirements.NoError(err)

	gmail, err := st.GetOrCreateSource("gmail", "preserve-gmail")
	requirements.NoError(err)
	gmailConversation, err := st.EnsureConversationWithType(gmail.ID, "gmail-thread", "email_thread", "Gmail")
	requirements.NoError(err)
	gmailMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: gmail.ID, ConversationID: gmailConversation,
		SourceMessageID: "gmail-message", MessageType: store.MessageTypeEmail,
	})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO gmail_drafts (
			draft_id, source_id, gmail_draft_id, current_message_id,
			current_gmail_message_id, thread_id, revision
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`), "preserve-gmail-draft", gmail.ID, "provider-draft", gmailMessageID,
		"provider-message", "gmail-thread", 1)
	requirements.NoError(err)

	before, err := readChatDraftPreservationSnapshot(st, slack.ID, slackConversation, archiveMessageID)
	requirements.NoError(err)
	draft, err := st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
		SourceID: slack.ID, SourceType: slack.SourceType, SourceIdentifier: slack.Identifier,
		ConversationID: slackConversation, Body: "local\nこんにちは 🌐",
	})
	requirements.NoError(err)
	_, err = st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "")
	requirements.NoError(err)
	requirements.NoError(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 2))
	after, err := readChatDraftPreservationSnapshot(st, slack.ID, slackConversation, archiveMessageID)
	requirements.NoError(err)
	assertions.Equal(before, after)
	var remaining int
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM chat_drafts")).Scan(&remaining))
	assertions.Equal(0, remaining)
}
