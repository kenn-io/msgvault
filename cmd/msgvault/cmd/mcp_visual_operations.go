package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/vector/visual"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func visualMCPCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, route := range []struct {
		name, id, method, path string
		fields                 []string
	}{
		{"get_visual_attachment_status", "getVisualAttachmentStatus", http.MethodGet, "/api/v1/multimodal/status", nil},
		{"start_visual_attachment_build", "startVisualAttachmentBuild", http.MethodPost, "/api/v1/multimodal/build", []string{"consent"}},
		{"resume_visual_attachment_build", "resumeVisualAttachmentBuild", http.MethodPost, "/api/v1/multimodal/run", nil},
		{"retry_visual_attachment_owner", "retryVisualAttachmentOwner", http.MethodPost, "/api/v1/multimodal/retry", []string{"message_id", "blob_hash"}},
		{"retire_visual_attachment_generation", "retireVisualAttachmentGeneration", http.MethodPost, "/api/v1/multimodal/retire", []string{"generation_id"}},
	} {
		if hasRoute(route.id, route.method, route.path) && mcpRequestPropertiesPresent(caps, route.id, route.fields...) {
			names = append(names, route.name)
		}
	}
	return names
}

type visualArguments struct {
	Coverage     bool   `json:"coverage,omitempty"`
	Consent      bool   `json:"consent,omitempty"`
	MessageID    int64  `json:"message_id,omitempty"`
	BlobHash     string `json:"blob_hash,omitempty"`
	GenerationID int64  `json:"generation_id,omitempty"`
}

type visualCoverageOptions struct{}

