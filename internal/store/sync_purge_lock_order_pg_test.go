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
			var runID int64
			requirements.NoError(blocker.QueryRowContext(ctx,
				`SELECT id FROM sync_runs WHERE id = $1 FOR UPDATE`, run,
			).Scan(&runID))
			writeDone := make(chan error, 1)
			go func() {
				writeDone <- tc.write(ctx, st.ScopedToSync(writing.ID, run))
			}()
			// Pause the native participant write at its sync fence.
			requirements.Eventually(func() bool {
				var waiting bool
				err := st.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock'
			AND POSITION('SET messages_processed = messages_processed' IN query) > 0)`).Scan(&waiting)
				return err == nil && waiting
			}, 10*time.Second, 10*time.Millisecond)
			purgeDone := make(chan error, 1)
			go func() { purgeDone <- st.PurgeChannelContext(ctx, purging.ID, "remove") }()
			requirements.Eventually(func() bool {
				var waiting bool
				err := st.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock'
			AND (POSITION('LOCK TABLE' IN query) > 0
			     OR POSITION('archive_metadata' IN query) > 0))`).Scan(&waiting)
				return err == nil && waiting
			}, 10*time.Second, 10*time.Millisecond)
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
