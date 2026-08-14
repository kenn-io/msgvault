package store_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAddIdentityMatchEvidenceConcurrentCallsConverge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	ctx := t.Context()
	secondSource, err := st.GetOrCreateSource("beeper", "second-account")
	require.NoError(err)
	left, err := st.EnsureParticipantByIdentifier("beeper", "evidence-left", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "evidence-right", "Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchStableProviderID, NormalizedValue: new("provider-user"),
		State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)

	type result struct {
		evidence *store.IdentityMatchEvidence
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, sourceID := range []int64{fixture.Source.ID, secondSource.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			evidence, addErr := st.AddIdentityMatchEvidenceContext(
				ctx, candidate.ID, store.IdentityMatchEvidenceInput{
					EvidenceKind: "stable_provider_id",
					Detail:       new("provider-user"),
					Source:       store.ProvenanceArchiveObservation,
					SourceID:     &sourceID,
				})
			results <- result{evidence: evidence, err: addErr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var evidenceIDs []int64
	for call := range results {
		require.NoError(call.err)
		require.NotNil(call.evidence)
		evidenceIDs = append(evidenceIDs, call.evidence.ID)
	}
	require.Len(evidenceIDs, 2)
	assert.Equal(evidenceIDs[0], evidenceIDs[1],
		"identical evidence writers should receive the same row")

	loaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Len(loaded.Evidence, 1)
	var supportCount int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT COUNT(*) FROM identity_match_evidence_sources WHERE evidence_id = ?`),
		evidenceIDs[0]).Scan(&supportCount))
	assert.Equal(2, supportCount,
		"the converged evidence row should retain both source supports")
}

func TestUsernameOnlyCandidateCannotBeAcceptedWithoutCorroboration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@bob:example.org", "Bob Example")
	require.NoError(err)
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchServiceScopeUsername, ServiceSlug: new("x"),
		NormalizedValue: new("shared"), State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	assert.True(created)
	for _, kind := range []string{"phone", "display_name"} {
		_, err := st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
			EvidenceKind: kind, Source: store.ProvenanceArchiveObservation,
		})
		require.NoError(err)
	}
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil,
	)
	require.ErrorIs(err, store.ErrIdentityMatchNotAcceptable)
	accepted, err := st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "user", new("confirmed"),
	)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)
	assert.Len(accepted.Evidence, 2)
}

func TestStableProviderIDCandidateMayBeAcceptedBySystem(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@alice2:example.org", "Alice Example")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(context.Background(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchStableProviderID, NormalizedValue: new("beeper-user-1"),
		State:  store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	accepted, err := st.DecideIdentityMatchCandidateContext(
		context.Background(), candidate.ID, store.IdentityMatchStateAccepted, "system", nil,
	)
	require.NoError(err)
	assert.NotNil(accepted.DecidedAt)
}

func TestStableProviderIDCandidateWithoutRecordedValueRequiresUserAcceptance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@alice2:example.org", "Alice Example")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchStableProviderID, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil,
	)
	require.ErrorIs(err, store.ErrIdentityMatchNotAcceptable,
		"a stable-provider-id basis without the matched value must not be system-accepted")
	accepted, err := st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "user", nil,
	)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)
}

func TestUpsertIdentityMatchCandidateEnforcesServiceScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("example", "scope-left", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "scope-right", "Right")
	require.NoError(err)
	input := store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchServiceScopeUsername, ServiceSlug: new("slack"),
		NormalizedValue: new("alice"), State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	}
	_, _, err = st.UpsertIdentityMatchCandidateContext(ctx, input)
	require.ErrorIs(err, store.ErrServiceScopeRequired,
		"a required-scope service must not accept an unscoped candidate")

	input.CrossScope = true
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(ctx, input)
	require.NoError(err, "a cross-scope username candidate keeps the service namespace")
	assert.True(created)
	assert.Nil(candidate.ScopeKind)
	assert.Nil(candidate.ScopeValue)

	input.CrossScope = false
	input.ServiceSlug = nil
	input.ScopeKind = new("workspace")
	_, _, err = st.UpsertIdentityMatchCandidateContext(ctx, input)
	require.ErrorIs(err, store.ErrServiceScopeIncomplete,
		"a scope kind without a value must not fragment candidate keys")
}

func TestBlankNormalizedValueDoesNotSatisfySystemAcceptance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@alice2:example.org", "Alice Example")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchStableProviderID, NormalizedValue: new("  "),
		State:  store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	assert.Nil(candidate.NormalizedValue,
		"a blank normalized value must be stored as absent, not as an empty string")
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil,
	)
	require.ErrorIs(err, store.ErrIdentityMatchNotAcceptable,
		"a blank stable ID must not satisfy the non-user acceptance guard")
}

func TestSystemAcceptedLinkedIdentityMatchCanBeRejectedAndUnlinked(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@accepted-left:beeper.local", "Test User")
	require.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@accepted-right:beeper.local", "Test User")
	require.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	accepted, _, err := st.AcceptIdentityMatchCandidateContext(
		ctx, candidate.ID, "system", new("stable provider ID"))
	require.NoError(err, "accept and link candidate")
	var owner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identity_match_candidate_id FROM participant_links
		 WHERE participant_a = ? AND participant_b = ?`),
		minInt64(left, right), maxInt64(left, right)).Scan(&owner),
		"read automated link ownership")
	assert.Equal(candidate.ID, owner)

	rejected, err := st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user", new("not the same person"))
	require.NoError(err, "a user can withdraw an automated match")
	assert.Equal(store.IdentityMatchStateRejected, rejected.State)

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload candidate")
	assert.Equal(store.IdentityMatchStateRejected, reloaded.State)
	assert.Equal("user", *reloaded.DecidedBy)
	assert.NotEqual(accepted.Notes, reloaded.Notes)
	assert.False(linkedPair(t, st, left, right),
		"rejecting an automated match must remove its direct participant link")
}

