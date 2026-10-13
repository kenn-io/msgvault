package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPSourceMessageCaptureIncludesNewConversations(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	const conversationFamily = "msgvault.message_archived"
	const sourceFamily = "msgvault.source_message_archived"
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic", Capabilities: []store.MCPEventCapability{
		{Family: conversationFamily, SourceType: "gmail", Kinds: []string{"message"}},
		{Family: sourceFamily, SourceType: "gmail", Kinds: []string{"message"}},
	}})
	require.NoError(err)
	firstConversation := f.ConvID
	persist := func(mode store.IngestMode, nativeID string) int64 {
		t.Helper()
		message := f.NewMessage().WithSourceMessageID(nativeID).Build()
		id, err := f.Store.WithIngestContext(store.IngestContext{Mode: mode}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "Synthetic archived body", Valid: true}})
		Require.NoError(t, err)
		return id
	}
	firstMessage := persist(store.IngestLive, "source-live-first")
	secondConversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "source-new-conversation", "email", "Synthetic second conversation")
	require.NoError(err)
	f.ConvID = secondConversation
	secondMessage := persist(store.IngestLive, "source-live-second")
	persist(store.IngestBackfill, "source-history-silent")
	persist(store.IngestUnknown, "source-unknown-silent")
	rows, err := f.Store.DB().QueryContext(t.Context(), f.Store.Rebind(`SELECT scope_kind,scope_id,source_id,conversation_id,message_id FROM mcp_event_log WHERE family=? ORDER BY seq`), sourceFamily)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var conversations, messages []int64
	for rows.Next() {
		var kind string
		var scope, source, conversation, message int64
		require.NoError(rows.Scan(&kind, &scope, &source, &conversation, &message))
		assert.Equal("source", kind)
		assert.Equal(f.Source.ID, scope)
		assert.Equal(f.Source.ID, source)
		conversations = append(conversations, conversation)
		messages = append(messages, message)
	}
	require.NoError(rows.Err())
	assert.Equal([]int64{firstConversation, secondConversation}, conversations)
	assert.Equal([]int64{firstMessage, secondMessage}, messages)
	var original int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family=?`), conversationFamily).Scan(&original))
	assert.Equal(2, original, "source fanout preserves distinct conversation occurrences")
}
