package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func relationshipRecordCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, route := range []struct {
		name, id, path string
		query          []string
	}{
		{"list_relationship_types", "listRelationshipTypes", "/api/v1/relationship-types", nil},
		{"list_person_relationships", "listPersonRelationships", "/api/v1/people/{id}/relationships", []string{"include_ended"}},
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
		{"create_person_relationship", "createPersonRelationship", http.MethodPost, "/api/v1/person-relationships", []string{"source_person_id", "target_person_id", "relationship_type_slug", "start_date", "end_date", "notes"}},
		{"update_person_relationship", "patchPersonRelationship", http.MethodPatch, "/api/v1/person-relationships/{id}", []string{"end_date", "notes"}},
		{"delete_person_relationship", "deletePersonRelationship", http.MethodDelete, "/api/v1/person-relationships/{id}", nil},
	} {
		if hasRoute(route.id, route.method, route.path) && mcpRequestPropertiesPresent(caps, route.id, route.properties...) && hasRoute("getPersonProfile", http.MethodGet, "/api/v1/people/{id}") && (route.name == "create_person_relationship" && hasRoute("listRelationshipTypes", http.MethodGet, "/api/v1/relationship-types") || route.name != "create_person_relationship" && hasRoute("getPersonRelationship", http.MethodGet, "/api/v1/person-relationships/{id}")) {
			names = append(names, route.name)
		}
	}
	return names
}

type relationshipArguments struct {
	ID                   int64   `json:"id,omitempty"`
	Revision             int64   `json:"revision,omitempty"`
	PersonID             int64   `json:"person_id,omitempty"`
	IncludeEnded         *bool   `json:"include_ended,omitempty"`
	Depth                *int64  `json:"depth,omitempty"`
	SourcePersonID       int64   `json:"source_person_id,omitempty"`
	TargetPersonID       int64   `json:"target_person_id,omitempty"`
	RelationshipTypeSlug string  `json:"relationship_type_slug,omitempty"`
	StartDate            *string `json:"start_date,omitempty"`
	EndDate              *string `json:"end_date,omitempty"`
	Notes                *string `json:"notes,omitempty"`
}

func relationshipInput(name string, args map[string]any) (relationshipArguments, bool) {
	input, err := decodeMCPOperationArguments[relationshipArguments](args)
	if err != nil {
		return input, false
	}
	if name == "update_person_relationship" && input.EndDate == nil && input.Notes == nil {
		return input, false
	}
	return input, true
}

