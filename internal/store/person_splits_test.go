package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type personSplitFixture struct {
	store                *store.Store
	survivor, absorbed   *store.Person
	survivorParticipant  int64
	absorbedParticipants []int64
	absorbedNameID       int64
	absorbedUID          string
	merge                *store.PersonMergeResult
}

func newPersonSplitFixture(t *testing.T) personSplitFixture {
	t.Helper()
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivorParticipant, err := st.EnsureParticipant(
		"split-survivor@example.com", "Survivor", "example.com")
	require.NoError(err)
	firstAbsorbed, err := st.EnsureParticipant(
		"split-absorbed-1@example.com", "Absorbed One", "example.com")
	require.NoError(err)
	secondAbsorbed, err := st.EnsureParticipant(
		"split-absorbed-2@example.com", "Absorbed Two", "example.com")
	require.NoError(err)
	_, err = st.LinkParticipants(firstAbsorbed, secondAbsorbed)
	require.NoError(err)
	survivor, _, err := st.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(firstAbsorbed)
	require.NoError(err)
	name, err := st.AddPersonNameContext(ctx, absorbed.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Absorbed Profile"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: survivor.ID, TargetPersonID: absorbed.ID,
		TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-fixture-merge", Actor: "test",
	})
	require.NoError(err)
	return personSplitFixture{
		store: st, survivor: &merged.Person, absorbed: absorbed,
		survivorParticipant:  survivorParticipant,
		absorbedParticipants: []int64{firstAbsorbed, secondAbsorbed},
		absorbedNameID:       name.Envelope.ID, absorbedUID: absorbed.VCardUID, merge: merged,
	}
}

func TestSplitPersonMerge_ExactReversal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	result, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         f.absorbedParticipants,
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-exact", Actor: "test",
	})
	require.NoError(err)
	assert.True(result.ExactReversal)
	assert.Empty(result.UnrestoredRows)
	assert.True(result.Split.ExactReversal)
	assert.Equal("retired_uid_alias_retargeted", result.UIDAliasDisposition)
	assert.Empty(result.AmbiguousRows)
	assert.NotEqual(f.absorbedUID, result.NewPerson.VCardUID)
	assert.Equal([]int64{f.survivorParticipant}, result.SourcePerson.ParticipantIDs)
	assert.Equal(f.absorbedParticipants, result.NewPerson.ParticipantIDs)
	cluster, err := f.store.ClusterMembers(f.absorbedParticipants[0])
	require.NoError(err)
	assert.Equal(f.absorbedParticipants, cluster)

	profile, err := f.store.GetPersonProfileContext(ctx, result.NewPerson.ID)
	require.NoError(err)
	require.Len(profile.Names, 1)
	assert.Equal(f.absorbedNameID, profile.Names[0].Envelope.ID)
	relationships, err := f.store.ListPersonRelationshipsContext(
		ctx, result.NewPerson.ID, store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(relationships, 1)
	assert.Equal(result.SourcePerson.ID, relationships[0].CounterpartPersonID)
	alias, err := f.store.ResolveRetiredPersonUIDContext(ctx, f.absorbedUID)
	require.NoError(err)
	require.NotNil(alias.SurvivingPersonID)
	assert.Equal(result.NewPerson.ID, *alias.SurvivingPersonID)
}

func TestSplitPersonMerge_ExactReversalIncludesLaterAbsorbedAlias(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	ctx := t.Context()
	alias, err := f.store.EnsureParticipant(
		"split-later-absorbed-alias@example.com", "Later Absorbed Alias", "example.com")
	require.NoError(err)
	_, err = f.store.LinkParticipants(
		f.survivorParticipant, f.absorbedParticipants[0])
	require.NoError(err)
	_, err = f.store.LinkParticipants(f.absorbedParticipants[0], alias)
	require.NoError(err)
	detail, err := f.store.GetPersonMergeContext(ctx, f.merge.Merge.ID)
	require.NoError(err)
	var aliasLineage *store.PersonMergeParticipant
	for index := range detail.Participants {
		if detail.Participants[index].ParticipantID == alias {
			aliasLineage = &detail.Participants[index]
			break
		}
	}
	require.NotNil(aliasLineage)
	assert.Equal("absorbed", aliasLineage.OriginSide)
	current, err := f.store.GetPersonContext(ctx, f.survivor.ID)
	require.NoError(err)
	selected := append(append([]int64{}, f.absorbedParticipants...), alias)

	result, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         selected,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-exact-later-absorbed-alias", Actor: "test",
	})
	require.NoError(err)
	assert.True(result.ExactReversal)
	assert.Equal([]int64{f.survivorParticipant}, result.SourcePerson.ParticipantIDs)
	assert.ElementsMatch(selected, result.NewPerson.ParticipantIDs)
	cluster, err := f.store.ClusterMembers(alias)
	require.NoError(err)
	assert.ElementsMatch(selected, cluster)
}

func TestSplitPersonMerge_ExactReversalRestoresIdentityCandidates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-candidate-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-candidate-absorbed@example.com", "Absorbed")
	other := mustPromotedPerson(t, st, "split-candidate-other@example.com", "Other")
	absorbedSource, err := st.GetOrCreateSource("gmail", "split-candidates-absorbed")
	require.NoError(err)
	survivorSource, err := st.GetOrCreateSource("gmail", "split-candidates-survivor")
	require.NoError(err)
	collapsedSource, err := st.GetOrCreateSource("gmail", "split-candidates-collapsed")
	require.NoError(err)
	collapsedService, _, err := st.EnsureCommunicationServiceContext(ctx,
		store.CommunicationServiceInput{
			Slug: "split-candidate-service", DisplayLabel: "Split Candidate Service",
			ScopePolicy: store.ScopePolicyNone, Normalization: store.NormalizationNone,
			NormalizationVersion: 1,
		})
	require.NoError(err)
	input := func(personID, sourceID int64) store.IdentityMatchCandidateInput {
		return store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: personID,
			RightKind: store.IdentityMatchPerson, RightID: other.ID,
			Basis: store.IdentityMatchDisplayName, NormalizedValue: new("same person"),
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser,
			SourceID: &sourceID,
		}
	}
	absorbedCandidate, _, err := st.UpsertIdentityMatchCandidateContext(
		ctx, input(absorbed.ID, absorbedSource.ID))
	require.NoError(err)
	survivorCandidate, _, err := st.UpsertIdentityMatchCandidateContext(
		ctx, input(survivor.ID, survivorSource.ID))
	require.NoError(err)
	selfCandidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx,
		store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: survivor.ID,
			RightKind: store.IdentityMatchPerson, RightID: absorbed.ID,
			Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate,
			ServiceSlug: &collapsedService.Slug,
			Source:      store.ProvenanceUser, SourceID: &collapsedSource.ID,
		})
	require.NoError(err)
	priorRedirectID := absorbedCandidate.ID + 10_000
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO identity_match_candidate_redirects
		(retired_candidate_id, surviving_candidate_id, endpoints_collapsed)
		VALUES (?, ?, FALSE)`), priorRedirectID, absorbedCandidate.ID)
	require.NoError(err)

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-candidate-merge", Actor: "test",
	})
	require.NoError(err)
	var repointedSourceRows int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'identity_match_candidate_sources'
		  AND action = 'repointed'`), merged.Merge.ID).Scan(&repointedSourceRows))
	assert.Equal(1, repointedSourceRows)
	require.NoError(st.RemoveSource(collapsedSource.ID))
	_, err = st.DB().ExecContext(ctx, st.Rebind(
		`DELETE FROM communication_services WHERE id = ?`), collapsedService.ID)
	require.NoError(err)
	current, err := st.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)
	result, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-candidate-exact", Actor: "test",
	})
	require.NoError(err)
	assert.False(result.ExactReversal)
	assert.NotEmpty(result.UnrestoredRows)

	for _, candidateID := range []int64{absorbedCandidate.ID, selfCandidate.ID} {
		var candidateCount, redirectCount int
		require.NoError(st.DB().QueryRowContext(ctx,
			st.Rebind(`SELECT COUNT(*) FROM identity_match_candidates WHERE id = ?`),
			candidateID).Scan(&candidateCount))
		require.NoError(st.DB().QueryRowContext(ctx,
			st.Rebind(`SELECT COUNT(*) FROM identity_match_candidate_redirects WHERE retired_candidate_id = ?`),
			candidateID).Scan(&redirectCount))
		assert.Equal(1, candidateCount)
		assert.Zero(redirectCount)
	}
	var priorRedirectTarget int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT surviving_candidate_id
		FROM identity_match_candidate_redirects WHERE retired_candidate_id = ?`),
		priorRedirectID).Scan(&priorRedirectTarget))
	assert.Equal(absorbedCandidate.ID, priorRedirectTarget)
	var restoredServiceID sql.NullInt64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT service_id
		FROM identity_match_candidates WHERE id = ?`), selfCandidate.ID).Scan(&restoredServiceID))
	assert.False(restoredServiceID.Valid)
	_, err = st.GetIdentityMatchCandidateContext(ctx, survivorCandidate.ID)
	require.NoError(err)
	for _, want := range []struct {
		candidateID, sourceID int64
		present               int
	}{
		{absorbedCandidate.ID, absorbedSource.ID, 1},
		{absorbedCandidate.ID, survivorSource.ID, 0},
		{survivorCandidate.ID, absorbedSource.ID, 0},
		{survivorCandidate.ID, survivorSource.ID, 1},
	} {
		var count int
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
			FROM identity_match_candidate_sources
			WHERE candidate_id = ? AND source_id = ?`),
			want.candidateID, want.sourceID).Scan(&count))
		assert.Equal(want.present, count)
	}
	var collapsedSourceRows int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM identity_match_candidate_sources
		WHERE candidate_id = ? AND source_id = ?`),
		selfCandidate.ID, collapsedSource.ID).Scan(&collapsedSourceRows))
	assert.Zero(collapsedSourceRows)
}

