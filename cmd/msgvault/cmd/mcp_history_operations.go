package cmd

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func historyMCPCapabilities(hasRoute func(string, string, string, ...string) bool) []string {
	names := []string{}
	if hasRoute("listOperationRuns", http.MethodGet, "/api/v1/operations/runs", "kind", "lane", "state", "started_from", "started_before", "limit", "cursor") {
		names = append(names, "list_operation_runs")
	}
	if hasRoute("getOperationRun", http.MethodGet, "/api/v1/operations/runs/{id}") {
		names = append(names, "get_operation_run")
	}
	if hasRoute("getOperationStatus", http.MethodGet, "/api/v1/operations/status") {
		names = append(names, "get_operation_status")
	}
	return names
}

type historyFilterArguments struct {
	Kind          *generated.ListOperationRunsQueryKind  `json:"kind,omitempty"`
	Lane          *generated.ListOperationRunsQueryLane  `json:"lane,omitempty"`
	State         *generated.ListOperationRunsQueryState `json:"state,omitempty"`
	StartedFrom   *string                                `json:"started_from,omitempty"`
	StartedBefore *string                                `json:"started_before,omitempty"`
	Limit         *int64                                 `json:"limit,omitempty"`
	Cursor        *string                                `json:"cursor,omitempty"`
}

func historyCanonicalTime(raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil //nolint:nilnil // An absent date means no bound in the owning API contract.
	}
	parsed, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != *raw {
		return nil, errors.New("invalid history date bound")
	}
	return &parsed, nil
}

func (b *daemonMCPOperations) executeHistoryOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if name != "list_operation_runs" && name != "get_operation_run" && name != "get_operation_status" {
		return nil, false, nil
	}
	for _, value := range args {
		if value == nil {
			return operationFailure("invalid_operation_arguments", false), true, nil
		}
	}
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "list_operation_runs":
		input, decodeErr := decodeMCPOperationArguments[historyFilterArguments](args)
		if decodeErr != nil {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
		}
		from, fromErr := historyCanonicalTime(input.StartedFrom)
		before, beforeErr := historyCanonicalTime(input.StartedBefore)
		query := &generated.ListOperationRunsQuery{Kind: input.Kind, Lane: input.Lane, State: input.State, StartedFrom: from, StartedBefore: before, Limit: input.Limit, Cursor: input.Cursor}
		if fromErr != nil || beforeErr != nil || query.Validate() != nil || (from != nil && before != nil && !from.Before(*before)) || (input.Cursor != nil && *input.Cursor == "") {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
		}
		result, err = readMCPOperationJSON[generated.OperationRunsResponse](ctx, b.client, http.MethodGet, "/api/v1/operations/runs", &generated.ListOperationRunsRequestOptions{Query: query}, http.StatusOK)
	case "get_operation_run":
		input, decodeErr := decodeMCPOperationArguments[struct {
			RunID string `json:"run_id"`
		}](args)
		// Tokens remain opaque. The owning daemon verifies their archive and key;
		// only reject values that cannot safely be one bounded path parameter.
		if decodeErr != nil || input.RunID == "" || len(input.RunID) > 32<<10 || !utf8.ValidString(input.RunID) || strings.ContainsAny(input.RunID, "/\\\x00\r\n") || strings.TrimSpace(input.RunID) != input.RunID {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
		}
		result, err = readMCPOperationJSON[generated.OperationRunDetail](ctx, b.client, http.MethodGet, "/api/v1/operations/runs/{id}", &generated.GetOperationRunRequestOptions{PathParams: &generated.GetOperationRunPath{ID: input.RunID}}, http.StatusOK)
	case "get_operation_status":
		if _, decodeErr := decodeMCPOperationArguments[struct{}](args); decodeErr != nil {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
		}
		result, err = readMCPOperationJSON[generated.OperationStatusResponse](ctx, b.client, http.MethodGet, "/api/v1/operations/status", nil, http.StatusOK)
	}
	return result, true, err
}
