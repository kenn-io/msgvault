package carddav

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestPublicationOwnershipFollowsServerReorderingAndPreservesResidue(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "Owned detail")
	source, plan, err := service.currentPublicationPlan(t.Context(), personID)
	require.NoError(err)
	fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
	pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{
		Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence),
	})
	require.NoError(err)
	reordered := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nNOTE:Owned detail\r\nPRODID:server\r\nX-REMOTE:Retained\r\nFN:Alice Example\r\nUID:person\r\nEND:VCARD\r\n")
	require.NoError(st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{
		Publication: *pending,
		Remote:      store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: "person", RemoteETag: `"canonical"`, RemoteBody: reordered, SemanticHash: pending.OutgoingSemanticHash},
	}))
	rebound, err := st.GetVCardResourceEnvelopeContext(t.Context(), fmt.Sprintf("carddav:%d", book.ID), pending.Href)
	require.NoError(err)
	prepared, err := vcard.UnmarshalResourceMetadata(plan.OutgoingEnvelopeMetadata)
	require.NoError(err)
	require.Len(rebound.NativeMappings, len(prepared.NativeMappings))
	assert.Equal(reordered, rebound.StoredBody)
	noteOwned := false
	for _, mapping := range rebound.NativeMappings {
		if mapping.Identity.OriginalName == "NOTE" {
			noteOwned = true
		}
	}
	assert.True(noteOwned)
	residue, err := rebound.RenderView(vcard.Version30)
	require.NoError(err)
	assert.Contains(string(residue), "X-REMOTE:Retained")
	for _, mapping := range rebound.NativeMappings {
		assert.NotEqual("X-REMOTE", mapping.Identity.OriginalName)
	}
}

func TestPublicationOwnershipRejectsAmbiguousDuplicateResponse(t *testing.T) {
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "Owned detail")
	source, plan, err := service.currentPublicationPlan(t.Context(), personID)
	require.NoError(err)
	fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
	pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{
		Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence),
	})
	require.NoError(err)
	duplicated := strings.Replace(string(plan.OutgoingBody), "END:VCARD", "NOTE:Owned detail\r\nEND:VCARD", 1)
	err = st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{
		Publication: *pending,
		// Hold the caller's semantic hash equal to isolate the store's ownership
		// check: this regression must fail if rebinding is removed.
		Remote: store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: "person", RemoteETag: `"canonical"`, RemoteBody: []byte(duplicated), SemanticHash: pending.OutgoingSemanticHash},
	})
	require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
}
