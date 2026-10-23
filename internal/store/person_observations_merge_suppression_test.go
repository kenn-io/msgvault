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
