//go:build pgvector

package pgvector

import (
	"go.kenn.io/msgvault/internal/testutil/vectorcheckpoints"
	"testing"
)

func TestBackendEmbeddingCheckpointLifecycle(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	vectorcheckpoints.Lifecycle(ctx, t, b)
}
