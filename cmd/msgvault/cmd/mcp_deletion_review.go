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
	switch name {
	case "list_deletions":
		input, err := decodeMCPOperationArguments[struct {
			Status *string `json:"status,omitempty"`
		}](args)
		if err != nil {
			return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		result, err := readMCPOperationJSONWithClassifier[generated.ListDeletionsResponse](ctx, b.client, http.MethodGet, "/api/v1/deletions", &generated.ListDeletionsRequestOptions{Query: &generated.ListDeletionsQuery{Status: input.Status}}, func(code string) bool { return code == "invalid_status" }, http.StatusOK)
		return result, true, err
	case "get_deletion":
		input, err := decodeMCPOperationArguments[struct {
			BatchID string `json:"batch_id"`
		}](args)
		if err != nil {
			return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		// The generated client substitutes path parameters verbatim into the URL.
		if deletion.ValidateManifestID(input.BatchID) != nil {
			return operationFailure("invalid_manifest_id", false), true, nil //nolint:nilerr // Invalid IDs become a fixed structured tool refusal.
		}
		result, err := readMCPOperationJSON[generated.DeletionManifestDetail](ctx, b.client, http.MethodGet, "/api/v1/deletions/{id}", &generated.GetDeletionRequestOptions{PathParams: &generated.GetDeletionPath{ID: input.BatchID}}, http.StatusOK)
		return result, true, err
	default:
		return nil, false, nil
	}
}
