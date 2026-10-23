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
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] has a mortgage")
	before := observationGet(t, f.store, id)
	key := personobs.ContentKey(personobs.LevelExplicit, "", "[PERSON] swims")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor) VALUES(?,?,'owner-forgotten','owner')`), in.PersonID, key)
	require.NoError(t, err)
	observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] cycles")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	moved := observationGet(t, f.store, id)
	assert.Equal(t, f.bobPersonID, moved.PersonID)
	assert.Greater(t, moved.Sequence, before.Sequence)
	assert.Contains(t, moved.Categories, personobs.CategoryFinances)
	var personID int64
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT person_id FROM person_observation_suppressions WHERE content_key=?`), key).Scan(&personID))
	assert.Equal(t, f.bobPersonID, personID)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Equal(t, newID, restored.PersonID)
	assert.Equal(t, before.Sequence, restored.Sequence)
	assert.Contains(t, restored.Categories, personobs.CategoryFinances)
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT person_id FROM person_observation_suppressions WHERE content_key=?`), key).Scan(&personID))
	assert.Equal(t, newID, personID)
	var clock int64
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT last_sequence FROM person_observation_clocks WHERE person_id=?`), newID).Scan(&clock))
	assert.Equal(t, before.Sequence, clock)
}
func TestObservationMergeConvertsPinnedExplicitMachineRow(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), id)
	require.NoError(t, err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	moved := observationGet(t, f.store, id)
	assert.Equal(t, personobs.OriginOwner, moved.Origin)
	assert.Nil(t, moved.GenerationID)
	assert.Empty(t, moved.ProgramFingerprint)
	assert.Equal(t, personfacts.DirectOther, moved.Directness)
	assert.Empty(t, moved.Evidence)
	assert.True(t, moved.Pinned)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Equal(t, newID, restored.PersonID)
	assert.Equal(t, personobs.OriginOwner, restored.Origin)
	assert.Nil(t, restored.GenerationID)
	assert.Empty(t, restored.Evidence)
}
func TestObservationMergeCopiesPinnedDerivedAsOwnerExplicit(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET owner_state='confirmed' WHERE id=?`), derived)
	require.NoError(t, err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	_, err = f.store.GetPersonObservation(t.Context(), derived)
	require.Error(t, err)
	page, err := f.store.ListPersonObservations(t.Context(), f.bobPersonID, personobs.Filter{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	copy := page.Items[0]
	assert.Equal(t, personobs.LevelExplicit, copy.Level)
	assert.Equal(t, personobs.OriginOwner, copy.Origin)
	assert.Equal(t, personobs.OwnerStateConfirmed, copy.OwnerState)
	assert.NotEqual(t, derived, copy.ID)
	detail := observationGet(t, f.store, copy.ID)
	var converted bool
	for _, event := range detail.Events {
		if event.Event == "merge-converted" {
			converted = true
			assert.Contains(t, event.Detail, "deductive")
		}
	}
	assert.True(t, converted)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(t, newID, observationGet(t, f.store, copy.ID).PersonID)
	_, err = f.store.GetPersonObservation(t.Context(), root)
	assert.Error(t, err)
}
func TestObservationMergeCollisionAndSplitRestoresOwnerState(t *testing.T) {
	f, in, _ := observationBatchFixture(t, "I run")
	survivor := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] runs")
	absorbed := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] runs")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE,owner_state='confirmed' WHERE id=?`), absorbed)
	require.NoError(t, err)
	rejected := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	require.NoError(t, f.store.RejectObservationForTest(t.Context(), rejected))
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	kept := observationGet(t, f.store, survivor)
	assert.True(t, kept.Pinned)
	assert.Equal(t, personobs.OwnerStateConfirmed, kept.OwnerState)
	moved := observationGet(t, f.store, absorbed)
	assert.Equal(t, personobs.StatusSuperseded, moved.Status)
	assert.Equal(t, personobs.StatusReasonPersonMergeCollision, moved.StatusReason)
	assert.Equal(t, personobs.StatusRejected, observationGet(t, f.store, rejected).Status)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	kept = observationGet(t, f.store, survivor)
	assert.False(t, kept.Pinned)
	assert.Empty(t, kept.OwnerState)
	restored := observationGet(t, f.store, absorbed)
	assert.Equal(t, newID, restored.PersonID)
	assert.Equal(t, personobs.StatusActive, restored.Status)
	assert.True(t, restored.Pinned)
	assert.Equal(t, personobs.OwnerStateConfirmed, restored.OwnerState)
	var suppressed int
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND cleared_at IS NULL`), newID).Scan(&suppressed))
	assert.Equal(t, 1, suppressed)
}
func TestObservationMergeDiscardsUnpinnedMachineRows(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	_, err := f.store.GetPersonObservation(t.Context(), id)
	assert.Error(t, err)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	page, err := f.store.ListPersonObservations(t.Context(), newID, personobs.Filter{})
	require.NoError(t, err)
	assert.Empty(t, page.Items)
}
func TestObservationMergeRewritesSubjectPersonID(t *testing.T) {
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] knows [OWNER]")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET subject_person_id=? WHERE id=?`), in.PersonID, id)
	require.NoError(t, err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	assert.Equal(t, &f.bobPersonID, observationGet(t, f.store, id).SubjectPersonID)
	newID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(t, &newID, observationGet(t, f.store, id).SubjectPersonID)
}
func TestPersonMergeSnapshotCapturesObservationChildTables(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	in.Proposals[0].ModelCategories = []personobs.Category{personobs.CategoryHealth}
	root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	for _, table := range []string{"person_observation_evidence", "person_observation_premises", "person_observation_categories", "person_observation_events"} {
		var count int
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_merge_rows WHERE merge_id=? AND table_name=?`), merge.Merge.ID, table).Scan(&count))
		assert.Positive(t, count, table)
	}
}
