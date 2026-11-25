package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCountedPGSource(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), src.ID, "thread", "")
	require.NoError(t, err)
	_, err = st.UpsertMessage(&Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "committed", MessageType: "email"})
	require.NoError(t, err)
	return st, src.ID, conv
}

func insertRawMessage(t *testing.T, tx *sql.Tx, sourceID, convID int64, sourceMessageID string) {
	t.Helper()
	_, err := tx.ExecContext(t.Context(), `INSERT INTO messages (source_id, source_message_id, conversation_id, message_type)
		VALUES ($1, $2, $3, 'email')`, sourceID, sourceMessageID, convID)
	require.NoError(t, err)
}

func requireSourceCounts(t *testing.T, st *Store, sourceID int64, want SourceMessageCounts) {
	t.Helper()
	counts, err := st.SourceMessageCountsContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, counts[sourceID])
	var pending int
	require.NoError(t, st.db.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM source_message_count_deltas`).Scan(&pending))
	assert.Zero(t, pending, "every committed delta is folded")
}

// A recount that runs while a writer's messages are uncommitted counts only
// committed messages; the writer adds its own message as it commits.
func TestRecountSourceMessagesDuringUncommittedWrite(t *testing.T) {
	require := require.New(t)
	st, sourceID, convID := newCountedPGSource(t)
	ctx := t.Context()

	writer, err := st.db.DB.BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = writer.Rollback() }()
	insertRawMessage(t, writer, sourceID, convID, "in-flight")

	require.NoError(st.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		return st.recountSourceMessagesTx(ctx, tx, sourceID)
	}))
	requireSourceCounts(t, st, sourceID, SourceMessageCounts{Live: 1})
	require.NoError(writer.Commit())
	requireSourceCounts(t, st, sourceID, SourceMessageCounts{Live: 2})
}

// Deltas follow savepoints, and a fold forced before commit leaves later
// statements of the same transaction to a second fold.
func TestSourceMessageCountsFoldAcrossSavepointsAndEarlyFold(t *testing.T) {
	require := require.New(t)
	st, sourceID, convID := newCountedPGSource(t)
	ctx := t.Context()

	tx, err := st.db.DB.BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	insertRawMessage(t, tx, sourceID, convID, "kept-1")
	_, err = tx.ExecContext(ctx, `SAVEPOINT before_discard`)
	require.NoError(err)
	insertRawMessage(t, tx, sourceID, convID, "discarded")
	_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT before_discard`)
	require.NoError(err)
	_, err = tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`)
	require.NoError(err)
	_, err = tx.ExecContext(ctx, `SET CONSTRAINTS ALL DEFERRED`)
	require.NoError(err)
	insertRawMessage(t, tx, sourceID, convID, "kept-2")
	_, err = tx.ExecContext(ctx, `UPDATE messages SET deleted_from_source_at = now() WHERE source_message_id = 'kept-1'`)
	require.NoError(err)
	require.NoError(tx.Commit())
	requireSourceCounts(t, st, sourceID, SourceMessageCounts{Live: 2, SourceDeleted: 1})
}
