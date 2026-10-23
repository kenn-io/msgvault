package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

func TestObservationMergeKeepsStricterSuppression(t *testing.T) {
	for _, tc := range []struct {
		name, absorbedReason string
		absorbedLater        bool
	}{{"wrong-beats-outdated", "wrong", false}, {"latest-outdated", "outdated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, _ := observationBatchFixture(t, "I run")
			key := personobs.ContentKey(personobs.LevelExplicit, "", "[PERSON] swims")
			survivorAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			absorbedAt := survivorAt.Add(-time.Hour)
			if tc.absorbedLater {
				absorbedAt = survivorAt.Add(time.Hour)
			}
			for _, row := range []struct {
				person int64
				reason string
				at     time.Time
			}{{f.bobPersonID, "outdated", survivorAt}, {in.PersonID, tc.absorbedReason, absorbedAt}} {
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,?,'owner',?)`), row.person, key, row.reason, row.at)
				require.NoError(err)
			}
			merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
			var reason string
			var at time.Time
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT reason,rejected_at FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&reason, &at))
			assert.Equal(tc.absorbedReason, reason)
			assert.True(absorbedAt.Equal(at), "expected absorbed rejection time")
			newID := observationSplit(t, f.store, merge, f.aliceID)
			for _, row := range []struct {
				person int64
				reason string
			}{{f.bobPersonID, "outdated"}, {newID, tc.absorbedReason}} {
				require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT reason FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), row.person, key).Scan(&reason))
				assert.Equal(row.reason, reason)
			}
		})
	}
}

func TestObservationSplitCopiesRejectionOfSharedSurvivorObservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	content := "[PERSON] remembers the red umbrella"
	absorbed := observationOwnerRow(t, f.store, in.PersonID, content)
	survivor := observationOwnerRow(t, f.store, f.bobPersonID, content)
	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	require.NoError(f.store.RejectObservationForTest(t.Context(), survivor))

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(f.bobPersonID, observationGet(t, f.store, survivor).PersonID)
	assert.Equal(personobs.StatusRejected, observationGet(t, f.store, survivor).Status)
	assert.Equal(newPersonID, observationGet(t, f.store, absorbed).PersonID)

	for _, personID := range []int64{f.bobPersonID, newPersonID} {
		var reason string
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT reason FROM person_observation_suppressions
			WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), personID, key).Scan(&reason))
		assert.Equal("owner-rejected", reason)
	}
}

func TestObservationSplitRestoresAbsorbedSuppressionAfterEarlierMergeSplit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	third := mustPromotedPerson(t, f.store, "third@example.test", "Third")
	content := "[PERSON] remembers the old station"
	observation := observationOwnerRow(t, f.store, third.ID, content)

	mergePerson := func(absorbedID int64, idempotencyKey string) *store.PersonMergeResult {
		survivor, err := f.store.GetPersonContext(t.Context(), f.bobPersonID)
		require.NoError(err)
		absorbed, err := f.store.GetPersonContext(t.Context(), absorbedID)
		require.NoError(err)
		merged, err := f.store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: f.bobPersonID, AbsorbedID: absorbedID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           idempotencyKey, Actor: "owner",
		})
		require.NoError(err)
		return merged
	}
	splitPerson := func(merge *store.PersonMergeResult, participantID int64, idempotencyKey string) int64 {
		source, err := f.store.GetPersonContext(t.Context(), merge.Person.ID)
		require.NoError(err)
		split, err := f.store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
			SourcePersonID: merge.Person.ID, ExpectedSourceRevision: source.Revision,
			MergeID: merge.Merge.ID, ParticipantIDs: []int64{participantID},
			IdempotencyKey: idempotencyKey, Actor: "owner",
		})
		require.NoError(err)
		return split.NewPerson.ID
	}

	mergeAlice := mergePerson(in.PersonID, "observation-merge-alice")
	mergeThird := mergePerson(third.ID, "observation-merge-third")
	require.NoError(f.store.RejectObservationForTest(t.Context(), observation))
	_ = splitPerson(mergeAlice, f.aliceID, "observation-split-alice")
	require.NotEmpty(third.ParticipantIDs)
	thirdID := splitPerson(mergeThird, third.ParticipantIDs[0], "observation-split-third")

	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	var sourceSuppressions, restoredSuppressions int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&sourceSuppressions))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), thirdID, key).Scan(&restoredSuppressions))
	assert.Zero(sourceSuppressions)
	assert.Equal(1, restoredSuppressions)
	restored := observationGet(t, f.store, observation)
	assert.Equal(thirdID, restored.PersonID)
	assert.Equal(personobs.StatusRejected, restored.Status)
}

func TestObservationSplitPreservesSuppressionForPostMergeSurvivorObservationWithAbsorbedKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	seed := applyObservationInput(t, f.store, in, generation)
	require.Len(seed.Items, 1)
	require.Equal("created", seed.Items[0].Outcome)

	content := in.Proposals[0].Content
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	survivorObservation := observationOwnerRow(t, f.store, f.bobPersonID, content)
	require.NoError(f.store.RejectObservationForTest(t.Context(), survivorObservation))

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	rejected := observationGet(t, f.store, survivorObservation)
	assert.Equal(f.bobPersonID, rejected.PersonID)
	assert.Equal(personobs.StatusRejected, rejected.Status)
	assert.Equal(personobs.StatusReasonOwnerRejected, rejected.StatusReason)

	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	for _, personID := range []int64{f.bobPersonID, newPersonID} {
		var reason string
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT reason FROM person_observation_suppressions
			WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), personID, key).Scan(&reason))
		assert.Equal("owner-rejected", reason)
	}
}

func TestObservationSplitKeepsNestedMergeSurvivorSuppression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	charlie := mustPromotedPerson(t, f.store, "charlie@example.test", "Charlie")
	aliceObservation := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] prefers tea")
	bobObservation := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] collects postcards")

	merge := func(survivorID, absorbedID int64, key string) *store.PersonMergeResult {
		survivor, err := f.store.GetPersonContext(t.Context(), survivorID)
		require.NoError(err)
		absorbed, err := f.store.GetPersonContext(t.Context(), absorbedID)
		require.NoError(err)
		result, err := f.store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: survivorID, AbsorbedID: absorbedID,
			ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey: key, Actor: "owner",
		})
		require.NoError(err)
		return result
	}

	aliceAndBob := merge(in.PersonID, f.bobPersonID, "merge-alice-and-bob")
	charlieAndAlice := merge(charlie.ID, in.PersonID, "merge-charlie-and-alice")
	require.Equal(charlie.ID, charlieAndAlice.Person.ID)
	require.Equal(charlie.ID, observationGet(t, f.store, aliceObservation).PersonID)
	require.Equal(charlie.ID, observationGet(t, f.store, bobObservation).PersonID)
	require.NoError(f.store.RejectObservationForTest(t.Context(), aliceObservation))

	source, err := f.store.GetPersonContext(t.Context(), charlie.ID)
	require.NoError(err)
	split, err := f.store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: charlie.ID, ExpectedSourceRevision: source.Revision,
		MergeID: aliceAndBob.Merge.ID, ParticipantIDs: []int64{f.bobID},
		IdempotencyKey: "split-bob-from-charlie", Actor: "owner",
	})
	require.NoError(err)

	assert.Equal(charlie.ID, observationGet(t, f.store, aliceObservation).PersonID)
	assert.Equal(split.NewPerson.ID, observationGet(t, f.store, bobObservation).PersonID)
	key := personobs.ContentKey(personobs.LevelExplicit, "", "[PERSON] prefers tea")
	for _, row := range []struct {
		personID int64
		want     int
	}{{charlie.ID, 1}, {split.NewPerson.ID, 0}} {
		var count int
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT COUNT(*) FROM person_observation_suppressions
			WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), row.personID, key).Scan(&count))
		assert.Equal(row.want, count)
	}
}
