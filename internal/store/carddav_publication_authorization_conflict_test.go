package store_test

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

func TestAuthorizedPublicationSettlementRejectsCarriedForeignConflict(t *testing.T) {
	for _, field := range []string{"foreign_conflict", "conflict_owned"} {
		t.Run(field, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st, _, book, mapping := seededCardDAVConflictMapping(t)
			backend, ok := any(st).(authorizedPublicationSettlementStore)
			requirements.True(ok)
			requirements.NotNil(mapping.PersonID)
			personID := *mapping.PersonID
			plan := reviewedCurrentPlan(t, st, personID)
			plan.Publication.Href = mapping.Href
			hash, err := carddav.SemanticHash(plan.Publication.OutgoingBody)
			plan.Publication.OutgoingSemanticHash = hash
			requirements.NoError(err)
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			plan.Fence = store.CardDAVCurrentReviewFence(source, plan.Publication.OutgoingBody, mapping.Href)
			plan.ApprovalToken = store.CardDAVReviewToken(plan.Fence)
			pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), plan)
			requirements.NoError(err)
			requirements.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)
			otherAccount, otherBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
				ConnectionName: "work", BaseURL: "https://dav.example.test", Username: "synthetic-other-user",
				PrincipalURL: "https://dav.example.test/other-principal/", HomeURL: "https://dav.example.test/books/",
				Books: []store.CardDAVDiscoveredBook{{CanonicalURL: book.CanonicalURL, DisplayName: "Synthetic Other Book", CanCreate: new(true)}},
			})
			requirements.NoError(err)
			requirements.Len(otherBooks, 1)
			otherBook := otherBooks[0]
			requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), otherBook.ID, store.CardDAVBookRoles{IsSubscribed: true, IsLookupSource: true}))
			otherBooks, err = st.ListCardDAVAddressBooksContext(t.Context(), otherAccount.ID)
			requirements.NoError(err)
			requirements.Len(otherBooks, 1)
			otherBook = otherBooks[0]
			remote := remoteResource(otherBook.CanonicalURL+"foreign.vcf", "synthetic-foreign-conflict", "Synthetic Foreign", "foreign-conflict@example.test", `"foreign-base"`)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: otherBook.ID, ConnectionGeneration: otherAccount.ConnectionGeneration, SyncRevision: otherBook.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
			requirements.NoError(err)
			otherMapping, err := st.GetCardDAVResourceContext(t.Context(), otherBook.ID, remote.Href)
			requirements.NoError(err)
			requirements.NotNil(otherMapping.PersonID)
			snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *otherMapping.PersonID)
			requirements.NoError(err)
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{AddressBookID: otherBook.ID, Href: remote.Href, ExpectedMappingRevision: otherMapping.MappingRevision, BaseLocalHash: otherMapping.LocalHash, LocalHash: snapshot.Fingerprint, BaseRemoteHash: otherMapping.RemoteSemanticHash, BaseRemoteETag: otherMapping.RemoteETag, RemoteETag: `"foreign-changed"`, LocalBody: remote.RemoteBody, RemoteBody: remote.RemoteBody})
			requirements.NoError(err)
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			if field == "foreign_conflict" {
				pending.ResolutionConflictID = conflict.ID
			} else {
				pending.ConflictOwned = true
			}
			canonical := store.CardDAVRemoteResource{Href: mapping.Href, RemoteUID: "review-person", RemoteETag: `"canonical"`, RemoteBody: plan.Publication.OutgoingBody, SemanticHash: plan.Publication.OutgoingSemanticHash, DisplayName: "Synthetic Corrected"}
			err = backend.CommitCardDAVPublicationAuthorizedContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *pending, Remote: canonical}, func(_ context.Context, _ *store.IdentityGrantSelection) error { return nil })
			requirements.ErrorIs(err, store.ErrCardDAVInvalidPlan)
			after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			beforeJSON, err := json.Marshal(before)
			requirements.NoError(err)
			afterJSON, err := json.Marshal(after)
			requirements.NoError(err)
			assertions.JSONEq(string(beforeJSON), string(afterJSON), "unsupported conflict input must preserve the pending publication")
			foreignAfter, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			requirements.NoError(err)
			foreignBeforeJSON, err := json.Marshal(conflict)
			requirements.NoError(err)
			foreignAfterJSON, err := json.Marshal(foreignAfter)
			requirements.NoError(err)
			assertions.JSONEq(string(foreignBeforeJSON), string(foreignAfterJSON), "foreign conflict audit is outside the admitted publication effect")
		})
	}
}
