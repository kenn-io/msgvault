package cmd

import (
	"context"
	"encoding/json/v2"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const attributeSourceKey = "source"
const attributeInvalidArguments = "invalid_arguments"

func recordAttributeMCPCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, r := range []struct {
		name, id, method, path, needs string
		query, properties             []string
	}{
		{"list_attribute_definitions", "listAttributeDefinitions", http.MethodGet, "/api/v1/attribute-definitions", "", []string{"object_type", "include_hidden"}, nil},
		{"list_person_attributes", "listPersonAttributes", http.MethodGet, "/api/v1/people/{id}/attributes", "", []string{"history", "slug", "universal_id"}, nil},
		{"set_person_attribute", "setPersonAttribute", http.MethodPut, "/api/v1/people/{id}/attributes/{slug}", "list_person_attributes", []string{"dry_run"}, []string{"value", "ordinal", "expected_value_id", "expected_empty", attributeSourceKey, "source_ref", "confidence", "actor", "active_from", "active_until"}},
		{"clear_person_attribute", "clearPersonAttribute", http.MethodDelete, "/api/v1/people/{id}/attributes/{slug}", "list_person_attributes", []string{"ordinal", "expected_value_id", "dry_run"}, nil},
		{"get_attribute_definition", "getAttributeDefinition", http.MethodGet, "/api/v1/attribute-definitions/{id}", "", nil, nil},
		{"create_attribute_definition", "createAttributeDefinition", http.MethodPost, "/api/v1/attribute-definitions", "", nil, []string{"object_type", "slug", "label", "description", "value_type", "field_type", "record_target", "cardinality", "display_order", "is_required", "is_searchable", "is_sensitive", "is_audited", "options", "vcard_property"}},
		{"patch_attribute_definition", "patchAttributeDefinition", http.MethodPatch, "/api/v1/attribute-definitions/{id}", "get_attribute_definition", nil, []string{"label", "description", "display_order", "is_sensitive", "is_active"}},
		{"delete_attribute_definition", "deleteAttributeDefinition", http.MethodDelete, "/api/v1/attribute-definitions/{id}", "get_attribute_definition", nil, nil},
	} {
		if hasRoute(r.id, r.method, r.path, r.query...) && mcpRequestPropertiesPresent(caps, r.id, r.properties...) &&
			(r.needs == "" || slices.Contains(names, r.needs)) {
			names = append(names, r.name)
		}
	}
	return names
}

type attributeListArgs struct {
	PersonID    int64   `json:"person_id"`
	History     *bool   `json:"history,omitempty"`
	Slug        *string `json:"slug,omitempty"`
	UniversalID *string `json:"universal_id,omitempty"`
}
type attributeSetArgs struct {
	generated.SetPersonAttributeBody `json:",inline"`

	PersonID int64  `json:"person_id"`
	Slug     string `json:"slug"`
	DryRun   *bool  `json:"dry_run,omitempty"`
}
type attributeClearArgs struct {
	PersonID        int64  `json:"person_id"`
	Slug            string `json:"slug"`
	Ordinal         *int64 `json:"ordinal,omitempty"`
	ExpectedValueID *int64 `json:"expected_value_id,omitempty"`
	DryRun          *bool  `json:"dry_run,omitempty"`
}
type attributeDefinitionsArgs struct {
	IncludeHidden *bool `json:"include_hidden,omitempty"`
}
type attributeDefinitionArgs struct {
	ID         int64                                       `json:"definition_id"`
	ETag       string                                      `json:"etag,omitempty"`
	Definition *generated.CreateAttributeDefinitionRequest `json:"definition,omitempty"`
	Changes    map[string]any                              `json:"changes,omitempty"`
}

// The generated PATCH body cannot represent explicit null independently of omission.
// Reuse its path and headers while retaining the native JSON merge field presence.
type attributePatchOptions struct {
	*generated.PatchAttributeDefinitionRequestOptions

	changes map[string]any
}

func (o *attributePatchOptions) GetBody() any { return o.changes }

func attributeRefusal(code string) bool {
	switch code {
	case "attribute_uniqueness_unsupported", "attribute_invalid", "attribute_value_invalid",
		"attribute_definition_not_found", "attribute_value_not_found", "attribute_definition_slug_conflict",
		"attribute_definition_universal_id_conflict", "attribute_definition_revision_conflict",
		"attribute_definition_not_deletable", "attribute_definition_has_values", "attribute_definition_not_writable",
		"attribute_definition_inactive", "attribute_value_conflict", "invalid_attribute_slug", "invalid_expected_value_id", "invalid_expected_empty",
		"invalid_ordinal", "invalid_object_type", "invalid_attribute_definition_id", "invalid_if_match", "attributes_unavailable", "server_busy":
		return true
	}
	return false
}

