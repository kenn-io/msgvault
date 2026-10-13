package store_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPublicationNoopReceiptSharesNativeTransaction(t *testing.T) {
	for _, mode := range []string{"allowed", "insert_failure", "nil_authorizer", "unreviewed", "revoked_before_receipt"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st, _, book, mapping := seededCardDAVConflictMapping(t)
			require.NotNil(mapping.PersonID)
			personID := *mapping.PersonID
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			plan := store.CardDAVPublicationPlan{PersonID: personID, AddressBookID: book.ID, Desired: true, Href: mapping.Href, OutgoingBody: mapping.RemoteBody, OutgoingSemanticHash: mapping.RemoteSemanticHash, LocalHash: source.Snapshot.Fingerprint}
			fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
			reviewed := store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)}
			before, err := json.Marshal(source)
			require.NoError(err)
			bound, err := st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-noop-key")
			require.NoError(err)
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			authorize := allow
			denied := errors.New("synthetic no-op authority revoked")
			if mode == "revoked_before_receipt" {
				calls := 0
				authorize = func(context.Context, *store.IdentityGrantSelection) error {
					calls++
					if calls > 1 {
						return denied
					}
					return nil
				}
			}
			if mode == "nil_authorizer" {
				authorize = nil
			}
			if mode == "insert_failure" {
				if st.IsPostgreSQL() {
					_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION synthetic_noop_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic noop receipt failure'; END; $$`)
					require.NoError(err)
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_noop_failure BEFORE INSERT ON carddav_publication_receipts FOR EACH ROW EXECUTE FUNCTION synthetic_noop_failure()`)
					require.NoError(err)
					t.Cleanup(func() {
						_, err := st.DB().ExecContext(context.Background(), `DROP TRIGGER IF EXISTS synthetic_noop_failure ON carddav_publication_receipts`)
						assert.NoError(err)
						_, err = st.DB().ExecContext(context.Background(), `DROP FUNCTION IF EXISTS synthetic_noop_failure()`)
						assert.NoError(err)
					})
				} else {
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_noop_failure BEFORE INSERT ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic noop receipt failure'); END`)
					require.NoError(err)
				}
			}
			var prepared *store.CardDAVPublication
			if mode == "unreviewed" {
				prepared, err = st.PrepareCardDAVPublicationContext(bound, plan)
			} else {
				prepared, err = st.PrepareReviewedCardDAVPublicationAuthorizedContext(bound, reviewed, authorize)
			}
			if mode != "allowed" {
				switch mode {
				case "insert_failure":
					require.ErrorContains(err, "synthetic noop receipt failure")
				case "revoked_before_receipt":
					require.ErrorIs(err, denied)
				default:
					require.ErrorIs(err, store.ErrCardDAVInvalidPlan)
				}
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(err)
				encoded, err := json.Marshal(after)
				require.NoError(err)
				assert.JSONEq(string(before), string(encoded))
				_, err = st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-noop-key", allow)
				require.ErrorIs(err, store.ErrCardDAVPublicationReceiptNotFound)
				return
			}
			require.NoError(err)
			require.True(prepared.Noop)
			require.Empty(prepared.PendingIntentID)
			receipt, err := st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-noop-key", allow)
			require.NoError(err)
			assert.True(receipt.Noop)
			assert.Equal("verified", receipt.State)
			assert.Equal(mapping.RemoteETag, receipt.RemoteETag)
			var nonce sql.NullString
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT pending_intent_id FROM carddav_publication_receipts WHERE receipt_id=?"), receipt.ID).Scan(&nonce))
			assert.False(nonce.Valid, "no-op receipts never fabricate provider dispatch nonces")
		})
	}
}
