package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
	"testing"
)

func TestObservationTerminalRowsStillPropagateSensitivity(t *testing.T) {
	for _, status := range []personobs.Status{personobs.StatusRejected, personobs.StatusSuperseded} {
		t.Run(string(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			child := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
			grandchild := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] stays active", child)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET status=? WHERE id=?`), status, child)
			require.NoError(err)
			in.Proposals[0].ModelCategories = []personobs.Category{personobs.CategoryHealth}
			applyObservationInput(t, f.store, in, generation)
			assert.Equal(status, observationGet(t, f.store, child).Status)
			for _, id := range []int64{child, grandchild} {
				detail := observationGet(t, f.store, id)
				assert.Contains(detail.Categories, personobs.CategoryHealth)
				assert.True(detail.Sensitive)
			}
		})
	}
}
