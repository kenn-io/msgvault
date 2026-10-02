package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	mcpImportsPath   = "/api/v1/imports"
	mcpImportJobPath = "/api/v1/imports/{job_id}"
)

func importMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	names := []string{}
	if hasRoute("getImportJob", http.MethodGet, mcpImportJobPath) {
		names = append(names, "get_import_job")
	}
	if hasRoute("getImportJob", http.MethodGet, mcpImportJobPath) && hasRoute("listSourceStatus", http.MethodGet, "/api/v1/sources/status") && hasRoute("createImportJob", http.MethodPost, mcpImportsPath) && mcpRequestPropertiesPresent(capabilities, "createImportJob", "account", "after", "before", "limit", "query", "noresume") {
		names = append(names, "create_import_job")
	}
	return names
}

func mcpImportArguments(args map[string]any) (generated.ImportJobRequest, error) {
	input, err := decodeMCPOperationArguments[generated.ImportJobRequest](args)
	if err != nil {
		return input, err
	}
	for _, value := range args {
		if value == nil {
			return input, errors.New("null import argument")
		}
	}
	input.Account = strings.TrimSpace(input.Account)
	if !mcpBoundedOpaqueValue(input.Account, 512) || input.Validate() != nil || input.Limit != nil && (*input.Limit < 0 || *input.Limit > 9007199254740991) || input.Query != nil && !mcpBoundedImportQuery(*input.Query) {
		return input, errors.New("invalid import arguments")
	}
	var after, before time.Time
	for _, bound := range []struct {
		value *string
		time  *time.Time
	}{{input.After, &after}, {input.Before, &before}} {
		if bound.value == nil {
			continue
		}
		parsed, err := time.Parse("2006-01-02", *bound.value)
		if err != nil || parsed.Format("2006-01-02") != *bound.value {
			return input, errors.New("invalid import date")
		}
		*bound.time = parsed
	}
	if input.After != nil && input.Before != nil && !after.Before(before) {
		return input, errors.New("import date bounds must increase")
	}
	return input, nil
}

func mcpBoundedImportQuery(query string) bool {
	return query == "" || mcpBoundedOpaqueValue(query, 4096)
}

func (b *daemonMCPOperations) importSource(ctx context.Context, input generated.ImportJobRequest) (generated.SourceStatus, error) {
	result, err := readMCPOperationJSON[generated.SourceStatusResponse](ctx, b.client, http.MethodGet, "/api/v1/sources/status", nil, http.StatusOK)
	if err != nil {
		return generated.SourceStatus{}, err
	}
	if result == nil || result.IsError {
		return generated.SourceStatus{}, &mcpserver.OperationRefusalError{Code: "source_status_unavailable"}
	}
	status, ok := result.Output.(generated.SourceStatusResponse)
	if !ok {
		return generated.SourceStatus{}, &mcpserver.OperationRefusalError{Code: "invalid_operation_response"}
	}
	var match *generated.SourceStatus
	selectorFound := false
	for _, source := range status.Sources {
		if !strings.EqualFold(source.Identifier, input.Account) && (source.DisplayName == nil || !strings.EqualFold(*source.DisplayName, input.Account)) {
			continue
		}
		selectorFound = true
		if source.SourceType == "" {
			source.SourceType = sourceTypeGmail
		}
		if source.SourceType != sourceTypeGmail && source.SourceType != sourceTypeIMAP {
			continue
		}
		if match != nil {
			return generated.SourceStatus{}, &mcpserver.OperationRefusalError{Code: "ambiguous_account"}
		}
		match = &source
	}
	if match == nil {
		code := "not_found"
		if selectorFound {
			code = "account_not_syncable"
		}
		return generated.SourceStatus{}, &mcpserver.OperationRefusalError{Code: code}
	}
	if match.SourceType == sourceTypeIMAP && input.Query != nil && *input.Query != "" {
		return generated.SourceStatus{}, &mcpserver.OperationRefusalError{Code: "validation_failed"}
	}
	return *match, nil
}

func (b *daemonMCPOperations) executeImportOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "create_import_job":
		input, err := mcpImportArguments(args)
		if err != nil {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed refusal.
		}
		if _, err := b.importSource(ctx, input); err != nil {
			if refusal, ok := errors.AsType[*mcpserver.OperationRefusalError](err); ok {
				return operationFailure(refusal.Code, false), true, nil
			}
			return nil, true, err
		}
		result, err := readMCPOperationJSON[generated.ImportJobResponse](ctx, b.client, http.MethodPost, mcpImportsPath, &generated.CreateImportJobRequestOptions{Body: &input}, http.StatusAccepted)
		return result, true, err
	case "get_import_job":
		input, err := decodeMCPOperationArguments[struct {
			JobID string `json:"job_id"`
		}](args)
		if err != nil || !mcpBoundedOpaqueValue(input.JobID, 256) || strings.ContainsAny(input.JobID, "/\\") {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed refusal.
		}
		result, err := readMCPOperationJSON[generated.ImportJobResponse](ctx, b.client, http.MethodGet, mcpImportJobPath, &generated.GetImportJobRequestOptions{PathParams: &generated.GetImportJobPath{JobID: input.JobID}}, http.StatusOK)
		return result, true, err
	default:
		return nil, false, nil
	}
}

func (b *daemonMCPOperations) importOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != "create_import_job" {
		return "", false, nil
	}
	input, err := mcpImportArguments(args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_arguments"}
	}
	source, err := b.importSource(ctx, input)
	if err != nil {
		return "", true, err
	}
	limit := "unlimited: absent or zero message limit"
	if input.Limit != nil && *input.Limit > 0 {
		limit = "bounded by the requested message limit"
	}
	data, err := json.Marshal(struct {
		Operation  string                     `json:"operation"`
		SourceID   int64                      `json:"source_id"`
		SourceType string                     `json:"source_type"`
		Request    generated.ImportJobRequest `json:"request"`
		Limit      string                     `json:"message_bound"`
		Dates      string                     `json:"date_bounds"`
		Effect     string                     `json:"effect"`
	}{name, source.ID, source.SourceType, input, limit, "Only supplied after/before dates bound the import; absent dates are unbounded", "Persist and schedule a historical import; acceptance is not completion. Provider credentials and source locking remain daemon-owned."}, json.Deterministic(true))
	return string(data), true, err
}
