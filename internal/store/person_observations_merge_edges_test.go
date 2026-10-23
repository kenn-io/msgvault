package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
)

func TestObservationSplitDerivedCopyCollision(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), derived)
	require.NoError(t, err)
	owner := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] exercises")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(t, newID, observationGet(t, f.store, owner).PersonID)
	page, err := f.store.ListPersonObservations(t.Context(), newID, personobs.Filter{Statuses: []personobs.Status{personobs.StatusActive}})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, owner, page.Items[0].ID)
	all, err := f.store.ListPersonObservations(t.Context(), newID, personobs.Filter{})
	require.NoError(t, err)
	assert.Len(t, all.Items, 2)
}
func TestObservationSplitKeepsPostMergeOwnerDecision(t *testing.T) {
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET owner_state='confirmed' WHERE id=?`), id)
	require.NoError(t, err)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Equal(t, newID, restored.PersonID)
	assert.Equal(t, personobs.OwnerStateConfirmed, restored.OwnerState)
}
func TestObservationSplitRestoresDuplicateActiveSuppressions(t *testing.T) {
	f, in, _ := observationBatchFixture(t, "I run")
	key := personobs.ContentKey(personobs.LevelExplicit, "", "[PERSON] swims")
	for _, personID := range []int64{in.PersonID, f.bobPersonID} {
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor) VALUES(?,?,'wrong','owner')`), personID, key)
		require.NoError(t, err)
	}
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	for _, personID := range []int64{newID, f.bobPersonID} {
		var count int
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), personID, key).Scan(&count))
		assert.Equal(t, 1, count)
	}
}

func TestObservationMergeOwnerReinforcedByMachineDropsCascadingCitations(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, in.Proposals[0].Content)
	result := applyObservationInput(t, f.store, in, generation)
	require.Equal(t, id, result.Items[0].ObservationID)
	require.Len(t, observationGet(t, f.store, id).Evidence, 1)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	moved := observationGet(t, f.store, id)
	assert.Equal(t, personobs.OriginOwner, moved.Origin)
	assert.Empty(t, moved.Evidence)
	assert.Zero(t, moved.MentionCount)
	assert.Nil(t, moved.FirstSeenAt)
	assert.Nil(t, moved.LastSeenAt)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Equal(t, newID, restored.PersonID)
	assert.Empty(t, restored.Evidence)
	assert.Zero(t, restored.MentionCount)
	assert.Nil(t, restored.FirstSeenAt)
	assert.Nil(t, restored.LastSeenAt)
}

func TestObservationSplitMovesAllEventOwnership(t *testing.T) {
	for _, machine := range []bool{false, true} {
		t.Run(fmt.Sprint(machine), func(t *testing.T) {
			f, in, generation := observationBatchFixture(t, "I run")
			var id int64
			if machine {
				id = applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), id)
				require.NoError(t, err)
			} else {
				id = observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
			}
			merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
			require.NoError(t, f.store.RejectObservationForTest(t.Context(), id))
			newID := observationSplit(t, f.store, merge, f.aliceID)
			assert.Equal(t, personobs.StatusRejected, observationGet(t, f.store, id).Status)
			var wrongOwner int
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE observation_id=? AND person_id<>?`), id, newID).Scan(&wrongOwner))
			assert.Zero(t, wrongOwner)
		})
	}
}

func TestObservationSplitDerivedCopyResolvesMergeOnlyCollision(t *testing.T) {
	for _, rejectAfterMerge := range []bool{false, true} {
		t.Run(fmt.Sprint(rejectAfterMerge), func(t *testing.T) {
			f, in, generation := observationBatchFixture(t, "I run")
			root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), derived)
			require.NoError(t, err)
			kept := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] exercises")
			before := observationGet(t, f.store, kept)
			merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
			var copyID int64
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT observation_id FROM person_observation_events WHERE event='merge-converted' AND related_observation_id=?`), derived).Scan(&copyID))
			require.Equal(t, personobs.StatusSuperseded, observationGet(t, f.store, copyID).Status)
			assert.True(t, observationGet(t, f.store, kept).UpdatedAt.After(before.UpdatedAt))
			var decisions int
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE observation_id=? AND event='merge-moved' AND detail='decisions'`), kept).Scan(&decisions))
			assert.Equal(t, 1, decisions)
			if rejectAfterMerge {
				require.NoError(t, f.store.RejectObservationForTest(t.Context(), copyID))
			}
			newID := observationSplit(t, f.store, merge, f.aliceID)
			copy := observationGet(t, f.store, copyID)
			assert.Equal(t, newID, copy.PersonID)
			if rejectAfterMerge {
				assert.Equal(t, personobs.StatusRejected, copy.Status)
			} else {
				assert.Equal(t, personobs.StatusActive, copy.Status)
				assert.Empty(t, copy.StatusReason)
			}
			assert.Equal(t, personobs.StatusActive, observationGet(t, f.store, kept).Status)
		})
	}
}

func TestObservationSplitPreservesInboundMachineProvenance(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET subject_person_id=?,support_flag='support_lapsed' WHERE id=?`), f.bobPersonID, id)
	require.NoError(t, err)
	before := observationGet(t, f.store, id)
	merge := observationMerge(t, f.store, in.PersonID, f.bobPersonID)
	newID := observationSplit(t, f.store, merge, f.bobID)
	restored := observationGet(t, f.store, id)
	assert.Equal(t, in.PersonID, restored.PersonID)
	assert.Equal(t, &newID, restored.SubjectPersonID)
	assert.Equal(t, before.Origin, restored.Origin)
	assert.Equal(t, before.GenerationID, restored.GenerationID)
	assert.Equal(t, before.ProgramFingerprint, restored.ProgramFingerprint)
	assert.Equal(t, before.Directness, restored.Directness)
	assert.Equal(t, before.MentionCount, restored.MentionCount)
	assert.Equal(t, before.SupportFlag, restored.SupportFlag)
	assert.Equal(t, before.FirstSeenAt, restored.FirstSeenAt)
	assert.Equal(t, before.LastSeenAt, restored.LastSeenAt)
}

func TestObservationSplitDerivedCopyRestoresSubjectUnlessEdited(t *testing.T) {
	for _, editAfterMerge := range []bool{false, true} {
		t.Run(fmt.Sprint(editAfterMerge), func(t *testing.T) {
			f, in, generation := observationBatchFixture(t, "I run")
			root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET subject_person_id=?,pinned=TRUE WHERE id=?`), in.PersonID, derived)
			require.NoError(t, err)
			merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
			var copyID int64
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT observation_id FROM person_observation_events WHERE event='merge-converted' AND related_observation_id=?`), derived).Scan(&copyID))
			assert.Equal(t, &f.bobPersonID, observationGet(t, f.store, copyID).SubjectPersonID)
			var editedID int64
			if editAfterMerge {
				participant, err := f.store.EnsureParticipant("charlie@example.test", "Charlie", "example.test")
				require.NoError(t, err)
				person, _, err := f.store.CreatePersonFromParticipant(participant)
				require.NoError(t, err)
				editedID = person.ID
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET subject_person_id=? WHERE id=?`), editedID, copyID)
				require.NoError(t, err)
			}
			newID := observationSplit(t, f.store, merge, f.aliceID)
			want := newID
			if editAfterMerge {
				want = editedID
			}
			assert.Equal(t, &want, observationGet(t, f.store, copyID).SubjectPersonID)
		})
	}
}
