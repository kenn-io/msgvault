// Package vectorcheckpoints checks the durable checkpoint lifecycle on real backends.
package vectorcheckpoints

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func Lifecycle(ctx context.Context, t *testing.T, b vector.Backend) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(err)
	checkpoints := b.EmbeddingCheckpoints()
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	entries := []vector.EmbeddingCheckpoint{
		{Owner: "message:1", Revision: "revision-a", Key: "0", Vectors: [][]float32{{1, 2, 3, 4}}},
		{Owner: "context:chat-a", Revision: "revision-a", Key: "input-a", Vectors: [][]float32{{4, 3, 2, 1}}},
	}
	require.NoError(checkpoints.Save(stopped, gen, entries))
	owners, err := checkpoints.Owners(ctx, gen, "", "", 1)
	require.NoError(err)
	assert.Equal([]string{"context:chat-a"}, owners)
	owners, err = checkpoints.Owners(ctx, gen, "message:", "context:chat-a", 300)
	require.NoError(err)
	assert.Equal([]string{"message:1"}, owners)
	badResponse := []vector.EmbeddingCheckpoint{
		{Owner: "message:1", Revision: "revision-a", Key: "0", Vectors: [][]float32{{9, 9, 9, 9}}},
		{Owner: "message:2", Revision: "revision-a", Key: "0", Vectors: [][]float32{{float32(math.NaN())}}},
	}
	require.Error(checkpoints.Save(ctx, gen, badResponse))
	saved, err := b.EmbeddingCheckpoints().Load(ctx, gen, "message:1", "revision-a")
	require.NoError(err)
	assert.Equal(map[string][][]float32{"0": {{1, 2, 3, 4}}}, saved)
	saved, err = checkpoints.Load(ctx, gen, "message:1", "revision-b")
	require.NoError(err)
	assert.Empty(saved, "an edited source invalidates its previous output")
	saved, err = checkpoints.Load(ctx, gen, "context:chat-a", "revision-a")
	require.NoError(err)
	require.Len(saved, 1, "unrelated source changes preserve this document")
	require.NoError(b.RetireGeneration(ctx, gen, false))
	saved, err = checkpoints.Load(ctx, gen, "context:chat-a", "revision-a")
	require.NoError(err)
	assert.Empty(saved, "retirement removes unpublished output")
	err = checkpoints.Save(stopped, gen, []vector.EmbeddingCheckpoint{{Owner: "message:1", Revision: "revision-c", Key: "0", Vectors: [][]float32{{1, 2, 3, 4}}}})
	require.ErrorIs(err, vector.ErrGenerationRetired)
	require.ErrorIs(err, context.Canceled, "a finalization failure preserves the original cancellation")
}
