package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func createParticipantMatchCandidate(
	t *testing.T,
	st *store.Store,
	leftID, rightID int64,
	confidence float64,
) *store.IdentityMatchCandidate {
	t.Helper()
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		t.Context(), store.IdentityMatchCandidateInput{
			LeftKind:   store.IdentityMatchParticipant,
			LeftID:     leftID,
			RightKind:  store.IdentityMatchParticipant,
			RightID:    rightID,
			Basis:      store.IdentityMatchDisplayName,
			State:      store.IdentityMatchStateCandidate,
			Confidence: &confidence,
			Source:     store.ProvenanceSystem,
		},
	)
	require.NoError(t, err)
	require.True(t, created)
	return candidate
}

func addMergeEvidence(
	t *testing.T, st *store.Store, candidateID int64, reference string,
) {
	t.Helper()
	_, err := st.AddIdentityMatchEvidenceContext(
		t.Context(), candidateID, store.IdentityMatchEvidenceInput{
			EvidenceKind: "synthetic_merge_evidence",
			EvidenceRef:  &reference,
			Source:       store.ProvenanceSystem,
		},
	)
	require.NoError(t, err)
}

func TestMergeParticipantsRewritesCandidatesAndDropsSelfLinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("third@example.com", "Third", "example.com")

	rewritten := createParticipantMatchCandidate(t, st, absorbed, third, 0.55)
	createParticipantMatchCandidate(t, st, absorbed, survivor, 0.45)

	require.NoError(st.MergeParticipants(absorbed, survivor))

	candidates, err := st.ListIdentityMatchCandidatesContext(
		t.Context(), nil, 100, 0,
	)
	require.NoError(err)
	require.Len(candidates, 1, "the merge-created self-link must be removed")
	assert.Equal(rewritten.ID, candidates[0].ID)
	assert.ElementsMatch(
		[]int64{survivor, third},
		[]int64{candidates[0].LeftID, candidates[0].RightID},
	)
}

func TestMergeParticipantsCollapsesCandidateEvidenceAndDecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("third@example.com", "Third", "example.com")

	absorbedCandidate := createParticipantMatchCandidate(t, st, absorbed, third, 0.85)
	survivorCandidate := createParticipantMatchCandidate(t, st, survivor, third, 0.40)
	absorbedNote := "accepted before participant merge"
	accepted, err := st.DecideIdentityMatchCandidateContext(
		t.Context(), absorbedCandidate.ID, store.IdentityMatchStateAccepted,
		"user", &absorbedNote,
	)
	require.NoError(err)
	addMergeEvidence(t, st, absorbedCandidate.ID, "absorbed-evidence")
	addMergeEvidence(t, st, survivorCandidate.ID, "survivor-evidence")

	require.NoError(st.MergeParticipants(absorbed, survivor))

	candidates, err := st.ListIdentityMatchCandidatesContext(
		t.Context(), nil, 100, 0,
	)
	require.NoError(err)
	require.Len(candidates, 1)
	merged := candidates[0]
	assert.Equal(absorbedCandidate.ID, merged.ID, "the lower stable ID must survive")
	assert.Equal(store.IdentityMatchStateAccepted, merged.State)
	assert.Equal(accepted.DecidedBy, merged.DecidedBy)
	assert.Equal(accepted.Notes, merged.Notes)
	require.NotNil(merged.Confidence)
	assert.InDelta(0.85, *merged.Confidence, 0)
	require.Len(merged.Evidence, 2)
	assert.ElementsMatch(
		[]string{"absorbed-evidence", "survivor-evidence"},
		[]string{*merged.Evidence[0].EvidenceRef, *merged.Evidence[1].EvidenceRef},
	)
}

