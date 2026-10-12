package carddav

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOwnerRecoverySettlesOriginalPublicationReceipt(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite settlement failure trigger; native receipt SQL contracts also run on PostgreSQL")
	for _, mode := range []string{"verified", "remote_changed"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped atomic.Bool
			var puts, gets atomic.Int32
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() {
					if r.Method == http.MethodPut {
						puts.Add(1)
					}
					if r.Method == http.MethodGet {
						gets.Add(1)
					}
				}
				baseHandler(w, r)
			}))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic owner receipt recovery")
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, allow)
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_owner_receipt_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic owner receipt failure'); END`)
			require.NoError(err)
			scoped.Store(true)
			first, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-owner-recovery", allow)
			require.ErrorContains(err, "synthetic owner receipt failure")
			require.NotNil(first)
			require.Equal("dispatching", first.State)
			beforeRecovery, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			require.NotEmpty(beforeRecovery.PendingIntentID)
			_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER synthetic_owner_receipt_failure`)
			require.NoError(err)
			if mode == "remote_changed" {
				person, err := st.GetPerson(personID)
				require.NoError(err)
				fixture.mu.Lock()
				fixture.body = []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Foreign Card\r\nEND:VCARD\r\n", person.VCardUID))
				fixture.etag = `"synthetic-foreign-etag"`
				fixture.mu.Unlock()
			}
			recovered, err := service.recoverPendingPublications(t.Context())
			if mode == "remote_changed" {
				require.ErrorIs(err, store.ErrCardDAVPublicationPending)
			} else {
				require.NoError(err)
			}
			assert.True(recovered[personID])
			pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			wantState := "verified"
			if mode == "remote_changed" {
				assert.Equal(beforeRecovery.PendingIntentID, pending.PendingIntentID, "an ambiguous observation must retain the original recovery intent")
				assert.Equal(beforeRecovery.OutgoingBody, pending.OutgoingBody)
				wantState = "dispatching"
			} else {
				assert.Empty(pending.PendingIntentID)
			}
			receipt, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-owner-recovery", allow)
			require.NoError(err)
			assert.Equal(first.ID, receipt.ID)
			assert.Equal(wantState, receipt.State, "owner reconciliation must preserve the original receipt and its pending intent together")
			assert.Equal(int32(1), puts.Load(), "owner recovery must not replay the provider write")
			assert.Equal(int32(2), gets.Load())
		})
	}
}
