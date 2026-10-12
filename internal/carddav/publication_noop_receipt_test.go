package carddav

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPublicationNoopReceiptCommitsWithReviewedNativeState(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite abort trigger; PostgreSQL receipt transaction checks run separately")
	for _, mode := range []string{"allowed", "receipt_insert_failure"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped atomic.Bool
			var requests atomic.Int32
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() {
					requests.Add(1)
				}
				baseHandler(w, r)
			}))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, allow)
			require.NoError(err)
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			beforeJSON, err := json.Marshal(before)
			require.NoError(err)
			bound, err := st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-noop-key")
			require.NoError(err)
			if mode == "receipt_insert_failure" {
				_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_noop_receipt_failure BEFORE INSERT ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic noop receipt failure'); END`)
				require.NoError(err)
			}
			scoped.Store(true)
			err = service.PublishReviewedPersonAuthorizedWithAdmission(bound, personID, preview.ApprovalToken, allow, func(context.Context, store.CardDAVPublication) error {
				assert.Fail("unchanged publication must not admit provider dispatch")
				return store.ErrCardDAVInvalidPlan
			})
			assert.Zero(requests.Load(), "unchanged reviewed publication performs no provider work")
			if mode == "receipt_insert_failure" {
				require.ErrorContains(err, "synthetic noop receipt failure")
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(err)
				afterJSON, err := json.Marshal(after)
				require.NoError(err)
				assert.JSONEq(string(beforeJSON), string(afterJSON), "receipt failure rolls back native approval, envelope, and mapping updates")
				_, err = st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-noop-key", allow)
				require.ErrorIs(err, store.ErrCardDAVPublicationReceiptNotFound)
				return
			}
			require.NoError(err)
			receipt, err := st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-noop-key", allow)
			require.NoError(err)
			assert.Equal("verified", receipt.State)
			assert.True(receipt.Noop)
			assert.Equal(personID, receipt.PersonID)
			assert.NotNil(receipt.VerifiedAt)
			assert.NotEmpty(receipt.BodySHA256)
			assert.NotEmpty(receipt.RemoteETag)
			pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			assert.Empty(pending.PendingIntentID)
		})
	}
}