func TestMergeParticipantsMarksOpposingCandidateDecisionsConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("third@example.com", "Third", "example.com")

	absorbedCandidate := createParticipantMatchCandidate(t, st, absorbed, third, 0.60)
	survivorCandidate := createParticipantMatchCandidate(t, st, survivor, third, 0.70)
	_, err := st.DecideIdentityMatchCandidateContext(
		t.Context(), absorbedCandidate.ID, store.IdentityMatchStateAccepted,
		"user", nil,
	)
	require.NoError(err)
	_, err = st.DecideIdentityMatchCandidateContext(
		t.Context(), survivorCandidate.ID, store.IdentityMatchStateRejected,
		"user", nil,
	)
	require.NoError(err)

	require.NoError(st.MergeParticipants(absorbed, survivor))

	candidates, err := st.ListIdentityMatchCandidatesContext(
		t.Context(), nil, 100, 0,
	)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(store.IdentityMatchStateConflict, candidates[0].State)
	require.NotNil(candidates[0].DecidedBy)
	assert.Equal("system", *candidates[0].DecidedBy)
	require.NotNil(candidates[0].DecidedAt)
}

func TestMergeParticipantsPreservesAppliedAcceptedCandidate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("applied-absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("applied-survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("applied-third@example.com", "Third", "example.com")

	absorbedCandidate := createParticipantMatchCandidate(t, st, absorbed, third, 0.60)
	survivorCandidate := createParticipantMatchCandidate(t, st, survivor, third, 0.70)
	accepted, _, err := st.AcceptIdentityMatchCandidateContext(
		t.Context(), absorbedCandidate.ID, "user", new("confirmed before merge"))
	requirements.NoError(err, "accept and apply absorbed candidate")
	_, err = st.DecideIdentityMatchCandidateContext(
		t.Context(), survivorCandidate.ID, store.IdentityMatchStateRejected,
		"user", new("rejected duplicate"))
	requirements.NoError(err, "reject survivor candidate")
	requirements.True(linkedPair(t, st, absorbed, third),
		"precondition: accepted candidate created the participant link")

	requirements.NoError(st.MergeParticipants(absorbed, survivor))

	candidates, err := st.ListIdentityMatchCandidatesContext(
		t.Context(), nil, 100, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	merged := candidates[0]
	assertions.Equal(store.IdentityMatchStateAccepted, merged.State,
		"candidate state must agree with the retained participant link")
	assertions.Equal(accepted.DecidedBy, merged.DecidedBy)
	assertions.Equal(accepted.Notes, merged.Notes)
	assertions.True(linkedPair(t, st, survivor, third),
		"participant merge must retain the applied identity relation")
}

func TestMergeParticipantsPrefersUserAcceptanceMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant(
		"decision-absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant(
		"decision-survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant(
		"decision-third@example.com", "Third", "example.com")

	olderSystem := upsertPairCandidate(
		t, st, absorbed, third, store.IdentityMatchStableProviderID)
	newerUser := upsertPairCandidate(
		t, st, survivor, third, store.IdentityMatchStableProviderID)
	_, _, err := st.AcceptIdentityMatchCandidateContext(
		t.Context(), olderSystem.ID, "system", new("automatic match"))
	require.NoError(err)
	userNote := "confirmed by profile owner"
	userAccepted, _, err := st.AcceptIdentityMatchCandidateContext(
		t.Context(), newerUser.ID, string(store.ProvenanceUser), &userNote)
	require.NoError(err)

	require.NoError(st.MergeParticipants(absorbed, survivor))
	candidates, err := st.ListIdentityMatchCandidatesContext(
		t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	merged := candidates[0]
	assert.Equal(store.IdentityMatchStateAccepted, merged.State)
	assert.Equal(userAccepted.DecidedBy, merged.DecidedBy,
		"an explicit user acceptance must outrank older system metadata")
	assert.Equal(userAccepted.Notes, merged.Notes)
}

func TestMergeParticipantsTransfersCollapsedSystemMatchOwnership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("owned-absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("owned-survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("owned-third@example.com", "Third", "example.com")

	absorbedCandidate := upsertPairCandidate(
		t, st, absorbed, third, store.IdentityMatchStableProviderID,
	)
	survivorCandidate := upsertPairCandidate(
		t, st, survivor, third, store.IdentityMatchStableProviderID,
	)
	_, _, err := st.AcceptIdentityMatchCandidateContext(
		t.Context(), absorbedCandidate.ID, "system", nil,
	)
	require.NoError(err)
	_, _, err = st.AcceptIdentityMatchCandidateContext(
		t.Context(), survivorCandidate.ID, "system", nil,
	)
	require.NoError(err)

	require.NoError(st.MergeParticipants(absorbed, survivor))
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	merged := candidates[0]
	assert.Equal(absorbedCandidate.ID, merged.ID)
	assert.Equal(store.IdentityMatchStateAccepted, merged.State)
	var owner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identity_match_candidate_id FROM participant_links
		 WHERE participant_a = ? AND participant_b = ?`),
		minInt64(survivor, third), maxInt64(survivor, third),
	).Scan(&owner))
	assert.Equal(merged.ID, owner)

	_, err = st.DecideIdentityMatchCandidateContext(
		t.Context(), merged.ID, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	assert.False(linkedPair(t, st, survivor, third))
}

func TestMergeParticipantsPreservesManualSupportWhenEdgesCollapse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("manual-absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("manual-survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("manual-third@example.com", "Third", "example.com")

	_, err := st.LinkParticipants(absorbed, third)
	require.NoError(err)
	candidate := upsertPairCandidate(
		t, st, survivor, third, store.IdentityMatchStableProviderID,
	)
	_, _, err = st.AcceptIdentityMatchCandidateContext(
		t.Context(), candidate.ID, "system", nil,
	)
	require.NoError(err)

	require.NoError(st.MergeParticipants(absorbed, survivor))
	var owner *int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identity_match_candidate_id FROM participant_links
		 WHERE participant_a = ? AND participant_b = ?`),
		minInt64(survivor, third), maxInt64(survivor, third),
	).Scan(&owner))
	assert.Nil(owner, "manual support must make the collapsed edge unowned")

	_, err = st.DecideIdentityMatchCandidateContext(
		t.Context(), candidate.ID, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	assert.True(linkedPair(t, st, survivor, third),
		"rejecting the automated explanation must preserve manual support")
}

