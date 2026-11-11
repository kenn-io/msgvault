package cmd

import (
	"context"
	"net/http"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func relationshipRecordCapabilities(hasRoute mcpRouteCheck) []string {
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
		result, err = readMCPOperationJSON[generated.RelationshipTypesResponse](ctx, b.client, http.MethodGet, "/api/v1/relationship-types", nil, http.StatusOK)
	case "get_relationship_type":
		if input.ID <= 0 {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSON[generated.RelationshipType](ctx, b.client, http.MethodGet, "/api/v1/relationship-types/{id}", &generated.GetRelationshipTypeRequestOptions{PathParams: &generated.GetRelationshipTypePath{ID: input.ID}}, http.StatusOK)
		if result != nil && !result.IsError {
			if value, ok := result.Output.(generated.RelationshipType); ok && result.ETag != "" {
				result.Output = mcpserver.RelationshipTypeRecord{ETag: result.ETag, RelationshipType: value}
			} else {
				result = operationFailure("invalid_operation_response", false)
			}
		}
	case "get_person_relationship_record":
		if input.ID <= 0 {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSON[generated.PersonRelationship](ctx, b.client, http.MethodGet, "/api/v1/person-relationships/{id}", &generated.GetPersonRelationshipRequestOptions{PathParams: &generated.GetPersonRelationshipPath{ID: input.ID}}, http.StatusOK)
		if result != nil && !result.IsError {
			if value, ok := result.Output.(generated.PersonRelationship); ok && result.ETag != "" {
				result.Output = mcpserver.PersonRelationshipRecord{ETag: result.ETag, Relationship: value}
			} else {
				result = operationFailure("invalid_operation_response", false)
			}
		}
	case "list_person_relationships":
		if input.PersonID <= 0 {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSON[generated.PersonRelationshipsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/relationships", &generated.ListPersonRelationshipsRequestOptions{PathParams: &generated.ListPersonRelationshipsPath{ID: input.PersonID}, Query: &generated.ListPersonRelationshipsQuery{IncludeEnded: input.IncludeEnded}}, http.StatusOK)
	case "list_person_relationship_reviews":
		var personID *int64
		if input.PersonID != 0 {
			if input.PersonID < 0 {
				return operationFailure("invalid_arguments", false), true, nil
			}
			personID = &input.PersonID
		}
		result, err = readMCPOperationJSON[generated.RelationshipReviewsResponse](ctx, b.client, http.MethodGet, "/api/v1/person-relationship-reviews", &generated.ListPersonRelationshipReviewsRequestOptions{Query: &generated.ListPersonRelationshipReviewsQuery{PersonID: personID, Status: input.Status}}, http.StatusOK)
	case "get_person_network":
		if input.PersonID <= 0 || input.Depth != nil && (*input.Depth < 1 || *input.Depth > 3) {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSON[generated.PersonNetwork](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/network", &generated.GetPersonNetworkRequestOptions{PathParams: &generated.GetPersonNetworkPath{ID: input.PersonID}, Query: &generated.GetPersonNetworkQuery{Depth: input.Depth, IncludeEnded: input.IncludeEnded}}, http.StatusOK)
	default:
		return nil, false, nil
	}
	return result, true, err
}
