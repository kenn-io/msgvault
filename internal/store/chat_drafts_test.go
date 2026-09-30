package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func allowChatDraft(string, string) error { return nil }

func TestChatDraftLifecycleKeepsDraftsOutOfArchive(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, provider := range []string{"slack", "teams", "discord"} {
		t.Run(provider, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			source, err := st.GetOrCreateSource(provider, provider+"-account")
			requirements.NoError(err)
			conversationID, err := st.EnsureConversationWithType(source.ID, provider+"-conversation", "channel", "General")
			requirements.NoError(err)
			messageID, err := st.UpsertMessage(&store.Message{
				SourceID: source.ID, ConversationID: conversationID,
				SourceMessageID: provider + "-message", MessageType: "chat",
			})
			requirements.NoError(err)
			before, err := st.GetStatsContext(t.Context())
			requirements.NoError(err)

			created, err := st.CreateChatDraftContext(t.Context(), conversationID, messageID, "hello", allowChatDraft)
			requirements.NoError(err)
			assertions.Equal(int64(1), created.Revision)
			assertions.Equal(source.ID, created.SourceID)
			assertions.Equal(provider+"-conversation", created.SourceConversationID)
			assertions.Equal(provider+"-message", created.ReplyToSourceMessageID)

			listed, err := st.ListChatDraftsContext(t.Context(), conversationID, allowChatDraft)
			requirements.NoError(err)
			assertions.Equal([]store.ChatDraft{created}, listed)

			edited, err := st.UpdateChatDraftContext(t.Context(), created.DraftID, 1, "")
			requirements.NoError(err)
			assertions.Equal(int64(2), edited.Revision)
			assertions.Empty(edited.Body)
			_, err = st.UpdateChatDraftContext(t.Context(), created.DraftID, 1, "stale")
			requirements.ErrorIs(err, store.ErrChatDraftRevisionConflict)
			requirements.ErrorIs(st.DeleteChatDraftContext(t.Context(), created.DraftID, 1), store.ErrChatDraftRevisionConflict)
			unchanged, err := st.GetChatDraftContext(t.Context(), created.DraftID)
			requirements.NoError(err)
			assertions.Equal(edited, unchanged)

			requirements.NoError(st.DeleteChatDraftContext(t.Context(), created.DraftID, 2))
			_, err = st.GetChatDraftContext(t.Context(), created.DraftID)
			requirements.ErrorIs(err, store.ErrChatDraftNotFound)
			after, err := st.GetStatsContext(t.Context())
			requirements.NoError(err)
			before.DatabaseSize, after.DatabaseSize = 0, 0
			assertions.Equal(before, after)
		})
	}
}

func TestChatDraftCreateRejectsWrongDestination(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	slack, err := st.GetOrCreateSource("slack", "slack-account")
	requirements.NoError(err)
	slackConversation, err := st.EnsureConversationWithType(slack.ID, "C1", "channel", "one")
	requirements.NoError(err)
	otherConversation, err := st.EnsureConversationWithType(slack.ID, "C2", "channel", "two")
	requirements.NoError(err)
	otherMessage, err := st.UpsertMessage(&store.Message{
		SourceID: slack.ID, ConversationID: otherConversation, SourceMessageID: "m2", MessageType: "chat",
	})
	requirements.NoError(err)
	mail, err := st.GetOrCreateSource("imap", "you@example.com")
	requirements.NoError(err)
	mailConversation, err := st.EnsureConversationWithType(mail.ID, "thread", "email_thread", "mail")
	requirements.NoError(err)

	_, err = st.CreateChatDraftContext(t.Context(), slackConversation, otherMessage, "x", allowChatDraft)
	requirements.ErrorIs(err, store.ErrChatDraftInvalidDestination)
	_, err = st.CreateChatDraftContext(t.Context(), mailConversation, 0, "x", allowChatDraft)
	requirements.ErrorIs(err, store.ErrChatDraftUnsupportedSource)
	_, err = st.CreateChatDraftContext(t.Context(), 0, 0, "x", allowChatDraft)
	requirements.ErrorIs(err, store.ErrChatDraftInvalidDestination)
	listed, err := st.ListChatDraftsContext(t.Context(), slackConversation, allowChatDraft)
	requirements.NoError(err)
	assertions.Empty(listed)
}