func TestMergeParticipantsRetainsDeterministicOwnerForCollapsedAutomatedEdges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("multiple-absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("multiple-survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("multiple-third@example.com", "Third", "example.com")

	createAccepted := func(left int64, providerID string) *store.IdentityMatchCandidate {
		candidate, created, err := st.UpsertIdentityMatchCandidateContext(
			t.Context(), store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: left,
				RightKind: store.IdentityMatchParticipant, RightID: third,
				Basis: store.IdentityMatchStableProviderID, NormalizedValue: &providerID,
				State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
			},
		)
		require.NoError(err)
		require.True(created)
		_, _, err = st.AcceptIdentityMatchCandidateContext(
			t.Context(), candidate.ID, "system", nil,
		)
		require.NoError(err)
		return candidate
	}
	first := createAccepted(absorbed, "provider-first")
	second := createAccepted(survivor, "provider-second")

	require.NoError(st.MergeParticipants(absorbed, survivor))
	var owner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identity_match_candidate_id FROM participant_links
		 WHERE participant_a = ? AND participant_b = ?`),
		minInt64(survivor, third), maxInt64(survivor, third),
	).Scan(&owner))
	assert.Equal(minInt64(first.ID, second.ID), owner)

	_, err := st.DecideIdentityMatchCandidateContext(
		t.Context(), owner, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	assert.True(linkedPair(t, st, survivor, third),
		"the other accepted candidate must take ownership")
	remainingOwner := maxInt64(first.ID, second.ID)
	_, err = st.DecideIdentityMatchCandidateContext(
		t.Context(), remainingOwner, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	assert.False(linkedPair(t, st, survivor, third),
		"rejecting every automated contribution must remove the link")
}

func TestMergeParticipantsPreservesAutomatedChainEdgeProvenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("chain-a@example.com", "A", "example.com")
	b := f.EnsureParticipant("chain-b@example.com", "B", "example.com")
	c := f.EnsureParticipant("chain-c@example.com", "C", "example.com")
	d := f.EnsureParticipant("chain-d@example.com", "D", "example.com")

	var tailCandidate *store.IdentityMatchCandidate
	for index, pair := range [][2]int64{{a, b}, {b, c}, {c, d}} {
		candidate := upsertPairCandidate(
			t, st, pair[0], pair[1], store.IdentityMatchStableProviderID,
		)
		_, _, err := st.AcceptIdentityMatchCandidateContext(
			t.Context(), candidate.ID, "system", nil,
		)
		require.NoError(err)
		if index == 2 {
			tailCandidate = candidate
		}
	}
	require.NotNil(tailCandidate)

	require.NoError(st.MergeParticipants(c, b))
	assert.True(linkedPair(t, st, a, d),
		"contracting the middle of the chain must preserve the cluster")

	_, err := st.DecideIdentityMatchCandidateContext(
		t.Context(), tailCandidate.ID, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	assert.False(linkedPair(t, st, a, d),
		"rejecting the surviving tail contribution must detach the tail")
}

func TestMergeParticipantsPreservesAcceptedCandidateLinkedByContraction(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("contracted-absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("contracted-survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("contracted-third@example.com", "Third", "example.com")

	_, err := st.LinkParticipants(absorbed, third)
	requirements.NoError(err, "create the identity edge that the merge will contract")
	absorbedCandidate := createParticipantMatchCandidate(t, st, absorbed, third, 0.60)
	survivorCandidate := createParticipantMatchCandidate(t, st, survivor, third, 0.70)
	_, err = st.DecideIdentityMatchCandidateContext(
		t.Context(), absorbedCandidate.ID, store.IdentityMatchStateRejected,
		"user", new("rejected duplicate"))
	requirements.NoError(err, "reject absorbed candidate")
	accepted, err := st.DecideIdentityMatchCandidateContext(
		t.Context(), survivorCandidate.ID, store.IdentityMatchStateAccepted,
		"user", new("confirmed survivor candidate"))
	requirements.NoError(err, "accept survivor candidate without applying it")
	requirements.False(linkedPair(t, st, survivor, third),
		"precondition: the survivor pair is linked only after contraction")

	requirements.NoError(st.MergeParticipants(absorbed, survivor))

	candidates, err := st.ListIdentityMatchCandidatesContext(
		t.Context(), nil, 100, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	merged := candidates[0]
	assertions.Equal(store.IdentityMatchStateAccepted, merged.State,
		"post-merge connectivity must preserve the accepted decision")
	assertions.Equal(accepted.DecidedBy, merged.DecidedBy)
	assertions.Equal(accepted.Notes, merged.Notes)
	assertions.True(linkedPair(t, st, survivor, third))
}

func TestMergeParticipantsCarriesConfidenceProvenanceFromDuplicate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("third@example.com", "Third", "example.com")

	userCandidate, created, err := st.UpsertIdentityMatchCandidateContext(
		t.Context(), store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: absorbed,
			RightKind: store.IdentityMatchParticipant, RightID: third,
			Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate,
			Source: store.ProvenanceUser,
		},
	)
	require.NoError(err)
	require.True(created)
	confidence := 0.90
	_, created, err = st.UpsertIdentityMatchCandidateContext(
		t.Context(), store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: survivor,
			RightKind: store.IdentityMatchParticipant, RightID: third,
			Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate,
			Confidence: &confidence, Source: store.ProvenanceSystem,
		},
	)
	require.NoError(err)
	require.True(created)

	require.NoError(st.MergeParticipants(absorbed, survivor))
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(userCandidate.ID, candidates[0].ID)
	assert.Equal(store.ProvenanceSystem, candidates[0].Source)
	require.NotNil(candidates[0].Confidence)
	assert.InDelta(confidence, *candidates[0].Confidence, 0)
}

func TestMergeParticipantsPreservesPromotedObservationConflictOrigin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	ctx := t.Context()
	absorbed := f.EnsureParticipant("promoted-absorbed@example.org", "Promoted Absorbed", "example.org")
	survivor := f.EnsureParticipant("generated-survivor@example.org", "Generated Survivor", "example.org")
	third := f.EnsureParticipant("merge-third@example.org", "Merge Third", "example.org")
	normalized := "merge-shared@example.org"

	input := store.ParticipantContactObservationInput{
		SourceID: &f.Source.ID, AddressKind: store.ContactAddressEmail,
		OriginalValue: normalized,
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceArchiveObservation,
		},
	}
	input.ProviderUserID = new("generated-survivor-provider")
	_, err := st.RecordContactObservationContext(ctx, survivor, input)
	require.NoError(err)
	input.ProviderUserID = new("third-provider")
	_, err = st.RecordContactObservationContext(ctx, third, input)
	require.NoError(err)

	confidence := 0.8
	note := "preserve promoted merge review"
	promoted, created, err := st.UpsertIdentityMatchCandidateContext(
		ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: absorbed,
			RightKind: store.IdentityMatchParticipant, RightID: third,
			Basis: store.IdentityMatchEmail, NormalizedValue: &normalized,
			State: store.IdentityMatchStateCandidate, Confidence: &confidence,
			Source: store.ProvenanceSystem, Notes: &note,
		},
	)
	require.NoError(err)
	require.True(created)
	addMergeEvidence(t, st, promoted.ID, "promoted-merge-evidence")
	input.ProviderUserID = new("promoted-absorbed-provider")
	_, err = st.RecordContactObservationContext(ctx, absorbed, input)
	require.NoError(err)

	require.NoError(st.MergeParticipants(absorbed, survivor))
	require.NoError(st.RemoveSource(f.Source.ID))
	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(store.IdentityMatchStateCandidate, candidates[0].State)
	assert.Equal(store.ProvenanceSystem, candidates[0].Source)
	assert.Equal(&note, candidates[0].Notes)
	require.Len(candidates[0].Evidence, 1)
	assert.Equal("promoted-merge-evidence", *candidates[0].Evidence[0].EvidenceRef)
}

func TestMergeParticipantsPreservesDecisionMetadataWhenConflictWins(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	ctx := t.Context()
	absorbed := f.EnsureParticipant("decided-absorbed@example.org", "Decided Absorbed", "example.org")
	survivor := f.EnsureParticipant("conflict-survivor@example.org", "Conflict Survivor", "example.org")
	third := f.EnsureParticipant("decided-third@example.org", "Decided Third", "example.org")
	normalized := "decision-shared@example.org"

	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: absorbed,
			RightKind: store.IdentityMatchParticipant, RightID: third,
			Basis: store.IdentityMatchEmail, NormalizedValue: &normalized,
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser,
		},
	)
	require.NoError(err)
	require.True(created)
	note := "accepted after manual review"
	accepted, err := st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "user", &note,
	)
	require.NoError(err)

	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: normalized,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	input.ProviderUserID = new("survivor-provider")
	_, err = st.RecordContactObservationContext(ctx, survivor, input)
	require.NoError(err)
	input.ProviderUserID = new("third-provider")
	conflicting, err := st.RecordContactObservationContext(ctx, third, input)
	require.NoError(err)
	require.True(conflicting.Conflicting)

	require.NoError(st.MergeParticipants(absorbed, survivor))

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	merged := candidates[0]
	assert.Equal(store.IdentityMatchStateConflict, merged.State)
	require.NotNil(merged.DecidedBy, "the user decision must survive the conflict collapse")
	assert.Equal("user", *merged.DecidedBy)
	assert.Equal(accepted.DecidedAt, merged.DecidedAt)
	require.NotNil(merged.Notes)
	assert.Equal(note, *merged.Notes)

	// Once the observation pair no longer conflicts, cleanup must restore
	// the accepted decision rather than demote to an undecided candidate.
	observations, err := st.ListParticipantObservationsContext(ctx, survivor, true)
	require.NoError(err)
	require.Len(observations, 1)
	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, survivor, observations[0].Envelope.ID, nil,
	))
	candidates, err = st.ListIdentityMatchCandidatesContext(ctx, nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	restored := candidates[0]
	assert.Equal(store.IdentityMatchStateAccepted, restored.State,
		"cleanup must restore the pre-conflict accepted decision")
	require.NotNil(restored.DecidedBy)
	assert.Equal("user", *restored.DecidedBy)
	require.NotNil(restored.Notes)
	assert.Equal(note, *restored.Notes)
}

func TestMergeParticipantsPreservesManualConflictAfterObservationCleanup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	ctx := t.Context()
	absorbed := f.EnsureParticipant("manual-absorbed@example.org", "Manual Absorbed", "example.org")
	survivor := f.EnsureParticipant("generated-survivor@example.org", "Generated Survivor", "example.org")
	third := f.EnsureParticipant("manual-third@example.org", "Manual Third", "example.org")
	normalized := "manual-shared@example.org"
	note := "manual conflict must remain reviewable"

	manual, created, err := st.UpsertIdentityMatchCandidateContext(
		ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: absorbed,
			RightKind: store.IdentityMatchParticipant, RightID: third,
			Basis: store.IdentityMatchEmail, NormalizedValue: &normalized,
			State: store.IdentityMatchStateConflict, Source: store.ProvenanceUser,
			Notes: &note,
		},
	)
	require.NoError(err)
	require.True(created)

	input := store.ParticipantContactObservationInput{
		SourceID: &f.Source.ID, AddressKind: store.ContactAddressEmail,
		OriginalValue: normalized,
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceArchiveObservation,
		},
	}
	input.ProviderUserID = new("generated-survivor-provider")
	_, err = st.RecordContactObservationContext(ctx, survivor, input)
	require.NoError(err)
	input.ProviderUserID = new("generated-third-provider")
	result, err := st.RecordContactObservationContext(ctx, third, input)
	require.NoError(err)
	require.True(result.Conflicting)

	require.NoError(st.MergeParticipants(absorbed, survivor))
	require.NoError(st.RemoveSource(f.Source.ID))
	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(manual.ID, candidates[0].ID)
	assert.Equal(store.IdentityMatchStateConflict, candidates[0].State)
	assert.Equal(store.ProvenanceUser, candidates[0].Source)
	assert.Equal(&note, candidates[0].Notes)
}

func TestMergeParticipantsKeepsCandidatesForDistinctNormalizedValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("third@example.com", "Third", "example.com")

	for participantID, value := range map[int64]string{
		absorbed: "first@example.org",
		survivor: "second@example.org",
	} {
		candidate, created, err := st.UpsertIdentityMatchCandidateContext(
			t.Context(), store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: participantID,
				RightKind: store.IdentityMatchParticipant, RightID: third,
				Basis: store.IdentityMatchEmail, NormalizedValue: &value,
				State:  store.IdentityMatchStateCandidate,
				Source: store.ProvenanceArchiveObservation,
			},
		)
		require.NoError(err)
		require.True(created)
		require.NotZero(candidate.ID)
	}

	require.NoError(st.MergeParticipants(absorbed, survivor))
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.ElementsMatch(
		[]string{"first@example.org", "second@example.org"},
		[]string{*candidates[0].NormalizedValue, *candidates[1].NormalizedValue},
	)
}

func TestMergeParticipantsRollsBackWhenCandidateRewriteFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	absorbed := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	survivor := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	third := f.EnsureParticipant("third@example.com", "Third", "example.com")
	createParticipantMatchCandidate(t, st, absorbed, third, 0.50)

	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(context.Background(), `
			CREATE FUNCTION fail_candidate_merge() RETURNS trigger AS $$
			BEGIN
				RAISE EXCEPTION 'forced candidate merge failure';
			END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER fail_candidate_merge
			BEFORE UPDATE OF left_id, right_id ON identity_match_candidates
			FOR EACH ROW EXECUTE FUNCTION fail_candidate_merge();
		`)
		require.NoError(err)
	} else {
		_, err := st.DB().ExecContext(context.Background(), `
			CREATE TRIGGER fail_candidate_merge
			BEFORE UPDATE OF left_id, right_id ON identity_match_candidates
			BEGIN
				SELECT RAISE(ABORT, 'forced candidate merge failure');
			END;
		`)
		require.NoError(err)
	}

	err := st.MergeParticipants(absorbed, survivor)
	require.Error(err)
	assert.Contains(err.Error(), "forced candidate merge failure")

	var participantCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM participants WHERE id = ?`), absorbed,
	).Scan(&participantCount))
	assert.Equal(1, participantCount, "the absorbed participant delete must roll back")

	candidates, listErr := st.ListIdentityMatchCandidatesContext(
		t.Context(), nil, 100, 0,
	)
	require.NoError(listErr)
	require.Len(candidates, 1)
	assert.ElementsMatch(
		[]int64{absorbed, third},
		[]int64{candidates[0].LeftID, candidates[0].RightID},
		"candidate endpoints must roll back",
	)
}
