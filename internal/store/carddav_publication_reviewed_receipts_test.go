package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

func TestPublicationReviewedReceiptMatchesOriginalRequest(t *testing.T) {
	for _, mode := range []string{"allowed", "wrong_token", "wrong_person", "revoked", "prepare_wrong_token", "prepare_wrong_person"} {
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
			token, target := reviewed.ApprovalToken, personID
			if mode == "prepare_wrong_token" {
				token = "different-original-token"
			}
			if mode == "prepare_wrong_person" {
				target++
			}
			bound, err := st.WithReviewedCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-reviewed-key", target, token)
			require.NoError(err)
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			before, err := json.Marshal(source)
			require.NoError(err)
			prepared, err := st.PrepareReviewedCardDAVPublicationAuthorizedContext(bound, reviewed, allow)
			if mode == "prepare_wrong_token" || mode == "prepare_wrong_person" {
				require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(err)
				encoded, err := json.Marshal(after)
				require.NoError(err)
				assert.JSONEq(string(before), string(encoded))
				return
			}
			require.NoError(err)
			require.True(prepared.Noop)
			if mode == "wrong_token" {
				token = "different-original-token"
			}
			if mode == "wrong_person" {
				target++
			}
			retry, err := st.WithReviewedCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-reviewed-key", target, token)
			require.NoError(err)
			authorize := allow
			denied := errors.New("synthetic reviewed-receipt grant revoked")
			if mode == "revoked" {
				authorize = func(context.Context, *store.IdentityGrantSelection) error { return denied }
			}
			receipt, err := st.ReviewedCardDAVPublicationReceiptContext(retry, authorize)
			switch mode {
			case "wrong_token", "wrong_person":
				require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
			case "revoked":
				require.ErrorIs(err, denied)
			default:
				require.NoError(err)
				assert.Equal("verified", receipt.State)
				assert.Equal(personID, receipt.PersonID)
			}
		})
	}
}

func TestPublicationReviewedReceiptGuardsPendingProviderAttempts(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st, _, book, mapping := seededCardDAVConflictMapping(t)
	require.NotNil(mapping.PersonID)
	personID := *mapping.PersonID
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Reviewed Receipt\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
	hash, err := carddav.SemanticHash(body)
	require.NoError(err)
	plan := store.CardDAVPublicationPlan{PersonID: personID, AddressBookID: book.ID, Desired: true, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
	fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
	token := store.CardDAVReviewToken(fence)
	bound, err := st.WithReviewedCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-update-key", personID, token)
	require.NoError(err)
	allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
	pending, err := st.PrepareReviewedCardDAVPublicationAuthorizedContext(bound, store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: token}, allow)
	require.NoError(err)
	require.False(pending.Noop)
	require.NoError(st.AdmitCardDAVPublicationReceiptContext(bound, *pending, allow))
	receipt, err := st.ReviewedCardDAVPublicationReceiptContext(bound, allow)
	require.NoError(err)
	assert.Equal("dispatching", receipt.State)
	var savedHash string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT posted_request_hash FROM carddav_publication_receipts WHERE receipt_id=?"), receipt.ID).Scan(&savedHash))
	assert.Len(savedHash, 64)
	assert.NotEqual(token, savedHash, "the original approval token is not stored in plaintext")
	wrong, err := st.WithReviewedCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-update-key", personID, "another-token")
	require.NoError(err)
	require.ErrorIs(st.AdmitCardDAVPublicationReceiptContext(wrong, *pending, allow), store.ErrCardDAVPublicationMismatch)
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		require.ErrorIs(st.AuthorizeCardDAVPublicationRequestContext(wrong, *pending, method, pending.Href, allow), store.ErrCardDAVPublicationMismatch)
		require.NoError(st.AuthorizeCardDAVPublicationRequestContext(bound, *pending, method, pending.Href, allow))
	}
	selected, recovery, err := st.ReviewedCardDAVPublicationRecoveryContext(bound, allow)
	require.NoError(err)
	require.NotNil(selected)
	require.NotNil(recovery)
	assert.Equal(receipt.ID, selected.ID)
	assert.Equal(pending.PendingIntentID, recovery.PendingIntentID)
	assert.Equal(body, recovery.OutgoingBody)
	selected, recovery, err = st.ReviewedCardDAVPublicationRecoveryContext(wrong, allow)
	require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
	assert.Nil(selected)
	assert.Nil(recovery)
	denied := errors.New("synthetic recovery source grant revoked")
	selected, recovery, err = st.ReviewedCardDAVPublicationRecoveryContext(bound, func(context.Context, *store.IdentityGrantSelection) error { return denied })
	require.ErrorIs(err, denied)
	assert.Nil(selected)
	assert.Nil(recovery)
	selected, recovery, err = st.ReviewedCardDAVPublicationRecoveryContext(t.Context(), allow)
	require.ErrorIs(err, store.ErrCardDAVInvalidPlan)
	assert.Nil(selected)
	assert.Nil(recovery)
	require.NoError(st.CommitCardDAVPublicationAuthorizedContext(bound, store.CardDAVCanonicalMutation{Publication: *pending, Remote: store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: source.Person.VCardUID, RemoteETag: `"synthetic-recovery-source"`, RemoteBody: body, SemanticHash: hash}}, allow))
	selected, recovery, err = st.ReviewedCardDAVPublicationRecoveryContext(bound, allow)
	require.NoError(err)
	require.NotNil(selected)
	assert.Equal(receipt.ID, selected.ID)
	assert.Equal("verified", selected.State)
	assert.Nil(recovery)
}
