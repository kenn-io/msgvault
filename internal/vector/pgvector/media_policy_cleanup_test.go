//go:build pgvector

package pgvector

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestMediaColdPolicyCleanupBoundsOrphans(t *testing.T) {
	b, st, _, _, generation, ctx := newPolicyBackend(t)
	st.DB().SetMaxOpenConns(1)
	b.db.SetMaxOpenConns(1)
	// Archive-free native writes model persisted orphan tokens left by an older
	// process. Reconciliation then uses the real bound archive and native tables.
	b.policyStore = nil
	docs := b.DocumentBackend()
	embeddings := make([]document.Embedding, 501)
	for i := range embeddings {
		embeddings[i] = document.Embedding{Token: fmt.Sprintf("orphan-%04d", i), Vector: []float32{1, 0, 0, 0}}
	}
	require.NoError(t, docs.PutUnpublished(ctx, document.GenerationID(generation), 4, embeddings))
	images := b.Visual()
	image := make([]float32, 1024)
	image[0] = 1
	for i := range 501 {
		require.NoError(t, images.PutUnpublished(ctx, visual.VectorToken(fmt.Sprintf("orphan-%04d", i)), image))
	}
	b.policyStore = st
	docs.policyStore = st
	n, after, more, err := docs.pruneDocumentPolicyBatch(ctx, "", false)
	require.NoError(t, err)
	assert.Equal(t, int64(500), n)
	assert.True(t, more)
	n, _, more, err = docs.pruneDocumentPolicyBatch(ctx, after, false)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.False(t, more)
	n, cursor, more, err := images.pruneVisualPolicyBatch(ctx, visualPolicyCursor{}, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(500), n)
	assert.True(t, more)
	n, _, more, err = images.pruneVisualPolicyBatch(ctx, cursor, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.False(t, more)
}
