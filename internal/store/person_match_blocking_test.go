package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonMatchBlockingSeedsMissingEmailPairOnlyOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "blocking-left", "Blocking Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "blocking-right", "Blocking Right")
	require.NoError(err)
	otherSource, err := st.GetOrCreateSource("beeper", "blocking-second")
	require.NoError(err)
	for _, item := range []struct{ participantID, sourceID int64 }{{left, fixture.Source.ID}, {right, otherSource.ID}} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', 'pair@example.test', 'pair@example.test', ?)`),
			item.participantID, item.sourceID, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}
	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 2)
	require.NoError(err)
	assert.Equal(1, created)
	candidates, err := st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(left, candidates[0].LeftID)
	assert.Equal(right, candidates[0].RightID)
	assert.Equal(store.ProvenanceArchiveObservation, candidates[0].Source)
	require.Len(candidates[0].Evidence, 1)
	assert.Equal(store.ProvenanceArchiveObservation, candidates[0].Evidence[0].Source)
	assert.Len(candidates[0].Evidence[0].SourceSupport, 2)
	for _, support := range candidates[0].Evidence[0].SourceSupport {
		assert.False(support.IsConservative)
	}
	for _, support := range candidates[0].SourceSupport {
		assert.False(support.IsConservative)
	}
	created, err = st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 2)
	require.NoError(err)
	assert.Zero(created)
}

func TestPersonMatchBlockingDeduplicatesPairsAndAddsCorroboration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "blocking-corroborated-left", "Corroborated Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "blocking-corroborated-right", "Corroborated Right")
	require.NoError(err)
	leftSource, err := st.GetOrCreateSource("beeper", "blocking-corroborated-left-source")
	require.NoError(err)
	rightSource, err := st.GetOrCreateSource("apple_id", "blocking-corroborated-right-source")
	require.NoError(err)
	for _, item := range []struct {
		participantID int64
		sourceID      int64
		address       string
	}{{left, leftSource.ID, "first@example.test"}, {left, leftSource.ID, "second@example.test"},
		{right, rightSource.ID, "first@example.test"}, {right, rightSource.ID, "second@example.test"}} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', ?, ?, ?)`),
			item.participantID, item.sourceID, item.address, item.address, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}

	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Equal(1, created)
	candidates, err := st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1, "the bounded pass should handle one distinct email match at a time")
	require.Len(candidates[0].Evidence, 1)
	assert.Len(candidates[0].Evidence[0].SourceSupport, 2, "both observation sources must be attached to the email evidence")
	assert.Len(candidates[0].SourceSupport, 2, "candidate support must include both observation sources")

	leftExtraSource, err := st.GetOrCreateSource("beeper", "blocking-corroborated-left-extra")
	require.NoError(err)
	rightExtraSource, err := st.GetOrCreateSource("apple_id", "blocking-corroborated-right-extra")
	require.NoError(err)
	for _, item := range []struct{ participantID, sourceID int64 }{{left, leftExtraSource.ID}, {right, rightExtraSource.ID}} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', 'first@example.test', 'first@example.test', ?)`),
			item.participantID, item.sourceID, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}
	created, err = st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Zero(created, "complete pair evidence must not be counted as newly created")
	candidates, err = st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Len(candidates[0].Evidence[0].SourceSupport, 4, "new source support must attach to an existing candidate")
	assert.Len(candidates[0].SourceSupport, 4)
	created, err = st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Equal(1, created, "the next distinct email pair must not be starved by already-supported observations")
	candidates, err = st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	assert.Len(candidates, 2)
}

func TestPersonMatchBlockingSkipsParticipantsAlreadyConnected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "connected-left", "Connected Left")
	require.NoError(err)
	middle, err := st.EnsureParticipantByIdentifier("apple_id", "connected-middle", "Connected Middle")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("signal", "connected-right", "Connected Right")
	require.NoError(err)
	unlinked, err := st.EnsureParticipantByIdentifier("telegram", "unlinked-peer", "Unlinked Peer")
	require.NoError(err)
	_, err = st.LinkParticipants(left, middle)
	require.NoError(err)
	_, err = st.LinkParticipants(middle, right)
	require.NoError(err)
	for _, participantID := range []int64{left, right, unlinked} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', 'connected@example.test', 'connected@example.test', ?)`),
			participantID, fixture.Source.ID, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}

	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Equal(1, created)
	candidates, err := st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(left, candidates[0].LeftID)
	assert.Equal(unlinked, candidates[0].RightID)
}

func TestPersonMatchBlockingSkipsHighFanoutEmails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	const highFanoutLimit = 20
	for i := range highFanoutLimit + 1 {
		participantID, err := st.EnsureParticipantByIdentifier("fixture", fmt.Sprintf("fanout-%d", i), "Fanout")
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', 'a-shared@example.test', 'a-shared@example.test', ?)`),
			participantID, fixture.Source.ID, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}

	left, err := st.EnsureParticipantByIdentifier("fixture", "bounded-left", "Bounded Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("fixture", "bounded-right", "Bounded Right")
	require.NoError(err)
	for _, participantID := range []int64{left, right} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', 'z-pair@example.test', 'z-pair@example.test', ?)`),
			participantID, fixture.Source.ID, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}

	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Equal(1, created)
	candidates, err := st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(left, candidates[0].LeftID)
	assert.Equal(right, candidates[0].RightID)
}

func TestPersonMatchBlockingSkipsArchiveObservationsWithoutSourceIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "unscoped-left", "Unscoped Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "unscoped-right", "Unscoped Right")
	require.NoError(err)
	for _, participantID := range []int64{left, right} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, NULL, 'email', 'unscoped@example.test', 'unscoped@example.test', ?)`),
			participantID, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}

	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Zero(created)
	candidates, err := st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	assert.Empty(candidates)
}

func TestPersonMatchBlockingDoesNotRelabelDeclaredObservationsAsArchiveEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "declared-left", "Declared Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "declared-right", "Declared Right")
	require.NoError(err)
	otherSource, err := st.GetOrCreateSource("apple_id", "declared-observation-source")
	require.NoError(err)
	for _, item := range []struct {
		participantID int64
		sourceID      int64
		provenance    store.Provenance
	}{
		{left, fixture.Source.ID, store.ProvenanceUser},
		{right, otherSource.ID, store.ProvenanceUser},
		{left, fixture.Source.ID, store.ProvenanceCardDAVImport},
		{right, otherSource.ID, store.ProvenanceCardDAVImport},
		{left, fixture.Source.ID, store.ProvenanceVCardImport},
		{right, otherSource.ID, store.ProvenanceVCardImport},
	} {
		address := string(item.provenance) + "@example.test"
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', ?, ?, ?)`),
			item.participantID, item.sourceID, address, address, item.provenance)
		require.NoError(err)
	}

	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 10)
	require.NoError(err)
	assert.Zero(created)
	candidates, err := st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	assert.Empty(candidates)
}
