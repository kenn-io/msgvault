package mcp

import (
	"context"
	"errors"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/vector"
)

type EmbeddingStatusReader interface {
	EmbeddingStatus(ctx context.Context, sourceID int64) (*vector.EmbeddingStatus, error)
}

func (h *handlers) getEmbeddingStatus(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.embeddingStatus == nil {
		return toolErrorResult("embeddings_status_unavailable: embedding status is unavailable"), nil
	}
	sourceID, err := positiveInt64Arg(req.GetArguments(), mcpFieldSourceID)
	if err != nil {
		return toolErrorResult("invalid_source_id: " + err.Error()), nil //nolint:nilerr // MCP tool errors are successful protocol responses.
	}
	status, err := h.embeddingStatus.EmbeddingStatus(ctx, sourceID)
	if err != nil {
		if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok && (apiErr.Code == "invalid_source_id" || apiErr.Code == "embeddings_status_unavailable") {
			return toolErrorResult(apiErr.Code + ": " + apiErr.Message), nil
		}
		return toolErrorResult("embeddings_status_unavailable: embedding status is unavailable"), nil
	}
	return jsonResult(status)
}
func embeddingStatusDefinition() toolDefinition {
	d := readDefinition(ToolGetEmbeddingStatus, "Get message embedding coverage, live scheduler state, throughput, ETA, and recent provider/DB batch timings. Source filters affect coverage only; diagnostics and throughput describe the whole run.", closedObject(map[string]*jsonschema.Schema{mcpFieldSourceID: safeIDSchema("Filter eligible/current/pending by source ID; ETA is null")}), outputSchemaFor[vector.EmbeddingStatus](), (*handlers).getEmbeddingStatus)
	d.availability = func(c catalogCapabilities) bool { return c.embeddingStatus }
	return d
}
