package archive_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/msgvault/pkg/archive"
)

// A sync or purge holds one connection for its lock and needs a second one
// for its work, so a smaller pool would block until the context ends.
func TestOpenRejectsPoolThatCannotHoldSyncLock(t *testing.T) {
	for _, size := range []int{1, -1} {
		_, err := archive.Open(t.Context(), archive.PostgreSQL{
			URL: "postgres://archive.invalid/msgvault", MaxOpenConnections: size,
		})
		assert.ErrorContains(t, err, "cannot hold a sync lock", "MaxOpenConnections %d", size)
	}
}

func TestInvalidEmbeddingDescriptor(t *testing.T) {
	opts := archive.PostgreSQL{URL: "postgres://archive.invalid/msgvault", Embeddings: &archive.Embeddings{Client: &embedclient.Client{}}}
	assert.ErrorContains(t, archive.Setup(t.Context(), opts), "embedding descriptor")
	_, err := archive.Open(t.Context(), opts)
	assert.ErrorContains(t, err, "embedding descriptor")
}