func attributeRequest(name string, args map[string]any) (string, string, runtime.RequestOptions, bool) {
	switch name {
	case "list_person_attributes":
		in, err := decodeMCPOperationArguments[attributeListArgs](args)
		if err != nil {
			return "", "", nil, false
		}
		return http.MethodGet, "/api/v1/people/{id}/attributes", &generated.ListPersonAttributesRequestOptions{PathParams: &generated.ListPersonAttributesPath{ID: in.PersonID}, Query: &generated.ListPersonAttributesQuery{History: in.History, Slug: in.Slug, UniversalID: in.UniversalID}}, true
	case "set_person_attribute":
		in, err := decodeMCPOperationArguments[attributeSetArgs](args)
		if err != nil || in.PersonID <= 0 || strings.TrimSpace(in.Slug) == "" || args["value"] == nil {
			return "", "", nil, false
		}
		if in.Source == nil || strings.TrimSpace(string(*in.Source)) == "" {
			in.Source = new(generated.SetPersonAttributeRequestSourceEnrichment)
		}
		if in.Actor == nil || strings.TrimSpace(*in.Actor) == "" {
			in.Actor = new("mcp")
		}
		return http.MethodPut, "/api/v1/people/{id}/attributes/{slug}", &generated.SetPersonAttributeRequestOptions{PathParams: &generated.SetPersonAttributePath{ID: in.PersonID, Slug: in.Slug}, Query: &generated.SetPersonAttributeQuery{DryRun: in.DryRun}, Body: &in.SetPersonAttributeBody}, true
	case "clear_person_attribute":
		in, err := decodeMCPOperationArguments[attributeClearArgs](args)
		if err != nil || in.PersonID <= 0 || strings.TrimSpace(in.Slug) == "" {
			return "", "", nil, false
		}
		return http.MethodDelete, "/api/v1/people/{id}/attributes/{slug}", &generated.ClearPersonAttributeRequestOptions{PathParams: &generated.ClearPersonAttributePath{ID: in.PersonID, Slug: in.Slug}, Query: &generated.ClearPersonAttributeQuery{Ordinal: in.Ordinal, ExpectedValueID: in.ExpectedValueID, DryRun: in.DryRun}}, true
	case "list_attribute_definitions":
		in, err := decodeMCPOperationArguments[attributeDefinitionsArgs](args)
		if err != nil {
			return "", "", nil, false
		}
		return http.MethodGet, "/api/v1/attribute-definitions", &generated.ListAttributeDefinitionsRequestOptions{Query: &generated.ListAttributeDefinitionsQuery{ObjectType: new("person"), IncludeHidden: in.IncludeHidden}}, true
	case "get_attribute_definition", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition":
		in, err := decodeMCPOperationArguments[attributeDefinitionArgs](args)
		if err != nil {
			return "", "", nil, false
		}
		switch name {
		case "create_attribute_definition":
			if in.Definition == nil {
				return "", "", nil, false
			}
			in.Definition.ObjectType = generated.CreateAttributeDefinitionRequestObjectTypePerson
			return http.MethodPost, "/api/v1/attribute-definitions", &generated.CreateAttributeDefinitionRequestOptions{Body: in.Definition}, true
		case "get_attribute_definition":
			return http.MethodGet, "/api/v1/attribute-definitions/{id}", &generated.GetAttributeDefinitionRequestOptions{PathParams: &generated.GetAttributeDefinitionPath{ID: in.ID}}, true
		case "patch_attribute_definition":
			return http.MethodPatch, "/api/v1/attribute-definitions/{id}", &attributePatchOptions{PatchAttributeDefinitionRequestOptions: &generated.PatchAttributeDefinitionRequestOptions{PathParams: &generated.PatchAttributeDefinitionPath{ID: in.ID}, Header: &generated.PatchAttributeDefinitionHeaders{IfMatch: in.ETag}}, changes: in.Changes}, true
		case "delete_attribute_definition":
			return http.MethodDelete, "/api/v1/attribute-definitions/{id}", &generated.DeleteAttributeDefinitionRequestOptions{PathParams: &generated.DeleteAttributeDefinitionPath{ID: in.ID}, Header: &generated.DeleteAttributeDefinitionHeaders{IfMatch: in.ETag}}, true
		}
	}
	return "", "", nil, false
}

func (b *daemonMCPOperations) executeRecordAttributeOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "list_person_attributes", "set_person_attribute", "clear_person_attribute", "list_attribute_definitions", "get_attribute_definition", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition":
	default:
		return nil, false, nil
	}
	method, path, options, ok := attributeRequest(name, args)
	if !ok {
		return operationFailure(attributeInvalidArguments, false), true, nil
	}
	var result *mcpserver.OperationResult
	var err error

	switch name {
	case "list_person_attributes":
		result, err = readMCPOperationJSONWithClassifier[generated.PersonAttributesResponse](ctx, b.client, method, path, options, attributeRefusal, http.StatusOK)
	case "set_person_attribute", "clear_person_attribute":
		result, err = readMCPOperationJSONWithClassifier[generated.PersonAttributeWrite](ctx, b.client, method, path, options, attributeRefusal, http.StatusOK)
	case "list_attribute_definitions":
		result, err = readMCPOperationJSONWithClassifier[generated.AttributeDefinitionsResponse](ctx, b.client, method, path, options, attributeRefusal, http.StatusOK)
	case "delete_attribute_definition":
		result, err = readMCPOperationJSONWithClassifier[mcpserver.AttributeDefinitionDeleted](ctx, b.client, method, path, options, attributeRefusal, http.StatusNoContent)
		if result != nil && !result.IsError {
			result.Output = mcpserver.AttributeDefinitionDeleted{Deleted: true}
		}
	default:
		result, err = readMCPOperationJSONWithClassifier[generated.AttributeDefinition](ctx, b.client, method, path, options, attributeRefusal, http.StatusOK, http.StatusCreated)
		if result != nil && !result.IsError {
			d, valid := result.Output.(generated.AttributeDefinition)
			if !valid || result.ETag == "" {
				result = operationFailure("invalid_operation_response", method != http.MethodGet)
			} else if d.ObjectType != "person" {
				result = operationFailure("attribute_definition_not_found", false)
			} else {
				result.Output = mcpserver.AttributeDefinitionReceipt{ETag: result.ETag, Definition: d}
			}
		}
	}
	return result, true, err
}

