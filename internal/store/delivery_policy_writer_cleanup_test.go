package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresDeliveryFenceWaitLockTimeoutRollsBackWriters(t *testing.T) {
	requirements := require.New(t)
	dbURL := skipUnlessPostgresInternal(t)

	tests := []struct {
		name  string
		start func(t *testing.T, st *Store) func(context.Context) error
	}{
		{
			name: "RFC822 backfill",
			start: func(t *testing.T, st *Store) func(context.Context) error {
				t.Helper()
				_, _, sourceID, plan := newInternalRFC822IDBackfillPlan(t, st, "fence-cleanup-backfill")
				return func(ctx context.Context) error {
					_, err := st.ApplyRFC822IDBackfill(ctx, []int64{sourceID}, plan, nil)
					return err
				}
			},
		},
		{
			name: "embedding generation",
			start: func(t *testing.T, st *Store) func(context.Context) error {
				t.Helper()
				requirements := require.New(t)
				source, err := st.GetOrCreateSource("gmail", "fence-cleanup-embed@example.test")
				requirements.NoError(err)
				conversationID, err := st.EnsureConversation(source.ID, "fence-cleanup-embed", "Fence cleanup")
				requirements.NoError(err)
				messageID, err := st.UpsertMessage(&Message{
					SourceID: source.ID, ConversationID: conversationID,
					SourceMessageID: "fence-cleanup-embed", MessageType: "email",
				})
				requirements.NoError(err)
				var lastModified time.Time
				requirements.NoError(st.db.QueryRowContext(t.Context(),
					`SELECT last_modified FROM messages WHERE id=?`, messageID).Scan(&lastModified))
				versions := []EmbedGenStamp{{ID: messageID, LastModified: lastModified}}
				return func(ctx context.Context) error {
					_, err := st.SetEmbedGenGroupIfUnchanged(ctx, versions, EmbedGenMetadataVersion{}, 1)
					return err
				}
			},
		},
		{
			name: "sync start",
			start: func(t *testing.T, st *Store) func(context.Context) error {
				t.Helper()
				requirements := require.New(t)
				source, err := st.GetOrCreateSource("gmail", "fence-cleanup-sync@example.test")
				requirements.NoError(err)
				return func(ctx context.Context) error {
					_, err := st.startSyncOnce(ctx, source.ID, "full", "", "", nil)
					return err
				}
			},
		},
	}

	const applicationName = "delivery_fence_cleanup_test"
	parsed, err := url.Parse(dbURL)
	requirements.NoError(err)
	query := parsed.Query()
	query.Set("application_name", applicationName)
	query.Set("lock_timeout", "2s")
	parsed.RawQuery = query.Encode()
	st := newPGStoreInternal(t, parsed.String())
	observerURL, err := url.Parse(dbURL)
	requirements.NoError(err)
	observerQuery := observerURL.Query()
	observerQuery.Set("application_name", applicationName+"_observer")
	observerURL.RawQuery = observerQuery.Encode()
	observerDB, err := sql.Open("pgx", observerURL.String())
	requirements.NoError(err)
	observerDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = observerDB.Close() })

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			operation := tc.start(t, st)

			blocker, err := st.db.BeginTx(t.Context(), nil)
			requirements.NoError(err)
			t.Cleanup(func() { _ = blocker.Rollback() })
			var blockerPID int
			requirements.NoError(blocker.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID))
			holdExclusiveDeliveryAdmissionFenceForTest(t.Context(), t, blocker)

			writerCtx, cancelWriter := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancelWriter()
			result := make(chan error, 1)
			go func() { result <- operation(writerCtx) }()

			var writerPID int
			requirements.Eventually(func() bool {
				err := observerDB.QueryRowContext(t.Context(), st.dialect.Rebind(`
					SELECT pid FROM pg_stat_activity
					WHERE application_name=? AND pid<>? AND state='active'
					  AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock_shared%'
					LIMIT 1`), applicationName, blockerPID).Scan(&writerPID)
				return err == nil
			}, 30*time.Second, 25*time.Millisecond, "writer never reached the delivery fence lock")
			select {
			case err := <-result:
				requirements.Error(err, "the bounded PostgreSQL lock wait should return an error")
				var pgErr *pgconn.PgError
				requirements.ErrorAs(err, &pgErr, "expected PostgreSQL lock timeout, got %v", err)
				requirements.Equal("55P03", pgErr.Code)
			case <-time.After(10 * time.Second):
				requirements.FailNow("writer did not return after its blocked fence wait was cancelled")
			}

			var backendState string
			err = observerDB.QueryRowContext(t.Context(), st.dialect.Rebind(
				`SELECT state FROM pg_stat_activity WHERE pid=?`), writerPID).Scan(&backendState)
			if errors.Is(err, sql.ErrNoRows) {
				return
			}
			requirements.NoError(err)
			assertions.NotContains(backendState, "idle in transaction",
				"failed %s writer must not return an open PostgreSQL transaction to the pool", tc.name)
		})
	}
}
