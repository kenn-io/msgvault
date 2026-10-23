package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func discoveredChats(t *testing.T, st *store.Store, query string) []int64 {
	t.Helper()
	page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: query})
	require.NoError(t, err, query)
	ids := []int64{}
	for _, result := range page.Results {
		ids = append(ids, result.ConversationID)
	}
	return ids
}

func TestSearchChatsMetadataContextLeavesMembershipDirty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(err)
	chat, err := st.EnsureConversationWithType(source.ID, "synthetic-room", "group_chat", "Synthetic chat")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "synthetic-message", ConversationID: chat, MessageType: "beeper"})
	require.NoError(err)
	page, err := st.SearchChatsMetadataContext(t.Context(), store.ChatDiscoveryQuery{Query: "synthetic"})
	require.NoError(err)
	require.Len(page.Results, 1)
	assert.Equal(chat, page.Results[0].ConversationID)
	var dirty int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM chat_members_dirty").Scan(&dirty))
	assert.Equal(1, dirty, "metadata scans must leave membership writes queued")
}

func TestChatDiscoveryFollowsArchiveChanges(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "changes-account")
	requirements.NoError(err)
	chat, err := st.EnsureConversationWithType(source.ID, "room", "group_chat", "")
	requirements.NoError(err)
	participant := func(name string) int64 {
		id, err := st.EnsureParticipant(name+"@example.test", name+" Member", "example.test")
		requirements.NoError(err)
		return id
	}
	message := func(id string, sender int64) int64 {
		messageID, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: id, ConversationID: chat,
			MessageType: "beeper", SenderID: sql.NullInt64{Int64: sender, Valid: true},
			SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}})
		requirements.NoError(err)
		return messageID
	}
	exec := func(statement string, args ...any) {
		_, err := st.DB().Exec(st.Rebind(statement), args...)
		requirements.NoError(err, statement)
	}
	first := message("first", participant("Alpha"))
	requirements.NoError(st.ReplaceMessageRecipients(first, "to", []int64{participant("Bravo")}, []string{"Kilo Alias"}))

	steps := []struct {
		name    string
		change  func()
		present []string
		absent  []string
	}{
		{name: "initial", change: func() {}, present: []string{"Alpha", "Bravo", "Kilo"}},
		{name: "renamed alias", change: func() {
			exec(`UPDATE message_recipients SET display_name = 'Lima Alias' WHERE message_id = ?`, first)
		}, present: []string{"Lima", "Bravo"}, absent: []string{"Kilo"}},
		{name: "new message", change: func() { message("second", participant("Delta")) }, present: []string{"Delta"}},
		{name: "hidden message", change: func() {
			exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE source_message_id = 'second'`)
		}, absent: []string{"Delta"}},
		{name: "reassigned sender", change: func() {
			exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, participant("Echo"), first)
		}, present: []string{"Echo"}, absent: []string{"Alpha"}},
		{name: "removed recipients", change: func() {
			exec(`DELETE FROM message_recipients WHERE message_id = ?`, first)
		}, absent: []string{"Lima", "Bravo"}},
		{name: "message added while not a chat", change: func() {
			exec(`UPDATE conversations SET conversation_type = 'email_thread' WHERE id = ?`, chat)
			message("third", participant("Foxtrot"))
			exec(`UPDATE conversations SET conversation_type = 'group_chat' WHERE id = ?`, chat)
		}, present: []string{"Foxtrot", "Echo"}},
		{name: "deleted message", change: func() {
			exec(`DELETE FROM messages WHERE source_message_id = 'third'`)
		}, present: []string{"Echo"}, absent: []string{"Foxtrot"}},
	}
	for _, step := range steps {
		step.change()
		for _, query := range step.present {
			assert.Equal(t, []int64{chat}, discoveredChats(t, st, query), "%s: %s", step.name, query)
		}
		for _, query := range step.absent {
			assert.Empty(t, discoveredChats(t, st, query), "%s: %s", step.name, query)
		}
	}
}

func seedAliasChat(t *testing.T, st *store.Store) int64 {
	t.Helper()
	requirements := require.New(t)
	source, err := st.GetOrCreateSource("beeper", "upgrade-account")
	requirements.NoError(err)
	chat, err := st.EnsureConversationWithType(source.ID, "upgrade-room", "direct_chat", "")
	requirements.NoError(err)
	sender, err := st.EnsureParticipant("upgrade@example.test", "Upgrade Sender", "example.test")
	requirements.NoError(err)
	recipient, err := st.EnsureParticipant("alias@example.test", "Plain Name", "example.test")
	requirements.NoError(err)
	message, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "upgrade", ConversationID: chat,
		MessageType: "beeper", SenderID: sql.NullInt64{Int64: sender, Valid: true},
		SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(message, "to", []int64{recipient}, []string{"Kilo Alias"}))
	return chat
}

func TestChatDiscoveryUpgradeBuildsMembersFromExistingMessages(t *testing.T) {
	requirements := require.New(t)
	st, err := store.OpenForTest(filepath.Join(t.TempDir(), "upgrade.db"))
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	requirements.NoError(st.InitSchema())
	chat := seedAliasChat(t, st)
	// An archive from before the projection has messages but no member rows.
	for _, statement := range []string{
		`DELETE FROM chat_members`, `DELETE FROM chat_members_dirty`,
		`DELETE FROM applied_migrations WHERE name = 'chat_members_v1'`,
	} {
		_, err = st.DB().Exec(statement)
		requirements.NoError(err)
	}
	assert.Empty(t, discoveredChats(t, st, "Upgrade"))
	requirements.NoError(st.InitSchema())
	assert.Equal(t, []int64{chat}, discoveredChats(t, st, "Upgrade Sender"))
	assert.Equal(t, []int64{chat}, discoveredChats(t, st, "Kilo"))
}

func TestChatDiscoveryReadOnlyStoreRefusesPendingChanges(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "readonly.db")
	writer, err := store.OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(writer.InitSchema())
	chat := seedAliasChat(t, writer)
	requirements.NoError(writer.Close())

	reader, err := store.OpenReadOnly(path)
	requirements.NoError(err)
	_, err = reader.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Upgrade"})
	require.ErrorIs(t, err, store.ErrChatMembersStale)
	requirements.NoError(reader.Close())

	writer, err = store.OpenForTest(path)
	requirements.NoError(err)
	assert.Equal(t, []int64{chat}, discoveredChats(t, writer, "Upgrade"))
	requirements.NoError(writer.Close())
	reader, err = store.OpenReadOnly(path)
	requirements.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	assert.Equal(t, []int64{chat}, discoveredChats(t, reader, "Kilo"))
}

func TestCopySubsetRebuildsChatMembers(t *testing.T) {
	requirements := require.New(t)
	srcPath := filepath.Join(t.TempDir(), "msgvault.db")
	src, err := store.Open(srcPath)
	requirements.NoError(err)
	requirements.NoError(src.InitSchema())
	source, err := src.GetOrCreateSource("beeper", "subset-account")
	requirements.NoError(err)
	chat, err := src.EnsureConversationWithType(source.ID, "subset-room", "direct_chat", "")
	requirements.NoError(err)
	sender, err := src.EnsureParticipant("subset@example.test", "Subset Sender", "example.test")
	requirements.NoError(err)
	_, err = src.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "subset", ConversationID: chat,
		MessageType: "beeper", SenderID: sql.NullInt64{Int64: sender, Valid: true},
		SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}})
	requirements.NoError(err)
	requirements.NoError(src.Close())

	dstDir := filepath.Join(t.TempDir(), "subset")
	_, err = store.CopySubset(srcPath, dstDir, 1, false)
	requirements.NoError(err)
	dst, err := store.Open(filepath.Join(dstDir, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { _ = dst.Close() })
	assert.Len(t, discoveredChats(t, dst, "Subset Sender"), 1)
}

// The daemon refreshes chat members, releases its mutation gate, then reads.
// A write in that gap must not surface a hidden message or one from another
// chat as a result's message_id.
func TestChatDiscoveryRejectsAnchorChangedAfterRefresh(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "stale-anchor-account")
	requirements.NoError(err)
	chat, err := st.EnsureConversationWithType(source.ID, "stale-room", "direct_chat", "Stale Room")
	requirements.NoError(err)
	other, err := st.EnsureConversationWithType(source.ID, "other-room", "direct_chat", "Elsewhere")
	requirements.NoError(err)
	insert := func(id string, sent time.Time) int64 {
		messageID, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: id, ConversationID: chat,
			MessageType: "beeper", SentAt: sql.NullTime{Time: sent, Valid: true}})
		requirements.NoError(err)
		return messageID
	}
	middle := insert("middle", time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC))
	newest := insert("newest", time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	oldest := insert("oldest", time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	// 10:00+02:00 is 08:00 UTC: older than both surviving anchors and the competing chat.
	insert("offset", time.Date(2026, 3, 1, 10, 0, 0, 0, time.FixedZone("plus-two", 2*60*60)))
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "undated", ConversationID: chat,
		MessageType: "beeper"})
	requirements.NoError(err)
	seedDiscoveryChat(t, st, source.ID, "Stale Competitor", "direct_chat", time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC))
	metadataAnchor := func() int64 {
		t.Helper()
		page, err := st.SearchChatsMetadataContext(t.Context(), store.ChatDiscoveryQuery{Query: "Stale", Limit: 1})
		requirements.NoError(err)
		requirements.Len(page.Results, 1)
		requirements.Equal(chat, page.Results[0].ConversationID)
		return page.Results[0].MessageID
	}
	requirements.NoError(st.RefreshChatMembersContext(t.Context()))
	requirements.Equal(newest, metadataAnchor())

	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), newest)
	requirements.NoError(err)
	assert.Equal(t, middle, metadataAnchor(), "a hidden anchor falls back to the newest live message")

	requirements.NoError(st.RefreshChatMembersContext(t.Context()))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET conversation_id = ? WHERE id = ?"), other, middle)
	requirements.NoError(err)
	assert.Equal(t, oldest, metadataAnchor(), "an anchor moved to another chat is not reported for this one")
}
