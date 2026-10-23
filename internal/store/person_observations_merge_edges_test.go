package store_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

func TestObservationSplitDerivedCopyCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), derived)
	require.NoError(err)
	owner := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] exercises")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(newID, observationGet(t, f.store, owner).PersonID)
	page, err := f.store.ListPersonObservations(t.Context(), newID, personobs.Filter{Statuses: []personobs.Status{personobs.StatusActive}})
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal(owner, page.Items[0].ID)
	all, err := f.store.ListPersonObservations(t.Context(), newID, personobs.Filter{})
	require.NoError(err)
	assert.Len(all.Items, 2)
}
func TestObservationSplitKeepsPostMergeOwnerDecision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET owner_state='confirmed' WHERE id=?`), id)
	require.NoError(err)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Equal(newID, restored.PersonID)
	assert.Equal(personobs.OwnerStateConfirmed, restored.OwnerState)
}
func TestObservationSplitRestoresDuplicateActiveSuppressions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	key := personobs.ContentKey(personobs.LevelExplicit, "", "[PERSON] swims")
	for _, personID := range []int64{in.PersonID, f.bobPersonID} {
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor) VALUES(?,?,'wrong','owner')`), personID, key)
		require.NoError(err)
	}
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	for _, personID := range []int64{newID, f.bobPersonID} {
		var count int
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), personID, key).Scan(&count))
		assert.Equal(1, count)
	}
}

func TestObservationMergeOwnerReinforcedByMachineDropsCascadingCitations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, in.Proposals[0].Content)
	result := applyObservationInput(t, f.store, in, generation)
	require.Equal(id, result.Items[0].ObservationID)
	require.Len(observationGet(t, f.store, id).Evidence, 1)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	moved := observationGet(t, f.store, id)
	assert.Equal(personobs.OriginOwner, moved.Origin)
	assert.Empty(moved.Evidence)
	assert.Zero(moved.MentionCount)
	assert.Nil(moved.FirstSeenAt)
	assert.Nil(moved.LastSeenAt)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Equal(newID, restored.PersonID)
	assert.Empty(restored.Evidence)
	assert.Zero(restored.MentionCount)
	assert.Nil(restored.FirstSeenAt)
	assert.Nil(restored.LastSeenAt)
}

func TestObservationSplitMovesAllEventOwnership(t *testing.T) {
	for _, machine := range []bool{false, true} {
		t.Run(strconv.FormatBool(machine), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			var id int64
			if machine {
				id = applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), id)
				require.NoError(err)
			} else {
				id = observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
			}
			merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
			require.NoError(f.store.RejectObservationForTest(t.Context(), id))
			newID := observationSplit(t, f.store, merge, f.aliceID)
			assert.Equal(personobs.StatusRejected, observationGet(t, f.store, id).Status)
			var wrongOwner int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE observation_id=? AND person_id<>?`), id, newID).Scan(&wrongOwner))
			assert.Zero(wrongOwner)
		})
	}
}

func TestObservationSplitDerivedCopyResolvesMergeOnlyCollision(t *testing.T) {
	for _, rejectAfterMerge := range []bool{false, true} {
		t.Run(strconv.FormatBool(rejectAfterMerge), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), derived)
			require.NoError(err)
			kept := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] exercises")
			before := observationGet(t, f.store, kept)
			merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
			var copyID int64
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT observation_id FROM person_observation_events WHERE event='merge-converted' AND related_observation_id=?`), derived).Scan(&copyID))
			require.Equal(personobs.StatusSuperseded, observationGet(t, f.store, copyID).Status)
			assert.True(observationGet(t, f.store, kept).UpdatedAt.After(before.UpdatedAt))
			var decisions int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE observation_id=? AND event='merge-moved' AND detail LIKE '%;owner-collision'`), kept).Scan(&decisions))
			assert.Equal(1, decisions)
			if rejectAfterMerge {
				require.NoError(f.store.RejectObservationForTest(t.Context(), copyID))
			}
			newID := observationSplit(t, f.store, merge, f.aliceID)
			copyObservation := observationGet(t, f.store, copyID)
			assert.Equal(newID, copyObservation.PersonID)
			if rejectAfterMerge {
				assert.Equal(personobs.StatusRejected, copyObservation.Status)
				copyKey := personobs.ContentKey(personobs.LevelExplicit, "", "[PERSON] exercises")
				derivedKey := personobs.ContentKey(personobs.LevelDeductive, "", "[PERSON] exercises")
				var sourceCopySuppressions, restoredCopySuppressions, restoredDerivedSuppressions int
				require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
					SELECT COUNT(*) FROM person_observation_suppressions
					WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, copyKey).Scan(&sourceCopySuppressions))
				require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
					SELECT COUNT(*) FROM person_observation_suppressions
					WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), newID, copyKey).Scan(&restoredCopySuppressions))
				require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
					SELECT COUNT(*) FROM person_observation_suppressions
					WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), newID, derivedKey).Scan(&restoredDerivedSuppressions))
				assert.Zero(sourceCopySuppressions)
				assert.Equal(1, restoredCopySuppressions)
				assert.Zero(restoredDerivedSuppressions)
			} else {
				assert.Equal(personobs.StatusActive, copyObservation.Status)
				assert.Empty(copyObservation.StatusReason)
			}
			assert.Equal(personobs.StatusActive, observationGet(t, f.store, kept).Status)
		})
	}
}

func TestObservationSplitPreservesInboundMachineProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET subject_person_id=?,support_flag='support_lapsed' WHERE id=?`), f.bobPersonID, id)
	require.NoError(err)
	before := observationGet(t, f.store, id)
	merge := observationMerge(t, f.store, in.PersonID, f.bobPersonID)
	newID := observationSplit(t, f.store, merge, f.bobID)
	restored := observationGet(t, f.store, id)
	assert.Equal(in.PersonID, restored.PersonID)
	assert.Equal(&newID, restored.SubjectPersonID)
	assert.Equal(before.Origin, restored.Origin)
	assert.Equal(before.GenerationID, restored.GenerationID)
	assert.Equal(before.ProgramFingerprint, restored.ProgramFingerprint)
	assert.Equal(before.Directness, restored.Directness)
	assert.Equal(before.MentionCount, restored.MentionCount)
	assert.Equal(before.SupportFlag, restored.SupportFlag)
	assert.Equal(before.FirstSeenAt, restored.FirstSeenAt)
	assert.Equal(before.LastSeenAt, restored.LastSeenAt)
}

