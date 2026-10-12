package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

// Removing receipt admission or decoupling its settlement from the native
// canonical mutation must fail this real Store lifecycle on either backend.
func TestMappedPublicationReceiptAdmissionAndSettlement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, _, book, mapping := seededCardDAVConflictMapping(t)
	requirements.NotNil(mapping.PersonID)
	personID := *mapping.PersonID
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	requirements.NoError(err)
	body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Receipt\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
	hash, err := carddav.SemanticHash(body)
	requirements.NoError(err)
	plan := store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: book.ID, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
	fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
	pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
	requirements.NoError(err)
	receipts, ok := any(st).(interface {
		WithCardDAVPublicationReceipt(ctx context.Context, principal, key string) (context.Context, error)
		AdmitCardDAVPublicationReceiptContext(ctx context.Context, expected store.CardDAVPublication, authorize store.PersonEditAuthorizer) error
		CommitCardDAVPublicationAuthorizedContext(ctx context.Context, mutation store.CardDAVCanonicalMutation, authorize store.PersonEditAuthorizer) error
	})
	requirements.True(ok, "mapped publications must durably admit and atomically settle receipts")
	ctx, err := receipts.WithCardDAVPublicationReceipt(t.Context(), "synthetic-principal", "synthetic-request")
	requirements.NoError(err)
	allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
	requirements.NoError(receipts.AdmitCardDAVPublicationReceiptContext(ctx, *pending, allow))
	var state string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT state FROM carddav_publication_receipts WHERE person_id=?"), personID).Scan(&state))
	assertions.Equal("dispatching", state)
	requirements.ErrorIs(receipts.AdmitCardDAVPublicationReceiptContext(ctx, *pending, allow), store.ErrCardDAVPublicationPending)
	mutation := store.CardDAVCanonicalMutation{Publication: *pending, Remote: store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: source.Person.VCardUID, RemoteETag: `"receipt-verified"`, RemoteBody: body, SemanticHash: hash}}
	requirements.NoError(receipts.CommitCardDAVPublicationAuthorizedContext(ctx, mutation, allow))
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT state FROM carddav_publication_receipts WHERE person_id=?"), personID).Scan(&state))
	assertions.Equal("verified", state)
	current, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	requirements.NoError(err)
	assertions.Empty(current.PendingOperation)
}
