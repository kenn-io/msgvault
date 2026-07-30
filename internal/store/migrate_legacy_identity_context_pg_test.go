package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateLegacyIdentityConfigContextCancelsBlockedRevisionUpdate(t *testing.T) {
	dbURL := skipUnlessPostgresInternal(t)

	tests := []struct {
		name         string
		revisionKey  string
		queryMatches string
	}{
		{
			// The migration takes the identity-mutation row lock up front
			// (BeginExclusive ordering contract), so a held identity-revision
			// row now blocks it in that lock's no-op UPDATE rather than in
			// the late revision bump.
			name:         "identity revision",
			revisionKey:  identityRevisionKey,
			queryMatches: "%SET value = value WHERE key = $1%",
		},
		{
			name:         "account identity revision",
			revisionKey:  accountIdentityRevisionKey,
			queryMatches: "%WHERE key = $1%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := newPGStoreInternal(t, dbURL)
			source, err := st.GetOrCreateSource("gmail", "alice@example.com")
			require.NoError(err, "create source")

			_, err = st.DB().Exec(`
				INSERT INTO archive_metadata (key, value)
				VALUES ($1, '0'), ($2, '0')
				ON CONFLICT (key) DO NOTHING`,
				identityRevisionKey, accountIdentityRevisionKey)
			require.NoError(err, "seed revision rows")

			blocker, err := st.DB().BeginTx(context.Background(), nil)
			require.NoError(err, "begin blocker transaction")
			blockerOpen := true
			t.Cleanup(func() {
				if blockerOpen {
					_ = blocker.Rollback()
				}
			})

			var blockerPID int
			require.NoError(
				blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID),
				"read blocker backend pid",
			)
			var revisionValue string
			require.NoError(blocker.QueryRow(
				`SELECT value FROM archive_metadata WHERE key = $1 FOR UPDATE`,
				tt.revisionKey,
			).Scan(&revisionValue), "lock revision row")

			ctx, cancel := context.WithCancel(context.Background())
			migrationDone := make(chan error, 1)
			go func() {
				_, _, _, _, migrationErr := st.MigrateLegacyIdentityConfigContext(
					ctx,
					[]string{"legacy@example.com"},
				)
				migrationDone <- migrationErr
			}()

			require.Eventually(func() bool {
				var waiting bool
				queryErr := st.DB().QueryRow(`
					SELECT EXISTS (
						SELECT 1
						FROM pg_stat_activity
						WHERE state = 'active'
						  AND wait_event_type = 'Lock'
						  AND $1 = ANY(pg_blocking_pids(pid))
						  AND query LIKE $2
					)`,
					blockerPID, tt.queryMatches,
				).Scan(&waiting)
				return queryErr == nil && waiting
			}, 3*time.Second, 10*time.Millisecond,
				"migration must reach and block in the target revision update")

			cancel()
			select {
			case migrationErr := <-migrationDone:
				require.ErrorIs(migrationErr, context.Canceled)
			case <-time.After(time.Second):
				require.NoError(blocker.Rollback(), "release blocker after cancellation timeout")
				blockerOpen = false
				<-migrationDone
				require.FailNow("migration did not stop after context cancellation")
			}

			var identityCount int
			require.NoError(st.DB().QueryRow(
				`SELECT COUNT(*) FROM account_identities
				 WHERE source_id = $1 AND address = $2`,
				source.ID, "legacy@example.com",
			).Scan(&identityCount), "count rolled-back identity")
			assert.Zero(identityCount, "migration identity insert must roll back")

			var markerCount int
			require.NoError(st.DB().QueryRow(
				`SELECT COUNT(*) FROM applied_migrations WHERE name = $1`,
				migrationLegacyIdentity,
			).Scan(&markerCount), "count rolled-back migration marker")
			assert.Zero(markerCount, "migration marker must not commit")

			require.NoError(blocker.Rollback(), "release blocker")
			blockerOpen = false
		})
	}
}