func TestSplitPersonMerge_ExactReversalKeepsExternalMergeLineage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-lineage-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-lineage-absorbed@example.com", "Absorbed")
	externalSurvivor := mustPromotedPerson(t, st,
		"split-lineage-external-survivor@example.com", "External Survivor")
	externalAbsorbed := mustPromotedPerson(t, st,
		"split-lineage-external-absorbed@example.com", "External Absorbed")
	relationshipIDs := make([]int64, 0, 2)
	for _, personID := range []int64{survivor.ID, absorbed.ID} {
		relationship, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
			SourcePersonID: personID, TargetPersonID: externalAbsorbed.ID,
			TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "test",
		})
		require.NoError(err)
		relationshipIDs = append(relationshipIDs, relationship.ID)
	}
	survivor, err := st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-external-lineage-profile-merge", Actor: "test",
	})
	require.NoError(err)
	externalSurvivor, err = st.GetPersonContext(ctx, externalSurvivor.ID)
	require.NoError(err)
	externalAbsorbed, err = st.GetPersonContext(ctx, externalAbsorbed.ID)
	require.NoError(err)
	externalMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: externalSurvivor.ID, AbsorbedID: externalAbsorbed.ID,
		ExpectedSurvivorRevision: externalSurvivor.Revision,
		ExpectedAbsorbedRevision: externalAbsorbed.Revision,
		IdempotencyKey:           "split-external-lineage-target-merge", Actor: "test",
	})
	require.NoError(err)
	current, err := st.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-external-lineage-exact", Actor: "test",
	})
	require.NoError(err)
	require.True(split.ExactReversal)
	for _, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
		relationships, listErr := st.ListPersonRelationshipsContext(
			ctx, personID, store.PersonRelationshipListOptions{})
		require.NoError(listErr)
		require.Len(relationships, 1)
		assert.Equal(externalMerge.Person.ID, relationships[0].CounterpartPersonID)
	}
	for _, relationshipID := range relationshipIDs {
		var sourcePersonID, targetPersonID int64
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT source_person_id,
			target_person_id FROM person_relationships WHERE id = ?`), relationshipID).
			Scan(&sourcePersonID, &targetPersonID))
		assert.NotEqual(externalAbsorbed.ID, sourcePersonID)
		assert.NotEqual(externalAbsorbed.ID, targetPersonID)
		assert.Contains([]int64{sourcePersonID, targetPersonID}, externalMerge.Person.ID)
	}
}

func TestSplitPersonMerge_ExactReversalSkipsUnsupportedGeneratedCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st,
		"split-generated-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st,
		"split-generated-absorbed@example.com", "Absorbed")
	other := mustPromotedPerson(t, st,
		"split-generated-other@example.com", "Other")
	absorbedSource, err := st.GetOrCreateSource("gmail", "split-generated-absorbed")
	require.NoError(err)
	survivorSource, err := st.GetOrCreateSource("gmail", "split-generated-survivor")
	require.NoError(err)
	input := func(personID, sourceID int64) store.IdentityMatchCandidateInput {
		return store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: personID,
			RightKind: store.IdentityMatchPerson, RightID: other.ID,
			Basis: store.IdentityMatchDisplayName, NormalizedValue: new("same generated person"),
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
			SourceID: &sourceID,
		}
	}
	absorbedCandidate, _, err := st.UpsertIdentityMatchCandidateContext(
		ctx, input(absorbed.ID, absorbedSource.ID))
	require.NoError(err)
	survivorCandidate, _, err := st.UpsertIdentityMatchCandidateContext(
		ctx, input(survivor.ID, survivorSource.ID))
	require.NoError(err)
	evidence, err := st.AddIdentityMatchEvidenceContext(ctx, absorbedCandidate.ID,
		store.IdentityMatchEvidenceInput{
			EvidenceKind: "shared_name", Source: store.ProvenanceArchiveObservation,
			SourceID: &absorbedSource.ID,
		})
	require.NoError(err)

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-generated-merge", Actor: "test",
	})
	require.NoError(err)
	require.NoError(st.RemoveSource(absorbedSource.ID))
	current, err := st.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-generated-exact", Actor: "test",
	})
	require.NoError(err)
	assert.False(split.ExactReversal)
	assert.NotEmpty(split.UnrestoredRows)
	for _, row := range []struct {
		table string
		id    int64
	}{
		{table: "identity_match_candidates", id: absorbedCandidate.ID},
		{table: "identity_match_evidence", id: evidence.ID},
	} {
		var count int
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(
			`SELECT COUNT(*) FROM `+row.table+` WHERE id = ?`), row.id).Scan(&count))
		assert.Zero(count)
	}
	_, err = st.GetIdentityMatchCandidateContext(ctx, survivorCandidate.ID)
	require.NoError(err)
}

func TestSplitPersonMerge_ExactReversalSkipsIndividuallyUnsupportedEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st,
		"split-evidence-support-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st,
		"split-evidence-support-absorbed@example.com", "Absorbed")
	other := mustPromotedPerson(t, st,
		"split-evidence-support-other@example.com", "Other")
	removedSource, err := st.GetOrCreateSource("gmail", "split-evidence-support-removed")
	require.NoError(err)
	remainingSource, err := st.GetOrCreateSource("gmail", "split-evidence-support-remaining")
	require.NoError(err)
	input := func(personID, sourceID int64) store.IdentityMatchCandidateInput {
		return store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: personID,
			RightKind: store.IdentityMatchPerson, RightID: other.ID,
			Basis: store.IdentityMatchDisplayName, NormalizedValue: new("supported generated person"),
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
			SourceID: &sourceID,
		}
	}
	absorbedCandidate, _, err := st.UpsertIdentityMatchCandidateContext(
		ctx, input(absorbed.ID, removedSource.ID))
	require.NoError(err)
	require.NoError(st.AttachIdentityMatchCandidateSourceContext(
		ctx, absorbedCandidate.ID, remainingSource.ID))
	_, _, err = st.UpsertIdentityMatchCandidateContext(
		ctx, input(survivor.ID, remainingSource.ID))
	require.NoError(err)
	evidence, err := st.AddIdentityMatchEvidenceContext(ctx, absorbedCandidate.ID,
		store.IdentityMatchEvidenceInput{
			EvidenceKind: "shared_name", Source: store.ProvenanceArchiveObservation,
			SourceID: &removedSource.ID,
		})
	require.NoError(err)

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-evidence-support-merge", Actor: "test",
	})
	require.NoError(err)
	require.NoError(st.RemoveSource(removedSource.ID))
	current, err := st.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-evidence-support-exact", Actor: "test",
	})
	require.NoError(err)
	assert.False(split.ExactReversal)
	assert.NotEmpty(split.UnrestoredRows)
	_, err = st.GetIdentityMatchCandidateContext(ctx, absorbedCandidate.ID)
	require.NoError(err, "the candidate retains independent source support")
	var evidenceCount, supportCount int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(
		`SELECT COUNT(*) FROM identity_match_evidence WHERE id = ?`),
		evidence.ID).Scan(&evidenceCount))
	assert.Zero(evidenceCount)
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM identity_match_candidate_sources WHERE candidate_id = ? AND source_id = ?`),
		absorbedCandidate.ID, remainingSource.ID).Scan(&supportCount))
	assert.Equal(1, supportCount)
}

