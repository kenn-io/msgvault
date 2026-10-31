package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

func observationOwnerRow(t *testing.T, st *store.Store, personID int64, content string) int64 {
	t.Helper()
	result := applyObservationInput(t, st, personobs.BatchInput{PersonID: personID, Proposals: []personobs.ProposedObservation{{Level: personobs.LevelExplicit, Origin: personobs.OriginOwner, Kind: personobs.KindState, Content: content, ConfidenceBP: 1000}}}, 0)
	require.Len(t, result.Items, 1)
	require.Equal(t, "created", result.Items[0].Outcome)
	return result.Items[0].ObservationID
}
func observationMerge(t *testing.T, st *store.Store, survivorID, absorbedID int64) *store.PersonMergeResult {
	t.Helper()
	survivor, err := st.GetPersonContext(t.Context(), survivorID)
	require.NoError(t, err)
	absorbed, err := st.GetPersonContext(t.Context(), absorbedID)
	require.NoError(t, err)
	result, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivorID, AbsorbedID: absorbedID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "observation-merge", Actor: "owner"})
	require.NoError(t, err)
	return result
}
func observationSplit(t *testing.T, st *store.Store, merge *store.PersonMergeResult, participantID int64) int64 {
	t.Helper()
	source, err := st.GetPersonContext(t.Context(), merge.Person.ID)
	require.NoError(t, err)
	result, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{SourcePersonID: source.ID, ExpectedSourceRevision: source.Revision, MergeID: merge.Merge.ID, ParticipantIDs: []int64{participantID}, IdempotencyKey: "observation-split", Actor: "owner"})
	require.NoError(t, err)
	return result.NewPerson.ID
}
func TestObservationMergeMovesOwnerRowsAndSuppressions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] has a mortgage")
	before := observationGet(t, f.store, id)
	key := personobs.ContentKey(personobs.LevelExplicit, "", "[PERSON] swims")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor) VALUES(?,?,'owner-forgotten','owner')`), in.PersonID, key)
	require.NoError(err)
	observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] cycles")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	moved := observationGet(t, f.store, id)
	assert.Equal(f.bobPersonID, moved.PersonID)
	assert.Greater(moved.Sequence, before.Sequence)
	assert.Contains(moved.Categories, personobs.CategoryFinances)
	var personID int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT person_id FROM person_observation_suppressions WHERE content_key=?`), key).Scan(&personID))
	assert.Equal(f.bobPersonID, personID)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Equal(newID, restored.PersonID)
	assert.Equal(before.Sequence, restored.Sequence)
	assert.Contains(restored.Categories, personobs.CategoryFinances)
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT person_id FROM person_observation_suppressions WHERE content_key=?`), key).Scan(&personID))
	assert.Equal(newID, personID)
	var clock int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT last_sequence FROM person_observation_clocks WHERE person_id=?`), newID).Scan(&clock))
	assert.Equal(before.Sequence, clock)
}
func TestObservationMergeConvertsPinnedExplicitMachineRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), id)
	require.NoError(err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	moved := observationGet(t, f.store, id)
	assert.Equal(personobs.OriginOwner, moved.Origin)
	assert.Nil(moved.GenerationID)
	assert.Empty(moved.ProgramFingerprint)
	assert.Equal(personfacts.DirectOther, moved.Directness)
	assert.Empty(moved.Evidence)
	assert.True(moved.Pinned)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Equal(newID, restored.PersonID)
	assert.Equal(personobs.OriginOwner, restored.Origin)
	assert.Nil(restored.GenerationID)
	assert.Empty(restored.Evidence)
}
func TestObservationMergeCopiesPinnedDerivedAsOwnerExplicit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET owner_state='confirmed' WHERE id=?`), derived)
	require.NoError(err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	_, err = f.store.GetPersonObservation(t.Context(), derived)
	require.Error(err)
	page, err := f.store.ListPersonObservations(t.Context(), f.bobPersonID, personobs.Filter{})
	require.NoError(err)
	require.Len(page.Items, 1)
	copyObservation := page.Items[0]
	assert.Equal(personobs.LevelExplicit, copyObservation.Level)
	assert.Equal(personobs.OriginOwner, copyObservation.Origin)
	assert.Equal(personobs.OwnerStateConfirmed, copyObservation.OwnerState)
	assert.NotEqual(derived, copyObservation.ID)
	detail := observationGet(t, f.store, copyObservation.ID)
	var converted bool
	for _, event := range detail.Events {
		if event.Event == "merge-converted" {
			converted = true
			assert.Contains(event.Detail, "deductive")
		}
	}
	assert.True(converted)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(newID, observationGet(t, f.store, copyObservation.ID).PersonID)
	_, err = f.store.GetPersonObservation(t.Context(), root)
	require.Error(err)
}
func TestObservationMergeCollisionAndSplitRestoresOwnerState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	survivor := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] runs")
	absorbed := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] runs")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE,owner_state='confirmed' WHERE id=?`), absorbed)
	require.NoError(err)
	rejected := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	require.NoError(f.store.RejectObservationForTest(t.Context(), rejected))
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	kept := observationGet(t, f.store, survivor)
	assert.True(kept.Pinned)
	assert.Equal(personobs.OwnerStateConfirmed, kept.OwnerState)
	moved := observationGet(t, f.store, absorbed)
	assert.Equal(personobs.StatusSuperseded, moved.Status)
	assert.Equal(personobs.StatusReasonPersonMergeCollision, moved.StatusReason)
	assert.Equal(personobs.StatusRejected, observationGet(t, f.store, rejected).Status)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	kept = observationGet(t, f.store, survivor)
	assert.False(kept.Pinned)
	assert.Empty(kept.OwnerState)
	restored := observationGet(t, f.store, absorbed)
	assert.Equal(newID, restored.PersonID)
	assert.Equal(personobs.StatusActive, restored.Status)
	assert.True(restored.Pinned)
	assert.Equal(personobs.OwnerStateConfirmed, restored.OwnerState)
	var suppressed int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND cleared_at IS NULL`), newID).Scan(&suppressed))
	assert.Equal(1, suppressed)
}
func TestObservationMergeDiscardsUnpinnedMachineRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	_, err := f.store.GetPersonObservation(t.Context(), id)
	require.Error(err)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	page, err := f.store.ListPersonObservations(t.Context(), newID, personobs.Filter{})
	require.NoError(err)
	assert.Empty(page.Items)
}
func TestObservationMergeRewritesSubjectPersonID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] knows [OWNER]")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET subject_person_id=? WHERE id=?`), in.PersonID, id)
	require.NoError(err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	assert.Equal(&f.bobPersonID, observationGet(t, f.store, id).SubjectPersonID)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(&newID, observationGet(t, f.store, id).SubjectPersonID)
}

