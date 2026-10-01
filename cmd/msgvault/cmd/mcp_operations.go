package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpAccountArgumentKey = "account"

type daemonMCPOperations struct {
	client           *daemonclient.Client
	supported        []string
	commands         []apiprotocol.MCPCommandDescriptor
	documentManifest string
}

func newDaemonMCPOperations(client *daemonclient.Client, capabilities *apiprotocol.MCPCapabilities) *daemonMCPOperations {
	backend := &daemonMCPOperations{client: client}
	if capabilities == nil {
		return backend
	}
	backend.commands = slices.Clone(capabilities.Commands)
	for i := range backend.commands {
		backend.commands[i].Flags = slices.Clone(backend.commands[i].Flags)
	}
	backend.supported = draftMCPCapabilities(capabilities)
	if capabilities.Delegated {
		return backend
	}

	hasRoute := func(id, method, path string, query ...string) bool {
		for _, route := range capabilities.Routes {
			if route.OperationID != id || route.Method != method || route.Path != path {
				continue
			}
			for _, name := range query {
				if !slices.Contains(route.QueryParameters, name) {
					return false
				}
			}
			return true
		}
		return false
	}
	if hasRoute("getSchedulerStatus", http.MethodGet, "/api/v1/scheduler/status") {
		backend.supported = append(backend.supported, "get_source_scheduler_status")
	}
	if hasRoute("triggerSync", http.MethodPost, "/api/v1/sync/{account}", "source_type") && hasRoute("listSourceStatus", http.MethodGet, "/api/v1/sources/status", "source_type") {
		backend.supported = append(backend.supported, "sync_source")
	}
	backend.supported = append(backend.supported, sourceMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, providerMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, documentMCPCapabilities(hasRoute, capabilities)...)
	return backend
}

func (b *daemonMCPOperations) capabilities() []string { return slices.Clone(b.supported) }

func (b *daemonMCPOperations) ExecuteOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, error) {
	if !slices.Contains(b.supported, name) {
		return operationFailure("operation_not_supported", false), nil
	}
	switch name {
	case "get_source_scheduler_status":
		return readMCPOperationJSON[generated.SchedulerStatusResponse](ctx, b.client, http.MethodGet, "/api/v1/scheduler/status", nil, http.StatusOK)
	case "sync_source":
		account, sourceType, err := mcpSyncArguments(args)
		if err != nil {
			return operationFailure("invalid_source", false), nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		return readMCPOperationJSON[generated.StatusMessageResponse](ctx, b.client, http.MethodPost, "/api/v1/sync/{account}", &generated.TriggerSyncRequestOptions{PathParams: &generated.TriggerSyncPath{Account: account}, Query: &generated.TriggerSyncQuery{SourceType: &sourceType}}, http.StatusAccepted)
	default:
		if result, handled, err := b.executeDocumentOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeDraftOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeProviderOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeSourceOperation(ctx, name, args); handled {
			return result, err
		}
		return operationFailure("operation_not_supported", false), nil
	}
}

func (b *daemonMCPOperations) OperationDisclosure(ctx context.Context, name string, args map[string]any) (string, error) {
	if slices.Contains(b.supported, name) {
		if disclosure, handled, err := b.documentOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.draftOperationDisclosure(name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.providerOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.sourceOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
	}
	if name != "sync_source" || !slices.Contains(b.supported, name) {
		return "", &mcpserver.OperationRefusalError{Code: "operation_not_supported"}
	}
	account, sourceType, err := mcpSyncArguments(args)
	if err != nil {
		return "", &mcpserver.OperationRefusalError{Code: "invalid_source"}
	}
	result, err := readMCPOperationJSON[generated.SourceStatusResponse](ctx, b.client, http.MethodGet, "/api/v1/sources/status", &generated.ListSourceStatusRequestOptions{Query: &generated.ListSourceStatusQuery{SourceType: &sourceType}}, http.StatusOK)
	if err != nil {
		return "", err
	}
	if result.IsError {
		return "", &mcpserver.OperationRefusalError{Code: "source_status_unavailable"}
	}
	status, ok := result.Output.(generated.SourceStatusResponse)
	if !ok {
		return "", errors.New("unexpected source status result")
	}
	for _, source := range status.Sources {
		if source.Identifier != account || source.SourceType != sourceType {
			continue
		}
		if !source.CanSync {
			return "", &mcpserver.OperationRefusalError{Code: "source_not_schedulable"}
		}
		disclosure := struct {
			Operation  string `json:"operation"`
			SourceID   int64  `json:"source_id"`
			SourceType string `json:"source_type"`
			Identifier string `json:"identifier"`
			Scope      string `json:"scope"`
			Acceptance string `json:"acceptance"`
		}{"sync_source", source.ID, sourceType, account, "configured scheduler scope; generic source jobs synchronize the shared configured sources of this type", "acceptance schedules work; it is not completion"}
		data, err := json.Marshal(disclosure, json.Deterministic(true))
		return string(data), err
	}
	return "", &mcpserver.OperationRefusalError{Code: "not_found"}
}

func mcpSyncArguments(args map[string]any) (string, string, error) {
	account, ok := args[mcpAccountArgumentKey].(string)
	if !ok || strings.TrimSpace(account) == "" {
		return "", "", errors.New("missing source identifier")
	}
	sourceType, ok := args["source_type"].(string)
	if !ok || strings.TrimSpace(sourceType) == "" {
		return "", "", errors.New("missing source type")
	}
	return account, sourceType, nil
}

func operationFailure(code string, uncertain bool) *mcpserver.OperationResult {
	return &mcpserver.OperationResult{IsError: true, Output: struct {
		Error                     string `json:"error"`
		OperationMayHaveCompleted bool   `json:"operation_may_have_completed"`
	}{code, uncertain}}
}

// The fixed route/request chosen by an adapter uses existing authentication and
// cancellation without the daemon client's busy retry loop. Mutations with an
// unclassified/interrupted response retain uncertain completion explicitly.
func readMCPOperationJSON[T any](ctx context.Context, client *daemonclient.Client, method, path string, options runtime.RequestOptions, success ...int) (*mcpserver.OperationResult, error) {
	writes := method != http.MethodGet
	response, err := client.DoGeneratedRequestWithContext(ctx, method, path, options)
	if err != nil {
		return operationFailure("operation_failed", writes), err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return operationFailure("operation_response_failed", writes), err
	}
	if len(data) > 1<<20 {
		return operationFailure("operation_response_limit_exceeded", writes), nil
	}
	if !slices.Contains(success, response.StatusCode) {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		code := failure.Error
		switch code {
		case "unauthorized", "not_found", "scheduler_unavailable", "source_not_schedulable", "store_unavailable", "missing_account", "sync_error", "operation_in_progress", "settings_conflict", "settings_edit_rejected", "if_match_required", "person_revision_conflict", "person_profile_not_found", "persons_unavailable", "cache_build_not_found", "cache_build_unavailable", "validation_failed", "bad_request", "invalid_participant_id", "participant_not_found", "analytical_cache_unavailable", "query_resource_exhausted", "engine_unavailable", "document_status_scope_unavailable", "document_status_unavailable":
			return operationFailure(code, false), nil
		default:
			return operationFailure("operation_refused", writes && response.StatusCode >= 500), nil
		}
	}
	var output T
	if err := json.Unmarshal(data, &output); err != nil {
		return operationFailure("invalid_operation_response", writes), err
	}
	return &mcpserver.OperationResult{Output: output, ETag: response.Header.Get("ETag")}, nil
}
