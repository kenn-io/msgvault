package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestObservationMergeConversionDropsCitationCounts(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), id)
	require.NoError(t, err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	converted := observationGet(t, f.store, id)
	assert.Zero(t, converted.MentionCount)
	assert.Nil(t, converted.FirstSeenAt)
	assert.Nil(t, converted.LastSeenAt)
	observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Zero(t, restored.MentionCount)
	assert.Nil(t, restored.FirstSeenAt)
	assert.Nil(t, restored.LastSeenAt)
}