func TestObservationSplitDerivedCopyRestoresSubjectUnlessEdited(t *testing.T) {
	for _, editAfterMerge := range []bool{false, true} {
		t.Run(strconv.FormatBool(editAfterMerge), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET subject_person_id=?,pinned=TRUE WHERE id=?`), in.PersonID, derived)
			require.NoError(err)
			merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
			var copyID int64
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT observation_id FROM person_observation_events WHERE event='merge-converted' AND related_observation_id=?`), derived).Scan(&copyID))
			assert.Equal(&f.bobPersonID, observationGet(t, f.store, copyID).SubjectPersonID)
			var editedID int64
			if editAfterMerge {
				participant, err := f.store.EnsureParticipant("charlie@example.test", "Charlie", "example.test")
				require.NoError(err)
				person, _, err := f.store.CreatePersonFromParticipant(participant)
				require.NoError(err)
				editedID = person.ID
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET subject_person_id=? WHERE id=?`), editedID, copyID)
				require.NoError(err)
			}
			newID := observationSplit(t, f.store, merge, f.aliceID)
			want := newID
			if editAfterMerge {
				want = editedID
			}
			assert.Equal(&want, observationGet(t, f.store, copyID).SubjectPersonID)
		})
	}
}

func TestObservationSplitWithdrawsDeductionWithMissingCrossingConjunct(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	absorbedPremise := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] runs")
	survivorPremise := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] swims")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)

	programFingerprint := in.ProgramFingerprint
	deductionGeneration := insertPersonFactSchemaGeneration(t, f.store, f.bobPersonID, "split-conjunct-survivor")
	derivedInput := personobs.BatchInput{
		PersonID: f.bobPersonID, ProgramFingerprint: programFingerprint,
		Generation: &personfacts.GenerationInput{PersonID: f.bobPersonID, ProgramFingerprint: programFingerprint},
		Proposals: []personobs.ProposedObservation{{
			Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning, Kind: personobs.KindState,
			Content: "[PERSON] both runs and swims", ConfidenceBP: 800,
			Premises: []personobs.PremiseInput{
				{ID: absorbedPremise, Role: personobs.PremiseRoleSupport},
				{ID: survivorPremise, Role: personobs.PremiseRoleSupport},
			},
		}},
	}
	derivedResult := applyObservationInput(t, f.store, derivedInput, deductionGeneration)
	require.Len(derivedResult.Items, 1)
	require.Equal("created", derivedResult.Items[0].Outcome)
	derived := derivedResult.Items[0].ObservationID

	observationSplit(t, f.store, merge, f.aliceID)
	after := observationGet(t, f.store, derived)
	assert.Equal(personobs.StatusUnsupported, after.Status)
	assert.Equal(personobs.StatusReasonPremisesWithdrawn, after.StatusReason)
	assert.Empty(after.Premises)
}
