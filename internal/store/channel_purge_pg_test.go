package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelPurgeLetsOtherSourceRecoveryFinish(t *testing.T) {
	require := require.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	removed, err := st.GetOrCreateSource("discord", "removed-guild")
	require.NoError(err)
	retained, err := st.GetOrCreateSource("discord", "retained-guild")
	require.NoError(err)
	conversation, err := st.EnsureConversation(removed.ID, "removed-channel", "Removed")
	require.NoError(err)
	_, err = st.UpsertMessage(&Message{SourceID: removed.ID, ConversationID: conversation, SourceMessageID: "removed-message", MessageType: "discord"})
	require.NoError(err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	var id int64
	// Pause recovery after its first source-row lock. The purge must wait
	// without taking locks needed by the rest of that recovery transaction.
	require.NoError(tx.QueryRowContext(ctx, "SELECT id FROM sources WHERE id=$1 FOR UPDATE", retained.ID).Scan(&id))
	var recoveryPID int
	require.NoError(tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&recoveryPID))
	done := make(chan error, 1)
	go func() { done <- st.PurgeChannelContext(ctx, removed.ID, "removed-channel") }()
	require.Eventually(func() bool {
		var waiting bool
		err := st.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))
			AND query LIKE 'LOCK TABLE %')`, recoveryPID).Scan(&waiting)
		return err == nil && waiting
	}, 15*time.Second, 10*time.Millisecond)
	require.NoError(st.recoverAbandonedSyncSourceQueries(ctx, tx, retained.ID, st.dialect.Now()))
	require.NoError(tx.Commit())
	require.NoError(<-done)
	var count int
	require.NoError(st.DB().QueryRowContext(ctx, "SELECT count(*) FROM conversations WHERE id=$1", conversation).Scan(&count))
	require.Zero(count)
	_, err = st.GetSourceByIDContext(ctx, retained.ID)
	require.NoError(err)
}
