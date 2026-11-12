package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// Active profile responses may include normalized and synthesized copies of
// request text plus metadata for up to 200 rows. Three times the native request
// limit provides room for those copies; merged history can still exceed it.
const organizationProfileResponseLimit = 3 * api.MaxOrganizationProfileRequestBytes

func organizationMCPCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
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

	for _, r := range []struct {
		name, id, method, path string
		query, properties      []string
	}{
		{"get_organization_media", "getOrganizationProfileMediaContent", http.MethodGet, "/api/v1/organizations/{id}/profile/media/{media_id}/content", nil, nil},
		{"create_organization", "createOrganization", http.MethodPost, "/api/v1/organizations", nil, []string{"name", "kind", "primary_domain", "description"}},
		{"update_organization", "patchOrganization", http.MethodPatch, "/api/v1/organizations/{id}", nil, []string{"name", "kind", "primary_domain", "description", "retired"}},
		{"delete_organization", "deleteOrganization", http.MethodDelete, "/api/v1/organizations/{id}", nil, nil},
		{"merge_organization", "mergeOrganization", http.MethodPost, "/api/v1/organizations/{id}/merge", nil, []string{"losing_organization_id", "losing_revision"}},
		{"replace_organization_profile", "putOrganizationProfile", http.MethodPut, "/api/v1/organizations/{id}/profile", nil, []string{"names", "identifiers", "addresses", "contact_points", "media", "categories"}},
		{"set_organization_attribute", "setOrganizationAttribute", http.MethodPost, "/api/v1/organizations/{id}/attributes", nil, []string{"definition_slug", "value", "source", "ordinal", "expected_value_id", "dry_run"}},
		{"clear_organization_attribute", "clearOrganizationAttribute", http.MethodDelete, "/api/v1/organizations/{id}/attributes/{slug}", []string{"ordinal", "expected_value_id", "dry_run"}, nil},
	} {
		if hasRoute(r.id, r.method, r.path, r.query...) && (len(r.properties) == 0 || mcpRequestPropertiesPresent(caps, r.id, r.properties...)) && (r.name == "get_organization_media" || r.name == "create_organization" || hasRoute("getOrganization", http.MethodGet, "/api/v1/organizations/{id}")) {
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
		return b.executeOrganizationWriteOrMedia(ctx, name, args)
	}
	input, err := decodeMCPOperationArguments[organizationReadArguments](args)
	if err != nil || !validLoopID(input.OrganizationID) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	var result *mcpserver.OperationResult
	switch name {
	case "get_organization":
		result, err = readMCPOperationJSONLimit[generated.OrganizationProfile](ctx, b.client, http.MethodGet, "/api/v1/organizations/{id}", &generated.GetOrganizationRequestOptions{PathParams: &generated.GetOrganizationPath{ID: input.OrganizationID}}, organizationProfileResponseLimit, http.StatusOK)
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

type organizationRequest struct {
	method, path string
	options      runtime.RequestOptions
	id           int64
	etag         string
}

func organizationWriteRequest(name string, args map[string]any) (organizationRequest, bool, bool) {
	var r organizationRequest
	valid := false
	switch name {
	case "create_organization":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationCreateArgs](args)
		valid = err == nil && args["body"] != nil
		r = organizationRequest{method: http.MethodPost, path: "/api/v1/organizations", options: &generated.CreateOrganizationRequestOptions{Body: &a.Body}}
	case "update_organization":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationUpdateArgs](args)
		valid = err == nil && args["body"] != nil
		r = organizationRequest{http.MethodPatch, "/api/v1/organizations/{id}", &generated.PatchOrganizationRequestOptions{PathParams: &generated.PatchOrganizationPath{ID: a.OrganizationID}, Header: &generated.PatchOrganizationHeaders{IfMatch: a.ETag}, Body: &a.Body}, a.OrganizationID, a.ETag}
	case "delete_organization":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationDeleteArgs](args)
		valid = err == nil
		r = organizationRequest{http.MethodDelete, "/api/v1/organizations/{id}", &generated.DeleteOrganizationRequestOptions{PathParams: &generated.DeleteOrganizationPath{ID: a.OrganizationID}, Header: &generated.DeleteOrganizationHeaders{IfMatch: a.ETag}}, a.OrganizationID, a.ETag}
	case "merge_organization":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationMergeArgs](args)
		valid = err == nil && args["body"] != nil
		r = organizationRequest{http.MethodPost, "/api/v1/organizations/{id}/merge", &generated.MergeOrganizationRequestOptions{PathParams: &generated.MergeOrganizationPath{ID: a.OrganizationID}, Header: &generated.MergeOrganizationHeaders{IfMatch: a.ETag}, Body: &a.Body}, a.OrganizationID, a.ETag}
	case "replace_organization_profile":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationProfileArgs](args)
		valid = err == nil && args["body"] != nil
		r = organizationRequest{http.MethodPut, "/api/v1/organizations/{id}/profile", &generated.PutOrganizationProfileRequestOptions{PathParams: &generated.PutOrganizationProfilePath{ID: a.OrganizationID}, Header: &generated.PutOrganizationProfileHeaders{IfMatch: a.ETag}, Body: &a.Body}, a.OrganizationID, a.ETag}
	case "set_organization_attribute":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationSetAttributeArgs](args)
		valid = err == nil && args["body"] != nil
		r = organizationRequest{http.MethodPost, "/api/v1/organizations/{id}/attributes", &generated.SetOrganizationAttributeRequestOptions{PathParams: &generated.SetOrganizationAttributePath{ID: a.OrganizationID}, Body: &a.Body}, a.OrganizationID, ""}
	case "clear_organization_attribute":
		a, err := decodeMCPOperationArguments[mcpserver.OrganizationClearAttributeArgs](args)
		valid = err == nil && store.ValidateAttributeSlug(a.Slug) == nil
		r = organizationRequest{http.MethodDelete, "/api/v1/organizations/{id}/attributes/{slug}", &generated.ClearOrganizationAttributeRequestOptions{PathParams: &generated.ClearOrganizationAttributePath{ID: a.OrganizationID, Slug: a.Slug}, Query: &generated.ClearOrganizationAttributeQuery{Ordinal: a.Ordinal, ExpectedValueID: a.ExpectedValueID, DryRun: a.DryRun}}, a.OrganizationID, ""}
	default:
		return r, false, false
	}
	if name != "create_organization" && !validLoopID(r.id) {
		valid = false
	}
	if name == "update_organization" || name == "delete_organization" || name == "merge_organization" || name == "replace_organization_profile" {
		valid = valid && r.etag != ""
	}
	return r, true, valid
}