func TestObservationMergeCollisionJournalsCategoriesForSurvivorSubjectReference(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	content := "[PERSON] knows [OWNER]"
	kept := observationOwnerRow(t, f.store, f.bobPersonID, content)
	incoming := observationOwnerRow(t, f.store, in.PersonID, content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_observations SET subject_person_id=? WHERE id=?`), in.PersonID, kept)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`INSERT INTO person_observation_categories(observation_id,category,source) VALUES(?,'health','owner')`), incoming)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_observations SET sensitive=TRUE WHERE id=?`), incoming)
	require.NoError(err)

	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	merged := observationGet(t, f.store, kept)
	assert.True(merged.Sensitive)
	assert.Contains(merged.Categories, personobs.CategoryHealth)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, kept)
	assert.Equal(f.bobPersonID, restored.PersonID)
	assert.Equal(&newPersonID, restored.SubjectPersonID)
	assert.False(restored.Sensitive)
	assert.NotContains(restored.Categories, personobs.CategoryHealth)
}

func TestPersonMergeSnapshotCapturesObservationChildTables(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	in.Proposals[0].ModelCategories = []personobs.Category{personobs.CategoryHealth}
	root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	for _, table := range []string{"person_observation_evidence", "person_observation_premises", "person_observation_categories", "person_observation_events"} {
		var count int
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_merge_rows WHERE merge_id=? AND table_name=?`), merge.Merge.ID, table).Scan(&count))
		assert.Positive(count, table)
	}
}
