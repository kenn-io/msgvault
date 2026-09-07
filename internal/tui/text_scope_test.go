package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func textAccountScopeModel(t *testing.T) Model {
	t.Helper()
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES
			(7, 'imessage', 'first@example.com'), (8, 'imessage', 'second@example.com');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(701, 7, 'chat-701', 'direct_chat', 'First'),
			(702, 8, 'chat-702', 'direct_chat', 'Second');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject) VALUES
			(801, 701, 7, 'message-801', 'imessage', '2026-01-01 10:00:00', 'hello first'),
			(802, 702, 8, 'message-802', 'imessage', '2026-01-01 11:00:00', 'hello second');
		CREATE VIRTUAL TABLE messages_fts USING fts5(subject, body);
		INSERT INTO messages_fts (rowid, subject, body) VALUES
			(801, 'hello first', ''), (802, 'hello second', '');
	`)
	require.NoError(t, err)
	model := New(query.NewSQLiteEngine(tdb.DB), Options{DataDir: t.TempDir(), Version: "test"})
	model.mode = modeTexts
	model.textState.sourceID = new(int64(7))
	return model
}

func TestGlobalTextSearchRespectsSelectedAccount(t *testing.T) {
	require := require.New(t)
	model := textAccountScopeModel(t)
	model, _ = sendKey(t, model, key('/'))
	model.searchInput.SetValue("hello")
	model, cmd := sendKey(t, model, keyEnter())
	require.NotNil(cmd)
	model = sendMsg(t, model, cmd())

	require.NoError(model.err)
	require.Len(model.textState.messages, 1)
	assert.Equal(t, int64(801), model.textState.messages[0].ID)
}

func TestTextConversationsResetPreservesSelectedAccount(t *testing.T) {
	require := require.New(t)
	model := textAccountScopeModel(t)
	model.textState.viewType = query.TextViewSources
	model.textState.level = textLevelAggregate
	model, cmd := sendKey(t, model, key('a'))
	require.NotNil(cmd)
	model = sendMsg(t, model, cmd())

	require.NoError(model.err)
	require.Len(model.textState.conversations, 1)
	assert.Equal(t, int64(701), model.textState.conversations[0].ConversationID)
}
