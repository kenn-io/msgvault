package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
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

type personMediaArgs struct {
	PersonID int64   `json:"person_id"`
	MediaID  int64   `json:"media_id"`
	Offset   *int64  `json:"offset,omitempty"`
	Length   *int64  `json:"length,omitempty"`
	SHA256   *string `json:"sha256,omitempty"`
}
type personPatchArgs struct {
	PersonID int64                                `json:"person_id"`
	ETag     string                               `json:"etag"`
	Patch    *generated.PersonProfilePatchRequest `json:"patch"`
}

func personRecordRefusal(code string) bool {
	switch code {
	case "profile_values_unavailable", "profile_media_not_found", "profile_media_content_unavailable", "profile_value_not_found", "invalid_profile_value", "profile_patch_too_large", "person_category_duplicate", "service_alias_conflict", "invalid_profile_media_id", "invalid_if_match", "server_busy":
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
		input, err := decodeMCPOperationArguments[personEditArguments](args)
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
		data, headers, failure, err := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/people/{id}/profile/media/{media_id}/content", &generated.GetPersonProfileMediaContentRequestOptions{PathParams: &generated.GetPersonProfileMediaContentPath{ID: input.PersonID, MediaID: input.MediaID}}, store.MaxPersonMediaBytes, personRecordRefusal, http.StatusOK)
		if failure != nil {
			return failure, true, err
		}
		chunk, err := mcpserver.PersonMediaChunk(data, headers.Get("Content-Type"), args)
		if err != nil {
			return invalid()
		}
		return &mcpserver.OperationResult{Output: chunk}, true, nil
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
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if current.ETag != input.ETag {
		return "", true, &mcpserver.OperationRefusalError{Code: "person_revision_conflict"}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", true, err
	}
	var changes map[string]any
	if err := json.Unmarshal(encoded, &changes); err != nil {
		return "", true, err
	}
	if input.Patch.Media != nil {
		patch := changes["patch"].(map[string]any)
		media := patch["media"].(map[string]any)
		adds, _ := media["add"].([]any)
		for i, item := range input.Patch.Media.Add {
			if item.Data == nil {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(*item.Data)
			if err != nil {
				return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
			}
			sum := sha256.Sum256(data)
			add := adds[i].(map[string]any)
			delete(add, "data")
			add["data_size"], add["data_sha256"] = len(data), hex.EncodeToString(sum[:])
		}
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current.Output, changes, "Atomically apply all requested adds and supersedes to this person."}, json.Deterministic(true))
	return string(data), true, err
}
