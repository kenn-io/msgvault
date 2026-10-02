package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

const documentStatusCurrentPath = "/api/v1/documents/status/current"
const documentFingerprintArgument = "fingerprint"

func isMCPDocumentOperation(name string) bool {
	switch name {
	case "get_document_index_status", "get_document_processing_policy", "consent_document_processing", "build_document_index", "resume_document_index", "retry_document_extraction":
		return true
	}
	return false
}

func resolveMCPDocumentManifest(path string, info HTTPStoreInfo, delegated bool) (string, error) {
	if path == "" {
		return "", nil
	}
	if delegated || info.Kind != HTTPStoreLocalDaemon {
		return "", errors.New("--document-capabilities requires the local daemon on the MCP host")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve operator-selected document manifest: %w", err)
	}
	if _, err := loadDocumentCapabilityManifest(absolute); err != nil {
		return "", fmt.Errorf("validate operator-selected document manifest: %w", err)
	}
	return absolute, nil
}

func documentMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	if capabilities == nil || capabilities.Delegated || !hasRoute("getCurrentDocumentIndexStatus", http.MethodGet, documentStatusCurrentPath) {
		return nil
	}
	return []string{"get_document_index_status"}
}
func (b *daemonMCPOperations) useDocumentManifest(path string) {
	if path == "" {
		return
	}
	b.documentManifest = path
	if !b.documentCommandHasFlags("policy", "capabilities", flagJSON) {
		return
	}
	b.supported = append(b.supported, "get_document_processing_policy")
	for _, operation := range []struct {
		name, command string
		flags         []string
	}{
		{"consent_document_processing", "consent-mistral", []string{"yes"}},
		{"build_document_index", documentBuildSubcommand, []string{"yes", "limit", "full-rebuild"}},
		{"resume_document_index", cmdUseResume, []string{"yes", "limit"}},
		{"retry_document_extraction", "retry", []string{"hash"}},
	} {
		if b.documentCommandHasFlags(operation.command, append([]string{"capabilities", flagJSON, documentFingerprintFlag}, operation.flags...)...) {
			b.supported = append(b.supported, operation.name)
		}
	}
}
func (b *daemonMCPOperations) documentCommandHasFlags(command string, flags ...string) bool {
	for _, descriptor := range b.commands {
		if descriptor.Name != "documents "+command || descriptor.Delegated {
			continue
		}
		for _, flag := range flags {
			if !slices.Contains(descriptor.Flags, flag) {
				return false
			}
		}
		return true
	}
	return false
}

type mcpDocumentInput struct {
	Fingerprint       string `json:"fingerprint,omitempty"`
	Limit             *int   `json:"limit,omitempty"`
	FullRebuild       *bool  `json:"full_rebuild,omitempty"`
	CanonicalBlobHash string `json:"canonical_blob_hash,omitempty"`
}

