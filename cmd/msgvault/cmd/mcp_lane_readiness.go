package cmd

import (
	"context"
	"net/http"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (b *daemonMCPOperations) executeLaneReadinessOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if name != "get_lane_readiness" {
		return nil, false, nil
	}
	if _, err := decodeMCPOperationArguments[struct{}](args); err != nil {
		return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
	}
	result, err := readMCPOperationJSON[generated.LaneReadinessResponse](ctx, b.client, http.MethodGet, "/api/v1/lanes/readiness", nil, http.StatusOK)
	return result, true, err
}
