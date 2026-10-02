//go:build sqlite_vec

package sqlitevec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestFusedSearchCollapsesChunksAndExcludesDeletedMessages(t *testing.T) {
	requirements := require.New(t)
	backend, ctx := newFusedBackendForTest(t)
	generation := seedAndEmbed(t, backend, map[int64][]float32{
		1: {1, 0, 0, 0}, 2: {0.9, 0.1, 0, 0},
		3: {0.8, 0.2, 0, 0}, 4: {0, 1, 0, 0}, 5: {1, 0, 0, 0},
	})
	// Deleted and live messages both have enough near-identical chunks to
	// fill the first vector fetch. The result still needs three live messages.
	var chunks []vector.Chunk
	for _, id := range []int64{1, 2} {
		for index := 1; index <= 40; index++ {
			value := []float32{1, 0, 0, 0}
			if id == 2 {
				value = []float32{0.9, 0.1, 0, 0}
			}
			chunks = append(chunks, vector.Chunk{MessageID: id, ChunkIndex: index, Vector: value})
		}
	}
	requirements.NoError(backend.Upsert(ctx, generation, chunks))
	_, err := backend.mainDB.Exec(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = 1`)
	requirements.NoError(err)
	_, err = backend.mainDB.Exec(`DELETE FROM messages WHERE id = 5`)
	requirements.NoError(err)
	_, err = backend.mainDB.Exec(`INSERT INTO messages_fts (rowid, subject, body)
		VALUES (4, 'schedule', 'meeting with several other unrelated filler words')`)
	requirements.NoError(err)

	hits, metadata, err := backend.FusedSearch(ctx, vector.FusedRequest{
		FTSTerms: []string{"meeting"}, QueryVec: []float32{1, 0, 0, 0},
		Generation: generation, KPerSignal: 3, Limit: 3, RRFK: 60,
	})
	requirements.NoError(err)
	requirements.Len(hits, 3)
	assert.Equal(t, []int64{2, 4, 3}, []int64{hits[0].MessageID, hits[1].MessageID, hits[2].MessageID})
	assert.False(t, metadata.PoolSaturated)
}
