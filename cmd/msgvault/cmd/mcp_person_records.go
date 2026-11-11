package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func personRecordCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, route := range []struct{ name, id, method, path string }{
		{"get_person_record", "getPersonStructuredProfile", http.MethodGet, "/api/v1/people/{id}/profile"},
		{"get_person_record_history", "getPersonProfileHistory", http.MethodGet, "/api/v1/people/{id}/profile/history"},
		{"get_person_record_media", "getPersonProfileMediaContent", http.MethodGet, "/api/v1/people/{id}/profile/media/{media_id}/content"},
	} {
		if hasRoute(route.id, route.method, route.path) {
			names = append(names, route.name)
		}
	}
	if slices.Contains(names, "get_person_record") && hasRoute("patchPersonStructuredProfile", http.MethodPatch, "/api/v1/people/{id}/profile") && mcpRequestPropertiesPresent(caps, "patchPersonStructuredProfile", "names", "contact_points", "addresses", "dates", "categories", "media") {
		names = append(names, "patch_person_record")
	}
	return names
}

type personRecordArgs struct {
	PersonID int64 `json:"person_id"`
}
type personMediaArgs struct {
	PersonID int64 `json:"person_id"`
	MediaID  int64 `json:"media_id"`
}
type personPatchArgs struct {
	PersonID int64                                `json:"person_id"`
	ETag     string                               `json:"etag"`
	Patch    *generated.PersonProfilePatchRequest `json:"patch"`
}

func personRecordRefusal(code string) bool {
	switch code {
	case "profile_values_unavailable", "profile_media_not_found", "profile_media_content_unavailable", "profile_value_not_found", "invalid_profile_value", "profile_patch_too_large", "person_category_duplicate", "service_alias_conflict", "invalid_profile_media_id", "invalid_if_match", "if_match_required", "person_revision_conflict", "person_profile_not_found", "person_enrichment_dispatch_in_progress", "unauthorized", "operation_in_progress", "server_busy":
		return true
	}
	return false
}
func recordProjection(result *mcpserver.OperationResult, writes bool) *mcpserver.OperationResult {
	if result == nil || result.IsError {
		return result
	}
	profile, ok := result.Output.(generated.StructuredPersonProfile)
	if !ok || result.ETag == "" {
		return operationFailure("invalid_operation_response", writes)
	}
	result.Output = mcpserver.StructuredPersonRecord{ETag: result.ETag, Profile: profile}
	return result
}
func (b *daemonMCPOperations) executePersonRecord(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	switch name {
	case "get_person_record", "get_person_record_history":
		input, err := decodeMCPOperationArguments[personRecordArgs](args)
		if err != nil || !validLoopID(input.PersonID) {
			return invalid()
		}
		if name == "get_person_record" {
			result, err := readMCPOperationJSONWithClassifier[generated.StructuredPersonProfile](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/profile", &generated.GetPersonStructuredProfileRequestOptions{PathParams: &generated.GetPersonStructuredProfilePath{ID: input.PersonID}}, personRecordRefusal, http.StatusOK)
			return recordProjection(result, false), true, err
		}
		result, err := readMCPOperationJSONWithClassifier[generated.PersonProfileHistory](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/profile/history", &generated.GetPersonProfileHistoryRequestOptions{PathParams: &generated.GetPersonProfileHistoryPath{ID: input.PersonID}}, personRecordRefusal, http.StatusOK)
		return result, true, err
	case "patch_person_record":
		input, err := decodeMCPOperationArguments[personPatchArgs](args)
		if err != nil || !validLoopID(input.PersonID) || input.ETag == "" || input.Patch == nil {
			return invalid()
		}
		result, err := readMCPOperationJSONWithClassifier[generated.StructuredPersonProfile](ctx, b.client, http.MethodPatch, "/api/v1/people/{id}/profile", &generated.PatchPersonStructuredProfileRequestOptions{PathParams: &generated.PatchPersonStructuredProfilePath{ID: input.PersonID}, Header: &generated.PatchPersonStructuredProfileHeaders{IfMatch: input.ETag}, Body: input.Patch}, personRecordRefusal, http.StatusOK)
		return recordProjection(result, true), true, err
	case "get_person_record_media":
		input, err := decodeMCPOperationArguments[personMediaArgs](args)
		if err != nil || !validLoopID(input.PersonID) || !validLoopID(input.MediaID) {
			return invalid()
		}
		response, err := b.client.DoGeneratedRequestWithContext(ctx, http.MethodGet, "/api/v1/people/{id}/profile/media/{media_id}/content", &generated.GetPersonProfileMediaContentRequestOptions{PathParams: &generated.GetPersonProfileMediaContentPath{ID: input.PersonID, MediaID: input.MediaID}})
		if err != nil {
			return operationFailure("operation_failed", false), true, err
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
		if err != nil {
			return operationFailure("operation_response_failed", false), true, err
		}
		if len(data) > 8<<20 {
			return operationFailure("operation_response_limit_exceeded", false), true, nil
		}
		if response.StatusCode != http.StatusOK {
			var failure struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(data, &failure)
			if personRecordRefusal(failure.Error) {
				return operationFailure(failure.Error, false), true, nil
			}
			return operationFailure("operation_refused", false), true, nil
		}
		return &mcpserver.OperationResult{Output: mcpserver.PersonMediaContent{MediaType: response.Header.Get("Content-Type"), DataBase64: base64.StdEncoding.EncodeToString(data)}}, true, nil
	default:
		return nil, false, nil
	}
}
func (b *daemonMCPOperations) personRecordDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != "patch_person_record" {
		return "", false, nil
	}
	input, err := decodeMCPOperationArguments[personPatchArgs](args)
	if err != nil || !validLoopID(input.PersonID) || input.ETag == "" || input.Patch == nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	current, _, err := b.executePersonRecord(ctx, "get_person_record", map[string]any{"person_id": input.PersonID})
	if err != nil {
		return "", true, err
	}
	if current == nil || current.IsError {
		code := "operation_context_unavailable"
		if current != nil {
			data, marshalErr := json.Marshal(current.Output)
			var failure struct {
				Error string `json:"error"`
			}
			if marshalErr == nil && json.Unmarshal(data, &failure) == nil && failure.Error != "" {
				code = failure.Error
			}
		}
		return "", true, &mcpserver.OperationRefusalError{Code: code}
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current.Output, args, "Atomically apply all requested adds and supersedes to this person."}, json.Deterministic(true))
	return string(data), true, err
}