func attributeContextRefusal(result *mcpserver.OperationResult) *mcpserver.OperationRefusalError {
	code := "operation_context_unavailable"
	if result != nil {
		switch failure := result.Output.(type) {
		case mcpOperationFailure:
			if !failure.OperationMayHaveCompleted {
				code = failure.Error
			}
		}
	}
	return &mcpserver.OperationRefusalError{Code: code}
}

func (b *daemonMCPOperations) recordAttributeDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	switch name {
	case "set_person_attribute", "clear_person_attribute", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition":
	default:
		return "", false, nil
	}
	_, _, options, ok := attributeRequest(name, args)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: attributeInvalidArguments}
	}
	var current any
	if name == "set_person_attribute" || name == "clear_person_attribute" {
		slug, _ := args["slug"].(string)
		listing, _, err := b.executeRecordAttributeOperation(ctx, "list_person_attributes", map[string]any{"person_id": args["person_id"], "slug": strings.TrimSpace(slug)})
		if err != nil || listing == nil || listing.IsError {
			return "", true, attributeContextRefusal(listing)
		}
		list, ok := listing.Output.(generated.PersonAttributesResponse)
		if !ok {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		for _, group := range list.Attributes {
			if group.Definition.UniversalID == store.AttributeUniversalIDNotes {
				return "", true, &mcpserver.OperationRefusalError{Code: "attribute_notes_use_update_person_notes"}
			}
		}
		if name == "set_person_attribute" {
			request, ok := options.(*generated.SetPersonAttributeRequestOptions)
			if !ok || request.Body == nil || request.Body.Source == nil || request.Body.Actor == nil {
				return "", true, &mcpserver.OperationRefusalError{Code: attributeInvalidArguments}
			}
			body := request.Body
			args[attributeSourceKey] = string(*body.Source)
			args["actor"] = *body.Actor
		}
		previewArgs := maps.Clone(args)
		previewArgs["dry_run"] = true
		result, _, err := b.executeRecordAttributeOperation(ctx, name, previewArgs)
		if err != nil || result == nil || result.IsError {
			return "", true, attributeContextRefusal(result)
		}
		preview, ok := result.Output.(generated.PersonAttributeWrite)
		if !ok || (name == "set_person_attribute" && preview.Value == nil) || (name == "clear_person_attribute" && preview.Superseded == nil) {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		var ordinal int64
		var superseded any
		unguarded := args["expected_value_id"] == nil && args["expected_empty"] != true
		if v := preview.Superseded; v != nil {
			ordinal = v.Ordinal
			superseded = map[string]any{"id": v.ID, "ordinal": v.Ordinal, "value": v.Value, attributeSourceKey: v.Source, "source_ref": v.SourceRef, "actor": v.Actor, "confidence": v.Confidence, "active_from": v.ActiveFrom, "created_at": v.CreatedAt}
			if unguarded {
				args["expected_value_id"] = v.ID
			}
		} else if name == "set_person_attribute" && unguarded {
			args["expected_empty"] = true
		}
		if preview.Value != nil {
			ordinal = preview.Value.Ordinal
		}
		if args["ordinal"] == nil {
			args["ordinal"] = ordinal
		}
		current = map[string]any{"ordinal": ordinal, "superseded": superseded}
	}
	if name == "patch_attribute_definition" || name == "delete_attribute_definition" {
		input, err := decodeMCPOperationArguments[attributeDefinitionArgs](args)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: attributeInvalidArguments}
		}
		result, _, err := b.executeRecordAttributeOperation(ctx, "get_attribute_definition", map[string]any{"definition_id": input.ID})
		if err != nil || result == nil || result.IsError {
			return "", true, attributeContextRefusal(result)
		}
		receipt, ok := result.Output.(mcpserver.AttributeDefinitionReceipt)
		if !ok {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		if receipt.ETag != input.ETag {
			return "", true, &mcpserver.OperationRefusalError{Code: "attribute_definition_revision_conflict"}
		}
		current = &receipt
	}
	effect := "Apply the attribute operation. Replaced values stay in history; an empty slot must remain empty."
	if dryRun, _ := args["dry_run"].(bool); dryRun {
		effect = "Preview only; nothing is written. Native value guards apply."
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current,omitempty"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
