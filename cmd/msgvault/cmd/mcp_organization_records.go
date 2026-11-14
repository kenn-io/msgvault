package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const organizationSourceField = "source"
const organizationAttributePreviewBytes = 2<<20 + 64<<10

func organizationMCPCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, r := range []struct {
		name, id, path string
		query          []string
	}{
		{"list_organizations", "listOrganizations", "/api/v1/organizations", []string{"q", "include_retired", "limit", "offset"}},
		{"get_organization", "getOrganization", "/api/v1/organizations/{id}", nil},
		{"list_organization_attributes", "listOrganizationAttributes", "/api/v1/organizations/{id}/attributes", []string{"definition_slug", "include_superseded"}},
	} {
		if hasRoute(r.id, http.MethodGet, r.path, r.query...) && (r.name != "get_organization" || hasRoute("getOrganizationHistory", http.MethodGet, "/api/v1/organizations/{id}/history")) {
			names = append(names, r.name)
		}
	}

	for _, r := range []struct {
		name, id, method, path string
		query, properties      []string
	}{
		{"create_organization", "createOrganization", http.MethodPost, "/api/v1/organizations", nil, []string{"name", "kind", "primary_domain", "description"}},
		{"update_organization", "patchOrganization", http.MethodPatch, "/api/v1/organizations/{id}", nil, []string{"name", "kind", "primary_domain", "description", "retired"}},
		{"delete_organization", "deleteOrganization", http.MethodDelete, "/api/v1/organizations/{id}", nil, nil},
		{"replace_organization_profile", "putOrganizationProfile", http.MethodPut, "/api/v1/organizations/{id}/profile", nil, []string{"names", "identifiers", "addresses", "contact_points", "media", "categories"}},
		{"set_organization_attribute", "setOrganizationAttribute", http.MethodPost, "/api/v1/organizations/{id}/attributes", nil, []string{"definition_slug", "value", organizationSourceField, "ordinal", "expected_value_id", "dry_run"}},
	} {
		if (r.name != "set_organization_attribute" || hasRoute("listOrganizationAttributes", http.MethodGet, "/api/v1/organizations/{id}/attributes", "definition_slug", "include_superseded")) && hasRoute(r.id, r.method, r.path, r.query...) && (len(r.properties) == 0 || mcpRequestPropertiesPresent(caps, r.id, r.properties...)) && (r.name == "create_organization" || (hasRoute("getOrganization", http.MethodGet, "/api/v1/organizations/{id}") && hasRoute("getOrganizationHistory", http.MethodGet, "/api/v1/organizations/{id}/history"))) {
			names = append(names, r.name)
		}
	}
	return names
}

type organizationReadArguments struct {
	Q                 *string `json:"q,omitempty"`
	IncludeRetired    *bool   `json:"include_retired,omitempty"`
	Limit             *int64  `json:"limit,omitempty"`
	Offset            *int64  `json:"offset,omitempty"`
	OrganizationID    int64   `json:"organization_id"`
	DefinitionSlug    *string `json:"definition_slug,omitempty"`
	IncludeSuperseded *bool   `json:"include_superseded,omitempty"`
}

func (b *daemonMCPOperations) executeOrganizationOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "get_organization", "list_organizations", "list_organization_attributes":
	default:
		return b.executeOrganizationWriteOrMedia(ctx, name, args)
	}
	input, err := decodeMCPOperationArguments[organizationReadArguments](args)
	if err != nil || (name != "list_organizations" && !validLoopID(input.OrganizationID)) {
		return operationFailure(invalidArguments, false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
	}
	var result *mcpserver.OperationResult
	switch name {
	case "list_organizations":
		result, err = readMCPOperationJSON[generated.OrganizationsResponse](ctx, b.client, http.MethodGet, "/api/v1/organizations", &generated.ListOrganizationsRequestOptions{Query: &generated.ListOrganizationsQuery{Q: input.Q, IncludeRetired: input.IncludeRetired, Limit: input.Limit, Offset: input.Offset}}, http.StatusOK)
	case "get_organization":
		path := "/api/v1/organizations/{id}"
		var opts runtime.RequestOptions = &generated.GetOrganizationRequestOptions{PathParams: &generated.GetOrganizationPath{ID: input.OrganizationID}}
		if input.IncludeSuperseded != nil && *input.IncludeSuperseded {
			path = "/api/v1/organizations/{id}/history"
			opts = &generated.GetOrganizationHistoryRequestOptions{PathParams: &generated.GetOrganizationHistoryPath{ID: input.OrganizationID}}
		}
		data, headers, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodGet, path, opts, mcpserver.MaxDownloadBytes, http.StatusOK)
		if failure != nil {
			return failure, true, readErr
		}
		if headers.Get("ETag") == "" {
			return operationFailure("invalid_operation_response", false), true, nil
		}
		return &mcpserver.OperationResult{Data: data, ETag: headers.Get("ETag")}, true, nil
	case "list_organization_attributes":
		data, _, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/organizations/{id}/attributes", &generated.ListOrganizationAttributesRequestOptions{PathParams: &generated.ListOrganizationAttributesPath{ID: input.OrganizationID}, Query: &generated.ListOrganizationAttributesQuery{DefinitionSlug: input.DefinitionSlug, IncludeSuperseded: input.IncludeSuperseded}}, mcpserver.MaxDownloadBytes, http.StatusOK)
		if failure != nil {
			return failure, true, readErr
		}
		return &mcpserver.OperationResult{Data: data}, true, nil
	}

	return result, true, err
}

