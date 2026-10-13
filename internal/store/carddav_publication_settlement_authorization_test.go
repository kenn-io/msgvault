package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

type authorizedPublicationSettlementStore interface {
	CommitCardDAVPublicationAuthorizedContext(ctx context.Context, input store.CardDAVCanonicalMutation, authorize store.PersonEditAuthorizer) error
}

func TestPublicationSettlementAuthorizationPreservesPendingIntentOnDenial(t *testing.T) {
	for _, allow := range []bool{false, true} {
		name := "denied"
		if allow {
			name = "allowed"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st, account, book, mapping := seededCardDAVConflictMapping(t)
			backend, ok := any(st).(authorizedPublicationSettlementStore)
			requirements.True(ok, "native canonical settlement must authorize inside its existing transaction")
			requirements.NotNil(mapping.PersonID)
			personID := *mapping.PersonID
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Corrected\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
			hash, err := carddav.SemanticHash(body)
			requirements.NoError(err)
			plan := store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: book.ID, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
			fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
			pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
			requirements.NoError(err)
			requirements.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			canonicalBody := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nPRODID:-//Synthetic Server//EN\r\nFN:Synthetic Corrected\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
			canonicalHash, err := carddav.SemanticHash(canonicalBody)
			requirements.NoError(err)
			requirements.Equal(hash, canonicalHash)
			canonical := store.CardDAVRemoteResource{Href: mapping.Href, RemoteUID: source.Person.VCardUID, RemoteETag: `"canonical"`, RemoteBody: canonicalBody, SemanticHash: canonicalHash, DisplayName: "Synthetic Corrected"}
			denied := errors.New("synthetic publication authority revoked")
			var seen *store.IdentityGrantSelection
			err = backend.CommitCardDAVPublicationAuthorizedContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *pending, Remote: canonical}, func(_ context.Context, scope *store.IdentityGrantSelection) error {
				seen = scope
				if !allow {
					return denied
				}
				return nil
			})
			if allow {
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, denied)
			}
			requirements.NotNil(seen)
			requirements.Len(seen.Persons, 1)
			assertions.Equal(personID, seen.Persons[0].ID)
			assertions.Equal(source.Person.VCardUID, seen.Persons[0].UID)
			requirements.Len(seen.AddressBooks, 1)
			assertions.Equal(account.ID, seen.AddressBooks[0].AccountID)
			assertions.Equal(book.ID, seen.AddressBooks[0].BookID)
			after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			requirements.NotNil(after.Publication)
			if !allow {
				beforeJSON, err := json.Marshal(before)
				requirements.NoError(err)
				afterJSON, err := json.Marshal(after)
				requirements.NoError(err)
				assertions.JSONEq(string(beforeJSON), string(afterJSON), "denied canonical settlement retains all pending evidence for authorized recovery")
				assertions.Equal(store.CardDAVMutationUpdate, after.Publication.PendingOperation)
			} else {
				assertions.Empty(after.Publication.PendingOperation)
				requirements.NotNil(after.Resource)
				assertions.Equal(`"canonical"`, after.Resource.RemoteETag)
				assertions.Equal(canonicalBody, after.Resource.RemoteBody)
			}
		})
	}
}
