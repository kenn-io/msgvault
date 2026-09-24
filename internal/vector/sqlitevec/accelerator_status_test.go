//go:build sqlite_vec

package sqlitevec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveAcceleratorAbsentReportsNil(t *testing.T) {
	backend := openOptimizeBackend(t, 4)
	generationID, err := backend.CreateGeneration(t.Context(), "model", 4, "model:test")
	require.NoError(t, err)

	status, err := backend.EffectiveAccelerator(t.Context(), generationID)
	require.NoError(t, err)
	assert.Nil(t, status, "a never-optimized generation has no accelerator status")
}

func TestEffectiveAcceleratorStoredReadyEligibleStaysReady(t *testing.T) {
	backend := openOptimizeBackend(t, 4)
	generationID := seedOptimizeVectors(t, backend, 2, 4)
	tableName := installReadyFlatAccelerator(t, backend, generationID, 4)

	status, err := backend.EffectiveAccelerator(t.Context(), generationID)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, AcceleratorReady, status.State)
	assert.Equal(t, tableName, status.TableName)
	assert.Equal(t, int64(2), status.IndexedCount)
}

func TestEffectiveAcceleratorIneligibleStoredReadyReportsStale(t *testing.T) {
	cases := map[string]func(backend *Backend, generationID int, tableName string) error{
		"vector_revision_mismatch": func(backend *Backend, generationID int, _ string) error {
			// Fires the embeddings_vector_identity_update maintenance trigger,
			// which bumps the generation's vector_revision past the
			// accelerator's stored source_revision.
			_, err := backend.db.Exec(`UPDATE embeddings
				SET message_id = message_id WHERE generation_id = ?`, generationID)
			return err
		},
		"indexed_count_mismatch": func(backend *Backend, generationID int, _ string) error {
			// Fires the embeddings_vector_delete maintenance trigger, which
			// decrements the generation's embedding_count below the
			// accelerator's indexed_count.
			_, err := backend.db.Exec(`DELETE FROM embeddings
				WHERE generation_id = ? AND message_id = 1`, generationID)
			return err
		},
		"vec1_version_mismatch": func(backend *Backend, generationID int, _ string) error {
			_, err := backend.db.Exec(`UPDATE vector_accelerators
				SET vec1_version = '0.0' WHERE generation_id = ?`, generationID)
			return err
		},
		"missing_physical_table": func(backend *Backend, _ int, tableName string) error {
			_, err := backend.db.Exec(`DROP TABLE ` + tableName)
			return err
		},
	}
	for name, invalidate := range cases {
		t.Run(name, func(t *testing.T) {
			backend := openOptimizeBackend(t, 4)
			generationID := seedOptimizeVectors(t, backend, 2, 4)
			tableName := installReadyFlatAccelerator(t, backend, generationID, 4)
			require.NoError(t, invalidate(backend, int(generationID), tableName))

			status, err := backend.EffectiveAccelerator(t.Context(), generationID)
			require.NoError(t, err)
			require.NotNil(t, status)
			assert.Equal(t, AcceleratorStale, status.State,
				"a stored-ready accelerator search cannot serve must display stale")
			assert.Equal(t, tableName, status.TableName,
				"display metadata must be preserved alongside the corrected state")

			raw, err := backend.Accelerator(t.Context(), generationID)
			require.NoError(t, err)
			require.NotNil(t, raw)
			assert.Equal(t, AcceleratorReady, raw.State,
				"the raw stored state must be left untouched by the display read")

			_, ready, err := backend.readyAccelerator(t.Context(), generationID, 4)
			require.NoError(t, err)
			assert.False(t, ready,
				"search must keep failing open to exact for the same state")
		})
	}
}

func TestEffectiveAcceleratorNonReadyStatesPassThrough(t *testing.T) {
	cases := map[string]AcceleratorState{
		"building": AcceleratorBuilding,
		"stale":    AcceleratorStale,
	}
	for name, stored := range cases {
		t.Run(name, func(t *testing.T) {
			backend := openOptimizeBackend(t, 4)
			generationID := seedOptimizeVectors(t, backend, 2, 4)
			installReadyFlatAccelerator(t, backend, generationID, 4)
			_, err := backend.db.Exec(`UPDATE vector_accelerators SET state = ?
				WHERE generation_id = ?`, string(stored), int64(generationID))
			require.NoError(t, err)

			status, err := backend.EffectiveAccelerator(t.Context(), generationID)
			require.NoError(t, err)
			require.NotNil(t, status)
			assert.Equal(t, stored, status.State,
				"non-ready stored states must display unchanged")
		})
	}
}
