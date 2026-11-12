package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func relationshipRecordCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, route := range []struct {
		name, id, path string
		query          []string
	}{
		{"list_relationship_types", "listRelationshipTypes", "/api/v1/relationship-types", nil},
		{"get_relationship_type", "getRelationshipType", "/api/v1/relationship-types/{id}", nil},
		{"get_person_relationship_record", "getPersonRelationship", "/api/v1/person-relationships/{id}", nil},
		{"list_person_relationships", "listPersonRelationships", "/api/v1/people/{id}/relationships", []string{"include_ended"}},
		{"list_person_relationship_reviews", "listPersonRelationshipReviews", "/api/v1/person-relationship-reviews", []string{"status", "person_id"}},
		{"get_person_network", "getPersonNetwork", "/api/v1/people/{id}/network", []string{"depth", "include_ended"}},
	} {
		if hasRoute(route.id, http.MethodGet, route.path, route.query...) {
			names = append(names, route.name)
		}
	}

	for _, route := range []struct {
		name, id, method, path string
		properties             []string
	}{
		{"create_relationship_type", "createRelationshipType", http.MethodPost, "/api/v1/relationship-types", []string{"slug", "forward_label", "reverse_label", "is_symmetric", "vcard_related_type", "color", "icon", "description"}},
		{"update_relationship_type", "patchRelationshipType", http.MethodPatch, "/api/v1/relationship-types/{id}", []string{"forward_label", "reverse_label", "vcard_related_type", "color", "icon", "description"}},
		{"delete_relationship_type", "deleteRelationshipType", http.MethodDelete, "/api/v1/relationship-types/{id}", nil},
		{"create_person_relationship", "createPersonRelationship", http.MethodPost, "/api/v1/person-relationships", []string{"source_person_id", "target_person_id", "relationship_type_slug", "start_date", "end_date", "notes"}},
		{"update_person_relationship", "patchPersonRelationship", http.MethodPatch, "/api/v1/person-relationships/{id}", []string{"end_date", "notes"}},
		{"delete_person_relationship", "deletePersonRelationship", http.MethodDelete, "/api/v1/person-relationships/{id}", nil},
	} {
		if hasRoute(route.id, route.method, route.path) && (len(route.properties) == 0 || mcpRequestPropertiesPresent(capabilities, route.id, route.properties...)) && (strings.HasPrefix(route.name, "create_") || strings.Contains(route.name, "relationship_type") && hasRoute("getRelationshipType", http.MethodGet, "/api/v1/relationship-types/{id}") || strings.Contains(route.name, "person_relationship") && hasRoute("getPersonRelationship", http.MethodGet, "/api/v1/person-relationships/{id}")) {
			names = append(names, route.name)
		}
	}
	return names
}

type relationshipReadArguments struct {
	ID           int64                                               `json:"id,omitempty"`
	PersonID     int64                                               `json:"person_id,omitempty"`
	IncludeEnded *bool                                               `json:"include_ended,omitempty"`
	Depth        *int64                                              `json:"depth,omitempty"`
	Status       *generated.ListPersonRelationshipReviewsQueryStatus `json:"status,omitempty"`
}