func TestSplitPersonMerge_ExactReversalSkipsUnsupportedEvidenceForCollapsedCandidate(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st,
		"split-collapsed-evidence-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st,
		"split-collapsed-evidence-absorbed@example.com", "Absorbed")
	removedSource, err := st.GetOrCreateSource("gmail", "split-collapsed-evidence-removed")
	require.NoError(err)
	remainingSource, err := st.GetOrCreateSource("gmail", "split-collapsed-evidence-remaining")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx,
		store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: survivor.ID,
			RightKind: store.IdentityMatchPerson, RightID: absorbed.ID,
			Basis: store.IdentityMatchDisplayName, NormalizedValue: new("collapsed generated person"),
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
			SourceID: &removedSource.ID,
		})
	require.NoError(err)
	require.NoError(st.AttachIdentityMatchCandidateSourceContext(
		ctx, candidate.ID, remainingSource.ID))
	evidence, err := st.AddIdentityMatchEvidenceContext(ctx, candidate.ID,
		store.IdentityMatchEvidenceInput{
			EvidenceKind: "shared_name", Source: store.ProvenanceArchiveObservation,
			SourceID: &removedSource.ID,
		})
	require.NoError(err)

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-collapsed-evidence-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.ErrorIs(err, store.ErrIdentityMatchNotFound)
	require.NoError(st.RemoveSource(removedSource.ID))
	current, err := st.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-collapsed-evidence-exact", Actor: "test",
	})
	require.NoError(err)
	assert.False(split.ExactReversal)
	assert.NotEmpty(split.UnrestoredRows)
	_, err = st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "independent candidate support survives endpoint restoration")
	var evidenceCount, supportCount int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(
		`SELECT COUNT(*) FROM identity_match_evidence WHERE id = ?`),
		evidence.ID).Scan(&evidenceCount))
	assert.Zero(evidenceCount)
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM identity_match_candidate_sources WHERE candidate_id = ? AND source_id = ?`),
		candidate.ID, remainingSource.ID).Scan(&supportCount))
	assert.Equal(1, supportCount)
}

func TestSplitPersonMerge_CompletedExactSplitReportsAlreadySplit(t *testing.T) {
	require := require.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	result, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         f.absorbedParticipants,
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-exact-first", Actor: "test",
	})
	require.NoError(err)

	_, err = f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: result.SourcePerson.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         f.absorbedParticipants,
		ExpectedSourceRevision: result.SourcePerson.Revision,
		IdempotencyKey:         "split-exact-again", Actor: "test",
	})
	require.ErrorIs(err, store.ErrPersonMergeAlreadySplit)
}

func TestSplitPersonMerge_ExactReversalAfterChainedMergeRebasesCompositeKeyJournal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	first := mustPromotedPerson(t, st, "split-chain-first@example.com", "First")
	second := mustPromotedPerson(t, st, "split-chain-second@example.com", "Second")
	third := mustPromotedPerson(t, st, "split-chain-third@example.com", "Third")
	entry, err := st.CreateDailyNoteEntryContext(ctx, store.DailyNoteEntryInput{
		LocalDate: "2026-08-19", Body: "belongs to second", Author: "test",
		PersonIDs: []int64{second.ID},
	})
	require.NoError(err)

	firstMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: first.ID, AbsorbedID: second.ID,
		ExpectedSurvivorRevision: first.Revision,
		ExpectedAbsorbedRevision: second.Revision,
		IdempotencyKey:           "split-chain-first-merge", Actor: "test",
	})
	require.NoError(err)
	third, err = st.GetPersonContext(ctx, third.ID)
	require.NoError(err)
	secondMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: third.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: third.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "split-chain-second-merge", Actor: "test",
	})
	require.NoError(err)

	result, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs:         second.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "split-chain-exact", Actor: "test",
	})
	require.NoError(err)
	assert.True(result.ExactReversal)

	var newPersonLinks, sourceLinks int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM daily_note_entry_persons WHERE entry_id = ? AND person_id = ?`),
		entry.ID, result.NewPerson.ID).Scan(&newPersonLinks))
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM daily_note_entry_persons WHERE entry_id = ? AND person_id = ?`),
		entry.ID, result.SourcePerson.ID).Scan(&sourceLinks))
	assert.Equal(1, newPersonLinks)
	assert.Zero(sourceLinks)
}

func TestSplitPersonMerge_ExactReversalAfterChainedMergeRebasesEmploymentJournal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	first := mustPromotedPerson(t, st, "split-chain-job-first@example.com", "First")
	second := mustPromotedPerson(t, st, "split-chain-job-second@example.com", "Second")
	third := mustPromotedPerson(t, st, "split-chain-job-third@example.com", "Third")
	organization := mustOrganization(t, st, "Split Chained Employment")
	secondEmployment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: second.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	firstMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: first.ID, AbsorbedID: second.ID,
		ExpectedSurvivorRevision: first.Revision,
		ExpectedAbsorbedRevision: second.Revision,
		IdempotencyKey:           "split-chain-job-first-merge", Actor: "test",
	})
	require.NoError(err)
	third, err = st.GetPersonContext(ctx, third.ID)
	require.NoError(err)
	secondMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: third.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: third.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "split-chain-job-second-merge", Actor: "test",
	})
	require.NoError(err)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs:         second.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "split-chain-job-first-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	newEmployments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: split.NewPerson.ID,
	})
	require.NoError(err)
	require.Len(newEmployments, 1)
	assert.Equal(secondEmployment.ID, newEmployments[0].ID)
	sourceEmployments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: split.SourcePerson.ID,
	})
	require.NoError(err)
	assert.Empty(sourceEmployments)
}

func TestSplitPersonMerge_LaterSplitRebasesEarlierCompositeKeyJournal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	first := mustPromotedPerson(t, st, "split-rebase-first@example.com", "First")
	second := mustPromotedPerson(t, st, "split-rebase-second@example.com", "Second")
	third := mustPromotedPerson(t, st, "split-rebase-third@example.com", "Third")
	entry, err := st.CreateDailyNoteEntryContext(ctx, store.DailyNoteEntryInput{
		LocalDate: "2026-08-20", Body: "belongs to second", Author: "test",
		PersonIDs: []int64{second.ID},
	})
	require.NoError(err)

	firstMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: first.ID, AbsorbedID: second.ID,
		ExpectedSurvivorRevision: first.Revision,
		ExpectedAbsorbedRevision: second.Revision,
		IdempotencyKey:           "split-rebase-first-merge", Actor: "test",
	})
	require.NoError(err)
	third, err = st.GetPersonContext(ctx, third.ID)
	require.NoError(err)
	secondMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: third.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: third.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "split-rebase-second-merge", Actor: "test",
	})
	require.NoError(err)

	laterSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         firstMerge.Person.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "split-rebase-second-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(laterSplit.ExactReversal)

	earlierSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: laterSplit.NewPerson.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs:         second.ParticipantIDs,
		ExpectedSourceRevision: laterSplit.NewPerson.Revision,
		IdempotencyKey:         "split-rebase-first-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(earlierSplit.ExactReversal)

	var newPersonLinks, sourceLinks int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM daily_note_entry_persons WHERE entry_id = ? AND person_id = ?`),
		entry.ID, earlierSplit.NewPerson.ID).Scan(&newPersonLinks))
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM daily_note_entry_persons WHERE entry_id = ? AND person_id = ?`),
		entry.ID, earlierSplit.SourcePerson.ID).Scan(&sourceLinks))
	assert.Equal(1, newPersonLinks)
	assert.Zero(sourceLinks)
}

func TestSplitPersonMerge_LaterSplitRebasesEarlierDeduplicatedRowJournal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	first := mustPromotedPerson(t, st, "split-dedup-first@example.com", "First")
	second := mustPromotedPerson(t, st, "split-dedup-second@example.com", "Second")
	third := mustPromotedPerson(t, st, "split-dedup-third@example.com", "Third")
	organization := mustOrganization(t, st, "Split Dedup Rebase Organization")
	secondEmployment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: second.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	thirdEmployment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: third.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	firstMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: first.ID, AbsorbedID: second.ID,
		ExpectedSurvivorRevision: first.Revision,
		ExpectedAbsorbedRevision: second.Revision,
		IdempotencyKey:           "split-dedup-first-merge", Actor: "test",
	})
	require.NoError(err)
	third, err = st.GetPersonContext(ctx, third.ID)
	require.NoError(err)
	secondMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: third.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: third.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "split-dedup-second-merge", Actor: "test",
	})
	require.NoError(err)

	laterSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         firstMerge.Person.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "split-dedup-second-split", Actor: "test",
	})
	require.NoError(err)
	earlierSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: laterSplit.NewPerson.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs:         second.ParticipantIDs,
		ExpectedSourceRevision: laterSplit.NewPerson.Revision,
		IdempotencyKey:         "split-dedup-first-split", Actor: "test",
	})
	require.NoError(err)

	thirdEmployments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: laterSplit.SourcePerson.ID,
	})
	require.NoError(err)
	require.Len(thirdEmployments, 1)
	assert.Equal(thirdEmployment.ID, thirdEmployments[0].ID)
	secondEmployments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: earlierSplit.NewPerson.ID,
	})
	require.NoError(err)
	require.Len(secondEmployments, 1)
	assert.Equal(secondEmployment.ID, secondEmployments[0].ID)
	firstEmployments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: earlierSplit.SourcePerson.ID,
	})
	require.NoError(err)
	assert.Empty(firstEmployments)
}

func TestSplitPersonMerge_LaterMergeIsPartialAfterEarlierSurvivorLineageSplit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	first := mustPromotedPerson(t, st, "split-nested-first@example.com", "First")
	second := mustPromotedPerson(t, st, "split-nested-second@example.com", "Second")
	third := mustPromotedPerson(t, st, "split-nested-third@example.com", "Third")
	secondName, err := st.AddPersonNameContext(ctx, second.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Nested Second"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	first, err = st.GetPersonContext(ctx, first.ID)
	require.NoError(err)
	second, err = st.GetPersonContext(ctx, second.ID)
	require.NoError(err)
	firstMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: first.ID, AbsorbedID: second.ID,
		ExpectedSurvivorRevision: first.Revision,
		ExpectedAbsorbedRevision: second.Revision,
		IdempotencyKey:           "split-nested-first-merge", Actor: "test",
	})
	require.NoError(err)
	third, err = st.GetPersonContext(ctx, third.ID)
	require.NoError(err)
	secondMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: firstMerge.Person.ID, AbsorbedID: third.ID,
		ExpectedSurvivorRevision: firstMerge.Person.Revision,
		ExpectedAbsorbedRevision: third.Revision,
		IdempotencyKey:           "split-nested-second-merge", Actor: "test",
	})
	require.NoError(err)

	earlierSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs:         second.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "split-nested-first-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(earlierSplit.ExactReversal)

	laterSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: earlierSplit.SourcePerson.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         third.ParticipantIDs,
		ExpectedSourceRevision: earlierSplit.SourcePerson.Revision,
		IdempotencyKey:         "split-nested-second-split", Actor: "test",
	})
	require.NoError(err)
	assert.False(laterSplit.ExactReversal)
	assert.Contains(laterSplit.NewPerson.ParticipantIDs, third.ParticipantIDs[0])
	var nameOwner int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT person_id
		FROM person_names WHERE id = ?`), secondName.Envelope.ID).Scan(&nameOwner))
	assert.Equal(earlierSplit.NewPerson.ID, nameOwner)
}

func TestSplitPersonMerge_ChainedPartialSplitsReleasePersonDeletion(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	first := mustPromotedPerson(t, st, "split-chain-first@example.com", "First")
	second := mustPromotedPerson(t, st, "split-chain-second@example.com", "Second")
	third := mustPromotedPerson(t, st, "split-chain-third@example.com", "Third")
	secondName, err := st.AddPersonNameContext(ctx, second.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Second Profile"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	second, err = st.GetPersonContext(ctx, second.ID)
	require.NoError(err)

	firstMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: first.ID, AbsorbedID: second.ID,
		ExpectedSurvivorRevision: first.Revision,
		ExpectedAbsorbedRevision: second.Revision,
		IdempotencyKey:           "split-chain-first-merge", Actor: "test",
	})
	require.NoError(err)
	secondMerge, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: third.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: third.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "split-chain-second-merge", Actor: "test",
	})
	require.NoError(err)

	_, err = st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         first.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "split-chain-survivor-first", Actor: "test",
	})
	require.ErrorIs(err, store.ErrPersonSplitParticipants)

	secondSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         second.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "split-chain-second-participant", Actor: "test",
	})
	require.NoError(err)
	secondProfile, err := st.GetPersonProfileContext(ctx, secondSplit.NewPerson.ID)
	require.NoError(err)
	require.Len(secondProfile.Names, 1)
	assert.Equal(t, secondName.Envelope.ID, secondProfile.Names[0].Envelope.ID)
	firstSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondSplit.SourcePerson.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         first.ParticipantIDs,
		ExpectedSourceRevision: secondSplit.SourcePerson.Revision,
		IdempotencyKey:         "split-chain-first-participant", Actor: "test",
	})
	require.NoError(err)
	require.NoError(st.DeletePersonContext(
		ctx, firstSplit.SourcePerson.ID, firstSplit.SourcePerson.Revision,
	))
}

func TestSplitPersonMerge_ExactReversalPreservesPostMergeRowEdits(t *testing.T) {
	require := require.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	_, err := f.store.DB().ExecContext(ctx,
		f.store.Rebind(`UPDATE person_names SET formatted = ? WHERE id = ?`),
		"Curated After Merge", f.absorbedNameID)
	require.NoError(err)
	result, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         f.absorbedParticipants,
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-exact-edited", Actor: "test",
	})
	require.NoError(err)
	profile, err := f.store.GetPersonProfileContext(ctx, result.NewPerson.ID)
	require.NoError(err)
	require.Len(profile.Names, 1)
	require.NotNil(profile.Names[0].Formatted)
	assert.Equal(t, "Curated After Merge", *profile.Names[0].Formatted)
}

func TestSplitPersonMerge_ExactReversalPreservesPostMergeRowDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-delete-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-delete-absorbed@example.com", "Absorbed")
	organization := mustOrganization(t, st, "Split Delete Organization")
	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: absorbed.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-delete-merge", Actor: "test",
	})
	require.NoError(err)
	employments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: merged.Person.ID,
	})
	require.NoError(err)
	require.Len(employments, 1)
	require.NoError(st.DeleteEmploymentContext(ctx, employments[0].ID, employments[0].Revision))
	current, err := st.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)
	merged.Person = *current

	result, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-delete-exact", Actor: "test",
	})
	require.NoError(err)
	assert.True(result.ExactReversal)
	for _, personID := range []int64{result.SourcePerson.ID, result.NewPerson.ID} {
		employments, err = st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: personID})
		require.NoError(err)
		assert.Empty(employments)
	}
}

func TestSplitPersonMerge_ExactReversalRestoresDeduplicatedRows(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-job-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-job-absorbed@example.com", "Absorbed")
	organization := mustOrganization(t, st, "Split Shared Employer")
	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: survivor.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	absorbedEmployment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: absorbed.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-job-merge", Actor: "test",
	})
	require.NoError(err)
	result, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-job-exact", Actor: "test",
	})
	require.NoError(err)
	employments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: result.NewPerson.ID,
	})
	require.NoError(err)
	require.Len(employments, 1)
	assert.Equal(t, absorbedEmployment.ID, employments[0].ID)
	assert.Greater(t, employments[0].Revision, absorbedEmployment.Revision)
}

