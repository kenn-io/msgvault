package cmd

import (
	"context"
	"encoding/json/v2"
	"maps"
	"math"
	"net/http"
	"slices"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const attributeSourceField = "source"

func recordAttributeMCPCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, r := range []struct {
		name, id, method, path, needs string
		query, properties             []string
	}{
		{"list_person_attributes", "listPersonAttributes", http.MethodGet, "/api/v1/people/{id}/attributes", "", []string{"history", "slug", "universal_id"}, nil},
		{"set_person_attribute", "setPersonAttribute", http.MethodPut, "/api/v1/people/{id}/attributes/{slug}", "", []string{"dry_run"}, []string{"value", "ordinal", "expected_value_id", attributeSourceField, "source_ref", "confidence", "actor", "active_from", "active_until"}},
		{"clear_person_attribute", "clearPersonAttribute", http.MethodDelete, "/api/v1/people/{id}/attributes/{slug}", "", []string{"ordinal", "expected_value_id", "dry_run"}, nil},
		{"list_attribute_definitions", "listAttributeDefinitions", http.MethodGet, "/api/v1/attribute-definitions", "", []string{"object_type", "include_hidden"}, nil},
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
	ObjectType    *string `json:"object_type,omitempty"`
	IncludeHidden *bool   `json:"include_hidden,omitempty"`
}
type attributeDefinitionArgs struct {
	ID         int64                                       `json:"definition_id"`
	ETag       string                                      `json:"etag,omitempty"`
	Definition *generated.CreateAttributeDefinitionRequest `json:"definition,omitempty"`
	Changes    map[string]any                              `json:"changes,omitempty"`
}

type attributeConflictOutput struct {
	Error                     string                          `json:"error"`
	OperationMayHaveCompleted bool                            `json:"operation_may_have_completed"`
	CurrentValueID            *int64                          `json:"current_value_id,omitempty"`
	CurrentValue              *generated.PersonAttributeValue `json:"current_value,omitempty"`
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
	case "attribute_uniqueness_unsupported", "attribute_invalid", "attribute_value_invalid", "attribute_definition_not_found", "attribute_value_not_found", "attribute_definition_slug_conflict", "attribute_definition_universal_id_conflict", "attribute_definition_revision_conflict", "attribute_definition_not_deletable", "attribute_definition_has_values", "attribute_definition_not_writable", "attribute_definition_inactive", "attribute_value_conflict", "invalid_attribute_slug", "invalid_expected_value_id", "invalid_ordinal", "invalid_object_type", "invalid_attribute_definition_id", "invalid_if_match", "attributes_unavailable", "server_busy":
		return true
	}
	return false
}

