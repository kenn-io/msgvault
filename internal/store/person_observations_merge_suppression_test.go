package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
)

func TestObservationMergeKeepsStricterSuppression(t *testing.T) {
	for _, tc := range []struct {
		name, absorbedReason string
		absorbedLater        bool
	}{{"wrong-beats-outdated", "wrong", false}, {"latest-outdated", "outdated", true}} {
		t.Run(tc.name, func(t *testing.T) {
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
				require.NoError(t, err)
			}
			merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
			var reason string
			var at time.Time
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT reason,rejected_at FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&reason, &at))
			assert.Equal(t, tc.absorbedReason, reason)
			assert.True(t, absorbedAt.Equal(at), "expected absorbed rejection time")
			newID := observationSplit(t, f.store, merge, f.aliceID)
			for _, row := range []struct {
				person int64
				reason string
			}{{f.bobPersonID, "outdated"}, {newID, tc.absorbedReason}} {
				require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT reason FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), row.person, key).Scan(&reason))
				assert.Equal(t, row.reason, reason)
			}
		})
	}
}
