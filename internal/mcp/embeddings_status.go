package mcp

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/vector"
)

type EmbeddingStatusReader interface {
	EmbeddingStatus(ctx context.Context, sourceID int64) (*vector.EmbeddingStatus, error)
}

func (h *handlers) getEmbeddingStatus(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.embeddingStatus == nil {
		return toolErrorResult("embeddings_status_unavailable: embedding status is unavailable"), nil
	}
	sourceID, err := positiveInt64Arg(req.GetArguments(), "source_id")
	if err != nil {
		return toolErrorResult("invalid_source_id: " + err.Error()), nil //nolint:nilerr // MCP tool errors are successful protocol responses.
	}
	status, err := h.embeddingStatus.EmbeddingStatus(ctx, sourceID)
	if err != nil {
		return toolErrorResult("embeddings_status_unavailable: embedding status is unavailable"), nil //nolint:nilerr // MCP tool errors are successful protocol responses without private provider errors.
	}
	return jsonResult(status)
}
func embeddingStatusDefinition() toolDefinition {
	d := readDefinition(ToolGetEmbeddingStatus, "Get message embedding coverage, live scheduler state, throughput, ETA, and recent provider/DB batch timings. Source filters affect coverage; diagnostics are generation-wide.", closedObject(map[string]*jsonschema.Schema{"source_id": safeIDSchema("Filter coverage by source ID")}), outputSchemaFor[vector.EmbeddingStatus](), (*handlers).getEmbeddingStatus)
	d.availability = func(c catalogCapabilities) bool { return c.embeddingStatus }
	return d
}
