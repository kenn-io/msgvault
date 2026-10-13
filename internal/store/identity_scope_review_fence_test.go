//go:build cgo

package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestIdentityScopeArtifactApprovalReservesWriterBeforeSnapshot(t *testing.T) {
	for _, kind := range []string{"conflict", "pending"} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var st *store.Store
			var approve func(context.Context) error
			if kind == "conflict" {
				var mapping *store.CardDAVResource
				st, _, _, mapping = seededCardDAVConflictMapping(t)
				conflict, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(mapping))
				requirements.NoError(err)
				plan := conflictApprovalPlan(t, st, conflict.ID)
				approve = func(ctx context.Context) error { return st.ApproveCardDAVConflictLocalContext(ctx, plan) }
			} else {
				st, _, _ = newCardDAVResourceStore(t)
				personID := inferenceReviewPerson(t, st)
				initial := reviewedCurrentPlan(t, st, personID)
				_, err := st.PrepareCardDAVPublicationContext(t.Context(), initial.Publication)
				requirements.NoError(err)
				source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				requirements.NoError(err)
				fence := store.CardDAVPendingReviewFence(source)
				plan := store.CardDAVPendingCreateApprovalPlan{Fence: fence, Body: source.Publication.OutgoingBody, ApprovalToken: store.CardDAVReviewToken(fence)}
				approve = func(ctx context.Context) error {
					_, err := st.ApprovePendingCardDAVCreateContext(ctx, plan)
					return err
				}
			}
			if st.IsPostgreSQL() {
				t.Skip("exercises SQLite snapshot upgrade")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var sequence int
			var name, path string
			requirements.NoError(st.DB().QueryRowContext(ctx, `PRAGMA database_list`).Scan(&sequence, &name, &path))
			writer, err := store.Open(path)
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(writer.Close()) })
			st.DB().SetMaxOpenConns(1)
			conn, err := st.DB().Conn(ctx)
			requirements.NoError(err)
			var once sync.Once
			interleaved := false
			var writeErr error
			requirements.NoError(conn.Raw(func(raw any) error {
				driver, ok := raw.(*sqlite3.SQLiteConn)
				requirements.True(ok)
				// Commit a real unrelated native writer immediately before the
				// identity INSERT executes. A preceding conflict/publication read
				// would leave this approval with an obsolete SQLite snapshot.
				driver.RegisterAuthorizer(func(operation int, table, _, _ string) int {
					if operation == sqlite3.SQLITE_INSERT && table == "archive_metadata" {
						once.Do(func() {
							interleaved = true
							_, writeErr = writer.GetOrCreateSourceContext(ctx, "imap", "interleaved-owner@example.test")
						})
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))
			requirements.NoError(conn.Close())
			defer func() {
				conn, err := st.DB().Conn(context.WithoutCancel(ctx))
				requirements.NoError(err)
				defer func() { requirements.NoError(conn.Close()) }()
				requirements.NoError(conn.Raw(func(raw any) error {
					driver, ok := raw.(*sqlite3.SQLiteConn)
					requirements.True(ok)
					driver.RegisterAuthorizer(nil)
					return nil
				}))
			}()
			err = approve(ctx)
			requirements.True(interleaved)
			requirements.NoError(writeErr)
			assertions.NoError(err, "reserve the writer before the review snapshot instead of leaking SQLITE_BUSY_SNAPSHOT")
		})
	}
}