func mcpDocumentArguments(name string, args map[string]any) (mcpDocumentInput, error) {
	allowed := []string{documentFingerprintArgument}
	switch name {
	case "get_document_index_status", "get_document_processing_policy":
		allowed = nil
	case "build_document_index":
		allowed = append(allowed, "limit", "full_rebuild")
	case "resume_document_index":
		allowed = append(allowed, "limit")
	case "retry_document_extraction":
		allowed = append(allowed, "canonical_blob_hash")
	case "consent_document_processing":
	default:
		return mcpDocumentInput{}, errors.New("unknown document operation")
	}
	for key, value := range args {
		if !slices.Contains(allowed, key) || value == nil {
			return mcpDocumentInput{}, errors.New("invalid document field")
		}
	}
	input, err := decodeMCPOperationArguments[mcpDocumentInput](args)
	if err != nil {
		return input, err
	}
	if len(allowed) > 0 && !exactDocumentFingerprint(input.Fingerprint) {
		return input, errors.New("invalid document fingerprint")
	}
	if input.Limit != nil && (*input.Limit < 1 || *input.Limit > 10000) {
		return input, errors.New("invalid document limit")
	}
	if name == "retry_document_extraction" && !exactDocumentFingerprint(input.CanonicalBlobHash) {
		return input, errors.New("invalid document hash")
	}
	return input, nil
}
func (b *daemonMCPOperations) executeDocumentOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if !isMCPDocumentOperation(name) {
		return nil, false, nil
	}
	input, err := mcpDocumentArguments(name, args)
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid input becomes a fixed public refusal.
	}
	if name == "get_document_index_status" {
		result, err := readMCPOperationJSON[mcpserver.DocumentIndexStatus](ctx, b.client, http.MethodGet, documentStatusCurrentPath, nil, http.StatusOK)
		if err == nil && !result.IsError {
			output, ok := result.Output.(mcpserver.DocumentIndexStatus)
			if !ok || !validMCPDocumentFailures(output.Status.Failures) {
				return operationFailure("invalid_document_response", false), true, nil
			}
		}
		return result, true, err
	}
	if b.documentManifest == "" {
		return operationFailure("document_manifest_unavailable", false), true, nil
	}
	if name == "get_document_processing_policy" {
		result, err := b.readDocumentPolicy(ctx)
		return result, true, err
	}
	command := ""
	switch name {
	case "consent_document_processing":
		command = "consent-mistral"
	case "build_document_index":
		command = documentBuildSubcommand
	case "resume_document_index":
		command = cmdUseResume
	case "retry_document_extraction":
		command = "retry"
	}
	argv := []string{documentsCommandName, command, "--capabilities=" + b.documentManifest, "--json", "--if-fingerprint=" + input.Fingerprint}
	if name == "retry_document_extraction" {
		argv = append(argv, "--hash="+input.CanonicalBlobHash)
	} else {
		argv = append(argv, "--yes")
	}
	if name == "build_document_index" || name == "resume_document_index" {
		limit := 100
		if input.Limit != nil {
			limit = *input.Limit
		}
		argv = append(argv, "--limit="+strconv.Itoa(limit))
		if input.FullRebuild != nil {
			argv = append(argv, "--full-rebuild="+strconv.FormatBool(*input.FullRebuild))
		}
	}
	stream, runErr := b.client.RunMCPCLICommand(ctx, argv, "")
	result, decodeErr := decodeMCPDocumentCLI(name, input, stream)
	if runErr != nil && stream != nil && !stream.Failed {
		return operationFailure("document_execution_failed", true), true, runErr
	}
	return result, true, decodeErr
}
func (b *daemonMCPOperations) readDocumentPolicy(ctx context.Context) (*mcpserver.OperationResult, error) {
	stream, err := b.client.RunMCPCLICommand(ctx, []string{documentsCommandName, "policy", "--capabilities=" + b.documentManifest, "--json"}, "")
	if err != nil {
		return operationFailure("document_policy_unavailable", false), err
	}
	if stream == nil || stream.Failed {
		return operationFailure("document_policy_unavailable", false), nil
	}
	var policy mcpserver.DocumentPolicy
	if err := json.Unmarshal([]byte(stream.Stdout), &policy, json.RejectUnknownMembers(true)); err != nil {
		return operationFailure("invalid_document_policy", false), err
	}
	if !exactDocumentFingerprint(policy.Fingerprint) || policy.ProfileID != "documents-v1:"+policy.Fingerprint || policy.Policy.Endpoint == "" || policy.Policy.Model == "" || len(policy.Policy.AllowedMediaTypes) == 0 || policy.Disclosure == "" {
		return operationFailure("invalid_document_policy", false), nil
	}
	return &mcpserver.OperationResult{Output: policy}, nil
}
func (b *daemonMCPOperations) documentOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if !isMCPDocumentOperation(name) {
		return "", false, nil
	}
	input, err := mcpDocumentArguments(name, args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	if b.documentManifest == "" {
		return "", true, &mcpserver.OperationRefusalError{Code: "document_manifest_unavailable"}
	}
	result, err := b.readDocumentPolicy(ctx)
	if err != nil {
		return "", true, err
	}
	if result.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "document_policy_unavailable"}
	}
	policy, ok := result.Output.(mcpserver.DocumentPolicy)
	if !ok {
		return "", true, errors.New("unexpected document policy result")
	}
	if policy.Fingerprint != input.Fingerprint {
		return "", true, &mcpserver.OperationRefusalError{Code: "document_policy_changed"}
	}
	if !policy.Enabled {
		return "", true, &mcpserver.OperationRefusalError{Code: "document_processing_disabled"}
	}
	if name != "consent_document_processing" && name != "retry_document_extraction" && (!policy.ExactConsent || !policy.ProfileEnabled) {
		return "", true, &mcpserver.OperationRefusalError{Code: "document_consent_required"}
	}
	effects := "Record exact upload consent; future explicit index passes can upload the disclosed attachments. This call does not contact the provider."
	if name == "build_document_index" || name == "resume_document_index" {
		effects = "Run the bounded extraction pass using exact consent. Disclosed bytes leave this host and provider charges may apply; a partial failure remains a failed result. Resume continues the current rebuild identity."
	}
	if name == "retry_document_extraction" {
		effects = "Reset failed extraction work under this exact profile. This call schedules a retry; it does not upload or complete extraction."
	}
	data, err := json.Marshal(struct {
		Operation string                   `json:"operation"`
		Policy    mcpserver.DocumentPolicy `json:"policy"`
		Arguments map[string]any           `json:"arguments"`
		Effect    string                   `json:"effect"`
	}{name, policy, args, effects}, json.Deterministic(true))
	return string(data), true, err
}
func validMCPDocumentFailures(failures []mcpserver.DocumentFailure) bool {
	if len(failures) > 20 {
		return false
	}
	for _, failure := range failures {
		if failure.CanonicalBlobHash != "" && !exactDocumentFingerprint(failure.CanonicalBlobHash) {
			return false
		}
		for _, value := range []string{failure.ReasonCode, failure.Detail, failure.State} {
			if len(value) > 1024 || !utf8.ValidString(value) || strings.ContainsFunc(value, func(r rune) bool {
				return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
			}) {
				return false
			}
		}
	}
	return true
}
func decodeMCPDocumentCLI(name string, input mcpDocumentInput, stream *daemonclient.MCPCLIResult) (*mcpserver.OperationResult, error) {
	if stream == nil || (stream.Failed && slices.Contains([]string{"invalid_cli_stream", "output_limit_exceeded"}, stream.ErrorCode)) {
		return operationFailure("document_execution_failed", true), nil
	}
	if stream.Failed {
		var failure struct {
			Error                     string `json:"error"`
			OperationMayHaveCompleted bool   `json:"operation_may_have_completed"`
		}
		if json.Unmarshal([]byte(stream.Stdout), &failure, json.RejectUnknownMembers(true)) == nil && slices.Contains([]string{"invalid_document_fingerprint", "document_policy_changed", "document_confirmation_required", "document_consent_required", "document_execution_failed"}, failure.Error) {
			return &mcpserver.OperationResult{Output: failure, IsError: true}, nil
		}
	}
	if name == "build_document_index" || name == "resume_document_index" {
		var output mcpserver.DocumentBuild
		if err := json.Unmarshal([]byte(stream.Stdout), &output, json.RejectUnknownMembers(true)); err != nil {
			return operationFailure("invalid_document_response", true), err
		}
		if output.Fingerprint != input.Fingerprint || output.ProfileID != "documents-v1:"+output.Fingerprint || output.RunID <= 0 || output.Attempted != output.Succeeded+output.Failed || output.Succeeded < 0 || output.Failed < 0 || !slices.Contains([]string{"completed", "pending", string(startupCacheBuildOutcomeFailed)}, output.State) || !validMCPDocumentFailures(output.Failures) || !validMCPDocumentFailures(output.Coverage.Failures) {
			return operationFailure("invalid_document_response", true), nil
		}
		return &mcpserver.OperationResult{Output: output, IsError: stream.Failed || output.State == string(startupCacheBuildOutcomeFailed) || output.Failed > 0}, nil
	}
	if stream.Failed {
		return operationFailure("document_execution_failed", true), nil
	}
	if name == "consent_document_processing" {
		var output mcpserver.DocumentConsent
		if err := json.Unmarshal([]byte(stream.Stdout), &output, json.RejectUnknownMembers(true)); err != nil {
			return operationFailure("invalid_document_response", true), err
		}
		if output.Fingerprint != input.Fingerprint || output.ProfileID != "documents-v1:"+output.Fingerprint || !output.ExactConsent || output.ConsentedAt == nil {
			return operationFailure("invalid_document_response", true), nil
		}
		return &mcpserver.OperationResult{Output: output}, nil
	}
	var output mcpserver.DocumentRetry
	if err := json.Unmarshal([]byte(stream.Stdout), &output, json.RejectUnknownMembers(true)); err != nil {
		return operationFailure("invalid_document_response", true), err
	}
	if output.Fingerprint != input.Fingerprint || output.ProfileID != "documents-v1:"+output.Fingerprint || output.CanonicalBlobHash != input.CanonicalBlobHash || !output.Reset {
		return operationFailure("invalid_document_response", true), nil
	}
	return &mcpserver.OperationResult{Output: output}, nil
}