func TestManualLinkConfirmationDetachesSystemCandidateOwnership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@confirm-left:beeper.local", "Test User")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@confirm-right:beeper.local", "Test User")
	require.NoError(err)
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "system acceptance")

	_, err = st.LinkParticipants(left, right)
	require.NoError(err, "manual confirmation of existing link")
	var owner sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identity_match_candidate_id FROM participant_links
		 WHERE participant_a = ? AND participant_b = ?`),
		minInt64(left, right), maxInt64(left, right)).Scan(&owner))
	assert.False(owner.Valid, "manual confirmation must detach automated ownership")

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err, "reject candidate after manual confirmation")
	assert.True(linkedPair(t, st, left, right),
		"rejecting the old automated explanation must preserve the confirmed link")
}

func TestRejectingOwnedSystemMatchTransfersEdgeToAnotherAcceptedCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@shared-left:beeper.local", "Test User")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@shared-right:beeper.local", "Test User")
	require.NoError(err)
	first := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	secondValue := "beeper:another-stable-id"
	second, created, err := st.UpsertIdentityMatchCandidateContext(ctx,
		store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis:           store.IdentityMatchStableProviderID,
			NormalizedValue: &secondValue,
			State:           store.IdentityMatchStateCandidate,
			Source:          store.ProvenanceArchiveObservation,
		})
	require.NoError(err)
	require.True(created)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, first.ID, "system", nil)
	require.NoError(err, "accept first candidate")
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, second.ID, "system", nil)
	require.NoError(err, "accept second candidate")

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, first.ID, store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err, "reject original owner")
	var owner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identity_match_candidate_id FROM participant_links
		 WHERE participant_a = ? AND participant_b = ?`),
		minInt64(left, right), maxInt64(left, right)).Scan(&owner))
	assert.Equal(second.ID, owner, "the other accepted candidate must inherit ownership")
	assert.True(linkedPair(t, st, left, right))

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, second.ID, store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err, "reject final supporting candidate")
	assert.False(linkedPair(t, st, left, right),
		"the edge can be removed once no accepted candidate supports it")
}