func TestSplitPersonMerge_ExactReversalPreservesDeletedDeduplicationTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-deleted-dedup-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-deleted-dedup-absorbed@example.com", "Absorbed")
	organization := mustOrganization(t, st, "Split Deleted Dedup Employer")
	for _, personID := range []int64{survivor.ID, absorbed.ID} {
		_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
			PersonID: personID, OrganizationID: organization.ID,
			Title: new("Engineer"), Source: store.ProvenanceUser,
		})
		require.NoError(err)
		_, err = st.SetPersonTrackingContext(ctx, personID, true)
		require.NoError(err)
	}
	survivor, err := st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-deleted-dedup-merge", Actor: "test",
	})
	require.NoError(err)
	employments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: merged.Person.ID,
	})
	require.NoError(err)
	require.Len(employments, 1)
	require.NoError(st.DeleteEmploymentContext(ctx, employments[0].ID, employments[0].Revision))
	_, err = st.SetPersonTrackingContext(ctx, merged.Person.ID, false)
	require.NoError(err)
	current, err := st.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-deleted-dedup-split", Actor: "test",
	})
	require.NoError(err)
	for _, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
		employments, err = st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: personID})
		require.NoError(err)
		assert.Empty(employments)
		tracking, err := st.GetPersonTrackingContext(ctx, personID)
		require.NoError(err)
		assert.False(tracking.Tracked)
	}
}

func TestSplitPersonMerge_ExactReversalAdvancesMovedRowRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-revision-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-revision-absorbed@example.com", "Absorbed")
	other := mustPromotedPerson(t, st, "split-revision-other@example.com", "Other")
	organization := mustOrganization(t, st, "Split Revision Organization")
	original, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: absorbed.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	originalRelationship, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: absorbed.ID, TargetPersonID: other.ID,
		TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	rawVCard := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Revision\r\nEND:VCARD\r\n")
	envelope := parseStoreEnvelope(t, rawVCard, "split-revision-book", "split-revision-card")
	envelope.CanonicalPersonUID = absorbed.VCardUID
	originalEnvelope, err := st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
		PersonID: absorbed.ID, Envelope: envelope,
	})
	require.NoError(err)
	survivorEnvelope := parseStoreEnvelope(t,
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Survivor\r\nEND:VCARD\r\n"),
		"split-revision-book", "split-revision-survivor-card")
	survivorEnvelope.CanonicalPersonUID = survivor.VCardUID
	_, err = st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
		PersonID: survivor.ID, Envelope: survivorEnvelope,
	})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-revision-merge", Actor: "test",
	})
	require.NoError(err)
	employments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: merged.Person.ID,
	})
	require.NoError(err)
	require.Len(employments, 1)
	mergedRevision := employments[0].Revision
	assert.Greater(mergedRevision, original.Revision)
	mergedRelationships, err := st.ListPersonRelationshipsContext(
		ctx, merged.Person.ID, store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(mergedRelationships, 1)
	mergedRelationshipRevision := mergedRelationships[0].Relationship.Revision
	assert.Greater(mergedRelationshipRevision, originalRelationship.Revision)
	mergedEnvelope, err := st.GetVCardResourceEnvelopeContext(
		ctx, "split-revision-book", "split-revision-card")
	require.NoError(err)
	assert.Greater(mergedEnvelope.Revision, originalEnvelope.Revision)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-revision-exact", Actor: "test",
	})
	require.NoError(err)
	employments, err = st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: split.NewPerson.ID,
	})
	require.NoError(err)
	require.Len(employments, 1)
	assert.Greater(employments[0].Revision, mergedRevision)
	splitRelationships, err := st.ListPersonRelationshipsContext(
		ctx, split.NewPerson.ID, store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(splitRelationships, 1)
	assert.Greater(splitRelationships[0].Relationship.Revision, mergedRelationshipRevision)
	splitEnvelope, err := st.GetVCardResourceEnvelopeContext(
		ctx, "split-revision-book", "split-revision-card")
	require.NoError(err)
	assert.Greater(splitEnvelope.Revision, mergedEnvelope.Revision)
	assert.Equal(split.NewPerson.ID, splitEnvelope.PersonID)
	assert.Equal(split.NewPerson.VCardUID, splitEnvelope.CanonicalPersonUID)
	keptEnvelope, err := st.GetVCardResourceEnvelopeContext(
		ctx, "split-revision-book", "split-revision-survivor-card")
	require.NoError(err)
	assert.Equal(split.SourcePerson.ID, keptEnvelope.PersonID)
	assert.Equal(split.SourcePerson.VCardUID, keptEnvelope.CanonicalPersonUID)
	err = st.DeleteEmploymentContext(ctx, employments[0].ID, original.Revision)
	require.ErrorIs(err, store.ErrEmploymentRevisionConflict)
}

func TestSplitPersonMerge_ExactReversalReleasesPersonDeletion(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-delete-reference-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-delete-reference-absorbed@example.com", "Absorbed")
	for personID, channel := range map[int64]string{
		survivor.ID: "email", absorbed.ID: "chat",
	} {
		_, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &channel},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	survivor, err := st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-delete-reference-merge", Actor: "test",
	})
	require.NoError(err)
	require.Len(merged.ReviewCandidates, 1)
	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-delete-reference-exact", Actor: "test",
	})
	require.NoError(err)
	err = st.DeletePersonContext(ctx, split.NewPerson.ID, split.NewPerson.Revision)
	require.NoError(err)
}

func TestSplitPersonMerge_ExactReversalLeavesMissingRecordTargetInactive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := newPersonMergeRecordReferenceFixture(t, "split-missing-candidate-target")
	_, err := f.store.DB().ExecContext(ctx, f.store.Rebind(
		`DELETE FROM persons WHERE id = ?`), f.absorbedTarget.ID)
	require.NoError(err)
	split, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.merge.Person.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         f.absorbed.ParticipantIDs,
		ExpectedSourceRevision: f.merge.Person.Revision,
		IdempotencyKey:         "split-missing-candidate-target-exact", Actor: "test",
	})
	require.NoError(err)
	require.True(split.ExactReversal)
	var personID, targetID int64
	var current bool
	require.NoError(f.store.DB().QueryRowContext(ctx, f.store.Rebind(`SELECT person_id,
		value_record_id, active_until IS NULL AND superseded_at IS NULL
		FROM person_attribute_values WHERE id = ?`), f.absorbedValueID).
		Scan(&personID, &targetID, &current))
	assert.Equal(split.NewPerson.ID, personID)
	assert.Equal(f.absorbedTarget.ID, targetID)
	assert.False(current, "a split must not reactivate a dangling record reference")
}

func TestSplitPersonMerge_ExactReversalRestoresRetainedCollisionsInPlace(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-retained-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-retained-absorbed@example.com", "Absorbed")
	resourceUID := "shared-resource"
	propertyID := "shared-property"
	sourceRef := "shared-book"
	for personID, formatted := range map[int64]string{
		survivor.ID: "Survivor Name", absorbed.ID: "Absorbed Name",
	} {
		_, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
			NameKind: store.PersonNameFormatted, Formatted: &formatted,
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceVCardImport, SourceRef: &sourceRef,
				SourceResourceUID: &resourceUID,
				VCard:             store.VCardIdentity{Property: "FN", PropID: &propertyID},
			},
		})
		require.NoError(err)
	}
	absorbedName, err := st.GetPersonProfileContext(ctx, absorbed.ID)
	require.NoError(err)
	require.Len(absorbedName.Names, 1)
	absorbedCategory, err := st.AddPersonCategoryContext(ctx, absorbed.ID, store.PersonCategoryInput{
		OriginalValue: "friends", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport},
	})
	require.NoError(err)
	_, err = st.AddPersonCategoryContext(ctx, survivor.ID, store.PersonCategoryInput{
		OriginalValue: "Friends", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	for _, personID := range []int64{survivor.ID, absorbed.ID} {
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	absorbedValues, err := st.ListPersonAttributeValuesContext(ctx, absorbed.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(absorbedValues, 1)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-retained-merge", Actor: "test",
	})
	require.NoError(err)
	result, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-retained-exact", Actor: "test",
	})
	require.NoError(err)
	for _, row := range []struct {
		table string
		id    int64
	}{
		{table: "person_names", id: absorbedName.Names[0].Envelope.ID},
		{table: "person_categories", id: absorbedCategory.Envelope.ID},
		{table: "person_attribute_values", id: absorbedValues[0].ID},
	} {
		var personID int64
		var current bool
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT person_id,
			active_until IS NULL AND superseded_at IS NULL FROM `+row.table+` WHERE id = ?`),
			row.id).Scan(&personID, &current))
		assert.Equal(result.NewPerson.ID, personID, row.table)
		assert.True(current, row.table)
	}
}

func TestSplitPersonMerge_ExactReversalFinalizesPendingCandidates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "split-candidate-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "split-candidate-absorbed@example.com", "Absorbed")
	for personID, channel := range map[int64]string{survivor.ID: "email", absorbed.ID: "chat"} {
		_, err := f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &channel},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	survivor, err := f.Store.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-candidate-merge", Actor: "test",
	})
	require.NoError(err)
	require.Len(merged.ReviewCandidates, 1)
	_, err = f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-candidate-exact", Actor: "split-reviewer",
	})
	require.NoError(err)
	var state, reviewedBy string
	var reviewedAt sql.NullTime
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT state, reviewed_by, reviewed_at
		FROM person_merge_review_candidates WHERE id = ?`), merged.ReviewCandidates[0].ID).
		Scan(&state, &reviewedBy, &reviewedAt))
	assert.Equal("rejected", state)
	assert.Equal("split-reviewer", reviewedBy)
	assert.True(reviewedAt.Valid)
}

func TestSplitPersonMerge_ExactReversalRejectsAcceptedCandidates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "split-reviewed-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "split-reviewed-absorbed@example.com", "Absorbed")
	for personID, channel := range map[int64]string{survivor.ID: "email", absorbed.ID: "chat"} {
		_, err := f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &channel},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	survivor, err := f.Store.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-reviewed-merge", Actor: "test",
	})
	require.NoError(err)
	require.Len(merged.ReviewCandidates, 1)
	accepted, err := f.Store.DecidePersonMergeCandidateContext(ctx,
		store.PersonMergeCandidateDecisionRequest{
			CandidateID: merged.ReviewCandidates[0].ID, PersonID: merged.Person.ID,
			ExpectedPersonRevision: merged.Person.Revision,
			Decision:               store.PersonMergeCandidateAccept, Actor: "reviewer",
		})
	require.NoError(err)
	assert.Equal("accepted", accepted.State)
	current, err := f.Store.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)
	var peopleBefore int
	require.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM persons`).Scan(&peopleBefore))

	_, err = f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-reviewed-exact", Actor: "test",
	})
	require.ErrorIs(err, store.ErrPersonSplitReviewed)
	var peopleAfter, splitCount int
	require.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM persons`).Scan(&peopleAfter))
	require.NoError(f.Store.DB().QueryRowContext(ctx,
		f.Store.Rebind(`SELECT COUNT(*) FROM person_splits WHERE merge_id = ?`), merged.Merge.ID).Scan(&splitCount))
	assert.Equal(peopleBefore, peopleAfter)
	assert.Zero(splitCount)
}

