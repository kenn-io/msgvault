//go:build sqlite_vec

package sqlitevec

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestVisualColdPolicyCleanupBoundsOrphans(t *testing.T) {
	b, st, _, _, _ := newPolicyBackend(t)
	ctx := t.Context()
	st.DB().SetMaxOpenConns(1)
	b.db.SetMaxOpenConns(1)
	// Archive-free native writes model persisted orphan tokens left by an older
	// process. Reconciliation then uses the real bound archive and native tables.
	b.policyStore = nil
	images := b.Visual()
	image := make([]float32, 1024)
	image[0] = 1
	for i := range 501 {
		require.NoError(t, images.PutUnpublished(ctx, visual.VectorToken(fmt.Sprintf("orphan-%04d", i)), image))
	}
	b.policyStore = st
	n, cursor, more, err := images.pruneVisualPolicyBatch(ctx, visualPolicyCursor{}, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(500), n)
	assert.True(t, more)
	n, _, more, err = images.pruneVisualPolicyBatch(ctx, cursor, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.False(t, more)
}
