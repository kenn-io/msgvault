package beeper

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImporterObservesNativeChatInboxState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	imp := NewImporter(st, NewClient("http://127.0.0.1:23373", testToken, 10000))
	var chat Chat
	requirements.NoError(json.Unmarshal([]byte(`{"id":"chat-a","accountID":"account-a","type":"single","isArchived":false,"isMarkedUnread":true,"unreadCount":0,"lastReadMessageSortKey":"8","draft":{"text":"preserve this composer"},"capabilities":{"archive":true,"markAsUnread":true},"participants":{"items":[],"hasMore":false}}`), &chat))
	observe := func() int64 {
		t.Helper()
		id, _, _, err := imp.ensureConversation(t.Context(), 0, source.ID, &chat, ImportOptions{AccountID: source.Identifier}, &ImportSummary{})
		require.NoError(t, err)
		return id
	}
	conversation := observe()
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "message-a", MessageType: "beeper"})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read = FALSE WHERE id = ?`), mid)
	requirements.NoError(err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "beeper", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeChat, ItemID: conversation, ProviderID: chat.ID}
	state, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(state)
	assertions.True(*state.Inbox)
	assertions.False(*state.Read, "explicit unread marker survives a zero unread count")
	assertions.True(*state.MarkedUnread)
	assertions.Equal([]string{"read-through:8"}, state.Flags)
	assertions.NotEmpty(state.Revision)
	assertions.NotContains(state.Revision, "preserve this composer")
	chat.IsArchived, chat.IsMarkedUnread = new(true), new(false)
	observe()
	state, err = st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	assertions.False(*state.Inbox)
	assertions.True(*state.Read)
	var uiRead bool
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id = ?`), mid).Scan(&uiRead))
	assertions.False(uiRead)
	chat.IsArchived, chat.IsMarkedUnread, chat.UnreadCount = nil, nil, nil
	observe()
	state, err = st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	assertions.Nil(state.Inbox)
	assertions.Nil(state.Read)
	assertions.Nil(state.MarkedUnread)
	for _, invalid := range []string{"merged", "foreign", "negative count"} {
		t.Run(invalid, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			chat.AccountID, chat.Merge = source.Identifier, nil
			chat.IsArchived, chat.IsMarkedUnread, chat.UnreadCount = new(false), new(false), new(0)
			observe()
			switch invalid {
			case "merged":
				chat.Merge = &ChatMerge{ChatIDs: []string{"chat-a", "chat-b"}, DefaultChatID: "chat-a"}
			case "foreign":
				chat.AccountID = "foreign-account"
			case "negative count":
				chat.UnreadCount = new(-1)
			}
			observe()
			unknown, err := st.GetInboxProviderState(t.Context(), target)
			requirements.NoError(err)
			requirements.NotNil(unknown)
			assertions.Nil(unknown.Inbox)
			assertions.Nil(unknown.Read)
			assertions.Nil(unknown.MarkedUnread)
		})
	}
}