func TestSplitPersonMerge_ExactReversalRejectsAcceptedAbsorbedCandidateFromEarlierMerge(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := storetest.New(t)
	first := mustPromotedPerson(t, f.Store, "split-reviewed-chain-first@example.com", "First")
	second := mustPromotedPerson(t, f.Store, "split-reviewed-chain-second@example.com", "Second")
	third := mustPromotedPerson(t, f.Store, "split-reviewed-chain-third@example.com", "Third")
	for personID, channel := range map[int64]string{first.ID: "email", second.ID: "chat"} {
		_, err := f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &channel},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	first, err := f.Store.GetPersonContext(ctx, first.ID)
	require.NoError(err)
	second, err = f.Store.GetPersonContext(ctx, second.ID)
	require.NoError(err)
	firstMerge, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: first.ID, AbsorbedID: second.ID,
		ExpectedSurvivorRevision: first.Revision,
		ExpectedAbsorbedRevision: second.Revision,
		IdempotencyKey:           "split-reviewed-chain-first-merge", Actor: "test",
	})
	require.NoError(err)
	require.Len(firstMerge.ReviewCandidates, 1)
	third, err = f.Store.GetPersonContext(ctx, third.ID)
	require.NoError(err)
	secondMerge, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: third.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: third.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "split-reviewed-chain-second-merge", Actor: "test",
	})
	require.NoError(err)
	accepted, err := f.Store.DecidePersonMergeCandidateContext(ctx,
		store.PersonMergeCandidateDecisionRequest{
			CandidateID: firstMerge.ReviewCandidates[0].ID, PersonID: secondMerge.Person.ID,
			ExpectedPersonRevision: secondMerge.Person.Revision,
			Decision:               store.PersonMergeCandidateAccept, Actor: "reviewer",
		})
	require.NoError(err)
	assert.Equal("accepted", accepted.State)
	current, err := f.Store.GetPersonContext(ctx, secondMerge.Person.ID)
	require.NoError(err)
	var peopleBefore int
	require.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM persons`).Scan(&peopleBefore))

	_, err = f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         firstMerge.Person.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "split-reviewed-chain-exact", Actor: "test",
	})
	require.ErrorIs(err, store.ErrPersonSplitReviewed)
	var peopleAfter, splitCount int
	require.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM persons`).Scan(&peopleAfter))
	require.NoError(f.Store.DB().QueryRowContext(ctx,
		f.Store.Rebind(`SELECT COUNT(*) FROM person_splits WHERE merge_id = ?`),
		secondMerge.Merge.ID).Scan(&splitCount))
	assert.Equal(peopleBefore, peopleAfter)
	assert.Zero(splitCount)
}

func TestSplitPersonMerge_ExactReversalAllowsCandidateAcceptedBeforeMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := storetest.New(t)
	first := mustPromotedPerson(t, f.Store, "split-preaccepted-first@example.com", "First")
	second := mustPromotedPerson(t, f.Store, "split-preaccepted-second@example.com", "Second")
	third := mustPromotedPerson(t, f.Store, "split-preaccepted-third@example.com", "Third")
	for personID, channel := range map[int64]string{first.ID: "email", second.ID: "chat"} {
		_, err := f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &channel},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	first, err := f.Store.GetPersonContext(ctx, first.ID)
	require.NoError(err)
	second, err = f.Store.GetPersonContext(ctx, second.ID)
	require.NoError(err)
	firstMerge, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: first.ID, AbsorbedID: second.ID,
		ExpectedSurvivorRevision: first.Revision,
		ExpectedAbsorbedRevision: second.Revision,
		IdempotencyKey:           "split-preaccepted-first-merge", Actor: "test",
	})
	require.NoError(err)
	require.Len(firstMerge.ReviewCandidates, 1)
	accepted, err := f.Store.DecidePersonMergeCandidateContext(ctx,
		store.PersonMergeCandidateDecisionRequest{
			CandidateID: firstMerge.ReviewCandidates[0].ID, PersonID: firstMerge.Person.ID,
			ExpectedPersonRevision: firstMerge.Person.Revision,
			Decision:               store.PersonMergeCandidateAccept, Actor: "reviewer",
		})
	require.NoError(err)
	assert.Equal("accepted", accepted.State)
	currentFirst, err := f.Store.GetPersonContext(ctx, firstMerge.Person.ID)
	require.NoError(err)
	third, err = f.Store.GetPersonContext(ctx, third.ID)
	require.NoError(err)
	secondMerge, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: third.ID, AbsorbedID: currentFirst.ID,
		ExpectedSurvivorRevision: third.Revision,
		ExpectedAbsorbedRevision: currentFirst.Revision,
		IdempotencyKey:           "split-preaccepted-second-merge", Actor: "test",
	})
	require.NoError(err)
	split, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         currentFirst.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "split-preaccepted-second-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
}

func TestSplitPersonMerge_IdempotencyReplaysCommittedResultAfterLaterChanges(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	f := newPersonSplitFixture(t)
	request := store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         f.absorbedParticipants,
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-immutable-replay", Actor: "test",
	}
	committed, err := f.store.SplitPersonMergeContext(ctx, request)
	require.NoError(err)
	_, err = f.store.UpdatePersonDisplayNameContext(
		ctx, committed.SourcePerson.ID, committed.SourcePerson.Revision, new("Changed source"))
	require.NoError(err)
	_, err = f.store.UpdatePersonDisplayNameContext(
		ctx, committed.NewPerson.ID, committed.NewPerson.Revision, new("Changed new person"))
	require.NoError(err)
	replayed, err := f.store.SplitPersonMergeContext(ctx, request)
	require.NoError(err)
	assertJSONEquivalent(t, committed, replayed)
	currentSource, err := f.store.GetPersonContext(ctx, committed.SourcePerson.ID)
	require.NoError(err)
	currentNew, err := f.store.GetPersonContext(ctx, committed.NewPerson.ID)
	require.NoError(err)
	_, err = f.store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: currentSource.ID, AbsorbedID: currentNew.ID,
		ExpectedSurvivorRevision: currentSource.Revision,
		ExpectedAbsorbedRevision: currentNew.Revision,
		IdempotencyKey:           "merge-after-split-replay", Actor: "test",
	})
	require.NoError(err)
	replayed, err = f.store.SplitPersonMergeContext(ctx, request)
	require.NoError(err)
	assertJSONEquivalent(t, committed, replayed)
}

func TestSplitPersonMerge_ExactReversalRestoresTracking(t *testing.T) {
	for _, test := range []struct {
		name            string
		trackSurvivor   bool
		wantSourceTrack bool
	}{
		{name: "absorbed-only", wantSourceTrack: false},
		{name: "both-profiles", trackSurvivor: true, wantSourceTrack: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ctx := t.Context()
			st := testutil.NewTestStore(t)
			survivor := mustPromotedPerson(t, st,
				"tracking-survivor-"+test.name+"@example.com", "Survivor")
			absorbed := mustPromotedPerson(t, st,
				"tracking-absorbed-"+test.name+"@example.com", "Absorbed")
			if test.trackSurvivor {
				_, err := st.SetPersonTrackingContext(ctx, survivor.ID, true)
				require.NoError(err)
			}
			_, err := st.SetPersonTrackingContext(ctx, absorbed.ID, true)
			require.NoError(err)
			merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
				SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
				ExpectedSurvivorRevision: survivor.Revision,
				ExpectedAbsorbedRevision: absorbed.Revision,
				IdempotencyKey:           "tracking-merge-" + test.name, Actor: "test",
			})
			require.NoError(err)
			mergedTracking, err := st.GetPersonTrackingContext(ctx, merged.Person.ID)
			require.NoError(err)
			assert.True(mergedTracking.Tracked)

			split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
				SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
				ParticipantIDs:         absorbed.ParticipantIDs,
				ExpectedSourceRevision: merged.Person.Revision,
				IdempotencyKey:         "tracking-split-" + test.name, Actor: "test",
			})
			require.NoError(err)
			require.True(split.ExactReversal)
			sourceTracking, err := st.GetPersonTrackingContext(ctx, split.SourcePerson.ID)
			require.NoError(err)
			newTracking, err := st.GetPersonTrackingContext(ctx, split.NewPerson.ID)
			require.NoError(err)
			assert.Equal(test.wantSourceTrack, sourceTracking.Tracked)
			assert.True(newTracking.Tracked)
		})
	}
}

func TestSplitPersonMerge_ExactReversalReconcilesDeduplicatedTracking(t *testing.T) {
	t.Run("preserves supported changes", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		ctx := t.Context()
		st := testutil.NewTestStore(t)
		survivor := mustPromotedPerson(t, st,
			"tracking-change-survivor@example.com", "Survivor")
		absorbed := mustPromotedPerson(t, st,
			"tracking-change-absorbed@example.com", "Absorbed")
		_, err := st.SetPersonTrackingContext(ctx, survivor.ID, true)
		require.NoError(err)
		_, err = st.SetPersonTrackingContext(ctx, absorbed.ID, true)
		require.NoError(err)
		merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "tracking-change-merge", Actor: "test",
		})
		require.NoError(err)
		changedAt := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
		_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE person_tracking
			SET tracked_at = ? WHERE person_id = ?`), changedAt, merged.Person.ID)
		require.NoError(err)

		split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
			SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
			ParticipantIDs:         absorbed.ParticipantIDs,
			ExpectedSourceRevision: merged.Person.Revision,
			IdempotencyKey:         "tracking-change-split", Actor: "test",
		})
		require.NoError(err)
		newTracking, err := st.GetPersonTrackingContext(ctx, split.NewPerson.ID)
		require.NoError(err)
		require.NotNil(newTracking.TrackedAt)
		assert.True(changedAt.Equal(*newTracking.TrackedAt))
	})

	t.Run("does not restore after reassignment", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		ctx := t.Context()
		st := testutil.NewTestStore(t)
		survivor := mustPromotedPerson(t, st,
			"tracking-reassign-survivor@example.com", "Survivor")
		absorbed := mustPromotedPerson(t, st,
			"tracking-reassign-absorbed@example.com", "Absorbed")
		reassigned := mustPromotedPerson(t, st,
			"tracking-reassign-target@example.com", "Target")
		_, err := st.SetPersonTrackingContext(ctx, survivor.ID, true)
		require.NoError(err)
		_, err = st.SetPersonTrackingContext(ctx, absorbed.ID, true)
		require.NoError(err)
		merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "tracking-reassign-merge", Actor: "test",
		})
		require.NoError(err)
		_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE person_tracking
			SET person_id = ? WHERE person_id = ?`), reassigned.ID, merged.Person.ID)
		require.NoError(err)

		split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
			SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
			ParticipantIDs:         absorbed.ParticipantIDs,
			ExpectedSourceRevision: merged.Person.Revision,
			IdempotencyKey:         "tracking-reassign-split", Actor: "test",
		})
		require.NoError(err)
		sourceTracking, err := st.GetPersonTrackingContext(ctx, split.SourcePerson.ID)
		require.NoError(err)
		newTracking, err := st.GetPersonTrackingContext(ctx, split.NewPerson.ID)
		require.NoError(err)
		reassignedTracking, err := st.GetPersonTrackingContext(ctx, reassigned.ID)
		require.NoError(err)
		assert.False(sourceTracking.Tracked)
		assert.False(newTracking.Tracked)
		assert.True(reassignedTracking.Tracked)
	})
}