func TestRejectingOwnedSystemMatchReappliesAcceptedCrossComponentCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	a, err := st.EnsureParticipantByIdentifier(
		"beeper", "@cross-a:beeper.local", "Cross A")
	require.NoError(err)
	b, err := st.EnsureParticipantByIdentifier(
		"beeper", "@cross-b:beeper.local", "Cross B")
	require.NoError(err)
	c, err := st.EnsureParticipantByIdentifier(
		"beeper", "@cross-c:beeper.local", "Cross C")
	require.NoError(err)

	ab := upsertPairCandidate(t, st, a, b, store.IdentityMatchStableProviderID)
	ac := upsertPairCandidate(t, st, a, c, store.IdentityMatchStableProviderID)
	bc := upsertPairCandidate(t, st, b, c, store.IdentityMatchStableProviderID)
	for _, candidate := range []*store.IdentityMatchCandidate{ab, ac, bc} {
		_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
		require.NoError(err)
	}

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, ab.ID, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	assert.True(linkedPair(t, st, a, b),
		"the remaining accepted B-C assertion must reconnect the split")

	var owner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identity_match_candidate_id FROM participant_links
		 WHERE participant_a = ? AND participant_b = ?`),
		minInt64(b, c), maxInt64(b, c)).Scan(&owner))
	assert.Equal(bc.ID, owner)

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, bc.ID, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	assert.False(linkedPair(t, st, a, b),
		"the reapplied assertion must retain ownership for later rejection")
	assert.True(linkedPair(t, st, a, c))
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func TestRejectingSystemMatchPreservesPreexistingManualLink(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@manual-left:beeper.local", "Test User")
	require.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@manual-right:beeper.local", "Test User")
	require.NoError(err, "ensure right")
	_, err = st.LinkParticipants(left, right)
	require.NoError(err, "create manual link")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "system acceptance of already-linked pair")

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err, "user rejection")
	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload candidate")
	assert.Equal(store.IdentityMatchStateRejected, reloaded.State)
	assert.True(linkedPair(t, st, left, right),
		"rejecting an automated explanation must not remove a pre-existing manual edge")
}

func TestRejectingSystemMatchRemovesItsOwnedEdgeAfterParticipantMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	absorbed, err := st.EnsureParticipantByIdentifier(
		"beeper", "@owned-absorbed:beeper.local", "Test User")
	require.NoError(err, "ensure absorbed")
	survivor, err := st.EnsureParticipantByIdentifier(
		"beeper", "@owned-survivor:beeper.local", "Test User")
	require.NoError(err, "ensure survivor")
	other, err := st.EnsureParticipantByIdentifier(
		"beeper", "@owned-other:beeper.local", "Test User")
	require.NoError(err, "ensure other")
	candidate := upsertPairCandidate(t, st, absorbed, other,
		store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "system acceptance")
	require.NoError(st.MergeParticipants(absorbed, survivor), "merge participant")

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload rewritten candidate")
	assert.Equal(survivor, reloaded.LeftID)
	assert.Equal(other, reloaded.RightID)
	var owner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identity_match_candidate_id FROM participant_links
		 WHERE participant_a = ? AND participant_b = ?`),
		minInt64(survivor, other), maxInt64(survivor, other)).Scan(&owner),
		"read rewritten automated link ownership")
	assert.Equal(candidate.ID, owner,
		"participant merge must preserve the candidate that owns the rewritten edge")

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err, "user rejection")
	assert.False(linkedPair(t, st, survivor, other),
		"rejection must remove the owned edge after endpoint rewrite")
}

