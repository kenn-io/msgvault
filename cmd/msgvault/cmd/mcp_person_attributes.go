package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"strings"

	"go.kenn.io/msgvault/internal/identitycontrol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type personAttributeArguments struct {
	PersonID        int64                     `json:"person_id"`
	ETag            string                    `json:"etag"`
	AttributeSlug   string                    `json:"attribute_slug"`
	ExpectedValueID *int64                    `json:"expected_value_id"`
	Ordinal         *int64                    `json:"ordinal,omitzero"`
	Value           *generated.AttributeValue `json:"value,omitzero"`
}

func validPersonAttributeArguments(input personAttributeArguments, isClear bool) bool {
	if !identitycontrol.ValidID(input.PersonID) || input.ETag == "" || strings.TrimSpace(input.AttributeSlug) == "" || input.ExpectedValueID == nil {
		return false
	}
	expected := *input.ExpectedValueID
	if expected != 0 && !identitycontrol.ValidID(expected) || input.Ordinal != nil && *input.Ordinal != 0 && !identitycontrol.ValidID(*input.Ordinal) {
		return false
	}
	if isClear {
		return expected > 0 && input.Value == nil
	}
	if input.Value == nil || input.Value.RecordID != nil || input.Value.RecordType != nil || !validMCPAttributeValue(*input.Value) {
		return false
	}
	switch input.Value.Type {
	case "text", "integer", "real", "boolean", "date", "timestamp", "json":
		return true
	default:
		return false
	}
}

func validMCPAttributeValue(value generated.AttributeValue) bool {
	const maximum = int64(9007199254740991)
	if value.Integer != nil && (*value.Integer < -maximum || *value.Integer > maximum) {
		return false
	}
	if len(value.JSON) == 0 {
		return true
	}
	var decoded any
	if err := json.Unmarshal(value.JSON, &decoded); err != nil {
		return false
	}
	return validMCPAttributeJSON(decoded)
}

func validMCPAttributeJSON(value any) bool {
	switch value := value.(type) {
	case float64:
		return math.Abs(value) <= 9007199254740991
	case []any:
		for _, child := range value {
			if !validMCPAttributeJSON(child) {
				return false
			}
		}
	case map[string]any:
		for _, child := range value {
			if !validMCPAttributeJSON(child) {
				return false
			}
		}
	}
	return true
}

func validMCPAttributeContext(attributes generated.PersonAttributesResponse) bool {
	for _, group := range attributes.Attributes {
		for _, values := range [][]generated.PersonAttributeValue{group.Current, group.History} {
			for _, value := range values {
				if !validMCPAttributeValue(value.Value) {
					return false
				}
			}
		}
	}
	return true
}

func validMCPAttributeWrite(write generated.PersonAttributeWrite) bool {
	for _, value := range []*generated.PersonAttributeValue{write.Value, write.Superseded} {
		if value != nil && !validMCPAttributeValue(value.Value) {
			return false
		}
	}
	return true
}

func (b *daemonMCPOperations) personAttributeWrite(ctx context.Context, input personAttributeArguments, isClear, dryRun bool) (*mcpserver.OperationResult, error) {
	if isClear {
		return readMCPOperationJSON[generated.PersonAttributeWrite](ctx, b.client, http.MethodDelete, "/api/v1/people/{id}/attributes/{slug}", &generated.ClearPersonAttributeRequestOptions{PathParams: &generated.ClearPersonAttributePath{ID: input.PersonID, Slug: input.AttributeSlug}, Header: &generated.ClearPersonAttributeHeaders{IfMatch: &input.ETag}, Query: &generated.ClearPersonAttributeQuery{Ordinal: input.Ordinal, ExpectedValueID: input.ExpectedValueID, DryRun: &dryRun}}, http.StatusOK)
	}
	return readMCPOperationJSON[generated.PersonAttributeWrite](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/attributes/{slug}", &generated.SetPersonAttributeRequestOptions{PathParams: &generated.SetPersonAttributePath{ID: input.PersonID, Slug: input.AttributeSlug}, Header: &generated.SetPersonAttributeHeaders{IfMatch: &input.ETag}, Query: &generated.SetPersonAttributeQuery{DryRun: &dryRun}, Body: &generated.SetPersonAttributeBody{Value: *input.Value, Ordinal: input.Ordinal, ExpectedValueID: input.ExpectedValueID}}, http.StatusOK)
}

func (b *daemonMCPOperations) personAttributeDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	input, err := decodeMCPOperationArguments[personAttributeArguments](args)
	if err != nil || !validPersonAttributeArguments(input, name == "clear_person_attribute") {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	current, _, err := b.executeSourceOperation(ctx, "get_person_attributes", map[string]any{"person_id": input.PersonID})
	if err != nil {
		return "", true, err
	}
	if current == nil || current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if current.ETag != input.ETag {
		return "", true, &mcpserver.OperationRefusalError{Code: "person_revision_conflict"}
	}
	preview, err := b.personAttributeWrite(ctx, input, name == "clear_person_attribute", true)
	if err != nil {
		return "", true, err
	}
	if preview == nil || preview.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "attribute_preview_refused"}
	}
	write, ok := preview.Output.(generated.PersonAttributeWrite)
	if !ok || !write.DryRun {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_response"}
	}
	if !validMCPAttributeWrite(write) {
		return "", true, &mcpserver.OperationRefusalError{Code: "unsupported_attribute_number"}
	}
	// Native preview timestamps describe a trial transaction. Bind confirmation
	// to the normalized value and selected slot, not its temporary clock values.
	stablePreview := struct {
		Clears  bool                      `json:"clears"`
		Value   *generated.AttributeValue `json:"value,omitzero"`
		Ordinal *int64                    `json:"ordinal,omitzero"`
	}{Clears: name == "clear_person_attribute", Ordinal: input.Ordinal}
	if write.Value != nil {
		stablePreview.Value = &write.Value.Value
		stablePreview.Ordinal = &write.Value.Ordinal
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current"`
		Changes   map[string]any `json:"changes"`
		Preview   any            `json:"preview"`
	}{name, current.Output, args, stablePreview}, json.Deterministic(true), jsontext.ReorderRawObjects(true))
	if len(data) == 0 && err == nil {
		err = errors.New("empty attribute disclosure")
	}
	return string(data), true, err
}
