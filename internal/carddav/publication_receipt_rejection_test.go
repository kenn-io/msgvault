package carddav

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPublicationReceiptProviderRejectionSettlesBeforeIntentRemoval(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite receipt-update abort trigger; Store rollback transaction is tested on both backends")
	for _, mode := range []string{"rejected", "throttled", "receipt_failure", "revoked_after_rejection", "generation_changed_after_rejection"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped, revoked atomic.Bool
			var puts, gets atomic.Int32
			var native *store.Store
			var bookID int64
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() {
					switch r.Method {
					case http.MethodPut:
						puts.Add(1)
					case http.MethodGet:
						gets.Add(1)
					default:
						assert.Fail("publication only dispatches its mapped PUT and canonical GET")
					}
				}
				baseHandler(w, r)
				if scoped.Load() && mode == "revoked_after_rejection" && r.Method == http.MethodPut {
					revoked.Store(true)
				}
				if scoped.Load() && mode == "generation_changed_after_rejection" && r.Method == http.MethodPut {
					_, err := native.DB().ExecContext(r.Context(), native.Rebind("UPDATE carddav_accounts SET connection_generation=connection_generation+1 WHERE id=(SELECT account_id FROM carddav_address_books WHERE id=?)"), bookID)
					assert.NoError(err)
				}
			}))
			t.Cleanup(server.Close)
			service, st, personID, book := seededMutationServiceForServer(t, server)
			native, bookID = st, book.ID
			require.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic rejected publication correction")
			denied := errors.New("synthetic rejection settlement authority revoked")
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			authorize := func(context.Context, *store.IdentityGrantSelection) error {
				if revoked.Load() {
					return denied
				}
				return nil
			}
			preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, authorize)
			require.NoError(err)
			bound, err := st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-rejected-key")
			require.NoError(err)
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			if mode == "receipt_failure" {
				_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_rejection_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic rejection receipt failure'); END`)
				require.NoError(err)
			}
			fixture.mu.Lock()
			fixture.putStatus = http.StatusBadRequest
			if mode == "throttled" {
				fixture.putStatus = http.StatusTooManyRequests
				fixture.putRetryAfter = "60"
			}
			fixture.mu.Unlock()
			scoped.Store(true)
			err = service.PublishReviewedPersonAuthorizedWithAdmission(bound, personID, preview.ApprovalToken, authorize, func(ctx context.Context, pending store.CardDAVPublication) error {
				return st.AdmitCardDAVPublicationReceiptContext(ctx, pending, authorize)
			})
			require.Error(err)
			assert.Equal(int32(1), puts.Load())
			assert.Zero(gets.Load(), "confirmed rejection never starts canonical observation")
			receipt, readErr := st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-rejected-key", allow)
			require.NoError(readErr)
			current, readErr := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(readErr)
			after, readErr := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(readErr)
			assert.Equal(before.Resource.RemoteETag, after.Resource.RemoteETag)
			assert.Equal(before.Resource.RemoteBody, after.Resource.RemoteBody)
			if mode == "receipt_failure" || mode == "revoked_after_rejection" || mode == "generation_changed_after_rejection" {
				switch mode {
				case "receipt_failure":
					require.ErrorContains(err, "synthetic rejection receipt failure")
				case "generation_changed_after_rejection":
					require.ErrorIs(err, store.ErrCardDAVStalePlan)
				default:
					require.ErrorIs(err, denied)
				}
				assert.Equal("dispatching", receipt.State)
				assert.NotEmpty(current.PendingIntentID, "unsaved rejection retains native recovery evidence")
				return
			}
			assert.Equal("rejected", receipt.State, "receipt outcome commits before the native pending intent is erased")
			assert.Empty(current.PendingIntentID)
			assert.Nil(receipt.VerifiedAt)
			encoded, readErr := json.Marshal(receipt)
			require.NoError(readErr)
			var wire struct {
				ErrorCode  string     `json:"error_code"`
				RetryAfter *time.Time `json:"retry_after"`
			}
			require.NoError(json.Unmarshal(encoded, &wire))
			if mode == "throttled" {
				assert.Equal("retry_after", wire.ErrorCode)
				require.NotNil(wire.RetryAfter)
			} else {
				assert.Equal("provider_rejected", wire.ErrorCode)
				assert.Nil(wire.RetryAfter)
			}
			_, readErr = st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-rejected-key", authorize)
			require.NoError(readErr)
			assert.Equal(int32(1), puts.Load(), "receipt reads never replay rejected provider attempts")
			assert.Zero(gets.Load())
		})
	}
}