// unsafeAttributeJSON rejects integers that may have been rounded by the MCP JSON decoder.
func unsafeAttributeJSON(value any) bool {
	switch v := value.(type) {
	case float64:
		return math.Trunc(v) == v && math.Abs(v) > maxJSONSafeInteger
	case []any:
		return slices.ContainsFunc(v, unsafeAttributeJSON)
	case map[string]any:
		for _, item := range v {
			if unsafeAttributeJSON(item) {
				return true
			}
		}
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
		value, _ := args["value"].(map[string]any)
		if err != nil || store.ValidateAttributeSlug(in.Slug) != nil || unsafeAttributeJSON(value["json"]) {
			return "", "", nil, false
		}
		return http.MethodPut, "/api/v1/people/{id}/attributes/{slug}", &generated.SetPersonAttributeRequestOptions{PathParams: &generated.SetPersonAttributePath{ID: in.PersonID, Slug: in.Slug}, Query: &generated.SetPersonAttributeQuery{DryRun: in.DryRun}, Body: &in.SetPersonAttributeBody}, true
	case "clear_person_attribute":
		in, err := decodeMCPOperationArguments[attributeClearArgs](args)
		if err != nil || store.ValidateAttributeSlug(in.Slug) != nil {
			return "", "", nil, false
		}
		return http.MethodDelete, "/api/v1/people/{id}/attributes/{slug}", &generated.ClearPersonAttributeRequestOptions{PathParams: &generated.ClearPersonAttributePath{ID: in.PersonID, Slug: in.Slug}, Query: &generated.ClearPersonAttributeQuery{Ordinal: in.Ordinal, ExpectedValueID: in.ExpectedValueID, DryRun: in.DryRun}}, true
	case "list_attribute_definitions":
		in, err := decodeMCPOperationArguments[attributeDefinitionsArgs](args)
		if err != nil {
			return "", "", nil, false
		}
		return http.MethodGet, "/api/v1/attribute-definitions", &generated.ListAttributeDefinitionsRequestOptions{Query: &generated.ListAttributeDefinitionsQuery{ObjectType: in.ObjectType, IncludeHidden: in.IncludeHidden}}, true
	case "get_attribute_definition", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition":
		in, err := decodeMCPOperationArguments[attributeDefinitionArgs](args)
		if err != nil {
			return "", "", nil, false
		}
		switch name {
		case "create_attribute_definition":
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
		return operationFailure(mcpInvalidArguments, false), true, nil
	}
	var result *mcpserver.OperationResult
	var err error
	classify := func(code string, body []byte) (any, bool) {
		if code == "attribute_value_conflict" && (name == "set_person_attribute" || name == "clear_person_attribute") {
			var conflict generated.PersonAttributeConflictResponse
			if json.Unmarshal(body, &conflict) == nil {
				return attributeConflictOutput{code, false, conflict.CurrentValueID, conflict.CurrentValue}, true
			}
		}
		return nil, attributeRefusal(code)
	}
	switch name {
	case "list_person_attributes":
		result, err = readMCPOperationJSONWithClassifier[generated.PersonAttributesResponse](ctx, b.client, method, path, options, classify, http.StatusOK)
	case "set_person_attribute", "clear_person_attribute":
		result, err = readMCPOperationJSONWithClassifier[generated.PersonAttributeWrite](ctx, b.client, method, path, options, classify, http.StatusOK)
	case "list_attribute_definitions":
		result, err = readMCPOperationJSONWithClassifier[generated.AttributeDefinitionsResponse](ctx, b.client, method, path, options, classify, http.StatusOK)
	case "delete_attribute_definition":
		result, err = readMCPOperationJSONWithClassifier[mcpserver.AttributeDefinitionDeleted](ctx, b.client, method, path, options, classify, http.StatusNoContent)
		if result != nil && !result.IsError {
			result.Output = mcpserver.AttributeDefinitionDeleted{Deleted: true}
		}
	default:
		result, err = readMCPOperationJSONWithClassifier[generated.AttributeDefinition](ctx, b.client, method, path, options, classify, http.StatusOK, http.StatusCreated)
		if result != nil && !result.IsError {
			d, valid := result.Output.(generated.AttributeDefinition)
			if !valid || result.ETag == "" {
				result = operationFailure("invalid_operation_response", method != http.MethodGet)
			} else {
				result.Output = mcpserver.AttributeDefinitionReceipt{ETag: result.ETag, Definition: d}
			}
		}
	}
	return result, true, err
}

func attributeContextRefusal(result *mcpserver.OperationResult) *mcpserver.OperationRefusalError {
	if result != nil && result.IsError {
		var code string
		switch failure := result.Output.(type) {
		case mcpOperationFailure:
			if !failure.OperationMayHaveCompleted {
				code = failure.Error
			}
		case attributeConflictOutput:
			if !failure.OperationMayHaveCompleted {
				code = failure.Error
			}
		}
		if code != "" {
			return &mcpserver.OperationRefusalError{Code: code}
		}
	}
	return &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
}

func (b *daemonMCPOperations) recordAttributeDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	switch name {
	case "set_person_attribute", "clear_person_attribute", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition":
	default:
		return "", false, nil
	}
	if _, _, _, ok := attributeRequest(name, args); !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: mcpInvalidArguments}
	}
	var current any
	if name == "set_person_attribute" || name == "clear_person_attribute" {
		previewArgs := maps.Clone(args)
		previewArgs["dry_run"] = true
		delete(previewArgs, "expected_value_id")
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
		if preview.Superseded != nil {
			v := preview.Superseded
			ordinal = v.Ordinal
			superseded = map[string]any{"id": v.ID, "ordinal": v.Ordinal, "value": v.Value, attributeSourceField: v.Source, "source_ref": v.SourceRef, "actor": v.Actor, "confidence": v.Confidence, "active_from": v.ActiveFrom, "created_at": v.CreatedAt}
		}
		if preview.Value != nil {
			ordinal = preview.Value.Ordinal
		}
		current = map[string]any{"ordinal": ordinal, "superseded": superseded}
	}
	if name == "patch_attribute_definition" || name == "delete_attribute_definition" {
		input, err := decodeMCPOperationArguments[attributeDefinitionArgs](args)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: mcpInvalidArguments}
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
	effect := "Apply the exact native attribute operation. Native revision and value guards apply."
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
