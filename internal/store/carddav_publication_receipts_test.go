package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

type publicationReceiptStore interface {
	WithCardDAVPublicationReceipt(ctx context.Context, principal, key string) (context.Context, error)
	AdmitCardDAVPublicationReceiptContext(ctx context.Context, expected store.CardDAVPublication, authorize store.PersonEditAuthorizer) error
}

func TestPublicationReceiptSettlesWithNativeIntent(t *testing.T) {
	for _, mode := range []string{"allowed", "settlement_failure", "denied", "nil_authorizer", "changed_intent", "owner_recovery", "owner_settlement_failure", "owner_hash_mismatch", "owner_canonical_mismatch", "unbound_scoped"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, _, book, mapping := seededCardDAVConflictMapping(t)
			backend, ok := any(st).(publicationReceiptStore)
			require.True(ok, "durable publication receipts must share native intent settlement")
			require.NotNil(mapping.PersonID)
			personID := *mapping.PersonID
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Receipt\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
			hash, err := carddav.SemanticHash(body)
			require.NoError(err)
			plan := store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: book.ID, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
			fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
			pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
			require.NoError(err)
			ctx, err := backend.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-key")
			require.NoError(err)
			authorize := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			expected := *pending
			if mode == "denied" {
				authorize = func(context.Context, *store.IdentityGrantSelection) error {
					return errors.New("synthetic revoked grant")
				}
			}
			if mode == "nil_authorizer" {
				authorize = nil
			}
			if mode == "changed_intent" {
				expected.PendingIntentID = "unselected-intent"
			}
			err = backend.AdmitCardDAVPublicationReceiptContext(ctx, expected, authorize)
			if mode == "denied" || mode == "nil_authorizer" || mode == "changed_intent" {
				require.Error(err)
				var count int
				require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM carddav_publication_receipts").Scan(&count))
				assert.Zero(count)
				current, err := st.GetCardDAVPublicationContext(t.Context(), personID)
				require.NoError(err)
				assert.Equal(pending.PendingIntentID, current.PendingIntentID)
				return
			}
			require.NoError(err)
			require.ErrorIs(backend.AdmitCardDAVPublicationReceiptContext(ctx, *pending, authorize), store.ErrCardDAVPublicationPending, "an admitted intent cannot authorize another dispatch")
			for _, alternate := range [][2]string{{"synthetic-owner", "another-key"}, {"another-owner", "synthetic-key"}} {
				otherCtx, err := backend.WithCardDAVPublicationReceipt(t.Context(), alternate[0], alternate[1])
				require.NoError(err)
				require.Error(backend.AdmitCardDAVPublicationReceiptContext(otherCtx, *pending, authorize), "one native pending intent cannot authorize a second dispatch under another caller/key")
			}
			nonUpdate := *pending
			nonUpdate.PendingOperation = store.CardDAVMutationDelete
			require.ErrorIs(st.CommitCardDAVPublicationAuthorizedContext(ctx, store.CardDAVCanonicalMutation{Publication: nonUpdate, Tombstone: true}, authorize), store.ErrCardDAVInvalidPlan, "receipt admission supports mapped updates only")
			var state, intent, etag string
			read := func() {
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT state,pending_intent_id,remote_etag FROM carddav_publication_receipts WHERE person_id=?"), personID).Scan(&state, &intent, &etag))
			}
			read()
			assert.Equal("dispatching", state)
			assert.Equal(pending.PendingIntentID, intent)
			assert.Empty(etag)
			if mode == "settlement_failure" || mode == "owner_settlement_failure" {
				if st.IsPostgreSQL() {
					_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION synthetic_receipt_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic receipt settlement failure'; END; $$`)
					require.NoError(err)
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_receipt_failure BEFORE UPDATE ON carddav_publication_receipts FOR EACH ROW EXECUTE FUNCTION synthetic_receipt_failure()`)
					require.NoError(err)
					t.Cleanup(func() {
						_, err := st.DB().ExecContext(context.Background(), `DROP TRIGGER IF EXISTS synthetic_receipt_failure ON carddav_publication_receipts`)
						assert.NoError(err)
						_, err = st.DB().ExecContext(context.Background(), `DROP FUNCTION IF EXISTS synthetic_receipt_failure()`)
						assert.NoError(err)
					})
				} else {
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_receipt_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic receipt settlement failure'); END`)
					require.NoError(err)
					t.Cleanup(func() {
						_, err := st.DB().ExecContext(context.Background(), `DROP TRIGGER IF EXISTS synthetic_receipt_failure`)
						assert.NoError(err)
					})
				}
			}
			mutation := store.CardDAVCanonicalMutation{Publication: *pending, Remote: store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: source.Person.VCardUID, RemoteETag: `"verified-receipt"`, RemoteBody: body, SemanticHash: hash}}
			if mode == "owner_canonical_mismatch" {
				mutation.Remote.RemoteBody = []byte(strings.ReplaceAll(string(body), "Synthetic Receipt", "Synthetic Other Card"))
				mutation.Remote.SemanticHash, err = carddav.SemanticHash(mutation.Remote.RemoteBody)
				require.NoError(err)
			}
			if mode == "owner_hash_mismatch" {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE carddav_publication_receipts SET request_hash=? WHERE pending_intent_id=?"), strings.Repeat("0", 64), pending.PendingIntentID)
				require.NoError(err)
			}
			switch mode {
			case "owner_recovery", "owner_settlement_failure", "owner_hash_mismatch", "owner_canonical_mismatch":
				err = st.CommitCardDAVPublicationContext(t.Context(), mutation)
			case "unbound_scoped":
				err = st.CommitCardDAVPublicationAuthorizedContext(t.Context(), mutation, authorize)
			default:
				err = st.CommitCardDAVPublicationAuthorizedContext(ctx, mutation, authorize)
			}
			current, readErr := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(readErr)
			read()
			switch mode {
			case "owner_hash_mismatch", "owner_canonical_mismatch", "unbound_scoped":
				wantErr := store.ErrCardDAVPublicationPending
				if mode == "unbound_scoped" {
					wantErr = store.ErrCardDAVInvalidPlan
				}
				require.ErrorIs(err, wantErr)
				assert.Equal("dispatching", state)
				assert.Equal(pending.PendingIntentID, current.PendingIntentID)
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(err)
				assert.Equal(mapping.RemoteETag, after.Resource.RemoteETag)
			case "settlement_failure", "owner_settlement_failure":
				require.ErrorContains(err, "synthetic receipt settlement failure")
				assert.Equal("dispatching", state)
				assert.Equal(pending.PendingIntentID, current.PendingIntentID, "receipt failure must retain native recovery evidence")
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(err)
				assert.Equal(mapping.RemoteETag, after.Resource.RemoteETag, "canonical observation must roll back with receipt")
			default:
				require.NoError(err)
				assert.Equal("verified", state)
				assert.Equal(`"verified-receipt"`, etag)
				assert.Empty(current.PendingIntentID)
			}
		})
	}
}