func (visualCoverageOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (visualCoverageOptions) GetQuery() (map[string]any, error) {
	return map[string]any{"coverage": "1"}, nil
}
func (visualCoverageOptions) GetBody() any                          { return nil }
func (visualCoverageOptions) GetHeader() (map[string]string, error) { return map[string]string{}, nil }

func visualInput(name string, args map[string]any) (visualArguments, error) {
	input, err := decodeMCPOperationArguments[visualArguments](args)
	if err != nil {
		return input, err
	}
	switch name {
	case "get_visual_attachment_status", "resume_visual_attachment_build":
	case "start_visual_attachment_build":
		if !input.Consent {
			return input, errors.New("consent required")
		}
	case "retry_visual_attachment_owner":
		input.BlobHash = strings.ToLower(strings.TrimSpace(input.BlobHash))
	case "retire_visual_attachment_generation":
	default:
		return input, errors.New("unsupported operation")
	}
	return input, nil
}

func (b *daemonMCPOperations) visualOperationDisclosure(name string, args map[string]any) (string, bool, error) {
	switch name {
	case "start_visual_attachment_build", "resume_visual_attachment_build", "retry_visual_attachment_owner", "retire_visual_attachment_generation":
	default:
		return "", false, nil
	}
	input, err := visualInput(name, args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	effects := map[string]string{
		"start_visual_attachment_build":       "Grant consent and run a bounded pass: eligible attachment bytes and bounded message content are sent to the configured provider; provider charges may apply",
		"resume_visual_attachment_build":      "Run a bounded pass under existing consent: eligible attachment bytes and bounded message content are sent to the configured provider; provider charges may apply",
		"retry_visual_attachment_owner":       "Retry this exact message and attachment under existing consent: eligible attachment bytes and bounded message content are sent to the configured provider; provider charges may apply",
		"retire_visual_attachment_generation": "Irreversibly retire the exact visual generation and delete its vectors",
	}
	data, err := json.Marshal(struct {
		Operation string          `json:"operation"`
		Effect    string          `json:"effect"`
		Arguments visualArguments `json:"arguments"`
	}{name, effects[name], input}, json.Deterministic(true))
	return string(data), true, err
}

func (b *daemonMCPOperations) executeVisualOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "get_visual_attachment_status", "start_visual_attachment_build", "resume_visual_attachment_build", "retry_visual_attachment_owner", "retire_visual_attachment_generation":
	default:
		return nil, false, nil
	}
	input, err := visualInput(name, args)
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid input is a structured refusal.
	}
	var method, path string
	var options runtime.RequestOptions
	switch name {
	case "get_visual_attachment_status":
		method, path = http.MethodGet, "/api/v1/multimodal/status"
		if input.Coverage {
			if !b.visualCoverage {
				return operationFailure("operation_not_supported", false), true, nil
			}
			options = visualCoverageOptions{}
		}
	case "start_visual_attachment_build":
		method, path = http.MethodPost, "/api/v1/multimodal/build"
		options = &generated.StartVisualAttachmentBuildRequestOptions{Body: &generated.StartVisualAttachmentBuildBody{Consent: true}}
	case "resume_visual_attachment_build":
		method, path = http.MethodPost, "/api/v1/multimodal/run"
	case "retry_visual_attachment_owner":
		method, path = http.MethodPost, "/api/v1/multimodal/retry"
		options = &generated.RetryVisualAttachmentOwnerRequestOptions{Body: &generated.RetryVisualAttachmentOwnerBody{MessageID: input.MessageID, BlobHash: input.BlobHash}}
	case "retire_visual_attachment_generation":
		method, path = http.MethodPost, "/api/v1/multimodal/retire"
		options = &generated.RetireVisualAttachmentGenerationRequestOptions{Body: &generated.RetireVisualAttachmentGenerationBody{GenerationID: input.GenerationID}}
	}
	if name == "retire_visual_attachment_generation" {
		result, err := readMCPOperationJSONWithClassifier[mcpserver.VisualRetireReceipt](ctx, b.client, method, path, options, visualRefusal, http.StatusNoContent)
		if result != nil && !result.IsError {
			result.Output = mcpserver.VisualRetireReceipt{Retired: true}
		}
		return result, true, err
	}
	result, err := readMCPOperationJSONWithClassifier[visual.Status](ctx, b.client, method, path, options, visualRefusal, http.StatusOK)
	if result != nil && !result.IsError {
		status, ok := result.Output.(visual.Status)
		if !ok {
			return operationFailure("invalid_operation_response", method != http.MethodGet), true, nil
		}
		result.Output = mcpserver.VisualOperationStatus{
			Generation: mcpserver.VisualGenerationStatus{ID: status.Generation.ID, State: string(status.Generation.State), Consented: status.Generation.Consented},
			Current:    status.Current, Stale: status.Stale, Tombstoned: status.Tombstoned,
			Retryable: status.Retryable, Terminal: status.Terminal,
			ActiveLeases: status.ActiveLeases, JournalHighWater: status.JournalHighWater, JournalCursor: status.JournalCursor,
			ReconciliationComplete: status.ReconciliationComplete, JournalLag: status.JournalLag, Converged: status.Converged,
			ConvergenceTotal: status.ConvergenceTotal, ConvergenceRatio: status.ConvergenceRatio, Usage: status.Usage,
			DuplicateCost: mcpserver.VisualDuplicateCost{AtLeastOnce: status.DuplicateCost.AtLeastOnce, ProviderIdempotent: status.DuplicateCost.ProviderIdempotent},
		}
		if name == "get_visual_attachment_status" && input.Coverage {
			projection, ok := result.Output.(mcpserver.VisualOperationStatus)
			if !ok {
				return operationFailure("invalid_operation_response", method != http.MethodGet), true, nil
			}
			projection.Eligible, projection.UnknownRole, projection.Unavailable = &status.Eligible, &status.UnknownRole, &status.Unavailable
			projection.Formats = &status.Formats
			result.Output = projection
		}
	}
	return result, true, err
}

func visualRefusal(code string) bool {
	switch code {
	case "visual_consent_required", "invalid_visual_owner", "invalid_visual_generation", "visual_search_not_ready", "visual_operation_active", "visual_generation_changed", "visual_coverage_busy", "cross_origin_loopback", "operation_history_unavailable", "rate_limit_exceeded", "visual_operation_scope_failed", "unauthorized", "forbidden", "not_found", "bad_request":
		return true
	default:
		return false
	}
}
