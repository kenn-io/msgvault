package carddav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPublicationReceiptRestartRecoveryNeverReplaysPUT(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite close/open and abort trigger; PostgreSQL receipt settlement is covered by Store backend tests")
	for _, mode := range []string{"allowed", "wrong_key", "missing_receipt"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped atomic.Bool
			var puts, gets atomic.Int32
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() {
					switch r.Method {
					case http.MethodPut:
						puts.Add(1)
					case http.MethodGet:
						gets.Add(1)
					default:
						assert.Fail("receipt recovery must only observe its mapped resource")
					}
				}
				baseHandler(w, r)
			}))
			t.Cleanup(server.Close)
			service, seeded, personID, _ := seededMutationServiceForServer(t, server)
			path := filepath.Join(t.TempDir(), "receipt-restart.db")
			require.NoError(seeded.BackupDatabase(path))
			st, err := store.Open(path)
			require.NoError(err)
			require.NoError(st.InitSchema())
			t.Cleanup(func() { assert.NoError(st.Close()) })
			service = NewService(st, service.dav().client)
			require.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic receipt recovery correction")
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, allow)
			require.NoError(err)
			bound, err := st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-key")
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_receipt_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic receipt settlement failure'); END`)
			require.NoError(err)
			scoped.Store(true)
			err = service.PublishReviewedPersonAuthorizedWithAdmission(bound, personID, preview.ApprovalToken, allow, func(ctx context.Context, pending store.CardDAVPublication) error {
				return st.AdmitCardDAVPublicationReceiptContext(ctx, pending, allow)
			})
			require.ErrorContains(err, "synthetic receipt settlement failure")
			assert.Equal(int32(1), puts.Load())
			assert.Equal(int32(1), gets.Load())
			receipt, err := st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-key", allow)
			require.NoError(err)
			assert.Equal("dispatching", receipt.State)
			pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			require.NotEmpty(pending.PendingIntentID)
			_, err = st.DB().ExecContext(t.Context(), "DROP TRIGGER synthetic_receipt_failure")
			require.NoError(err)
			client := service.dav().client
			require.NoError(st.Close())
			reopened, err := store.Open(path)
			require.NoError(err)
			st = reopened
			require.NoError(reopened.InitSchema())
			service = NewService(reopened, client)
			key := "synthetic-key"
			if mode == "wrong_key" {
				key = "synthetic-unselected-key"
			}
			if mode == "missing_receipt" {
				_, err = reopened.DB().ExecContext(t.Context(), "DELETE FROM carddav_publication_receipts")
				require.NoError(err)
			}
			bound, err = reopened.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", key)
			require.NoError(err)
			err = service.RecoverPublicationAuthorized(bound, *pending, allow)
			assert.Equal(int32(1), puts.Load(), "restart recovery must never replay PUT")
			if mode != "allowed" {
				require.Error(err)
				assert.Equal(int32(1), gets.Load(), "receipt mismatch must fail before provider observation")
				retained, err := reopened.GetCardDAVPublicationContext(t.Context(), personID)
				require.NoError(err)
				assert.Equal(pending.PendingIntentID, retained.PendingIntentID)
				return
			}
			require.NoError(err)
			assert.Equal(int32(2), gets.Load())
			settled, err := reopened.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-key", allow)
			require.NoError(err)
			assert.Equal(receipt.ID, settled.ID)
			assert.Equal("verified", settled.State)
			current, err := reopened.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			assert.Empty(current.PendingIntentID)
			require.Error(service.RecoverPublicationAuthorized(bound, *pending, allow))
			assert.Equal(int32(1), puts.Load())
			assert.Equal(int32(2), gets.Load())
		})
	}
}
