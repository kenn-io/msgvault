package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPublicationTargetDriftRetainsRecoveryEvidence(t *testing.T) {
	for _, action := range []string{"refresh", "settle", "reject", "retry"} {
		t.Run(action, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, _, book, mapping := seededCardDAVConflictMapping(t)
			pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic target drift")
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			ctx, err := st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-target-key")
			requirements.NoError(err)
			requirements.NoError(st.AdmitCardDAVPublicationReceiptContext(ctx, *pending, allow))
			person, err := st.GetPersonContext(ctx, pending.PersonID)
			requirements.NoError(err)
			// Model restored authority drift, which must fail closed even when
			// a normal role change would be refused by the ownership guard.
			_, err = st.DB().ExecContext(ctx, st.Rebind("UPDATE carddav_address_books SET is_write_target=FALSE WHERE id=?"), book.ID)
			requirements.NoError(err)
			switch action {
			case "refresh":
				_, err = st.RefreshCardDAVPublicationFenceAuthorizedContext(ctx, *pending, allow)
			case "settle":
				err = st.CommitCardDAVPublicationAuthorizedContext(ctx, store.CardDAVCanonicalMutation{
					Publication: *pending, Remote: store.CardDAVRemoteResource{Href: pending.Href,
						RemoteUID: person.VCardUID, RemoteETag: `"synthetic"`, RemoteBody: pending.OutgoingBody,
						SemanticHash: pending.OutgoingSemanticHash},
				}, allow)
			case "reject":
				err = st.RollbackCardDAVPublicationAuthorizedContext(ctx, pending, allow)
			case "retry":
				err = st.SetCardDAVPublicationRetryAfterAuthorizedContext(ctx, *pending, time.Now().Add(time.Minute), allow)
			}
			requirements.ErrorIs(err, store.ErrCardDAVNoWriteTarget)
			after, err := st.GetCardDAVPublicationContext(ctx, pending.PersonID)
			requirements.NoError(err)
			assertions.Equal(pending, after)
			var state string
			requirements.NoError(st.DB().QueryRowContext(ctx, "SELECT state FROM carddav_publication_receipts").Scan(&state))
			assertions.Equal("dispatching", state)
		})
	}
}