func (b *daemonMCPOperations) executeRelationshipRead(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "list_relationship_types", "get_relationship_type", "get_person_relationship_record", "list_person_relationships", "list_person_relationship_reviews", "get_person_network":
	default:
		return nil, false, nil
	}
	input, err := decodeMCPOperationArguments[relationshipReadArguments](args)
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil
	}
	var result *mcpserver.OperationResult
	switch name {
	case "list_relationship_types":
		result, err = readMCPOperationJSONWithClassifier[generated.RelationshipTypesResponse](ctx, b.client, http.MethodGet, "/api/v1/relationship-types", nil, relationshipRefusal, http.StatusOK)
	case "get_relationship_type":
		if !validLoopID(input.ID) {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSONWithClassifier[generated.RelationshipType](ctx, b.client, http.MethodGet, "/api/v1/relationship-types/{id}", &generated.GetRelationshipTypeRequestOptions{PathParams: &generated.GetRelationshipTypePath{ID: input.ID}}, relationshipRefusal, http.StatusOK)
		if result != nil && !result.IsError {
			if value, ok := result.Output.(generated.RelationshipType); ok && result.ETag != "" {
				result.Output = mcpserver.RelationshipTypeRecord{ETag: result.ETag, RelationshipType: value}
			} else {
				result = operationFailure("invalid_operation_response", false)
			}
		}
	case "get_person_relationship_record":
		if !validLoopID(input.ID) {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSONWithClassifier[generated.PersonRelationship](ctx, b.client, http.MethodGet, "/api/v1/person-relationships/{id}", &generated.GetPersonRelationshipRequestOptions{PathParams: &generated.GetPersonRelationshipPath{ID: input.ID}}, relationshipRefusal, http.StatusOK)
		if result != nil && !result.IsError {
			if value, ok := result.Output.(generated.PersonRelationship); ok && result.ETag != "" {
				value.SourceRef = nil
				value.SourceResourceUID = nil
				result.Output = mcpserver.PersonRelationshipRecord{ETag: result.ETag, Relationship: value}
			} else {
				result = operationFailure("invalid_operation_response", false)
			}
		}
	case "list_person_relationships":
		if !validLoopID(input.PersonID) {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSONWithClassifier[generated.PersonRelationshipsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/relationships", &generated.ListPersonRelationshipsRequestOptions{PathParams: &generated.ListPersonRelationshipsPath{ID: input.PersonID}, Query: &generated.ListPersonRelationshipsQuery{IncludeEnded: input.IncludeEnded}}, relationshipRefusal, http.StatusOK)
	case "list_person_relationship_reviews":
		var personID *int64
		if input.PersonID != 0 {
			if !validLoopID(input.PersonID) {
				return operationFailure("invalid_arguments", false), true, nil
			}
			personID = &input.PersonID
		}
		result, err = readMCPOperationJSONWithClassifier[generated.RelationshipReviewsResponse](ctx, b.client, http.MethodGet, "/api/v1/person-relationship-reviews", &generated.ListPersonRelationshipReviewsRequestOptions{Query: &generated.ListPersonRelationshipReviewsQuery{PersonID: personID, Status: input.Status}}, relationshipRefusal, http.StatusOK)
	case "get_person_network":
		if !validLoopID(input.PersonID) || input.Depth != nil && (*input.Depth < 1 || *input.Depth > 3) {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSONWithClassifier[generated.PersonNetwork](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/network", &generated.GetPersonNetworkRequestOptions{PathParams: &generated.GetPersonNetworkPath{ID: input.PersonID}, Query: &generated.GetPersonNetworkQuery{Depth: input.Depth, IncludeEnded: input.IncludeEnded}}, relationshipRefusal, http.StatusOK)
	default:
		return nil, false, nil
	}
	if result != nil && !result.IsError {
		switch value := result.Output.(type) {
		case generated.PersonRelationshipsResponse:
			for i := range value.Relationships {
				value.Relationships[i].Relationship.SourceRef = nil
				value.Relationships[i].Relationship.SourceResourceUID = nil
			}
			result.Output = value
		case generated.RelationshipReviewsResponse:
			for i := range value.Reviews {
				value.Reviews[i].SourceRef = nil
				value.Reviews[i].SourceResourceUID = nil
			}
			result.Output = value
		}
	}
	return result, true, err
}

// relationshipBodyOptions retains explicit null and empty PATCH members while
// reusing the generated route, path and header serialization.
type relationshipBodyOptions struct {
	runtime.RequestOptions
	body map[string]any
}

func (o relationshipBodyOptions) GetBody() any { return o.body }

type relationshipWriteArguments struct {
	ID                   int64   `json:"id,omitempty"`
	ETag                 string  `json:"etag,omitempty"`
	Slug                 string  `json:"slug,omitempty"`
	ForwardLabel         *string `json:"forward_label,omitempty"`
	ReverseLabel         *string `json:"reverse_label,omitempty"`
	IsSymmetric          *bool   `json:"is_symmetric,omitempty"`
	VCardRelatedType     *string `json:"vcard_related_type,omitempty"`
	Color                *string `json:"color,omitempty"`
	Icon                 *string `json:"icon,omitempty"`
	Description          *string `json:"description,omitempty"`
	SourcePersonID       int64   `json:"source_person_id,omitempty"`
	TargetPersonID       int64   `json:"target_person_id,omitempty"`
	RelationshipTypeSlug string  `json:"relationship_type_slug,omitempty"`
	StartDate            *string `json:"start_date,omitempty"`
	EndDate              *string `json:"end_date,omitempty"`
	Notes                *string `json:"notes,omitempty"`
}

func relationshipWriteInput(name string, args map[string]any) (relationshipWriteArguments, map[string]any, bool) {
	input, err := decodeMCPOperationArguments[relationshipWriteArguments](args)
	if err != nil {
		return input, nil, false
	}
	var allowed []string
	switch name {
	case "create_relationship_type":
		allowed = []string{"slug", "forward_label", "reverse_label", "is_symmetric", "vcard_related_type", "color", "icon", "description"}
	case "update_relationship_type":
		allowed = []string{"id", "etag", "forward_label", "reverse_label", "vcard_related_type", "color", "icon", "description"}
	case "delete_relationship_type", "delete_person_relationship":
		allowed = []string{"id", "etag"}
	case "create_person_relationship":
		allowed = []string{"source_person_id", "target_person_id", "relationship_type_slug", "start_date", "end_date", "notes"}
	case "update_person_relationship":
		allowed = []string{"id", "etag", "end_date", "notes"}
	default:
		return input, nil, false
	}
	body := make(map[string]any)
	for key, value := range args {
		if !slices.Contains(allowed, key) {
			return input, nil, false
		}
		if key == "id" || key == "etag" {
			continue
		}
		if value == nil && !(name == "update_person_relationship" && key == "notes") && !(name == "create_person_relationship" && key == "notes") && !(name == "create_relationship_type" && slices.Contains([]string{"vcard_related_type", "color", "icon", "description"}, key)) {
			return input, nil, false
		}
		body[key] = value
	}
	switch name {
	case "create_relationship_type":
		if input.Slug == "" || input.ForwardLabel == nil || input.ReverseLabel == nil {
			return input, nil, false
		}
	case "create_person_relationship":
		if !validLoopID(input.SourcePersonID) || !validLoopID(input.TargetPersonID) || input.RelationshipTypeSlug == "" {
			return input, nil, false
		}
	case "update_relationship_type", "update_person_relationship":
		if !validLoopID(input.ID) || input.ETag == "" || len(body) == 0 {
			return input, nil, false
		}
	case "delete_relationship_type", "delete_person_relationship":
		if !validLoopID(input.ID) || input.ETag == "" {
			return input, nil, false
		}
	}
	if name == "update_person_relationship" {
		if v, ok := body["end_date"]; ok {
			date, ok := v.(string)
			if !ok || strings.TrimSpace(date) == "" {
				return input, nil, false
			}
		}
	}
	return input, body, true
}

func relationshipRefusal(code string) bool {
	switch code {
	case "invalid_relationship", "invalid_relationship_type", "invalid_partial_date", "invalid_status", "invalid_if_match", "if_match_required", "relationship_revision_conflict", "relationship_type_not_found", "person_relationship_not_found", "person_not_found", "relationship_type_slug_conflict", "relationship_type_related_type_conflict", "relationship_type_not_deletable", "relationship_type_in_use", "person_relationship_duplicate", "person_relationships_unavailable":
		return true
	}
	return false
}

func (b *daemonMCPOperations) executeRelationshipWrite(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "create_relationship_type", "update_relationship_type", "delete_relationship_type", "create_person_relationship", "update_person_relationship", "delete_person_relationship":
	default:
		return nil, false, nil
	}
	input, body, ok := relationshipWriteInput(name, args)
	if !ok {
		return operationFailure("invalid_arguments", false), true, nil
	}
	var method, path string
	var options runtime.RequestOptions
	switch name {
	case "create_relationship_type":
		method, path = http.MethodPost, "/api/v1/relationship-types"
		options = relationshipBodyOptions{&generated.CreateRelationshipTypeRequestOptions{}, body}
	case "update_relationship_type":
		method, path = http.MethodPatch, "/api/v1/relationship-types/{id}"
		options = relationshipBodyOptions{&generated.PatchRelationshipTypeRequestOptions{PathParams: &generated.PatchRelationshipTypePath{ID: input.ID}, Header: &generated.PatchRelationshipTypeHeaders{IfMatch: input.ETag}}, body}
	case "delete_relationship_type":
		method, path = http.MethodDelete, "/api/v1/relationship-types/{id}"
		options = &generated.DeleteRelationshipTypeRequestOptions{PathParams: &generated.DeleteRelationshipTypePath{ID: input.ID}, Header: &generated.DeleteRelationshipTypeHeaders{IfMatch: input.ETag}}
	case "create_person_relationship":
		method, path = http.MethodPost, "/api/v1/person-relationships"
		options = relationshipBodyOptions{&generated.CreatePersonRelationshipRequestOptions{}, body}
	case "update_person_relationship":
		method, path = http.MethodPatch, "/api/v1/person-relationships/{id}"
		options = relationshipBodyOptions{&generated.PatchPersonRelationshipRequestOptions{PathParams: &generated.PatchPersonRelationshipPath{ID: input.ID}, Header: &generated.PatchPersonRelationshipHeaders{IfMatch: input.ETag}}, body}
	case "delete_person_relationship":
		method, path = http.MethodDelete, "/api/v1/person-relationships/{id}"
		options = &generated.DeletePersonRelationshipRequestOptions{PathParams: &generated.DeletePersonRelationshipPath{ID: input.ID}, Header: &generated.DeletePersonRelationshipHeaders{IfMatch: input.ETag}}
	}
	if method == http.MethodDelete {
		_, _, failure, err := readMCPOperationBytes(ctx, b.client, method, path, options, 1<<20, relationshipRefusal, http.StatusNoContent)
		if failure != nil || err != nil {
			return failure, true, err
		}
		return &mcpserver.OperationResult{Output: mcpserver.RelationshipDeletion{Deleted: true}}, true, nil
	}
	if strings.Contains(name, "relationship_type") {
		result, err := readMCPOperationJSONWithClassifier[generated.RelationshipType](ctx, b.client, method, path, options, relationshipRefusal, http.StatusOK, http.StatusCreated)
		if result != nil && !result.IsError {
			if value, ok := result.Output.(generated.RelationshipType); ok && result.ETag != "" {
				result.Output = mcpserver.RelationshipTypeRecord{ETag: result.ETag, RelationshipType: value}
			} else {
				result = operationFailure("invalid_operation_response", true)
			}
		}
		return result, true, err
	}
	result, err := readMCPOperationJSONWithClassifier[generated.PersonRelationship](ctx, b.client, method, path, options, relationshipRefusal, http.StatusOK, http.StatusCreated)
	if result != nil && !result.IsError {
		if value, ok := result.Output.(generated.PersonRelationship); ok && result.ETag != "" {
			value.SourceRef = nil
			value.SourceResourceUID = nil
			result.Output = mcpserver.PersonRelationshipRecord{ETag: result.ETag, Relationship: value}
		} else {
			result = operationFailure("invalid_operation_response", true)
		}
	}
	return result, true, err
}

func (b *daemonMCPOperations) relationshipDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	switch name {
	case "create_relationship_type", "update_relationship_type", "delete_relationship_type", "create_person_relationship", "update_person_relationship", "delete_person_relationship":
	default:
		return "", false, nil
	}
	input, _, ok := relationshipWriteInput(name, args)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	var current any
	if input.ID > 0 {
		readName := "get_person_relationship_record"
		if strings.Contains(name, "relationship_type") {
			readName = "get_relationship_type"
		}
		result, _, err := b.executeRelationshipRead(ctx, readName, map[string]any{"id": input.ID})
		if err != nil {
			return "", true, err
		}
		if result == nil || result.IsError {
			if result != nil {
				if failure, ok := result.Output.(operationFailureOutput); ok {
					return "", true, &mcpserver.OperationRefusalError{Code: failure.Error}
				}
			}
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		if result.ETag != input.ETag {
			return "", true, &mcpserver.OperationRefusalError{Code: "relationship_revision_conflict"}
		}
		current = result.Output
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current"`
		Changes   map[string]any `json:"changes"`
	}{name, current, args}, json.Deterministic(true))
	return string(data), true, err
}
