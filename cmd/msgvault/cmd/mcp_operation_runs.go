package cmd

import (
	"context"
	"net/http"
	"strings"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func operationRunMCPCapabilities(hasRoute mcpRouteCheck) []string {
	var names []string
	if hasRoute("getOperationStatus", http.MethodGet, "/api/v1/operations/status") {
		names = append(names, "get_operation_status")
	}
	if hasRoute("listOperationRuns", http.MethodGet, "/api/v1/operations/runs", "kind", "lane", "state", "started_from", "started_before", "limit", "cursor") {
		names = append(names, "list_operation_runs")
	}
	if hasRoute("getOperationRun", http.MethodGet, "/api/v1/operations/runs/{id}") {
		names = append(names, "get_operation_run")
	}
	return names
}

type operationRunsArguments struct {
	generated.ListOperationRunsQuery
	StartedFrom   *string `json:"started_from,omitempty"`
	StartedBefore *string `json:"started_before,omitempty"`
}

// The generated query uses time.Time, which normalizes noncanonical input.
// Preserve the caller's text so the daemon applies its own timestamp rules.
type operationRunsRequestOptions struct {
	*generated.ListOperationRunsRequestOptions
	startedFrom, startedBefore *string
}

func (o operationRunsRequestOptions) GetQuery() (map[string]any, error) {
	query, err := o.ListOperationRunsRequestOptions.GetQuery()
	if err != nil {
		return nil, err
	}
	if o.startedFrom != nil {
		query["started_from"] = *o.startedFrom
	}
	if o.startedBefore != nil {
		query["started_before"] = *o.startedBefore
	}
	return query, nil
}

func operationRunRefusal(code string) bool {
	switch code {
	case "invalid_cursor", "invalid_kind", "invalid_lane", "invalid_state", "invalid_limit", "invalid_started_from", "invalid_started_before", "invalid_operation_run_id", "operation_run_not_found", "operation_history_unavailable", "operation_history_conflict", "operation_history_failed":
		return true
	}
	return false
}

func (b *daemonMCPOperations) executeOperationRunRead(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "get_operation_status":
		if _, e := decodeMCPOperationArguments[struct{}](args); e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSONWithClassifier[generated.OperationStatusResponse](ctx, b.client, http.MethodGet, "/api/v1/operations/status", nil, operationRunRefusal, http.StatusOK)
	case "list_operation_runs":
		input, e := decodeMCPOperationArguments[operationRunsArguments](args)
		if e != nil {
			return invalid()
		}
		options := operationRunsRequestOptions{ListOperationRunsRequestOptions: &generated.ListOperationRunsRequestOptions{Query: &input.ListOperationRunsQuery}, startedFrom: input.StartedFrom, startedBefore: input.StartedBefore}
		result, err = readMCPOperationJSONWithClassifier[generated.OperationRunsResponse](ctx, b.client, http.MethodGet, "/api/v1/operations/runs", options, operationRunRefusal, http.StatusOK)
	case "get_operation_run":
		input, e := decodeMCPOperationArguments[struct {
			ID string `json:"id"`
		}](args)
		// Native tokens use URL-safe base64 and dot-separated key IDs. Reject path
		// metacharacters before the generated client substitutes the raw value.
		if e != nil || input.ID == "" || input.ID == "." || input.ID == ".." || strings.Trim(input.ID, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.") != "" {
			return invalid()
		}
		result, err = readMCPOperationJSONWithClassifier[generated.OperationRunDetail](ctx, b.client, http.MethodGet, "/api/v1/operations/runs/{id}", &generated.GetOperationRunRequestOptions{PathParams: &generated.GetOperationRunPath{ID: input.ID}}, operationRunRefusal, http.StatusOK)
	default:
		return nil, false, nil
	}
	return result, true, err
}