func TestSplitPersonMerge_ExactReversalPreservesRecreatedTracking(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st,
		"tracking-recreated-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st,
		"tracking-recreated-absorbed@example.com", "Absorbed")
	_, err := st.SetPersonTrackingContext(ctx, absorbed.ID, true)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "tracking-recreated-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = st.SetPersonTrackingContext(ctx, merged.Person.ID, false)
	require.NoError(err)
	_, err = st.SetPersonTrackingContext(ctx, merged.Person.ID, true)
	require.NoError(err)
	replacementTime := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE person_tracking
		SET tracked_at = ? WHERE person_id = ?`), replacementTime, merged.Person.ID)
	require.NoError(err)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "tracking-recreated-split", Actor: "test",
	})
	require.NoError(err)
	sourceTracking, err := st.GetPersonTrackingContext(ctx, split.SourcePerson.ID)
	require.NoError(err)
	newTracking, err := st.GetPersonTrackingContext(ctx, split.NewPerson.ID)
	require.NoError(err)
	assert.True(sourceTracking.Tracked)
	require.NotNil(sourceTracking.TrackedAt)
	assert.True(replacementTime.Equal(*sourceTracking.TrackedAt))
	assert.True(newTracking.Tracked)
	require.NotNil(newTracking.TrackedAt)
	assert.False(replacementTime.Equal(*newTracking.TrackedAt))
}

func TestSplitPersonMerge_ExactReversalThreeWayRestoresMergeOwnedFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-fields-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-fields-absorbed@example.com", "Absorbed")
	survivorOrg := mustOrganization(t, st, "Split Survivor Primary")
	absorbedOrg := mustOrganization(t, st, "Split Absorbed Primary")
	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: survivor.ID, OrganizationID: survivorOrg.ID, Title: new("Engineer"),
		IsPrimary: new(true), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	absorbedEmployment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: absorbed.ID, OrganizationID: absorbedOrg.ID, Title: new("Advisor"),
		IsPrimary: new(true), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-fields-merge", Actor: "test",
	})
	require.NoError(err)

	// The merge demotes the absorbed primary. A later title edit belongs to
	// the user and must survive while the merge-owned demotion is reversed.
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE employments SET title = ? WHERE id = ?`),
		"Curated Advisor", absorbedEmployment.ID)
	require.NoError(err)
	result, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-fields-exact", Actor: "test",
	})
	require.NoError(err)
	employments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: result.NewPerson.ID,
	})
	require.NoError(err)
	require.Len(employments, 1)
	assert.Equal(absorbedEmployment.ID, employments[0].ID)
	assert.True(employments[0].IsPrimary, "split reverses the merge-owned demotion")
	require.NotNil(employments[0].Title)
	assert.Equal("Curated Advisor", *employments[0].Title,
		"split preserves a field edited after the merge")
}

func TestSplitPersonMerge_ExactReversalPreservesPostMergePersonReassignment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-reassign-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-reassign-absorbed@example.com", "Absorbed")
	third := mustPromotedPerson(t, st, "split-reassign-third@example.com", "Third")
	organization := mustOrganization(t, st, "Split Reassignment Organization")
	absorbedEmployment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: absorbed.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-reassign-merge", Actor: "test",
	})
	require.NoError(err)
	mergedEmployment, err := st.GetEmploymentContext(ctx, absorbedEmployment.ID)
	require.NoError(err)
	reassigned, err := st.UpdateEmploymentContext(ctx, mergedEmployment.ID,
		mergedEmployment.Revision, store.EmploymentInput{
			PersonID: third.ID, OrganizationID: organization.ID,
			Title: new("Engineer"), Source: store.ProvenanceUser,
		})
	require.NoError(err)
	assert.Equal(third.ID, reassigned.PersonID)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-reassign-exact", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	reassigned, err = st.GetEmploymentContext(ctx, absorbedEmployment.ID)
	require.NoError(err)
	assert.Equal(third.ID, reassigned.PersonID)
	for _, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
		employments, listErr := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
			PersonID: personID,
		})
		require.NoError(listErr)
		assert.Empty(employments)
	}
}

func TestSplitPersonMerge_ExactReversalSkipsDeletedRelationshipType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st,
		"split-dependency-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st,
		"split-dependency-absorbed@example.com", "Absorbed")
	reviewOwner := mustPromotedPerson(t, st,
		"split-dependency-review@example.com", "Review Owner")
	relationshipType, err := st.CreateRelationshipTypeContext(ctx,
		store.RelationshipTypeInput{
			Slug: "former-colleague", ForwardLabel: "former colleague",
			ReverseLabel: "former colleague", IsSymmetric: true,
		})
	require.NoError(err)
	selfEdge, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: survivor.ID, TargetPersonID: absorbed.ID,
		TypeSlug: relationshipType.Slug, Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	var reviewID int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO person_relationship_reviews (
		person_id, raw_related_value, raw_related_type, value_kind,
		accepted_relationship_id, status, source, created_by, reviewed_by, reviewed_at
	) VALUES (?, ?, ?, 'uri', ?, 'accepted', 'user', 'test', 'test', CURRENT_TIMESTAMP)
	RETURNING id`), reviewOwner.ID, absorbed.VCardUID, relationshipType.Slug,
		selfEdge.ID).Scan(&reviewID))
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-dependency-merge", Actor: "test",
	})
	require.NoError(err)
	var acceptedAfterMerge sql.NullInt64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT accepted_relationship_id
		FROM person_relationship_reviews WHERE id = ?`), reviewID).Scan(&acceptedAfterMerge))
	assert.False(acceptedAfterMerge.Valid)
	require.NoError(st.DeleteRelationshipTypeContext(
		ctx, relationshipType.ID, relationshipType.Revision))

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-dependency-exact", Actor: "test",
	})
	require.NoError(err)
	assert.False(split.ExactReversal)
	assert.NotEmpty(split.UnrestoredRows)
	for _, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
		relationships, listErr := st.ListPersonRelationshipsContext(
			ctx, personID, store.PersonRelationshipListOptions{})
		require.NoError(listErr)
		assert.Empty(relationships)
	}
	var acceptedAfterSplit sql.NullInt64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT accepted_relationship_id
		FROM person_relationship_reviews WHERE id = ?`), reviewID).Scan(&acceptedAfterSplit))
	assert.False(acceptedAfterSplit.Valid)
}

func TestSplitPersonMerge_ExactReversalNullsMissingDependencyOnRecreatedReview(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st,
		"split-recreated-review-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st,
		"split-recreated-review-absorbed@example.com", "Absorbed")
	other := mustPromotedPerson(t, st,
		"split-recreated-review-other@example.com", "Other")
	matched := mustPromotedPerson(t, st,
		"split-recreated-review-matched@example.com", "Matched")
	relationshipType, err := st.CreateRelationshipTypeContext(ctx,
		store.RelationshipTypeInput{
			Slug: "former-teammate", ForwardLabel: "former teammate",
			ReverseLabel: "former teammate", IsSymmetric: true,
		})
	require.NoError(err)
	survivorEdge, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: survivor.ID, TargetPersonID: other.ID,
		TypeSlug: relationshipType.Slug, Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	absorbedEdge, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: absorbed.ID, TargetPersonID: other.ID,
		TypeSlug: relationshipType.Slug, Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	insertReview := func(personID, relationshipID int64) int64 {
		t.Helper()
		var reviewID int64
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO person_relationship_reviews (
			person_id, raw_related_value, raw_related_type, value_kind,
			accepted_relationship_id, status, source, source_ref, created_by,
			reviewed_by, reviewed_at
		) VALUES (?, 'shared-review', ?, 'text', ?, 'accepted', 'user',
			'split-recreated-review', 'test', 'test', CURRENT_TIMESTAMP)
		RETURNING id`), personID, relationshipType.Slug, relationshipID).Scan(&reviewID))
		return reviewID
	}
	_ = insertReview(survivor.ID, survivorEdge.ID)
	absorbedReviewID := insertReview(absorbed.ID, absorbedEdge.ID)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE person_relationship_reviews
		SET matched_person_id = ? WHERE id = ?`), matched.ID, absorbedReviewID)
	require.NoError(err)

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-recreated-review-merge", Actor: "test",
	})
	require.NoError(err)
	var reviewCount int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(
		`SELECT COUNT(*) FROM person_relationship_reviews WHERE id = ?`),
		absorbedReviewID).Scan(&reviewCount))
	assert.Zero(reviewCount)
	matched, err = st.GetPersonContext(ctx, matched.ID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(ctx, matched.ID, matched.Revision))
	require.NoError(st.DeletePersonRelationshipContext(
		ctx, survivorEdge.ID, survivorEdge.Revision))
	require.NoError(st.DeleteRelationshipTypeContext(
		ctx, relationshipType.ID, relationshipType.Revision))

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-recreated-review-exact", Actor: "test",
	})
	require.NoError(err)
	assert.False(split.ExactReversal)
	assert.NotEmpty(split.UnrestoredRows)
	var acceptedRelationshipID, matchedPersonID sql.NullInt64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT accepted_relationship_id,
		matched_person_id
		FROM person_relationship_reviews WHERE id = ?`),
		absorbedReviewID).Scan(&acceptedRelationshipID, &matchedPersonID))
	assert.False(acceptedRelationshipID.Valid)
	assert.False(matchedPersonID.Valid)
}