func organizationRefusal(code string) bool {
	switch code {
	case "organization_not_found", "organization_revision_conflict", "organization_has_employments", "organization_merge_conflict", "organization_profile_too_large", "invalid_organization", "invalid_source", "invalid_attribute_definition", "attribute_definition_not_found", "attribute_definition_inactive", "attribute_definition_not_writable", "invalid_attribute_value", "attribute_value_not_found", "attribute_value_conflict", "invalid_person_id", "profile_media_not_found", "profile_media_content_unavailable", "invalid_if_match", "invalid_ordinal", "invalid_expected_value_id", "organizations_unavailable":
		return true
	}
	return false
}

func (b *daemonMCPOperations) executeOrganizationWriteOrMedia(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if name == "get_organization_media" {
		result, err := b.organizationMedia(ctx, args)
		return result, true, err
	}
	r, handled, valid := organizationWriteRequest(name, args)
	if !handled {
		return nil, false, nil
	}
	if !valid {
		return operationFailure("invalid_arguments", false), true, nil
	}
	if name == "delete_organization" {
		_, _, failure, err := readMCPOperationBytes(ctx, b.client, r.method, r.path, r.options, 1<<20, http.StatusNoContent)
		if failure != nil {
			return failure, true, err
		}
		return &mcpserver.OperationResult{Output: mcpserver.OrganizationDeleted{Deleted: true}}, true, nil
	}
	if name == "set_organization_attribute" || name == "clear_organization_attribute" {
		result, err := readMCPOperationJSON[generated.OrganizationAttributeWrite](ctx, b.client, r.method, r.path, r.options, http.StatusOK, http.StatusCreated)
		return result, true, err
	}
	if name == "replace_organization_profile" {
		result, err := readMCPOperationJSONLimit[generated.OrganizationProfile](ctx, b.client, r.method, r.path, r.options, organizationProfileResponseLimit, http.StatusOK)
		if result != nil && !result.IsError {
			profile, ok := result.Output.(generated.OrganizationProfile)
			if !ok || result.ETag == "" {
				return operationFailure("invalid_operation_response", true), true, nil
			}
			result.Output = mcpserver.OrganizationRecord{ETag: result.ETag, Profile: profile}
		}
		return result, true, err
	}
	result, err := readMCPOperationJSON[generated.Organization](ctx, b.client, r.method, r.path, r.options, http.StatusOK, http.StatusCreated)
	if result != nil && !result.IsError {
		org, ok := result.Output.(generated.Organization)
		if !ok || result.ETag == "" {
			return operationFailure("invalid_operation_response", true), true, nil
		}
		result.Output = mcpserver.OrganizationMutation{ETag: result.ETag, Organization: org}
	}
	return result, true, err
}

type organizationMediaArguments struct {
	OrganizationID int64   `json:"organization_id"`
	MediaID        int64   `json:"media_id"`
	Offset         *int64  `json:"offset,omitempty"`
	Length         *int64  `json:"length,omitempty"`
	SHA256         *string `json:"sha256,omitempty"`
}