func TestUserAcceptedIdentityMatchRemainsProtectedFromRejection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@user-accepted-left:beeper.local", "Test User")
	require.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@user-accepted-right:beeper.local", "Test User")
	require.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	accepted, _, err := st.AcceptIdentityMatchCandidateContext(
		ctx, candidate.ID, "user", new("confirmed by user"))
	require.NoError(err, "user acceptance")

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user", new("rejected later"))
	require.ErrorIs(err, store.ErrIdentityMatchAlreadyAccepted)
	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload candidate")
	assert.Equal(store.IdentityMatchStateAccepted, reloaded.State)
	assert.Equal(accepted.DecidedBy, reloaded.DecidedBy)
	assert.True(linkedPair(t, st, left, right),
		"a user-accepted link must remain protected")
}

func TestRejectedSystemMatchCannotBeReplayedBySystem(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@rejected-left:beeper.local", "Test User")
	require.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@rejected-right:beeper.local", "Test User")
	require.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "system acceptance")
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err, "user rejection")

	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.ErrorIs(err, store.ErrIdentityMatchRejected,
		"a stale system importer must not revive a user rejection")
	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload candidate")
	assert.Equal(store.IdentityMatchStateRejected, reloaded.State)
	assert.False(linkedPair(t, st, left, right))
}

func TestAppliedIdentityMatchCannotBecomeConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@applied-left:beeper.local", "Test User")
	require.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@applied-right:beeper.local", "Test User")
	require.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	accepted, _, err := st.AcceptIdentityMatchCandidateContext(
		ctx, candidate.ID, "system", new("stable provider ID"))
	require.NoError(err, "accept and link candidate")

	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateConflict, "system",
		new("late conflicting evidence"))
	require.ErrorIs(err, store.ErrIdentityMatchAlreadyApplied,
		"an applied match must not become a conflict")

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload candidate")
	assert.Equal(store.IdentityMatchStateAccepted, reloaded.State)
	assert.Equal(accepted.DecidedBy, reloaded.DecidedBy)
	assert.Equal(accepted.Notes, reloaded.Notes)
	assert.True(linkedPair(t, st, left, right))
}

func TestUpsertIdentityMatchCandidateRejectsDecisionStates(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("example", "left", "Left User")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right", "Right User")
	require.NoError(err)

	for _, state := range []store.IdentityMatchState{
		store.IdentityMatchStateAccepted,
		store.IdentityMatchStateRejected,
	} {
		_, created, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchStableProviderID, State: state,
			Source: store.ProvenanceArchiveObservation,
		})
		require.ErrorIs(err, store.ErrInvalidIdentityMatchState, state)
		require.False(created, state)
	}

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Empty(candidates, "decision states must not create candidates directly")
}

func TestRejectedCandidateIsRetainedAndEndpointsCanonical(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@bob:example.org", "Bob Example")
	require.NoError(err)
	input := store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	}
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, input)
	require.NoError(err)
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	input.LeftID, input.RightID = right, left
	again, created, err := st.UpsertIdentityMatchCandidateContext(ctx, input)
	require.NoError(err)
	assert.False(created)
	assert.Equal(store.IdentityMatchStateRejected, again.State)
	_, _, err = st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: left,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.ErrorIs(err, store.ErrIdentityMatchSelfLink)
}

func TestObservationConflictPromotesNeutralCandidateAndPreservesReviewProvenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "left-promotion", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right-promotion", "Right")
	require.NoError(err)
	normalized := "shared@example.org"
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchEmail, NormalizedValue: &normalized,
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceSystem,
		},
	)
	require.NoError(err)
	require.True(created)
	note := "keep this review note"
	candidate, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateCandidate, "user", &note,
	)
	require.NoError(err)
	require.NotNil(candidate.DecidedAt)

	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: normalized,
		ProviderUserID: new("provider-left"),
		Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	_, err = st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.ProviderUserID = new("provider-right")
	result, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	require.True(result.Conflicting)
	require.NotNil(result.CandidateID)
	assert.Equal(candidate.ID, *result.CandidateID)

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	promoted := candidates[0]
	assert.Equal(candidate.ID, promoted.ID)
	assert.Equal(store.IdentityMatchStateConflict, promoted.State)
	assert.Equal(store.ProvenanceSystem, promoted.Source)
	assert.Equal(candidate.DecidedBy, promoted.DecidedBy)
	assert.Equal(candidate.DecidedAt, promoted.DecidedAt)
	assert.Equal(candidate.Notes, promoted.Notes)
}

