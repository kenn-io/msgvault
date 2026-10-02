package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpVisualPrefix = "/api/v1/multimodal"

var mcpVisualWrites = []struct {
	name, id, path string
	properties     []string
}{
	{"build_visual_index", "startVisualAttachmentBuild", "/build", []string{"consent"}},
	{"resume_visual_index", "resumeVisualAttachmentBuild", "/run", nil},
	{"retry_visual_attachment", "retryVisualAttachmentOwner", "/retry", []string{"message_id", "blob_hash"}},
	{"retire_visual_generation", "retireVisualAttachmentGeneration", "/retire", []string{"generation_id"}},
}

func visualMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	names := []string{}
	if hasRoute("getDocumentVectorStatus", http.MethodGet, "/api/v1/documents/vectors/status", "generation_id", "after_token", "limit") {
		names = append(names, "get_document_vector_status")
	}
	if !hasRoute("getVisualAttachmentStatus", http.MethodGet, mcpVisualPrefix+"/status", "coverage") || !hasRoute("getVisualRuntimePolicy", http.MethodGet, mcpVisualPrefix+"/policy") {
		return names
	}
	names = append(names, "get_visual_index_status")
	for _, route := range mcpVisualWrites {
		properties := append([]string{"expected_generation_id", "expected_generation_fingerprint", "expected_policy_fingerprint"}, route.properties...)
		if hasRoute(route.id, http.MethodPost, mcpVisualPrefix+route.path) && mcpRequestPropertiesPresent(capabilities, route.id, properties...) {
			names = append(names, route.name)
		}
	}
	return names
}

type mcpVisualArguments struct {
	ExpectedGenerationID          int64   `json:"expected_generation_id"`
	ExpectedGenerationFingerprint string  `json:"expected_generation_fingerprint"`
	ExpectedPolicyFingerprint     string  `json:"expected_policy_fingerprint"`
	MessageID                     *int64  `json:"message_id,omitempty"`
	BlobHash                      *string `json:"blob_hash,omitempty"`
	GenerationID                  *int64  `json:"generation_id,omitempty"`
}

func mcpBoundedOpaqueValue(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func mcpPositiveSafeID(value int64) bool { return value > 0 && value <= 9007199254740991 }

func mcpVisualInput(name string, args map[string]any) (mcpVisualArguments, error) {
	input, err := decodeMCPCardDAVArguments[mcpVisualArguments](args)
	if err != nil {
		return input, err
	}
	if !mcpPositiveSafeID(input.ExpectedGenerationID) || !mcpBoundedOpaqueValue(input.ExpectedGenerationFingerprint, 1024) || !mcpBoundedOpaqueValue(input.ExpectedPolicyFingerprint, 1024) {
		return input, errors.New("visual guard is required")
	}
	if name == "retry_visual_attachment" {
		if input.MessageID == nil || !mcpPositiveSafeID(*input.MessageID) || input.BlobHash == nil || !exactDocumentFingerprint(*input.BlobHash) {
			return input, errors.New("invalid visual target")
		}
	} else if input.MessageID != nil || input.BlobHash != nil {
		return input, errors.New("unexpected visual target")
	}
	if name == "retire_visual_generation" {
		if input.GenerationID == nil || *input.GenerationID != input.ExpectedGenerationID {
			return input, errors.New("invalid retirement generation")
		}
	} else if input.GenerationID != nil {
		return input, errors.New("unexpected retirement generation")
	}
	return input, nil
}

func (b *daemonMCPOperations) executeVisualOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if name == "get_document_vector_status" {
		result, err := b.readMCPDocumentVectorStatus(ctx, args)
		return result, true, err
	}
	if name == "get_visual_index_status" {
		input, err := decodeMCPCardDAVArguments[struct {
			Coverage bool `json:"coverage,omitempty"`
		}](args)
		if err != nil {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
		}
		coverage := generated.N0
		if input.Coverage {
			coverage = generated.N1
		}
		result, err := readMCPOperationJSON[generated.Status](ctx, b.client, http.MethodGet, mcpVisualPrefix+"/status", &generated.GetVisualAttachmentStatusRequestOptions{Query: &generated.GetVisualAttachmentStatusQuery{Coverage: &coverage}}, http.StatusOK)
		if err != nil || result.IsError {
			return result, true, err
		}
		status, ok := result.Output.(generated.Status)
		if !ok {
			return operationFailure("invalid_operation_response", false), true, nil
		}
		policyResult, err := b.readMCPVisualPolicy(ctx)
		if err != nil {
			return policyResult, true, err
		}
		output := mcpserver.VisualIndexStatus{Status: status}
		if !policyResult.IsError {
			policy, ok := policyResult.Output.(generated.VisualRuntimePolicy)
			if !ok {
				return operationFailure("invalid_operation_response", false), true, nil
			}
			output.CurrentPolicy = &policy
		}
		return &mcpserver.OperationResult{Output: output}, true, nil
	}
	var routePath string
	for _, route := range mcpVisualWrites {
		if route.name == name {
			routePath = route.path
			break
		}
	}
	if routePath == "" {
		return nil, false, nil
	}
	input, err := mcpVisualInput(name, args)
	if err != nil {
		return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
	}
	if name == "retire_visual_generation" {
		body := generated.VisualRetireRequest{GenerationID: *input.GenerationID, ExpectedGenerationID: &input.ExpectedGenerationID, ExpectedGenerationFingerprint: &input.ExpectedGenerationFingerprint, ExpectedPolicyFingerprint: &input.ExpectedPolicyFingerprint}
		result, err := readMCPOperationJSON[struct{}](ctx, b.client, http.MethodPost, mcpVisualPrefix+routePath, &generated.RetireVisualAttachmentGenerationRequestOptions{Body: &body}, http.StatusNoContent)
		if err == nil && !result.IsError {
			result.Output = mcpserver.VisualRetirement{GenerationID: body.GenerationID, Retired: true}
		}
		return result, true, err
	}
	var result *mcpserver.OperationResult
	switch name {
	case "build_visual_index":
		body := generated.VisualBuildRequest{Consent: true, ExpectedGenerationID: &input.ExpectedGenerationID, ExpectedGenerationFingerprint: &input.ExpectedGenerationFingerprint, ExpectedPolicyFingerprint: &input.ExpectedPolicyFingerprint}
		result, err = readMCPOperationJSON[generated.Status](ctx, b.client, http.MethodPost, mcpVisualPrefix+routePath, &generated.StartVisualAttachmentBuildRequestOptions{Body: &body}, http.StatusOK)
	case "resume_visual_index":
		body := generated.VisualResumeRequest{ExpectedGenerationID: &input.ExpectedGenerationID, ExpectedGenerationFingerprint: &input.ExpectedGenerationFingerprint, ExpectedPolicyFingerprint: &input.ExpectedPolicyFingerprint}
		result, err = readMCPOperationJSON[generated.Status](ctx, b.client, http.MethodPost, mcpVisualPrefix+routePath, &generated.ResumeVisualAttachmentBuildRequestOptions{Body: &body}, http.StatusOK)
	case "retry_visual_attachment":
		body := generated.VisualRetryRequest{MessageID: *input.MessageID, BlobHash: *input.BlobHash, ExpectedGenerationID: &input.ExpectedGenerationID, ExpectedGenerationFingerprint: &input.ExpectedGenerationFingerprint, ExpectedPolicyFingerprint: &input.ExpectedPolicyFingerprint}
		result, err = readMCPOperationJSON[generated.Status](ctx, b.client, http.MethodPost, mcpVisualPrefix+routePath, &generated.RetryVisualAttachmentOwnerRequestOptions{Body: &body}, http.StatusOK)
	}
	return result, true, err
}

