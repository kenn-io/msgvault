package cmd

import (
	"context"
	"net/http"

	"go.kenn.io/msgvault/internal/deletion"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func deletionReviewMCPCapabilities(hasRoute mcpRouteCheck) []string {
	var names []string
	if hasRoute("listDeletions", http.MethodGet, "/api/v1/deletions", "status") {
		names = append(names, "list_deletions")
	}
	if hasRoute("getDeletion", http.MethodGet, "/api/v1/deletions/{id}") {
		names = append(names, "get_deletion")
	}
	return names
}

func (b *daemonMCPOperations) executeDeletionReviewOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	classify := func(code string) bool {
		return code == "invalid_status" || code == "invalid_manifest_id" || code == "cli_store_unavailable"
	}
	switch name {
	case "list_deletions":
		input, err := decodeMCPOperationArguments[struct {
			Status *string `json:"status,omitempty"`
		}](args)
		if err != nil {
			return operationFailure("invalid_arguments", false), true, nil
		}
		if input.Status != nil && !deletion.IsValidStatus(deletion.Status(*input.Status)) {
			return operationFailure("invalid_status", false), true, nil
		}
		result, err := readMCPOperationJSONWithClassifier[generated.ListDeletionsResponse](ctx, b.client, http.MethodGet, "/api/v1/deletions", &generated.ListDeletionsRequestOptions{Query: &generated.ListDeletionsQuery{Status: input.Status}}, classify, http.StatusOK)
		return result, true, err
	case "get_deletion":
		input, err := decodeMCPOperationArguments[struct {
			ManifestID string `json:"manifest_id"`
		}](args)
		if err != nil {
			return operationFailure("invalid_arguments", false), true, nil
		}
		if deletion.ValidateManifestID(input.ManifestID) != nil {
			return operationFailure("invalid_manifest_id", false), true, nil
		}
		result, err := readMCPOperationJSONWithClassifier[generated.DeletionManifestDetail](ctx, b.client, http.MethodGet, "/api/v1/deletions/{id}", &generated.GetDeletionRequestOptions{PathParams: &generated.GetDeletionPath{ID: input.ManifestID}}, classify, http.StatusOK)
		return result, true, err
	default:
		return nil, false, nil
	}
}
