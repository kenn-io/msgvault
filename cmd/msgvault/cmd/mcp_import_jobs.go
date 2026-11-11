package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func importJobMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	var names []string
	if hasRoute("createImportJob", http.MethodPost, "/api/v1/imports") &&
		mcpRequestPropertiesPresent(capabilities, "createImportJob", "account", "limit", "after", "before", "query", "noresume") {
		names = append(names, "create_import_job")
	}
	if hasRoute("getImportJob", http.MethodGet, "/api/v1/imports/{job_id}") {
		names = append(names, "get_import_job")
	}
	return names
}

func decodeImportJobArguments(args map[string]any) (generated.ImportJobRequest, bool) {
	input, err := decodeMCPOperationArguments[generated.ImportJobRequest](args)
	return input, err == nil && strings.TrimSpace(input.Account) != "" && input.Limit != nil && *input.Limit > 0
}
func importJobRefusal(code string) bool {
	switch code {
	case "validation_failed", "ambiguous_account", "account_not_syncable", "sync_already_active", "server_busy", "service_unavailable", "request_too_large", "unsupported_media_type":
		return true
	}
	return false
}
func (b *daemonMCPOperations) executeImportJob(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "create_import_job":
		input, ok := decodeImportJobArguments(args)
		if !ok {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err := readMCPOperationJSONWithClassifier[generated.ImportJobResponse](ctx, b.client, http.MethodPost, "/api/v1/imports", &generated.CreateImportJobRequestOptions{Body: &input}, importJobRefusal, http.StatusAccepted)
		return result, true, err
	case "get_import_job":
		input, err := decodeMCPOperationArguments[struct {
			JobID string `json:"job_id"`
		}](args)
		if err != nil || input.JobID == "" || input.JobID == "." || input.JobID == ".." || url.PathEscape(input.JobID) != input.JobID {
			return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		result, err := readMCPOperationJSON[generated.ImportJobResponse](ctx, b.client, http.MethodGet, "/api/v1/imports/{job_id}", &generated.GetImportJobRequestOptions{PathParams: &generated.GetImportJobPath{JobID: input.JobID}}, http.StatusOK)
		return result, true, err
	}
	return nil, false, nil
}
func importJobDisclosure(name string, args map[string]any) (string, bool, error) {
	if name != "create_import_job" {
		return "", false, nil
	}
	input, ok := decodeImportJobArguments(args)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	data, err := json.Marshal(struct {
		Operation string                     `json:"operation"`
		Request   generated.ImportJobRequest `json:"request"`
		Effect    string                     `json:"effect"`
	}{name, input, "Start a bounded historical import using the configured provider; acceptance is not completion. Do not retry an uncertain outcome automatically."}, json.Deterministic(true))
	return string(data), true, err
}
