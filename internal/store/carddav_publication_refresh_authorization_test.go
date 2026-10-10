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

type authorizedPublicationRefreshStore interface {
	RefreshCardDAVPublicationFenceAuthorizedContext(ctx context.Context, expected store.CardDAVPublication, authorize store.PersonEditAuthorizer) (*store.CardDAVPublication, error)
}

func prepareSyntheticReviewedMappedUpdate(t *testing.T, st *store.Store, mapping *store.CardDAVResource, name string) *store.CardDAVPublication {
	t.Helper()
	require.NotNil(t, mapping.PersonID)
	personID := *mapping.PersonID
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(t, err)
	body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:%s\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID, name))
	hash, err := carddav.SemanticHash(body)
	require.NoError(t, err)
	plan := store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: mapping.AddressBookID, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
	fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
	pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
	require.NoError(t, err)
	require.Equal(t, store.CardDAVMutationUpdate, pending.PendingOperation)
	return pending
}

func TestPublicationRefreshAuthorizationBindsOriginalPendingIntent(t *testing.T) {
	for _, field := range []string{"allowed", "denied", "stale_revision", "superseded", "foreign_conflict", "conflict_owned"} {
		t.Run(field, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st, account, book, mapping := seededCardDAVConflictMapping(t)
			backend, ok := any(st).(authorizedPublicationRefreshStore)
			requirements.True(ok, "native recovery-fence refresh must authorize the original pending mutation")
			pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Corrected")
			books, err := st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
			requirements.NoError(err)
			requirements.Len(books, 1)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, NextSyncToken: "synthetic-next-token"})
			requirements.NoError(err)
			if field == "superseded" {
				requirements.NoError(st.RollbackCardDAVPublicationContext(t.Context(), pending))
				replacement := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Replacement")
				requirements.NotEqual(pending.MutationRevision, replacement.MutationRevision)
			}
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), pending.PersonID)
			requirements.NoError(err)
			requirements.NotNil(before.Publication)
			requirements.NotNil(before.Resource)
			if field != "superseded" {
				requirements.NotEqual(before.Book.SyncRevision, before.Publication.BookSyncRevision)
			}
			expected := *pending
			switch field {
			case "stale_revision":
				expected.MutationRevision++
			case "foreign_conflict":
				expected.ResolutionConflictID = 42
			case "conflict_owned":
				expected.ConflictOwned = true
			}
			denied := errors.New("synthetic recovery authority revoked")
			var seen *store.IdentityGrantSelection
			refreshed, err := backend.RefreshCardDAVPublicationFenceAuthorizedContext(t.Context(), expected, func(_ context.Context, scope *store.IdentityGrantSelection) error {
				seen = scope
				if field == "denied" {
					return denied
				}
				return nil
			})
			switch field {
			case "allowed":
				requirements.NoError(err)
				requirements.NotNil(refreshed)
			case "denied":
				requirements.ErrorIs(err, denied)
			case "stale_revision", "superseded":
				requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
			default:
				requirements.ErrorIs(err, store.ErrCardDAVInvalidPlan)
			}
			if field == "allowed" || field == "denied" {
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
			requirements.NotNil(after.Publication)
			if field == "allowed" {
				assertions.Equal(before.Book.SyncRevision, refreshed.BookSyncRevision)
				assertions.Equal(before.Resource.MappingRevision, refreshed.MappingRevision)
				before.Publication.BookSyncRevision = before.Book.SyncRevision
				before.Publication.MappingRevision = before.Resource.MappingRevision
			} else {
				assertions.Nil(refreshed)
			}
			beforeJSON, err := json.Marshal(before)
			requirements.NoError(err)
			afterJSON, err := json.Marshal(after)
			requirements.NoError(err)
			assertions.JSONEq(string(beforeJSON), string(afterJSON), "recovery refresh may only change admitted fences, preserving exact outgoing intent and review approval")
		})
	}
}
