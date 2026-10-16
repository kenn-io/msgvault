package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelPurgeDoesNotDeadlockWithSyncPhoneParticipant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(context.Context, *Store) error
	}{
		{"messaging", func(_ context.Context, st *Store) error {
			_, err := st.EnsureParticipantByPhone("+12025550123", "Example participant", "sms")
			return err
		}},
		{"meeting", func(ctx context.Context, st *Store) error {
			_, err := st.EnsurePhoneParticipantContext(ctx, "+12025550123", "Example participant")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
			st.DB().SetMaxOpenConns(12)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			writing, err := st.GetOrCreateSource("sms", "writing-account")
			requirements.NoError(err)
			purging, err := st.GetOrCreateSource("slack", "purging-workspace")
			requirements.NoError(err)
			removed, err := st.EnsureConversation(purging.ID, "remove", "Remove channel")
			requirements.NoError(err)
			run, err := st.StartSyncContext(ctx, writing.ID, "full")
			requirements.NoError(err)
			defer func() { requirements.NoError(st.releaseSyncExecutionLock(run)) }()

			blocker, err := st.DB().BeginTx(ctx, nil)
			requirements.NoError(err)
			defer func() { _ = blocker.Rollback() }()
			var runID, blockerPID int64
			requirements.NoError(blocker.QueryRowContext(ctx,
				`SELECT id, pg_backend_pid() FROM sync_runs WHERE id = $1 FOR UPDATE`, run,
			).Scan(&runID, &blockerPID))
			writeDone := make(chan error, 1)
			go func() {
				writeDone <- tc.write(ctx, st.ScopedToSync(writing.ID, run))
			}()
			// Pause the native participant write at its sync fence. Matching
			// blocking PIDs ignores waits from tests sharing the server.
			writerPID := waitForPGBlockedQuery(ctx, t, st, int(blockerPID), "%SET messages_processed = messages_processed%")
			purgeDone := make(chan error, 1)
			go func() { purgeDone <- st.PurgeChannelContext(ctx, purging.ID, "remove") }()
			// The purge waits on this writer for the identity row with the
			// fixed order, or for the sync_runs table lock with the old one.
			requirements.Eventually(func() bool {
				var waiting bool
				err := st.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))
			AND (query LIKE '%LOCK TABLE%' OR query LIKE '%archive_metadata%'))`, writerPID).Scan(&waiting)
				return err == nil && waiting
			}, 15*time.Second, 10*time.Millisecond, "purge did not wait for writer %d", writerPID)
			// Identity must precede the sync fence, or maintenance holds identity
			// while waiting for the writer's sync-run table lock.
			requirements.NoError(blocker.Rollback())
			requirements.NoError(<-writeDone)
			requirements.NoError(<-purgeDone)
			var inserted int
			requirements.NoError(st.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM participants WHERE phone_number = '+12025550123'`,
			).Scan(&inserted))
			assertions.Equal(1, inserted)
			exists, err := st.ConversationExistsContext(ctx, removed)
			requirements.NoError(err)
			assertions.False(exists)
		})
	}
}