func TestSplitPersonMerge_ExactReversalRestoresRelationshipReviewDependency(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-review-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-review-absorbed@example.com", "Absorbed")
	other := mustPromotedPerson(t, st, "split-review-other@example.com", "Other")
	reviewOwner := mustPromotedPerson(t, st, "split-review-owner@example.com", "Review Owner")
	pendingOwner := mustPromotedPerson(t, st, "split-review-pending@example.com", "Pending Owner")
	survivorEdge, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: survivor.ID, TargetPersonID: other.ID, TypeSlug: "friend",
		Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	absorbedEdge, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: absorbed.ID, TargetPersonID: other.ID, TypeSlug: "friend",
		Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	resolution, err := st.ResolveRelatedValueContext(ctx, store.RelatedImport{
		PersonID: reviewOwner.ID, RawValue: survivor.VCardUID, RawType: "friend",
		ValueKind: store.RelatedValueKindURI, Source: store.ProvenanceVCardImport,
		Actor: "test", SourceRef: new("dependency-review"),
	})
	require.NoError(err)
	require.NotNil(resolution.Review)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE person_relationship_reviews
		SET accepted_relationship_id = ?, matched_person_id = NULL WHERE id = ?`),
		absorbedEdge.ID, resolution.Review.ID)
	require.NoError(err)
	var pendingReviewID int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO person_relationship_reviews (
		person_id, raw_related_value, raw_related_type, value_kind, matched_person_id,
		status, source, source_ref, created_by
	) VALUES (?, ?, 'friend', 'uri', ?, 'pending', 'vcard_import', ?, 'test') RETURNING id`),
		pendingOwner.ID, absorbed.VCardUID, absorbed.ID, "split-review-pending").Scan(&pendingReviewID))
	var reviewProjectionBefore, pendingProjectionBefore int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), reviewOwner.ID).Scan(&reviewProjectionBefore))
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), pendingOwner.ID).Scan(&pendingProjectionBefore))
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-review-merge", Actor: "test",
	})
	require.NoError(err)
	var acceptedAfterMerge int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT accepted_relationship_id
		FROM person_relationship_reviews WHERE id = ?`), resolution.Review.ID).Scan(&acceptedAfterMerge))
	assert.Equal(survivorEdge.ID, acceptedAfterMerge)
	var reviewProjectionAfterMerge, pendingProjectionAfterMerge int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), reviewOwner.ID).Scan(&reviewProjectionAfterMerge))
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), pendingOwner.ID).Scan(&pendingProjectionAfterMerge))
	assert.Equal(reviewProjectionBefore+1, reviewProjectionAfterMerge)
	assert.Equal(pendingProjectionBefore+1, pendingProjectionAfterMerge)

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-review-exact", Actor: "test",
	})
	require.NoError(err)
	var acceptedAfterSplit int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT accepted_relationship_id
		FROM person_relationship_reviews WHERE id = ?`), resolution.Review.ID).Scan(&acceptedAfterSplit))
	assert.Equal(absorbedEdge.ID, acceptedAfterSplit)
	var pendingMatchedPersonID int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT matched_person_id
		FROM person_relationship_reviews WHERE id = ?`), pendingReviewID).Scan(&pendingMatchedPersonID))
	assert.Equal(split.NewPerson.ID, pendingMatchedPersonID)
	var reviewProjectionAfterSplit, pendingProjectionAfterSplit int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), reviewOwner.ID).Scan(&reviewProjectionAfterSplit))
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), pendingOwner.ID).Scan(&pendingProjectionAfterSplit))
	assert.Equal(reviewProjectionAfterMerge+1, reviewProjectionAfterSplit)
	assert.Equal(pendingProjectionAfterMerge+1, pendingProjectionAfterSplit)
}

func TestSplitPersonMerge_ExactReversalRestoresIdentityEvidenceDependency(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t, st, "split-evidence-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, st, "split-evidence-absorbed@example.com", "Absorbed")
	other := mustPromotedPerson(t, st, "split-evidence-other@example.com", "Other")
	source, err := st.GetOrCreateSource("gmail", "split-evidence")
	require.NoError(err)
	input := func(personID int64) store.IdentityMatchCandidateInput {
		return store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: personID,
			RightKind: store.IdentityMatchPerson, RightID: other.ID,
			Basis: store.IdentityMatchDisplayName, NormalizedValue: new("same person"),
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser,
			SourceID: &source.ID,
		}
	}
	absorbedCandidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, input(absorbed.ID))
	require.NoError(err)
	survivorCandidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, input(survivor.ID))
	require.NoError(err)
	evidence, err := st.AddIdentityMatchEvidenceContext(ctx, absorbedCandidate.ID,
		store.IdentityMatchEvidenceInput{
			EvidenceKind: "shared_name", Source: store.ProvenanceUser, SourceID: &source.ID,
		})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-evidence-merge", Actor: "test",
	})
	require.NoError(err)
	var candidateAfterMerge int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT candidate_id
		FROM identity_match_evidence WHERE id = ?`), evidence.ID).Scan(&candidateAfterMerge))
	assert.Equal(t, survivorCandidate.ID, candidateAfterMerge)

	_, err = st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-evidence-exact", Actor: "test",
	})
	require.NoError(err)
	var candidateAfterSplit int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT candidate_id
		FROM identity_match_evidence WHERE id = ?`), evidence.ID).Scan(&candidateAfterSplit))
	assert.Equal(t, absorbedCandidate.ID, candidateAfterSplit)
}

func TestSplitPersonMerge_Partial(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	postMergeName, err := f.store.AddPersonNameContext(ctx, f.survivor.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Post Merge"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	f.survivor, err = f.store.GetPersonContext(ctx, f.survivor.ID)
	require.NoError(err)
	result, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         []int64{f.absorbedParticipants[0]},
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-partial", Actor: "test",
	})
	require.NoError(err)
	assert.False(result.ExactReversal)
	assert.Equal("retired_uid_alias_unchanged", result.UIDAliasDisposition)
	assert.Equal([]int64{f.absorbedParticipants[0]}, result.NewPerson.ParticipantIDs)
	assert.ElementsMatch([]int64{f.survivorParticipant, f.absorbedParticipants[1]},
		result.SourcePerson.ParticipantIDs)
	assert.NotEmpty(result.AmbiguousRows)
	sourceProfile, err := f.store.GetPersonProfileContext(ctx, result.SourcePerson.ID)
	require.NoError(err)
	var sourceNameIDs []int64
	for _, name := range sourceProfile.Names {
		sourceNameIDs = append(sourceNameIDs, name.Envelope.ID)
	}
	assert.Contains(sourceNameIDs, f.absorbedNameID)
	assert.Contains(sourceNameIDs, postMergeName.Envelope.ID)
	newProfile, err := f.store.GetPersonProfileContext(ctx, result.NewPerson.ID)
	require.NoError(err)
	assert.Empty(newProfile.Names)
	alias, err := f.store.ResolveRetiredPersonUIDContext(ctx, f.absorbedUID)
	require.NoError(err)
	require.NotNil(alias.SurvivingPersonID)
	assert.Equal(result.SourcePerson.ID, *alias.SurvivingPersonID)
}

func TestSplitPersonMerge_PartialRejectsAcceptedCandidateAcrossBoundary(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	selected := f.absorbedParticipants[0]
	retained := f.absorbedParticipants[1]
	_, err := f.store.UnlinkParticipants(selected, retained)
	require.NoError(err)
	candidate := upsertPairCandidate(
		t, f.store, selected, retained, store.IdentityMatchStableProviderID)
	_, _, err = f.store.AcceptIdentityMatchCandidateContext(
		ctx, candidate.ID, "system", nil)
	require.NoError(err)
	lo, hi := normalizeEdgeForTest(selected, retained)
	var linkOwner int64
	require.NoError(f.store.DB().QueryRowContext(ctx, f.store.Rebind(`
		SELECT identity_match_candidate_id FROM participant_links
		WHERE participant_a = ? AND participant_b = ?`), lo, hi).Scan(&linkOwner))
	require.Equal(candidate.ID, linkOwner, "accepted candidate must own the crossing link")

	_, err = f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         []int64{selected},
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-partial-accepted-candidate", Actor: "test",
	})
	require.NoError(err)
	reloaded, err := f.store.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateRejected, reloaded.State)
	require.NotNil(reloaded.DecidedBy)
	assert.Equal("user", *reloaded.DecidedBy)
	assert.False(linkedPair(t, f.store, selected, retained))
}

func TestSplitPersonMerge_PartialReplayExcludesSurvivorJournalRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	_, err := f.store.DB().ExecContext(ctx, f.store.Rebind(`UPDATE person_merge_rows
		SET origin_side = 'survivor'
		WHERE merge_id = ? AND table_name = 'person_names' AND original_row_id = ?`),
		f.merge.Merge.ID, f.absorbedNameID)
	require.NoError(err)
	request := store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         []int64{f.absorbedParticipants[0]},
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-partial-replay", Actor: "test",
	}
	first, err := f.store.SplitPersonMergeContext(ctx, request)
	require.NoError(err)
	replayed, err := f.store.SplitPersonMergeContext(ctx, request)
	require.NoError(err)
	assert.Equal(first.AmbiguousRows, replayed.AmbiguousRows)
	for _, row := range replayed.AmbiguousRows {
		if row.TableName == "person_names" && row.OriginalRowID != nil {
			assert.NotEqual(f.absorbedNameID, *row.OriginalRowID)
		}
	}
}

func TestSplitPersonMerge_CutsIdentityLinks(t *testing.T) {
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	result, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         []int64{f.absorbedParticipants[0]},
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-links", Actor: "test",
	})
	require.NoError(t, err)
	selectedCluster, err := f.store.ClusterMembers(result.NewPerson.ParticipantIDs[0])
	require.NoError(t, err)
	assert.Equal(t, []int64{result.NewPerson.ParticipantIDs[0]}, selectedCluster)
}

func TestSplitPersonMerge_SequentialPartialSplitsReleasePersonDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	first, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         []int64{f.absorbedParticipants[0]},
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-first-partial", Actor: "test",
	})
	require.NoError(err)
	second, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: first.SourcePerson.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         []int64{f.absorbedParticipants[1]},
		ExpectedSourceRevision: first.SourcePerson.Revision,
		IdempotencyKey:         "split-second-partial", Actor: "test",
	})
	require.NoError(err)
	assert.False(second.ExactReversal)
	assert.Equal("retired_uid_alias_unchanged", second.UIDAliasDisposition)
	require.NoError(f.store.DeletePersonContext(
		ctx, second.SourcePerson.ID, second.SourcePerson.Revision,
	))
}

func TestSplitPersonMerge_Validation(t *testing.T) {
	require := require.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	base := store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         []int64{f.absorbedParticipants[0]},
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-validation", Actor: "test",
	}
	stale := base
	stale.ExpectedSourceRevision++
	_, err := f.store.SplitPersonMergeContext(ctx, stale)
	require.ErrorIs(err, store.ErrPersonSplitRevision)
	unknown := base
	unknown.IdempotencyKey = "split-unknown"
	unknown.ParticipantIDs = []int64{f.survivorParticipant}
	_, err = f.store.SplitPersonMergeContext(ctx, unknown)
	require.ErrorIs(err, store.ErrPersonSplitParticipants)

	first, err := f.store.SplitPersonMergeContext(ctx, base)
	require.NoError(err)
	replayed, err := f.store.SplitPersonMergeContext(ctx, base)
	require.NoError(err)
	assertJSONEquivalent(t, first, replayed)
	changedActor := base
	changedActor.Actor = "different-actor"
	_, err = f.store.SplitPersonMergeContext(ctx, changedActor)
	require.ErrorIs(err, store.ErrPersonSplitIdempotency)
	changed := base
	changed.ParticipantIDs = []int64{f.absorbedParticipants[1]}
	_, err = f.store.SplitPersonMergeContext(ctx, changed)
	require.ErrorIs(err, store.ErrPersonSplitIdempotency)

	current, err := f.store.GetPersonContext(ctx, f.survivor.ID)
	require.NoError(err)
	alreadySplit := base
	alreadySplit.ExpectedSourceRevision = current.Revision
	alreadySplit.IdempotencyKey = "split-already-split"
	_, err = f.store.SplitPersonMergeContext(ctx, alreadySplit)
	require.ErrorIs(err, store.ErrPersonMergeAlreadySplit)

	mixed := alreadySplit
	mixed.IdempotencyKey = "split-mixed"
	mixed.ParticipantIDs = []int64{f.survivorParticipant, f.absorbedParticipants[1]}
	_, err = f.store.SplitPersonMergeContext(ctx, mixed)
	require.ErrorIs(err, store.ErrPersonSplitParticipants)
}

func TestSplitPersonMerge_RecomputesActivityAndContactState(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	survivorParticipant := f.EnsureParticipant(
		"split-activity-survivor@example.com", "Survivor", "example.com")
	absorbedParticipant := f.EnsureParticipant(
		"split-activity-absorbed@example.com", "Absorbed", "example.com")
	ownerParticipant := f.EnsureParticipant(
		"split-activity-owner@example.com", "Owner", "example.com")
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "split-activity-owner@example.com", "test"))
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)

	messageIDs := make([]int64, 0, 2)
	for label, senderID := range map[string]int64{
		"survivor": survivorParticipant,
		"absorbed": absorbedParticipant,
	} {
		message := f.NewMessage().
			WithSourceMessageID("split-activity-" + label).
			WithSentAt(time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)).
			Build()
		message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
		messageID, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		messageIDs = append(messageIDs, messageID)
		require.NoError(f.Store.ReplaceMessageRecipients(
			messageID, "from", []int64{senderID}, []string{label}))
		require.NoError(f.Store.ReplaceMessageRecipients(
			messageID, "to", []int64{ownerParticipant}, []string{"Owner"}))
	}
	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(ctx)
	require.NoError(err)
	survivor, err = f.Store.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-activity-merge", Actor: "test",
	})
	require.NoError(err)
	postMergeMessage := f.NewMessage().
		WithSourceMessageID("split-activity-absorbed-after-merge").
		WithSentAt(time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)).
		Build()
	postMergeMessage.SenderID = sql.NullInt64{Int64: absorbedParticipant, Valid: true}
	postMergeMessageID, err := f.Store.UpsertMessage(postMergeMessage)
	require.NoError(err)
	messageIDs = append(messageIDs, postMergeMessageID)
	require.NoError(f.Store.ReplaceMessageRecipients(
		postMergeMessageID, "from", []int64{absorbedParticipant}, []string{"Absorbed"}))
	require.NoError(f.Store.ReplaceMessageRecipients(
		postMergeMessageID, "to", []int64{ownerParticipant}, []string{"Owner"}))
	_, err = projector.RunOnce(ctx)
	require.NoError(err)

	result, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         []int64{absorbedParticipant},
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-activity", Actor: "test",
	})
	require.NoError(err)
	rows, err := f.Store.DB().QueryContext(ctx, f.Store.Rebind(`SELECT message_id, person_id
		FROM activity_event_persons WHERE message_id IN (?, ?, ?) ORDER BY message_id`),
		messageIDs[0], messageIDs[1], messageIDs[2])
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	linkedPeople := []int64{}
	for rows.Next() {
		var messageID, personID int64
		require.NoError(rows.Scan(&messageID, &personID))
		linkedPeople = append(linkedPeople, personID)
	}
	require.NoError(rows.Err())
	assert.ElementsMatch(t,
		[]int64{result.SourcePerson.ID, result.NewPerson.ID, result.NewPerson.ID}, linkedPeople)
	for personID, want := range map[int64]int64{
		result.SourcePerson.ID: 1,
		result.NewPerson.ID:    2,
	} {
		var count int64
		require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT interaction_count
			FROM person_contact_state WHERE person_id = ?`), personID).Scan(&count))
		assert.Equal(t, want, count)
	}
}

