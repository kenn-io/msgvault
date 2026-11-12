package cmd

import (
	"context"
	"net/http"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func deletionSelectionCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	if !hasRoute("explore", http.MethodPost, "/api/v1/explore") ||
		!hasRoute("preflightExploreSelection", http.MethodPost, "/api/v1/explore/preflight") ||
		!mcpRequestPropertiesPresent(caps, "preflightExploreSelection", "selection") {
		return nil
	}
	names := []string{"explore_deletion_selection", "preflight_deletion_selection"}
	if hasRoute("stageDeletion", http.MethodPost, "/api/v1/deletions") && mcpRequestPropertiesPresent(caps, "stageDeletion", "selection", "operation_token", "dry_run", "description") {
		names = append(names, "stage_deletion_selection")
	}
	return names
}

type deletionSelectionArgs struct {
	Selection      generated.ExploreSelection `json:"selection"`
	OperationToken string                     `json:"operation_token,omitempty"`
	DryRun         bool                       `json:"dry_run,omitempty"`
	Description    string                     `json:"description,omitempty"`
}

func deletionSelectionRefusal(code string) bool {
	return code != ""
}

func (b *daemonMCPOperations) executeDeletionSelection(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if name == "explore_deletion_selection" {
		input, err := decodeMCPOperationArguments[generated.ExploreHTTPRequest](args)
		if err != nil {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err := readMCPOperationJSONWithClassifier[generated.ExploreHTTPResponse](ctx, b.client, http.MethodPost, "/api/v1/explore", &generated.ExploreRequestOptions{Body: &input}, deletionSelectionRefusal, http.StatusOK)
		return result, true, err
	}
	if name != "preflight_deletion_selection" && name != "stage_deletion_selection" {
		return nil, false, nil
	}
	input, err := decodeMCPOperationArguments[deletionSelectionArgs](args)
	if err != nil || (name == "preflight_deletion_selection" && input.OperationToken != "") {
		return operationFailure("invalid_arguments", false), true, nil
	}
	if name == "preflight_deletion_selection" {
		body := generated.PreflightExploreSelectionBody{Selection: input.Selection}
		result, err := readMCPOperationJSONWithClassifier[generated.ExplorePreflightResponse](ctx, b.client, http.MethodPost, "/api/v1/explore/preflight", &generated.PreflightExploreSelectionRequestOptions{Body: &body}, deletionSelectionRefusal, http.StatusOK)
		return result, true, err
	}
	body := generated.StageDeletionBody{Selection: &input.Selection, OperationToken: &input.OperationToken, DryRun: &input.DryRun, Description: &input.Description}
	status := http.StatusCreated
	if input.DryRun {
		status = http.StatusOK
	}
	result, err := readMCPOperationJSONWithClassifier[generated.StageDeletionResponse](ctx, b.client, http.MethodPost, "/api/v1/deletions", &generated.StageDeletionRequestOptions{Body: &body}, deletionSelectionRefusal, status)
	return result, true, err
}