type organizationRequest struct {
	method, path string
	options      runtime.RequestOptions
	id           int64
}

func organizationWriteRequest(name string, args map[string]any) (organizationRequest, bool, bool) {
	var r organizationRequest
	var valid bool
	switch name {
	case "create_organization":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationCreateArgs](args)
		valid = err == nil && args["body"] != nil
		r = organizationRequest{method: http.MethodPost, path: "/api/v1/organizations", options: &generated.CreateOrganizationRequestOptions{Body: &a.Body}}
	case "update_organization":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationUpdateArgs](args)
		valid = err == nil && args["body"] != nil
		r = organizationRequest{http.MethodPatch, "/api/v1/organizations/{id}", &generated.PatchOrganizationRequestOptions{PathParams: &generated.PatchOrganizationPath{ID: a.OrganizationID}, Header: &generated.PatchOrganizationHeaders{IfMatch: a.ETag}, Body: &a.Body}, a.OrganizationID}
	case "delete_organization":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationDeleteArgs](args)
		valid = err == nil
		r = organizationRequest{http.MethodDelete, "/api/v1/organizations/{id}", &generated.DeleteOrganizationRequestOptions{PathParams: &generated.DeleteOrganizationPath{ID: a.OrganizationID}, Header: &generated.DeleteOrganizationHeaders{IfMatch: a.ETag}}, a.OrganizationID}
	case "replace_organization_profile":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationProfileArgs](args)
		valid = err == nil && args["body"] != nil
		r = organizationRequest{http.MethodPut, "/api/v1/organizations/{id}/profile", &generated.PutOrganizationProfileRequestOptions{PathParams: &generated.PutOrganizationProfilePath{ID: a.OrganizationID}, Header: &generated.PutOrganizationProfileHeaders{IfMatch: a.ETag}, Body: &a.Body}, a.OrganizationID}
	case "set_organization_attribute":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationSetAttributeArgs](args)
		valid = err == nil && args["body"] != nil && a.Body.Ordinal != nil && a.Body.ExpectedValueID != nil
		r = organizationRequest{http.MethodPost, "/api/v1/organizations/{id}/attributes", &generated.SetOrganizationAttributeRequestOptions{PathParams: &generated.SetOrganizationAttributePath{ID: a.OrganizationID}, Body: &a.Body}, a.OrganizationID}
	default:
		return r, false, false
	}
	if name != "create_organization" && !validLoopID(r.id) {
		valid = false
	}
	return r, true, valid
}

func (b *daemonMCPOperations) executeOrganizationWriteOrMedia(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	r, handled, valid := organizationWriteRequest(name, args)
	if !handled {
		return nil, false, nil
	}
	if !valid {
		return operationFailure(invalidArguments, false), true, nil
	}
	if name == "delete_organization" {
		_, _, failure, err := readMCPOperationBytes(ctx, b.client, r.method, r.path, r.options, 1<<20, http.StatusNoContent)
		if failure != nil {
			return failure, true, err
		}
		return &mcpserver.OperationResult{Output: mcpserver.OrganizationDeleted{Deleted: true}}, true, nil
	}
	if name == "set_organization_attribute" {
		write, failure, err := b.readOrganizationAttributeWrite(ctx, r)
		if failure != nil || err != nil {
			return failure, true, err
		}
		if write.Value == nil {
			return operationFailure("invalid_operation_response", true), true, nil
		}
		receipt := mcpserver.OrganizationAttributeReceipt{DryRun: write.DryRun, Ordinal: write.Value.Ordinal}
		if !write.DryRun {
			receipt.ValueID = write.Value.ID
			if write.Superseded != nil {
				receipt.SupersededID = &write.Superseded.ID
			}
		}
		return &mcpserver.OperationResult{Output: receipt}, true, nil
	}
	if name == "replace_organization_profile" {
		data, headers, failure, err := readMCPOperationBytes(ctx, b.client, r.method, r.path, r.options, mcpserver.MaxDownloadBytes, http.StatusOK)
		if failure != nil {
			return failure, true, err
		}
		etag := headers.Get("ETag")
		if etag == "" {
			return operationFailure("invalid_operation_response", true), true, nil
		}
		record := mcpserver.OrganizationRecord{ETag: etag}
		if len(data) <= 1<<20 {
			var profile generated.OrganizationProfile
			if err := json.Unmarshal(data, &profile); err != nil {
				return operationFailure("invalid_operation_response", true), true, err
			}
			record.Profile = &profile
		}
		result := &mcpserver.OperationResult{Output: record, ETag: etag}
		return result, true, err
	}
	result, err := readMCPOperationJSON[generated.Organization](ctx, b.client, r.method, r.path, r.options, http.StatusOK, http.StatusCreated)
	result = etagProjection(result, true, func(etag string, org generated.Organization) mcpserver.OrganizationMutation {
		return mcpserver.OrganizationMutation{ETag: etag, Organization: org}
	})
	return result, true, err
}

