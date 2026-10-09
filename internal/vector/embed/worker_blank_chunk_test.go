//go:build sqlite_vec

package embed

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

func TestWorker_FiltersBlankChunks(t *testing.T) {
	f := newWorkerFixture(t, 1)
	_, err := f.MainDB.Exec(`UPDATE messages SET subject = ''`)
	require.NoError(t, err)
	blankWindow := strings.Repeat("\x7f", 8)
	body := blankWindow + "界abcdefg" + blankWindow + "終hijklmn" + blankWindow
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ?`, body)
	require.NoError(t, err)
	// Seed an older five-window representation. Re-embedding must replace it
	// without leaving orphaned tail rows when only two windows survive.
	var oldChunks []vector.Chunk
	for i := range 5 {
		oldChunks = append(oldChunks, vector.Chunk{
			MessageID: 1, ChunkIndex: i, Vector: []float32{1, 1, 0, 0},
			SourceCharLen: 8, ChunkCharStart: 8 * i, ChunkCharEnd: 8 * (i + 1),
		})
	}
	require.NoError(t, f.Backend.Upsert(t.Context(), f.BuildingGen, oldChunks))
	client, calls := newWorkerHTTPClient(t)
	w := newTestWorker(f, 8)
	w.deps.Client = client
	w.deps.MaxInputChars = 8 // The production overlap is zero at this size.

	batch, err := w.embedBatch(t.Context(), []int64{1})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"界abcdefg", "終hijklmn"}}, calls())
	assert.Equal(t, []int64{1}, batch.embeddedIDs)
	assert.Empty(t, batch.empty)
	require.Len(t, batch.chunks, 2)
	for i, chunk := range batch.chunks {
		assert.Equal(t, int64(1), chunk.MessageID)
		assert.Equal(t, i, chunk.ChunkIndex)
		assert.Equal(t, 8+16*i, chunk.ChunkCharStart)
		assert.Equal(t, 16+16*i, chunk.ChunkCharEnd)
		assert.Equal(t, 8, chunk.SourceCharLen)
		assert.Equal(t, []float32{1, float32(i + 1), 0, 0}, chunk.Vector)
		assert.True(t, chunk.Truncated, "retained chunks are hard cuts before the original final span")
	}

	res, err := w.RunBackstop(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded)
	assert.Zero(t, res.Failed)
	assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)))
	var count int
	require.NoError(t, f.VectorsDB.QueryRow(`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`, f.BuildingGen).Scan(&count))
	assert.Equal(t, 2, count)
	_, err = w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.Len(t, calls(), 2, "covered message is not embedded again")
}

func TestWorker_SkipMarksAllFilteredChunks(t *testing.T) {
	for _, n := range []int{1, 2} {
		name := "entire batch filtered"
		if n == 2 {
			name = "mixed batch"
		}
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t, n)
			_, err := f.MainDB.Exec(`UPDATE messages SET subject = ''`)
			require.NoError(t, err)
			// The input has visible content, but the chunk budget retains only
			// controls. Whole-message blank detection cannot handle this case.
			body := strings.Repeat("\x7f", (maxSpansPerMessage+1)*8) + "visible tail"
			_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body)
			require.NoError(t, err)
			if n == 2 {
				_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = 'visible' WHERE message_id = 2`)
				require.NoError(t, err)
			}
			client, calls := newWorkerHTTPClient(t)
			w := newTestWorker(f, 8)
			w.deps.Client = client
			w.deps.MaxInputChars = 8
			res, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
			require.NoError(t, err)
			assert.Equal(t, n-1, res.Succeeded)
			assert.Zero(t, res.Failed)
			assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)))
			if n == 1 {
				assert.Empty(t, calls())
			} else {
				assert.Equal(t, [][]string{{"visible"}}, calls())
			}
			var blankVectors int
			require.NoError(t, f.VectorsDB.QueryRow(`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`, f.BuildingGen).Scan(&blankVectors))
			assert.Zero(t, blankVectors)
			res, err = w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
			require.NoError(t, err)
			assert.Zero(t, res.Claimed)
			assert.Len(t, calls(), n-1)
		})
	}
}
