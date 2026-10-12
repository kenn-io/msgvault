package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

func TestPublicationRejectedReceiptSharesRollbackTransaction(t *testing.T) {
	for _, throttled := range []bool{false, true} {
		for _, mode := range []string{"allowed", "receipt_failure", "denied", "nil_authorizer", "wrong_key", "changed_generation"} {
			t.Run(fmt.Sprintf("throttled_%t_%s", throttled, mode), func(t *testing.T) {
				require, assert := require.New(t), assert.New(t)
				st, account, book, mapping := seededCardDAVConflictMapping(t)
				require.NotNil(mapping.PersonID)
				personID := *mapping.PersonID
				source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(err)
				body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Rejected\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
				hash, err := carddav.SemanticHash(body)
				require.NoError(err)
				plan := store.CardDAVPublicationPlan{PersonID: personID, AddressBookID: book.ID, Desired: true, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
				fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
				allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
				pending, err := st.PrepareReviewedCardDAVPublicationAuthorizedContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)}, allow)
				require.NoError(err)
				bound, err := st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-rejected-key")
				require.NoError(err)
				require.NoError(st.AdmitCardDAVPublicationReceiptContext(bound, *pending, allow))
				originalGate := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
				requestedGate := originalGate.Add(time.Hour)
				require.NoError(st.SetCardDAVRetryAfterContext(t.Context(), originalGate, account.ID))
				if mode == "changed_generation" {
					_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE carddav_accounts SET connection_generation=connection_generation+1 WHERE id=?"), account.ID)
					require.NoError(err)
				}
				before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(err)
				beforeJSON, err := json.Marshal(before)
				require.NoError(err)
				denied := errors.New("synthetic rejection authority revoked")
				authorize := allow
				if mode == "denied" {
					authorize = func(context.Context, *store.IdentityGrantSelection) error { return denied }
				}
				if mode == "nil_authorizer" {
					authorize = nil
				}
				if mode == "wrong_key" {
					bound, err = st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-unselected-key")
					require.NoError(err)
				}
				if mode == "receipt_failure" {
					if st.IsPostgreSQL() {
						_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION synthetic_rejected_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic rejection receipt failure'; END; $$`)
						require.NoError(err)
						_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_rejected_failure BEFORE UPDATE ON carddav_publication_receipts FOR EACH ROW EXECUTE FUNCTION synthetic_rejected_failure()`)
						require.NoError(err)
						t.Cleanup(func() {
							_, err := st.DB().ExecContext(context.Background(), `DROP TRIGGER IF EXISTS synthetic_rejected_failure ON carddav_publication_receipts`)
							assert.NoError(err)
							_, err = st.DB().ExecContext(context.Background(), `DROP FUNCTION IF EXISTS synthetic_rejected_failure()`)
							assert.NoError(err)
						})
					} else {
						_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_rejected_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic rejection receipt failure'); END`)
						require.NoError(err)
					}
				}
				if throttled {
					err = st.RollbackCardDAVPublicationThrottleAuthorizedContext(bound, pending, requestedGate, authorize)
				} else {
					err = st.RollbackCardDAVPublicationAuthorizedContext(bound, pending, authorize)
				}
				receipt, readErr := st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-rejected-key", allow)
				require.NoError(readErr)
				after, readErr := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(readErr)
				gate, readErr := st.GetCardDAVRetryAfterContext(t.Context(), account.ID)
				require.NoError(readErr)
				require.NotNil(gate)
				if mode != "allowed" {
					switch mode {
					case "receipt_failure":
						require.ErrorContains(err, "synthetic rejection receipt failure")
					case "denied":
						require.ErrorIs(err, denied)
					case "nil_authorizer":
						require.ErrorIs(err, store.ErrCardDAVInvalidPlan)
					case "wrong_key":
						require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
					case "changed_generation":
						require.ErrorIs(err, store.ErrCardDAVStalePlan)
					}
					afterJSON, readErr := json.Marshal(after)
					require.NoError(readErr)
					assert.JSONEq(string(beforeJSON), string(afterJSON), "unsaved rejection retains complete native recovery evidence")
					assert.Equal("dispatching", receipt.State)
					assert.True(originalGate.Equal(*gate), "failed rejection cannot extend the owning retry gate")
					return
				}
				require.NoError(err)
				assert.Equal("rejected", receipt.State)
				require.NotNil(after.Publication)
				assert.Empty(after.Publication.PendingIntentID)
				assert.Equal(mapping.MappingRevision, after.Resource.MappingRevision)
				if throttled {
					assert.Equal("retry_after", receipt.ErrorCode)
					require.NotNil(receipt.RetryAfter)
					assert.True(requestedGate.Equal(*gate))
					assert.True(gate.Equal(*receipt.RetryAfter), "receipt carries the actual owning-account retry deadline")
				} else {
					assert.Equal("provider_rejected", receipt.ErrorCode)
					assert.Nil(receipt.RetryAfter)
					assert.True(originalGate.Equal(*gate))
				}
			})
		}
	}
}