func (b *daemonMCPOperations) organizationOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	r, handled, valid := organizationWriteRequest(name, args)
	if !handled {
		return "", false, nil
	}
	if !valid {
		return "", true, &mcpserver.OperationRefusalError{Code: invalidArguments}
	}
	var current any
	var preview any
	if r.id != 0 {
		result, _, err := b.executeOrganizationOperation(ctx, "get_organization", map[string]any{"organization_id": r.id})
		if err != nil {
			result = nil
		}
		if err != nil || result == nil || result.IsError {
			return "", true, operationReadFailure(result, nativeDefiniteRefusal)
		}
		var profile generated.OrganizationProfile
		if err := json.Unmarshal(result.Data, &profile); err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		name := profile.Organization.Name
		if len(name) > 256 {
			name = strings.ToValidUTF8(name[:256], "") + "…"
		}
		current = struct {
			ETag string `json:"etag"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		}{result.ETag, name, profile.Organization.Kind}
	}
	if name == "set_organization_attribute" {
		switch options := r.options.(type) {
		case *generated.SetOrganizationAttributeRequestOptions:
			body := *options.Body
			body.DryRun = new(true)
			options.Body = &body

		}
		write, result, err := b.readOrganizationAttributeWrite(ctx, r)
		if err != nil {
			result = nil
		}
		if err != nil || result != nil {
			return "", true, operationReadFailure(result, nativeDefiniteRefusal)
		}
		var ordinal int64
		var superseded any
		if v := write.Superseded; v != nil {
			ordinal = v.Ordinal
			superseded = map[string]any{"id": v.ID, "ordinal": v.Ordinal, "value": v.Value, organizationSourceField: v.Source, "source_ref": v.SourceRef, "actor": v.Actor, "confidence": v.Confidence, "active_from": v.ActiveFrom, "created_at": v.CreatedAt}
		}
		if write.Value != nil {
			ordinal = write.Value.Ordinal
		}
		preview = map[string]any{"ordinal": ordinal, "superseded": superseded}
	}
	// Copy arguments for display; confirmation and execution retain the originals.
	raw, err := json.Marshal(args, json.Deterministic(true))
	if err != nil {
		return "", true, err
	}
	var changes map[string]any
	if err := json.Unmarshal(raw, &changes); err != nil {
		return "", true, err
	}
	if body, ok := changes["body"].(map[string]any); ok {
		if media, ok := body["media"].([]any); ok {
			for _, item := range media {
				if row, ok := item.(map[string]any); ok {
					if data, ok := row["data"].(string); ok {
						size, digest, err := summarizeInlineMedia(data)
						if err != nil {
							return "", true, &mcpserver.OperationRefusalError{Code: invalidArguments}
						}
						row["data"] = map[string]any{"bytes": size, "sha256": digest}
					}
				}
			}
		}
	}
	effect := map[string]string{"create_organization": "create an organization", "update_organization": "replace mutable fields; omitted fields are cleared except retired", "delete_organization": "delete the organization only if it has no employment records", "replace_organization_profile": "replace all profile collections; omitted collections are cleared", "set_organization_attribute": "set the named typed attribute"}[name]
	disclosure, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current,omitempty"`
		Preview   any            `json:"preview,omitempty"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current, preview, changes, effect}, json.Deterministic(true))
	return string(disclosure), true, err
}

func (b *daemonMCPOperations) readOrganizationAttributeWrite(ctx context.Context, r organizationRequest) (generated.OrganizationAttributeWrite, *mcpserver.OperationResult, error) {
	data, _, failure, err := readMCPOperationBytes(ctx, b.client, r.method, r.path, r.options, organizationAttributePreviewBytes, http.StatusOK, http.StatusCreated)
	var write generated.OrganizationAttributeWrite
	if failure != nil || err != nil {
		return write, failure, err
	}
	if err := json.Unmarshal(data, &write); err != nil {
		return write, operationFailure("invalid_operation_response", true), err
	}
	return write, nil, nil
}
