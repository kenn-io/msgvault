package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestObservationMergeConversionDropsCitationCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), id)
	require.NoError(err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	converted := observationGet(t, f.store, id)
	assert.Zero(converted.MentionCount)
	assert.Nil(converted.FirstSeenAt)
	assert.Nil(converted.LastSeenAt)
	observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, id)
	assert.Zero(restored.MentionCount)
	assert.Nil(restored.FirstSeenAt)
	assert.Nil(restored.LastSeenAt)
}
