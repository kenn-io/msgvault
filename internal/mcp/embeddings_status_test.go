package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

type embeddingStatusFixture struct{}

func (embeddingStatusFixture) EmbeddingStatus(context.Context, int64) (*vector.EmbeddingStatus, error) {
	return &vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{State: "building"}, Pending: 12}, nil
}

func TestEmbeddingStatusMCPProtocolCatalog(t *testing.T) {
	opts := ServeOptions{EmbeddingStatus: embeddingStatusFixture{}}
	result := rawCallTool(t, opts, ToolGetEmbeddingStatus, map[string]any{})
	require.NotEqual(t, true, result["isError"])
	assert.InDelta(t, float64(12), toolStructuredContent(t, result)["pending"], 0.0001)
}