func TestSplitPersonMerge_RecomputesAbsorbedActivityRemovedByOwnerMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	ownerParticipant := f.EnsureParticipant(
		"split-owner-merge@example.com", "Owner", "example.com")
	absorbedParticipant := f.EnsureParticipant(
		"split-owner-merge-absorbed@example.com", "Absorbed", "example.com")
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "split-owner-merge@example.com", "test"))
	owner, _, err := f.Store.CreatePersonFromParticipant(ownerParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)

	message := f.NewMessage().
		WithSourceMessageID("split-owner-merge-activity").
		WithSentAt(time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)).
		Build()
	message.SenderID = sql.NullInt64{Int64: absorbedParticipant, Valid: true}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{absorbedParticipant}, []string{"Absorbed"}))
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{ownerParticipant}, []string{"Owner"}))
	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(ctx)
	require.NoError(err)
	var linkedPersonID int64
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT person_id
		FROM activity_event_persons WHERE message_id = ?`), messageID).Scan(&linkedPersonID))
	assert.Equal(absorbed.ID, linkedPersonID)

	owner, err = f.Store.GetPersonContext(ctx, owner.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: owner.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: owner.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "split-owner-activity-merge", Actor: "test",
	})
	require.NoError(err)
	var mergedLinks int
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT COUNT(*)
		FROM activity_event_persons WHERE message_id = ?`), messageID).Scan(&mergedLinks))
	assert.Zero(mergedLinks)

	split, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         []int64{absorbedParticipant},
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-owner-activity-split", Actor: "test",
	})
	require.NoError(err)
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT person_id
		FROM activity_event_persons WHERE message_id = ?`), messageID).Scan(&linkedPersonID))
	assert.Equal(split.NewPerson.ID, linkedPersonID)
}

func TestSplitPersonMerge_Rollback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	ctx := context.Background()
	if f.store.IsPostgreSQL() {
		_, err := f.store.DB().ExecContext(ctx, `
			CREATE FUNCTION fail_person_split_name() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'forced person split failure'; END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER fail_person_split_name BEFORE UPDATE ON person_names
			FOR EACH ROW EXECUTE FUNCTION fail_person_split_name();`)
		require.NoError(err)
	} else {
		_, err := f.store.DB().ExecContext(ctx, `CREATE TRIGGER fail_person_split_name
			BEFORE UPDATE ON person_names BEGIN
				SELECT RAISE(ABORT, 'forced person split failure');
			END`)
		require.NoError(err)
	}
	_, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         f.absorbedParticipants,
		ExpectedSourceRevision: f.survivor.Revision,
		IdempotencyKey:         "split-rollback", Actor: "test",
	})
	require.Error(err)
	assert.Contains(err.Error(), "forced person split failure")
	current, getErr := f.store.GetPersonContext(ctx, f.survivor.ID)
	require.NoError(getErr)
	assert.Equal(f.survivor.Revision, current.Revision)
	assert.ElementsMatch(append([]int64{f.survivorParticipant}, f.absorbedParticipants...),
		current.ParticipantIDs)
	var splitCount int
	require.NoError(f.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM person_splits`).Scan(&splitCount))
	assert.Zero(splitCount)
	var nameOwner int64
	require.NoError(f.store.DB().QueryRowContext(ctx,
		f.store.Rebind(`SELECT person_id FROM person_names WHERE id = ?`), f.absorbedNameID).Scan(&nameOwner))
	assert.Equal(f.survivor.ID, nameOwner)
}

func TestPersonSplitConcurrencySplitSplit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	release := personOperationContentionBarrier(t, f.store, 2)
	results := make(chan error, 2)
	for _, key := range []string{"concurrent-split-a", "concurrent-split-b"} {
		go func() {
			_, err := f.store.SplitPersonMergeContext(context.Background(), store.PersonSplitRequest{
				SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
				ParticipantIDs:         f.absorbedParticipants,
				ExpectedSourceRevision: f.survivor.Revision,
				IdempotencyKey:         key, Actor: "test",
			})
			results <- err
		}()
	}
	release()
	errs := []error{<-results, <-results}
	assert.Equal(1, countNilErrors(errs), "exactly one conflicting split may commit")
	for _, err := range errs {
		if err != nil {
			assert.True(errors.Is(err, store.ErrPersonSplitRevision) ||
				errors.Is(err, store.ErrPersonSplitParticipants) ||
				errors.Is(err, store.ErrPersonMergeAlreadySplit),
				"loser must report a typed stale-lineage error: %v", err)
		}
	}
	current, err := f.store.GetPersonContext(context.Background(), f.survivor.ID)
	require.NoError(err)
	assert.Equal(f.survivor.Revision+1, current.Revision)
	assertPersonSplitConcurrencyState(t, f.store, 1)
}

func TestPersonSplitConcurrencyProfileUpdate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSplitFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := f.store.SplitPersonMergeContext(context.Background(), store.PersonSplitRequest{
			SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
			ParticipantIDs:         f.absorbedParticipants,
			ExpectedSourceRevision: f.survivor.Revision,
			IdempotencyKey:         "concurrent-split-profile", Actor: "test",
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := f.store.UpdatePersonDisplayNameContext(
			context.Background(), f.survivor.ID, f.survivor.Revision, new("Concurrent Name"))
		results <- err
	}()
	close(start)
	errs := []error{<-results, <-results}
	assert.Equal(1, countNilErrors(errs), "split and stale profile update cannot both commit")
	current, err := f.store.GetPersonContext(context.Background(), f.survivor.ID)
	require.NoError(err)
	assert.Equal(f.survivor.Revision+1, current.Revision)
	var splitCount int
	require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM person_splits`).Scan(&splitCount))
	assert.Contains([]int{0, 1}, splitCount)
	assertPersonSplitConcurrencyState(t, f.store, splitCount)
}

func assertPersonSplitConcurrencyState(t *testing.T, st *store.Store, wantSplits int) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	var splitCount, orphanRows, orphanParticipants int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM person_splits`).Scan(&splitCount))
	assert.Equal(wantSplits, splitCount)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM person_merge_rows row_record
		LEFT JOIN person_merges merge_record ON merge_record.id = row_record.merge_id
		WHERE merge_record.id IS NULL`).Scan(&orphanRows))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM person_merge_participants lineage
		LEFT JOIN person_merges merge_record ON merge_record.id = lineage.merge_id
		WHERE merge_record.id IS NULL`).Scan(&orphanParticipants))
	assert.Zero(orphanRows)
	assert.Zero(orphanParticipants)
	assertSQLiteForeignKeysClean(t, st)
}
