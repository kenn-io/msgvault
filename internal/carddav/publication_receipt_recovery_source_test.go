package carddav

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReviewedReceiptRecoverySelectsOnlyOriginalNativeIntent(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite settlement failure trigger; receipt recovery SQL also receives PostgreSQL coverage")
	for _, mode := range []string{"allowed", "wrong_token", "revoked", "changed_intent", "settled"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var calls atomic.Int32
			base := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				base(w, r)
			}))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic explicit receipt recovery")
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, allow)
			require.NoError(err)
			if mode != "settled" {
				_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_recovery_source_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic recovery source failure'); END`)
				require.NoError(err)
			}
			first, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-recovery-source", allow)
			if mode == "settled" {
				require.NoError(err)
			} else {
				require.ErrorContains(err, "synthetic recovery source failure")
			}
			require.NotNil(first)
			before, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			if mode == "changed_intent" {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE carddav_publications SET pending_intent_id=? WHERE person_id=?"), "synthetic-unrelated-intent", personID)
				require.NoError(err)
			}
			token := preview.ApprovalToken
			if mode == "wrong_token" {
				token = "synthetic-different-original-token"
			}
			bound, err := st.WithReviewedCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-recovery-source", personID, token)
			require.NoError(err)
			denied := errors.New("synthetic recovery authority revoked")
			authorize := allow
			if mode == "revoked" {
				authorize = func(context.Context, *store.IdentityGrantSelection) error { return denied }
			}
			beforeCalls := calls.Load()
			receipt, pending, err := st.ReviewedCardDAVPublicationRecoveryContext(bound, authorize)
			switch mode {
			case "wrong_token", "changed_intent":
				require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
				assert.Nil(receipt)
				assert.Nil(pending)
			case "revoked":
				require.ErrorIs(err, denied)
				assert.Nil(receipt)
				assert.Nil(pending)
			default:
				require.NoError(err)
				require.NotNil(receipt)
				assert.Equal(first.ID, receipt.ID)
				if mode == "settled" {
					assert.Nil(pending)
				} else {
					require.NotNil(pending)
					assert.Equal(before.PendingIntentID, pending.PendingIntentID)
					assert.Equal(before.OutgoingBody, pending.OutgoingBody)
				}
			}
			assert.Equal(beforeCalls, calls.Load(), "recovery source selection never contacts the provider")
			if mode != "settled" {
				_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER synthetic_recovery_source_failure`)
				require.NoError(err)
			} else {
				service.remote = nil
			}
			recovered, err := service.ReconcileReviewedPersonWithReceipt(t.Context(), personID, token, "synthetic-owner", "synthetic-recovery-source", authorize)
			switch mode {
			case "wrong_token", "changed_intent":
				require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
				assert.Nil(recovered)
			case "revoked":
				require.ErrorIs(err, denied)
				assert.Nil(recovered)
			default:
				require.NoError(err)
				require.NotNil(recovered)
				assert.Equal(first.ID, recovered.ID)
				assert.Equal("verified", recovered.State)
			}
			wantCalls := beforeCalls
			if mode == "allowed" {
				wantCalls++
			}
			assert.Equal(wantCalls, calls.Load(), "explicit reconciliation performs one GET and never repeats PUT")
			if mode == "allowed" || mode == "settled" {
				again, err := service.ReconcileReviewedPersonWithReceipt(t.Context(), personID, token, "synthetic-owner", "synthetic-recovery-source", authorize)
				require.NoError(err)
				require.NotNil(again)
				assert.Equal(first.ID, again.ID)
				assert.Equal(wantCalls, calls.Load())
			}
		})
	}
}
