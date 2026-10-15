package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

type embeddingStatusFixture struct{ sourceID int64 }

func (f *embeddingStatusFixture) EmbeddingStatus(_ context.Context, sourceID int64) (*vector.EmbeddingStatus, error) {
	f.sourceID = sourceID
	return &vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{State: "building"}, Pending: 12}, nil
}

func TestEmbeddingStatusToolReturnsStructuredStatus(t *testing.T) {
	fixture := &embeddingStatusFixture{}
	opts := ServeOptions{EmbeddingStatus: fixture}
	result := rawCallTool(t, opts, ToolGetEmbeddingStatus, map[string]any{"source_id": float64(7)})
	require.NotEqual(t, true, result["isError"])
	assert.Equal(t, int64(7), fixture.sourceID)
	assert.InDelta(t, float64(12), toolStructuredContent(t, result)["pending"], 0.0001)
}

func TestEmbeddingStatusToolMapsDaemonErrors(t *testing.T) {
	for _, tt := range []struct {
		code, message string
		status        int
	}{
		{"invalid_source_id", "source_id is outside [vector.embed.scope]", http.StatusBadRequest},
		{"internal_error", "synthetic private failure detail", http.StatusInternalServerError},
	} {
		t.Run(tt.message, func(t *testing.T) {
			assert := assert.New(t)
			client := task4DaemonErrorClient(t, "/api/v1/embeddings/status", tt.code, tt.message, tt.status)
			result := rawCallTool(t, ServeOptions{EmbeddingStatus: client}, ToolGetEmbeddingStatus, map[string]any{"source_id": float64(7)})
			require.Equal(t, true, result["isError"])
			text := toolErrorTextFromResult(t, result)
			if tt.code == "internal_error" {
				assert.NotContains(text, tt.message)
			} else {
				assert.Contains(text, tt.code)
				assert.Contains(text, tt.message)
			}
		})
	}
}
