//go:build sqlite_vec

package sqlitevec

import (
	"go.kenn.io/msgvault/internal/testutil/vectorcheckpoints"
	"testing"
)

func TestBackendEmbeddingCheckpointLifecycle(t *testing.T) {
	b, ctx := newBackendForTest(t)
	vectorcheckpoints.Lifecycle(ctx, t, b)
}
