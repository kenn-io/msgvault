package cmd

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestStoreAPIAdapterImplementsCtxMessageStore is a compile-time guard that the
// production adapter wired into api.Server satisfies the optional context-aware
// read interface. If it drifts, api.Server silently falls back to the
// background-context read path.
func TestStoreAPIAdapterImplementsCtxMessageStore(t *testing.T) {
	var adapter api.MessageStore = &storeAPIAdapter{}
	_, ok := adapter.(api.CtxMessageStore)
	require.True(t, ok, "storeAPIAdapter must implement api.CtxMessageStore")
}

var _ api.CtxMessageStore = (*storeAPIAdapter)(nil)

// TestStoreAPIAdapterContextReadsHonorCancellation verifies the adapter's
// context-aware read methods thread the caller's context into the underlying
// store, so an abandoned or timed-out request is cancelled instead of running
// to completion on a background context.
func TestStoreAPIAdapterContextReadsHonorCancellation(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	_, err = st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")

	adapter := &storeAPIAdapter{store: st}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = adapter.GetStatsContext(ctx)
	require.ErrorIs(err, context.Canceled, "GetStatsContext must honor a cancelled context")

	_, _, err = adapter.ListMessagesContext(ctx, 0, 10)
	require.ErrorIs(err, context.Canceled, "ListMessagesContext must honor a cancelled context")

	_, err = adapter.GetMessageContext(ctx, 1)
	require.ErrorIs(err, context.Canceled, "GetMessageContext must honor a cancelled context")

	_, err = adapter.GetMessagesSummariesByIDsContext(ctx, []int64{1})
	require.ErrorIs(err, context.Canceled, "GetMessagesSummariesByIDsContext must honor a cancelled context")
}

var _ api.ConversationWindowStore = (*storeAPIAdapter)(nil)

// TestStoreAPIAdapterImplementsConversationWindowStore is a compile-time
// guard that the production adapter wired into api.Server satisfies the
// context-aware conversation reader. Without it, GET /conversations/{id}
// falls back to an unexported interface the adapter can never satisfy and
// returns 503 conversation_unavailable in daemon mode.
func TestStoreAPIAdapterImplementsConversationWindowStore(t *testing.T) {
	var adapter api.MessageStore = &storeAPIAdapter{}
	_, ok := adapter.(api.ConversationWindowStore)
	require.True(t, ok, "storeAPIAdapter must implement api.ConversationWindowStore")
}

// TestStoreAPIAdapterConversationWindowContext verifies the adapter's
// conversation-window pass-throughs return a real window from the
// underlying store, both unbounded and time-bounded.
func TestStoreAPIAdapterConversationWindowContext(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	conversationID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")

	sentAt := time.Date(2015, 1, 7, 12, 0, 0, 0, time.UTC)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "msg-1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
	})
	require.NoError(err, "UpsertMessage")

	adapter := &storeAPIAdapter{store: st}
	ctx := context.Background()

	exists, err := adapter.ConversationExistsContext(ctx, conversationID)
	require.NoError(err, "ConversationExistsContext")
	require.True(exists, "conversation should exist")

	window, err := adapter.GetConversationWindowContext(ctx, conversationID, messageID, 5, 5, nil, nil)
	require.NoError(err, "GetConversationWindowContext unbounded")
	require.Len(window.Messages, 1, "unbounded window should contain the seeded message")
	require.Equal(messageID, window.Messages[0].ID, "unbounded window message ID")

	start := sentAt.Add(-time.Hour)
	end := sentAt.Add(time.Hour)
	bounded, err := adapter.GetConversationWindowContext(ctx, conversationID, messageID, 5, 5, &start, &end)
	require.NoError(err, "GetConversationWindowContext bounded")
	require.Len(bounded.Messages, 1, "bounded window should contain the seeded message")
	require.Equal(messageID, bounded.Messages[0].ID, "bounded window message ID")
}