func TestIdentityMatchCandidatesKeepDistinctNormalizedValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "left-value", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right-value", "Right")
	require.NoError(err)

	var ids []int64
	for _, value := range []string{"first@example.org", "second@example.org"} {
		candidate, created, err := st.UpsertIdentityMatchCandidateContext(
			ctx, store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: left,
				RightKind: store.IdentityMatchParticipant, RightID: right,
				Basis: store.IdentityMatchEmail, NormalizedValue: &value,
				State:  store.IdentityMatchStateCandidate,
				Source: store.ProvenanceArchiveObservation,
			},
		)
		require.NoError(err)
		assert.True(created, value)
		ids = append(ids, candidate.ID)
	}

	assert.NotEqual(ids[0], ids[1])
	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.ElementsMatch(
		[]string{"first@example.org", "second@example.org"},
		[]string{*candidates[0].NormalizedValue, *candidates[1].NormalizedValue},
	)
}

func TestIdentityMatchCandidateRequiresExistingEndpoints(t *testing.T) {
	requirements := require.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@candidate-owner:example.org", "Candidate Owner",
	)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	point, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "owner@example.org",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	observation, err := st.RecordContactObservationContext(
		ctx, participantID, store.ParticipantContactObservationInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "observed@example.org",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		},
	)
	requirements.NoError(err)

	for _, test := range []struct {
		name string
		kind store.IdentityMatchEndpointKind
		id   int64
	}{
		{name: "participant", kind: store.IdentityMatchParticipant, id: participantID},
		{name: "person", kind: store.IdentityMatchPerson, id: person.ID},
		{name: "observation", kind: store.IdentityMatchObservation,
			id: observation.Observation.Envelope.ID},
		{name: "contact point", kind: store.IdentityMatchContactPoint,
			id: point.Envelope.ID},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			_, created, err := st.UpsertIdentityMatchCandidateContext(
				ctx, store.IdentityMatchCandidateInput{
					LeftKind: test.kind, LeftID: test.id + 1_000_000,
					RightKind: store.IdentityMatchParticipant, RightID: participantID,
					Basis:  store.IdentityMatchDisplayName,
					State:  store.IdentityMatchStateCandidate,
					Source: store.ProvenanceSystem,
				},
			)
			require.ErrorIs(err, store.ErrIdentityMatchEndpointNotFound)
			require.False(created)
		})
	}
}

func TestDeletePersonRemovesCandidatesForDeletedProfileEndpoints(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	owner, err := st.EnsureParticipantByIdentifier("example", "owner", "Owner")
	require.NoError(err)
	other, err := st.EnsureParticipantByIdentifier("example", "other", "Other")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(owner)
	require.NoError(err)
	point, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "owner@example.org",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	for _, endpoint := range []struct {
		kind store.IdentityMatchEndpointKind
		id   int64
	}{
		{kind: store.IdentityMatchPerson, id: person.ID},
		{kind: store.IdentityMatchContactPoint, id: point.Envelope.ID},
	} {
		_, _, err = st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
			LeftKind: endpoint.kind, LeftID: endpoint.id,
			RightKind: store.IdentityMatchParticipant, RightID: other,
			Basis:  store.IdentityMatchDisplayName,
			State:  store.IdentityMatchStateCandidate,
			Source: store.ProvenanceSystem,
		})
		require.NoError(err)
	}

	person, err = st.GetPersonContext(ctx, person.ID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(ctx, person.ID, person.Revision))
	var remaining int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM identity_match_candidates`).Scan(&remaining))
	assert.Zero(remaining, "deleted person and contact-point endpoints must not dangle")
}
