package cmd

import (
	"context"
	"net/http"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func organizationMCPCapabilities(hasRoute mcpRouteCheck) []string {
	var names []string
	for _, r := range []struct {
		name, id, path string
		query          []string
	}{
		{"get_organization", "getOrganization", "/api/v1/organizations/{id}", nil},
		{"get_organization_history", "getOrganizationHistory", "/api/v1/organizations/{id}/history", nil},
		{"list_organization_attributes", "listOrganizationAttributes", "/api/v1/organizations/{id}/attributes", []string{"definition_slug", "include_superseded"}},
	} {
		if hasRoute(r.id, http.MethodGet, r.path, r.query...) {
			names = append(names, r.name)
		}
	}
	return names
}

type organizationReadArguments struct {
	OrganizationID    int64   `json:"organization_id"`
	DefinitionSlug    *string `json:"definition_slug,omitempty"`
	IncludeSuperseded *bool   `json:"include_superseded,omitempty"`
}

func (b *daemonMCPOperations) executeOrganizationOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "get_organization", "get_organization_history", "list_organization_attributes":
	default:
		return nil, false, nil
	}
	input, err := decodeMCPOperationArguments[organizationReadArguments](args)
	if err != nil || input.OrganizationID <= 0 || (name != "list_organization_attributes" && (input.DefinitionSlug != nil || input.IncludeSuperseded != nil)) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	var result *mcpserver.OperationResult
	switch name {
	case "get_organization":
		result, err = readMCPOperationJSON[generated.OrganizationProfile](ctx, b.client, http.MethodGet, "/api/v1/organizations/{id}", &generated.GetOrganizationRequestOptions{PathParams: &generated.GetOrganizationPath{ID: input.OrganizationID}}, http.StatusOK)
	case "get_organization_history":
		result, err = readMCPOperationJSON[generated.OrganizationProfile](ctx, b.client, http.MethodGet, "/api/v1/organizations/{id}/history", &generated.GetOrganizationHistoryRequestOptions{PathParams: &generated.GetOrganizationHistoryPath{ID: input.OrganizationID}}, http.StatusOK)
	case "list_organization_attributes":
		result, err = readMCPOperationJSON[generated.OrganizationAttributesResponse](ctx, b.client, http.MethodGet, "/api/v1/organizations/{id}/attributes", &generated.ListOrganizationAttributesRequestOptions{PathParams: &generated.ListOrganizationAttributesPath{ID: input.OrganizationID}, Query: &generated.ListOrganizationAttributesQuery{DefinitionSlug: input.DefinitionSlug, IncludeSuperseded: input.IncludeSuperseded}}, http.StatusOK)
	}
	if result != nil && !result.IsError && name != "list_organization_attributes" {
		profile, ok := result.Output.(generated.OrganizationProfile)
		if !ok || result.ETag == "" {
			return operationFailure("invalid_operation_response", false), true, nil
		}
		result.Output = mcpserver.OrganizationRecord{ETag: result.ETag, Profile: profile}
	}
	return result, true, err
}
