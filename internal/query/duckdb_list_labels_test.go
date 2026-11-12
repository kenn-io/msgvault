package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

// TestDuckDBEngine_ListMessages_IncludesLabels verifies that ListMessages on a
// Parquet-backed engine attaches each message's labels from the live archive,
// as Search does, rather than returning summaries with no labels.
func TestDuckDBEngine_ListMessages_IncludesLabels(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("owner@example.com")
	labeled := b.AddMessage(MessageOpt{
		Subject: "Labeled", SentAt: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), ConversationID: 501,
	})
	unlabeled := b.AddMessage(MessageOpt{
		Subject: "Unlabeled", SentAt: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), ConversationID: 502,
	})
	analyticsDir, cleanup := b.Build()
	t.Cleanup(cleanup)

	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.ExecContext(t.Context(), `
		INSERT INTO sources (id, source_type, identifier) VALUES (?, 'gmail', 'owner@example.com');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
			VALUES (501, ?, 'thread-501', 'email_thread'), (502, ?, 'thread-502', 'email_thread');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at)
			VALUES (?, 501, ?, 'msg-labeled', 'email', '2026-08-20 10:00:00'),
			       (?, 502, ?, 'msg-unlabeled', 'email', '2026-08-20 09:00:00');
		INSERT INTO labels (id, source_id, source_label_id, name, label_type)
			VALUES (91, ?, 'INBOX', 'INBOX', 'system'), (92, ?, 'Label_1', 'Correspondence', 'user');
		INSERT INTO message_labels (message_id, label_id) VALUES (?, 91), (?, 92);
	`, sourceID, sourceID, sourceID, labeled, sourceID, unlabeled, sourceID, sourceID, sourceID, labeled, labeled)
	require.NoError(err)

	engine, err := NewDuckDBEngine(t.Context(), analyticsDir, "", tdb.DB)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(engine.Close()) })

	messages, err := engine.ListMessages(t.Context(), MessageFilter{})
	require.NoError(err)
	require.Len(messages, 2)

	byID := map[int64]MessageSummary{}
	for _, m := range messages {
		byID[m.ID] = m
	}
	assert.ElementsMatch([]string{"INBOX", "Correspondence"}, byID[labeled].Labels, "labeled message labels")
	assert.Empty(byID[unlabeled].Labels, "unlabeled message labels")
}