func (b *daemonMCPOperations) executeRelationshipRecord(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if !slices.Contains([]string{"list_relationship_types", "list_person_relationships", "get_person_network", "create_person_relationship", "update_person_relationship", "delete_person_relationship"}, name) {
		return nil, false, nil
	}
	input, ok := relationshipInput(name, args)
	if !ok {
		return operationFailure(invalidArguments, false), true, nil
	}
	switch name {
	case "list_relationship_types":
		result, err := readMCPOperationJSON[generated.RelationshipTypesResponse](ctx, b.client, http.MethodGet, "/api/v1/relationship-types", nil, http.StatusOK)
		return result, true, err
	case "list_person_relationships":
		data, _, failure, err := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/people/{id}/relationships", &generated.ListPersonRelationshipsRequestOptions{PathParams: &generated.ListPersonRelationshipsPath{ID: input.PersonID}, Query: &generated.ListPersonRelationshipsQuery{IncludeEnded: input.IncludeEnded}}, mcpserver.MaxDownloadBytes, nil, http.StatusOK)
		if failure != nil || err != nil {
			return failure, true, err
		}
		return &mcpserver.OperationResult{Data: data}, true, nil
	case "get_person_network":
		result, err := readMCPOperationJSON[generated.PersonNetwork](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/network", &generated.GetPersonNetworkRequestOptions{PathParams: &generated.GetPersonNetworkPath{ID: input.PersonID}, Query: &generated.GetPersonNetworkQuery{Depth: input.Depth, IncludeEnded: input.IncludeEnded}}, http.StatusOK)
		return result, true, err
	}
	var method, path string
	var options runtime.RequestOptions
	switch name {
	case "create_person_relationship":
		method, path = http.MethodPost, "/api/v1/person-relationships"
		options = &generated.CreatePersonRelationshipRequestOptions{Body: &generated.CreatePersonRelationshipBody{SourcePersonID: input.SourcePersonID, TargetPersonID: input.TargetPersonID, RelationshipTypeSlug: input.RelationshipTypeSlug, StartDate: input.StartDate, EndDate: input.EndDate, Notes: input.Notes}}
	case "update_person_relationship":
		method, path = http.MethodPatch, "/api/v1/person-relationships/{id}"
		options = &generated.PatchPersonRelationshipRequestOptions{PathParams: &generated.PatchPersonRelationshipPath{ID: input.ID}, Header: &generated.PatchPersonRelationshipHeaders{IfMatch: personRelationshipETag(input.ID, input.Revision)}, Body: &generated.PatchPersonRelationshipBody{EndDate: input.EndDate, Notes: input.Notes}}
	case "delete_person_relationship":
		method, path = http.MethodDelete, "/api/v1/person-relationships/{id}"
		options = &generated.DeletePersonRelationshipRequestOptions{PathParams: &generated.DeletePersonRelationshipPath{ID: input.ID}, Header: &generated.DeletePersonRelationshipHeaders{IfMatch: personRelationshipETag(input.ID, input.Revision)}}
	}
	if method == http.MethodDelete {
		_, _, failure, err := readMCPOperationBytes(ctx, b.client, method, path, options, 1<<20, nil, http.StatusNoContent)
		if failure != nil || err != nil {
			return failure, true, err
		}
		return &mcpserver.OperationResult{Output: mcpserver.RelationshipDeletion{Deleted: true}}, true, nil
	}
	result, err := readMCPOperationJSON[generated.PersonRelationship](ctx, b.client, method, path, options, http.StatusOK, http.StatusCreated)
	return result, true, err
}

func (b *daemonMCPOperations) relationshipDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if !slices.Contains([]string{"create_person_relationship", "update_person_relationship", "delete_person_relationship"}, name) {
		return "", false, nil
	}
	input, ok := relationshipInput(name, args)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: invalidArguments}
	}
	var current any
	sourceID, targetID := input.SourcePersonID, input.TargetPersonID
	label := ""
	if name != "create_person_relationship" {
		result, err := readMCPOperationJSON[generated.PersonRelationship](ctx, b.client, http.MethodGet, "/api/v1/person-relationships/{id}", &generated.GetPersonRelationshipRequestOptions{PathParams: &generated.GetPersonRelationshipPath{ID: input.ID}}, http.StatusOK)
		if err != nil {
			return "", true, err
		}
		if result == nil || result.IsError {
			return "", true, disclosureRefusal(result)
		}
		record, ok := result.Output.(generated.PersonRelationship)
		if !ok {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		if record.Revision != input.Revision {
			return "", true, &mcpserver.OperationRefusalError{Code: "relationship_revision_conflict"}
		}
		current = record
		sourceID, targetID, label = record.SourcePersonID, record.TargetPersonID, record.ForwardLabel
	} else {
		result, err := readMCPOperationJSON[generated.RelationshipTypesResponse](ctx, b.client, http.MethodGet, "/api/v1/relationship-types", nil, http.StatusOK)
		if err != nil {
			return "", true, err
		}
		if result == nil || result.IsError {
			return "", true, disclosureRefusal(result)
		}
		types, ok := result.Output.(generated.RelationshipTypesResponse)
		if !ok {
			return "", true, disclosureRefusal(nil)
		}
		for _, typ := range types.RelationshipTypes {
			if typ.Slug == input.RelationshipTypeSlug {
				label = typ.ForwardLabel
				break
			}
		}
		if label == "" {
			return "", true, &mcpserver.OperationRefusalError{Code: "relationship_type_not_found"}
		}
	}
	var people [2]generated.Person
	for i, id := range []int64{sourceID, targetID} {
		result, err := readMCPOperationJSON[generated.Person](ctx, b.client, http.MethodGet, "/api/v1/people/{id}", &generated.GetPersonProfileRequestOptions{PathParams: &generated.GetPersonProfilePath{ID: id}}, http.StatusOK)
		if err != nil {
			return "", true, err
		}
		if result == nil || result.IsError {
			return "", true, disclosureRefusal(result)
		}
		person, ok := result.Output.(generated.Person)
		if !ok {
			return "", true, disclosureRefusal(nil)
		}
		people[i] = person
	}
	type disclosedPerson struct {
		ID          int64   `json:"id"`
		DisplayName *string `json:"display_name"`
	}
	data, err := json.Marshal(struct {
		Operation    string          `json:"operation"`
		Current      any             `json:"current"`
		Changes      map[string]any  `json:"changes"`
		Source       disclosedPerson `json:"source"`
		Target       disclosedPerson `json:"target"`
		Relationship string          `json:"relationship"`
	}{name, current, args, disclosedPerson{ID: people[0].ID, DisplayName: people[0].DisplayName}, disclosedPerson{ID: people[1].ID, DisplayName: people[1].DisplayName}, label}, json.Deterministic(true))
	return string(data), true, err
}