func (b *daemonMCPOperations) readMCPDocumentVectorStatus(ctx context.Context, args map[string]any) (*mcpserver.OperationResult, error) {
	input, err := decodeMCPCardDAVArguments[generated.GetDocumentVectorStatusQuery](args)
	if err != nil || (input.GenerationID != nil && !mcpPositiveSafeID(*input.GenerationID)) || (input.Limit != nil && (*input.Limit < 1 || *input.Limit > 1000)) || (input.AfterToken != nil && !mcpBoundedOpaqueValue(*input.AfterToken, 32<<10)) {
		return operationFailure("invalid_operation_arguments", false), nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
	}
	return readMCPOperationJSON[generated.DocumentVectorOperationsResponse](ctx, b.client, http.MethodGet, "/api/v1/documents/vectors/status", &generated.GetDocumentVectorStatusRequestOptions{Query: &input}, http.StatusOK)
}

func (b *daemonMCPOperations) readMCPVisualPolicy(ctx context.Context) (*mcpserver.OperationResult, error) {
	return readMCPOperationJSON[generated.VisualRuntimePolicy](ctx, b.client, http.MethodGet, mcpVisualPrefix+"/policy", nil, http.StatusOK)
}

func (b *daemonMCPOperations) visualOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	found := false
	for _, route := range mcpVisualWrites {
		if route.name == name {
			found = true
			break
		}
	}
	if !found {
		return "", false, nil
	}
	input, err := mcpVisualInput(name, args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_arguments"}
	}
	result, err := b.readMCPVisualPolicy(ctx)
	if err != nil {
		return "", true, err
	}
	if result.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "visual_policy_unavailable"}
	}
	policy, ok := result.Output.(generated.VisualRuntimePolicy)
	if !ok || policy.GenerationID != input.ExpectedGenerationID || policy.GenerationFingerprint != input.ExpectedGenerationFingerprint || policy.CurrentPolicyFingerprint != input.ExpectedPolicyFingerprint {
		return "", true, &mcpserver.OperationRefusalError{Code: "visual_policy_changed"}
	}
	effect := "Run one bounded visual pass under exact consent. Media and context in this scope may leave this host and incur provider charges; recorded provider idempotency is not assumed."
	if name == "build_visual_index" {
		effect = "Record exact hosted-processing consent and run one bounded visual pass. Media and context in this scope may leave this host and incur provider charges; recorded provider idempotency is not assumed."
	}
	if name == "retire_visual_generation" {
		effect = "Retire the exact current generation and delete its backend vectors. No media is uploaded and no archived message is deleted."
	}
	data, err := json.Marshal(struct {
		Operation string                        `json:"operation"`
		Policy    generated.VisualRuntimePolicy `json:"policy"`
		Arguments map[string]any                `json:"arguments"`
		Effect    string                        `json:"effect"`
	}{name, policy, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
