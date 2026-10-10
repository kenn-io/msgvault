package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type authorizedPublicationRequestStore interface {
	AuthorizeCardDAVPublicationRequestContext(ctx context.Context, expected store.CardDAVPublication, method, target string, authorize store.PersonEditAuthorizer) error
}

func TestPublicationRequestAuthorizationChecksCurrentNativeIntentBeforeDispatch(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		for _, change := range []string{"allowed", "denied", "wrong_nonce", "wrong_target", "book_revision", "projection", "stale_generation"} {
			t.Run(method+"/"+change, func(t *testing.T) {
				requirements, assertions := require.New(t), assert.New(t)
				st, account, book, mapping := seededCardDAVConflictMapping(t)
				backend, ok := any(st).(authorizedPublicationRequestStore)
				requirements.True(ok, "provider dispatch must authorize the exact native pending intent")
				pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Corrected")
				expected := *pending
				target := pending.Href
				switch change {
				case "wrong_nonce":
					expected.PendingIntentID = "synthetic-obsolete-intent"
				case "wrong_target":
					target = book.CanonicalURL + "unselected.vcf"
				case "book_revision":
					books, err := st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
					requirements.NoError(err)
					requirements.Len(books, 1)
					_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, NextSyncToken: "synthetic-book-drift"})
					requirements.NoError(err)
				case "projection":
					_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: pending.PersonID, Text: "Synthetic later inference", Source: store.ProvenanceExtraction})
					requirements.NoError(err)
				case "stale_generation":
					expected.ConnectionGeneration--
				}
				before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), pending.PersonID)
				requirements.NoError(err)
				denied := errors.New("synthetic provider dispatch authority revoked")
				var seen *store.IdentityGrantSelection
				err = backend.AuthorizeCardDAVPublicationRequestContext(t.Context(), expected, method, target, func(_ context.Context, scope *store.IdentityGrantSelection) error {
					seen = scope
					if change == "denied" {
						return denied
					}
					return nil
				})
				admitted := change == "allowed" || (method == http.MethodGet && (change == "projection" || change == "book_revision"))
				switch {
				case admitted:
					requirements.NoError(err)
				case change == "denied":
					requirements.ErrorIs(err, denied)
				case change == "wrong_target":
					requirements.ErrorIs(err, store.ErrCardDAVInvalidPlan)
				default:
					requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
				}
				if admitted || change == "denied" {
					requirements.NotNil(seen)
					requirements.Len(seen.Persons, 1)
					assertions.Equal(pending.PersonID, seen.Persons[0].ID)
					assertions.Equal(before.Person.VCardUID, seen.Persons[0].UID)
					requirements.Len(seen.AddressBooks, 1)
					assertions.Equal(account.ID, seen.AddressBooks[0].AccountID)
					assertions.Equal(book.ID, seen.AddressBooks[0].BookID)
				} else {
					assertions.Nil(seen)
				}
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), pending.PersonID)
				requirements.NoError(err)
				beforeJSON, err := json.Marshal(before)
				requirements.NoError(err)
				afterJSON, err := json.Marshal(after)
				requirements.NoError(err)
				assertions.JSONEq(string(beforeJSON), string(afterJSON), "dispatch check must preserve all native pending intent and projection evidence")
			})
		}
	}
}