func (b *daemonMCPOperations) organizationMedia(ctx context.Context, args map[string]any) (*mcpserver.OperationResult, error) {
	a, err := decodeMCPOperationArguments[organizationMediaArguments](args)
	if err != nil || !validLoopID(a.OrganizationID) || !validLoopID(a.MediaID) {
		return operationFailure("invalid_arguments", false), nil
	}
	data, headers, failure, err := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/organizations/{id}/profile/media/{media_id}/content", &generated.GetOrganizationProfileMediaContentRequestOptions{PathParams: &generated.GetOrganizationProfileMediaContentPath{ID: a.OrganizationID, MediaID: a.MediaID}}, store.MaxPersonMediaBytes, http.StatusOK)
	if failure != nil {
		return failure, err
	}
	chunk, err := mcpserver.PersonMediaChunk(data, headers.Get("Content-Type"), args)
	if err != nil {
		return operationFailure("invalid_arguments", false), nil
	}
	return &mcpserver.OperationResult{Output: chunk}, nil
}

func organizationContextRefusal(result *mcpserver.OperationResult) *mcpserver.OperationRefusalError {
	if result != nil && result.IsError {
		if failure, ok := result.Output.(struct {
			Error                     string `json:"error"`
			OperationMayHaveCompleted bool   `json:"operation_may_have_completed"`
		}); ok && failure.Error != "" && !failure.OperationMayHaveCompleted {
			return &mcpserver.OperationRefusalError{Code: failure.Error}
		}
	}
	return &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
}

func (b *daemonMCPOperations) organizationOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	r, handled, valid := organizationWriteRequest(name, args)
	if !handled {
		return "", false, nil
	}
	if !valid {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	var current any
	var losing any
	var preview any
	if r.id != 0 {
		result, _, err := b.executeOrganizationOperation(ctx, "get_organization", map[string]any{"organization_id": r.id})
		if err != nil || result == nil || result.IsError {
			return "", true, organizationContextRefusal(result)
		}
		current = result.Output
	}
	if name == "set_organization_attribute" || name == "clear_organization_attribute" {
		switch options := r.options.(type) {
		case *generated.SetOrganizationAttributeRequestOptions:
			body := *options.Body
			body.DryRun = new(true)
			body.ExpectedValueID = nil
			options.Body = &body
		case *generated.ClearOrganizationAttributeRequestOptions:
			query := *options.Query
			query.DryRun = new(true)
			query.ExpectedValueID = nil
			options.Query = &query
		}
		result, err := readMCPOperationJSON[generated.OrganizationAttributeWrite](ctx, b.client, r.method, r.path, r.options, http.StatusOK, http.StatusCreated)
		if err != nil || result == nil || result.IsError {
			return "", true, organizationContextRefusal(result)
		}
		write, ok := result.Output.(generated.OrganizationAttributeWrite)
		if !ok {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		var ordinal int64
		var superseded any
		if v := write.Superseded; v != nil {
			ordinal = v.Ordinal
			superseded = map[string]any{"id": v.ID, "ordinal": v.Ordinal, "value": v.Value, "source": v.Source, "source_ref": v.SourceRef, "actor": v.Actor, "confidence": v.Confidence, "active_from": v.ActiveFrom, "created_at": v.CreatedAt}
		}
		if write.Value != nil {
			ordinal = write.Value.Ordinal
		}
		preview = map[string]any{"ordinal": ordinal, "superseded": superseded}
	}
	if name == "merge_organization" {
		input, _ := decodeMCPOperationArguments[mcpserver.OrganizationMergeArgs](args)
		result, _, err := b.executeOrganizationOperation(ctx, "get_organization", map[string]any{"organization_id": input.Body.LosingOrganizationID})
		if err != nil || result == nil || result.IsError {
			return "", true, organizationContextRefusal(result)
		}
		losing = result.Output
	}
	// Hash all exact arguments and replace inline media with its digest in the human disclosure.
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
						bytes, err := base64.StdEncoding.DecodeString(data)
						if err != nil {
							return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
						}
						digest := sha256.Sum256(bytes)
						row["data"] = map[string]any{"bytes": len(bytes), "sha256": hex.EncodeToString(digest[:])}
					}
				}
			}
		}
	}
	effect := map[string]string{"create_organization": "create an organization", "update_organization": "replace mutable fields; omitted fields are cleared except retired", "delete_organization": "delete the organization only if it has no employment records", "merge_organization": "merge the losing organization, moving employment and superseding its active profile and attributes", "replace_organization_profile": "replace all profile collections; omitted collections are cleared", "set_organization_attribute": "set the named typed attribute", "clear_organization_attribute": "supersede the named typed attribute"}[name]
	disclosure, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current,omitempty"`
		Losing    any            `json:"losing,omitempty"`
		Preview   any            `json:"preview,omitempty"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current, losing, preview, changes, effect}, json.Deterministic(true))
	return string(disclosure), true, err
}
