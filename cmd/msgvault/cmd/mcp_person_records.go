package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"slices"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const maxPersonRecordDisclosureBytes = api.MaxPersonProfilePatchBytes + (1 << 20)

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
	case "profile_values_unavailable", "profile_media_not_found", "profile_media_content_unavailable", "profile_value_not_found", "invalid_profile_value", "profile_patch_too_large", "person_category_duplicate", "service_alias_conflict", "invalid_if_match", "server_busy":
		return true
	}
	return false
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
			data, headers, failure, err := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/people/{id}/profile", &generated.GetPersonStructuredProfileRequestOptions{PathParams: &generated.GetPersonStructuredProfilePath{ID: input.PersonID}}, mcpserver.MaxDownloadBytes, personRecordRefusal, http.StatusOK)
			if failure != nil {
				return failure, true, err
			}
			if headers.Get("ETag") == "" {
				return operationFailure("invalid_operation_response", false), true, nil
			}
			return &mcpserver.OperationResult{Data: data, ETag: headers.Get("ETag")}, true, nil
		}
		result, err := readMCPOperationJSONWithClassifier[generated.PersonProfileHistory](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/profile/history", &generated.GetPersonProfileHistoryRequestOptions{PathParams: &generated.GetPersonProfileHistoryPath{ID: input.PersonID}}, personRecordRefusal, http.StatusOK)
		return result, true, err
	case "patch_person_record":
		input, err := decodeMCPOperationArguments[personPatchArgs](args)
		if err != nil || !validLoopID(input.PersonID) || input.ETag == "" || input.Patch == nil {
			return invalid()
		}
		data, headers, failure, err := readMCPOperationBytes(ctx, b.client, http.MethodPatch, "/api/v1/people/{id}/profile", &generated.PatchPersonStructuredProfileRequestOptions{PathParams: &generated.PatchPersonStructuredProfilePath{ID: input.PersonID}, Header: &generated.PatchPersonStructuredProfileHeaders{IfMatch: input.ETag}, Body: input.Patch}, mcpserver.MaxDownloadBytes, personRecordRefusal, http.StatusOK)
		if failure != nil {
			return failure, true, err
		}
		if headers.Get("ETag") == "" {
			return operationFailure("invalid_operation_response", true), true, nil
		}
		record := mcpserver.StructuredPersonRecord{ETag: headers.Get("ETag")}
		if len(data) <= 1<<20 {
			var profile generated.StructuredPersonProfile
			if err := json.Unmarshal(data, &profile); err != nil {
				return operationFailure("invalid_operation_response", true), true, err
			}
			record.Profile = &profile
		}
		return &mcpserver.OperationResult{Output: record, ETag: record.ETag}, true, nil
	case "get_person_record_media":
		input, err := decodeMCPOperationArguments[personMediaArgs](args)
		if err != nil || !validLoopID(input.PersonID) || !validLoopID(input.MediaID) {
			return invalid()
		}
		data, headers, failure, err := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/people/{id}/profile/media/{media_id}/content", &generated.GetPersonProfileMediaContentRequestOptions{PathParams: &generated.GetPersonProfileMediaContentPath{ID: input.PersonID, MediaID: input.MediaID}}, mcpserver.MaxDownloadBytes, personRecordRefusal, http.StatusOK)
		if failure != nil {
			return failure, true, err
		}
		return &mcpserver.OperationResult{Data: data, MediaType: headers.Get("Content-Type")}, true, nil
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
	count := 0
	if p := input.Patch.Names; p != nil {
		count += len(p.Add) + len(p.Supersede)
	}
	if p := input.Patch.ContactPoints; p != nil {
		count += len(p.Add) + len(p.Supersede)
	}
	if p := input.Patch.Addresses; p != nil {
		count += len(p.Add) + len(p.Supersede)
	}
	if p := input.Patch.Dates; p != nil {
		count += len(p.Add) + len(p.Supersede)
	}
	if p := input.Patch.Categories; p != nil {
		count += len(p.Add) + len(p.Supersede)
	}
	if p := input.Patch.Media; p != nil {
		count += len(p.Add) + len(p.Supersede)
	}
	if count == 0 {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_profile_value"}
	}
	if count > store.MaxPersonProfilePatchOperations {
		return "", true, &mcpserver.OperationRefusalError{Code: "profile_patch_too_large"}
	}
	// The generated client's runtime uses v1 JSON for request bodies.
	encodedPatch, err := runtime.MarshalJSON(input.Patch)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	if len(encodedPatch) > api.MaxPersonProfilePatchBytes {
		return "", true, &mcpserver.OperationRefusalError{Code: "profile_patch_too_large"}
	}
	type mediaAdd struct {
		generated.PersonMediaInputRequest

		DataSize   *int    `json:"data_size,omitzero"`
		DataSHA256 *string `json:"data_sha256,omitzero"`
	}
	type mediaPatch struct {
		Add       []mediaAdd `json:"add,omitempty"`
		Supersede []int64    `json:"supersede,omitempty"`
	}
	patch := struct {
		generated.PersonProfilePatchRequest

		Media *mediaPatch `json:"media,omitempty"`
	}{PersonProfilePatchRequest: *input.Patch}
	if input.Patch.Media != nil {
		patch.Media = &mediaPatch{Supersede: input.Patch.Media.Supersede}
		for _, item := range input.Patch.Media.Add {
			add := mediaAdd{PersonMediaInputRequest: item}
			if item.Data != nil {
				data, err := base64.StdEncoding.DecodeString(*item.Data)
				if err != nil {
					return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
				}
				if len(data) > store.MaxPersonMediaBytes {
					return "", true, &mcpserver.OperationRefusalError{Code: "invalid_profile_value"}
				}
				sum := sha256.Sum256(data)
				size, digest := len(data), hex.EncodeToString(sum[:])
				add.Data, add.DataSize, add.DataSHA256 = nil, &size, &digest
			}
			patch.Media.Add = append(patch.Media.Add, add)
		}
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
	var profile personDisclosureProfile
	if err := json.Unmarshal(current.Data, &profile); err != nil {
		return "", true, err
	}
	selected := struct {
		Person        generated.Person `json:"person"`
		Names         []any            `json:"names,omitempty"`
		ContactPoints []any            `json:"contact_points,omitempty"`
		Addresses     []any            `json:"addresses,omitempty"`
		Dates         []any            `json:"dates,omitempty"`
		Categories    []any            `json:"categories,omitempty"`
		Media         []any            `json:"media,omitempty"`
	}{Person: profile.Person}
	selectValues := func(values personDisclosureProfile) (bool, error) {
		missing := false
		selectSection := func(dest *[]any, values []jsontext.Value, ids []int64) error {
			var err error
			*dest, missing, err = superseded(values, ids, missing)
			return err
		}
		if p := input.Patch.Names; p != nil {
			if err := selectSection(&selected.Names, values.Names, p.Supersede); err != nil {
				return false, err
			}
		}
		if p := input.Patch.ContactPoints; p != nil {
			if err := selectSection(&selected.ContactPoints, values.ContactPoints, p.Supersede); err != nil {
				return false, err
			}
		}
		if p := input.Patch.Addresses; p != nil {
			if err := selectSection(&selected.Addresses, values.Addresses, p.Supersede); err != nil {
				return false, err
			}
		}
		if p := input.Patch.Dates; p != nil {
			if err := selectSection(&selected.Dates, values.Dates, p.Supersede); err != nil {
				return false, err
			}
		}
		if p := input.Patch.Categories; p != nil {
			if err := selectSection(&selected.Categories, values.Categories, p.Supersede); err != nil {
				return false, err
			}
		}
		if p := input.Patch.Media; p != nil {
			if err := selectSection(&selected.Media, values.Media, p.Supersede); err != nil {
				return false, err
			}
		}
		return missing, nil
	}
	missing, err := selectValues(profile)
	if err != nil {
		return "", true, err
	}
	if missing {
		data, _, failure, err := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/people/{id}/profile/history", &generated.GetPersonProfileHistoryRequestOptions{PathParams: &generated.GetPersonProfileHistoryPath{ID: input.PersonID}}, mcpserver.MaxDownloadBytes, personRecordRefusal, http.StatusOK)
		if err != nil {
			return "", true, err
		}
		if failure != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		var history personDisclosureProfile
		if err := json.Unmarshal(data, &history); err != nil {
			return "", true, err
		}
		if history.Person.Revision != profile.Person.Revision {
			return "", true, &mcpserver.OperationRefusalError{Code: "person_revision_conflict"}
		}
		missing, err = selectValues(history)
		if err != nil {
			return "", true, err
		}
	}
	if missing {
		return "", true, &mcpserver.OperationRefusalError{Code: "profile_value_not_found"}
	}
	changes := struct {
		PersonID int64  `json:"person_id"`
		ETag     string `json:"etag"`
		Patch    any    `json:"patch"`
	}{input.PersonID, input.ETag, patch}
	data, err := json.Marshal(struct {
		Operation string `json:"operation"`
		Current   any    `json:"current"`
		Changes   any    `json:"changes"`
		Effect    string `json:"effect"`
	}{name, selected, changes, "Atomically apply all requested adds and supersedes to this person."}, json.Deterministic(true))
	if err == nil && len(data) > maxPersonRecordDisclosureBytes {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_response_limit_exceeded"}
	}
	return string(data), true, err
}

type personDisclosureProfile struct {
	Person        generated.Person `json:"person"`
	Names         []jsontext.Value `json:"names"`
	ContactPoints []jsontext.Value `json:"contact_points"`
	Addresses     []jsontext.Value `json:"addresses"`
	Dates         []jsontext.Value `json:"dates"`
	Categories    []jsontext.Value `json:"categories"`
	Media         []jsontext.Value `json:"media"`
}

func superseded(values []jsontext.Value, ids []int64, missing bool) ([]any, bool, error) {
	var selected []any
	found := make(map[int64]bool, len(ids))
	for _, value := range values {
		var entry struct {
			Envelope generated.ValueEnvelope `json:"envelope"`
		}
		if err := json.Unmarshal(value, &entry); err != nil {
			return nil, false, err
		}
		meta := entry.Envelope
		if meta.SupersededAt == nil && slices.Contains(ids, meta.ID) {
			found[meta.ID] = true
			selected = append(selected, value)
		}
	}
	for _, value := range ids {
		missing = missing || !found[value]
	}
	return selected, missing, nil
}
