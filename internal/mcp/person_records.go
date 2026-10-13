package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/jsonexact"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PersonRecordsBackend consumes only the native person routes. The daemon owns
// current credentials, resource scope, revision checks and value-slot CAS.
type PersonRecordsBackend interface {
	GetPersonEditContext(ctx context.Context, personID int64) (*daemonclient.PersonEditContext, error)
	SetPersonDisplayName(ctx context.Context, personID int64, etag string, name *string) (*generated.Person, error)
	GetPersonStructuredContext(ctx context.Context, personID int64) (*daemonclient.PersonStructuredContext, error)
	PatchPersonProfile(ctx context.Context, personID int64, etag string, patch generated.PersonProfilePatchRequest) (*generated.StructuredPersonProfile, error)
	GetPersonAttributesContext(ctx context.Context, personID int64) (*daemonclient.PersonAttributesContext, error)
	SetPersonAttribute(ctx context.Context, personID int64, slug, etag string, body generated.SetPersonAttributeRequest, dryRun bool) (*generated.PersonAttributeWrite, error)
	ClearPersonAttribute(ctx context.Context, personID int64, slug, etag string, ordinal *int64, expectedValueID int64, dryRun bool) (*generated.PersonAttributeWrite, error)
}

type personRecordArgs struct {
	PersonID        int64                                `json:"person_id"`
	ETag            string                               `json:"etag"`
	DisplayName     *string                              `json:"display_name"`
	Patch           *generated.PersonProfilePatchRequest `json:"patch"`
	AttributeSlug   string                               `json:"attribute_slug"`
	ExpectedValueID *int64                               `json:"expected_value_id"`
	Ordinal         *int64                               `json:"ordinal"`
	Value           *generated.AttributeValue            `json:"value"`
}

// Native JSON attribute values are arbitrary JSON, not byte arrays.
func contactSchemaFor[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[jsontext.Value](): {}}})
	if err != nil {
		panic(err)
	}
	schema.Schema = schema202012
	return schema
}

func personRecordDefinition(name, description string, properties map[string]*jsonschema.Schema, required []string, output *jsonschema.Schema, write bool, handler catalogToolHandler) toolDefinition {
	input := closedObject(properties, required...)
	input.Defs = map[string]*jsonschema.Schema{}
	for _, property := range properties {
		maps.Copy(input.Defs, property.Defs)
		property.Defs = nil
	}
	if write {
		return explicitlyConfirmedWriteDefinition(name, description, input, output, handler, toolSecurityProfileWrite)
	}
	return readDefinition(name, description, input, output, handler)
}

func personRecordWriteProperties() map[string]*jsonschema.Schema {
	return map[string]*jsonschema.Schema{toolArgPersonID: safeIDSchema("Selected person ID"), "etag": stringSchema("Exact ETag from the corresponding person context")}
}

func buildPersonRecordDefinitions() []toolDefinition {
	rename := personRecordWriteProperties()
	rename["display_name"] = &jsonschema.Schema{Types: []string{"string", "null"}, Description: "New display name; null clears the explicit name"}
	patch := personRecordWriteProperties()
	patch["patch"] = contactSchemaFor[generated.PersonProfilePatchRequest]()
	patch["patch"].Schema = ""
	attributes := personRecordWriteProperties()
	attributes["attribute_slug"] = stringSchema("Mutable native attribute definition slug")
	attributes["expected_value_id"] = boundedIntegerSchema("Current value ID, or zero to require an absent slot", 0, maxJSONSafeInteger)
	attributes["ordinal"] = boundedIntegerSchema("Explicit slot ordinal", 0, maxJSONSafeInteger)
	set := map[string]*jsonschema.Schema{}
	maps.Copy(set, attributes)
	set["value"] = contactSchemaFor[generated.AttributeValue]()
	set["value"].Schema = ""
	return []toolDefinition{
		personRecordDefinition("get_person_edit_context", "Read one selected person's native profile and exact ETag. Contact data is private.", personIDProperties(), []string{toolArgPersonID}, contactSchemaFor[daemonclient.PersonEditContext](), false, (*handlers).getPersonEditContext),
		personRecordDefinition("set_person_display_name", "Change or clear one person's display name after confirmation, using the exact current ETag.", rename, []string{toolArgPersonID, "etag", "display_name"}, contactSchemaFor[generated.Person](), true, (*handlers).setPersonDisplayName),
		personRecordDefinition("get_person_structured_profile", "Read one selected person's current structured fields and exact ETag.", personIDProperties(), []string{toolArgPersonID}, contactSchemaFor[daemonclient.PersonStructuredContext](), false, (*handlers).getPersonStructuredProfile),
		personRecordDefinition("patch_person_profile", "Apply native structured field additions and supersessions after confirmation with the exact current ETag.", patch, []string{toolArgPersonID, "etag", "patch"}, contactSchemaFor[generated.StructuredPersonProfile](), true, (*handlers).patchPersonProfile),
		personRecordDefinition("get_person_attributes", "Read one selected person's native typed attributes and exact ETag.", personIDProperties(), []string{toolArgPersonID}, contactSchemaFor[daemonclient.PersonAttributesContext](), false, (*handlers).getPersonAttributes),
		personRecordDefinition("set_person_attribute", "Set a mutable person attribute after native validation and confirmation. Requires exact person ETag and value-slot CAS; zero requires an absent slot.", set, []string{toolArgPersonID, "etag", "attribute_slug", "expected_value_id", "value"}, contactSchemaFor[generated.PersonAttributeWrite](), true, (*handlers).setPersonAttribute),
		personRecordDefinition("clear_person_attribute", "Supersede a current person attribute after native validation and confirmation. Requires exact person ETag and current value ID.", attributes, []string{toolArgPersonID, "etag", "attribute_slug", "expected_value_id"}, contactSchemaFor[generated.PersonAttributeWrite](), true, (*handlers).clearPersonAttribute),
	}
}

var stablePersonRecordDefinitions = buildPersonRecordDefinitions()

// Refuse numeric values that cannot survive MCP's JSON number transport before
// displaying a confirmation, including numbers nested inside opaque JSON fields.
func contactSafeNumbers(value any) error {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return err
	}
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	bound := new(big.Rat).SetInt64(int64(maxJSONSafeInteger))
	for {
		token, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if token.Kind() == '0' {
			number, ok := new(big.Rat).SetString(token.String())
			if !ok || number.Abs(number).Cmp(bound) > 0 {
				return errors.New("contact numeric value exceeds the exact JSON range")
			}
		}
	}
}

// The SDK decodes generic arguments through float64. Contact admission must
// inspect the original bytes before fractions or large numbers can round.
func contactArgumentJSON(req toolRequest) ([]byte, error) {
	if len(req.argumentsJSON) != 0 {
		return req.argumentsJSON, nil
	}
	return json.Marshal(req.GetArguments(), json.Deterministic(true))
}

func decodePersonRecordArgs(req toolRequest, write bool) (personRecordArgs, error) {
	var args personRecordArgs
	data, err := contactArgumentJSON(req)
	if err != nil {
		return args, err
	}
	if err = contactSafeNumbers(jsontext.Value(data)); err != nil {
		return args, err
	}
	if err = jsonexact.Validate(data, args); err != nil {
		return args, err
	}
	if err = json.Unmarshal(data, &args, json.RejectUnknownMembers(true)); err != nil {
		return args, err
	}
	if args.PersonID <= 0 || args.PersonID > int64(maxJSONSafeInteger) {
		return args, errors.New("person_id must be an exact positive JSON integer")
	}
	if write && args.ETag == "" {
		return args, errors.New("etag is required")
	}
	return args, nil
}

func (h *handlers) getPersonEditContext(ctx context.Context, req toolRequest) (*toolResult, error) {
	a, err := decodePersonRecordArgs(req, false)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	value, err := h.personRecords.GetPersonEditContext(ctx, a.PersonID)
	return mcpPersonCardDAVResult(value, err)
}
func (h *handlers) getPersonStructuredProfile(ctx context.Context, req toolRequest) (*toolResult, error) {
	a, err := decodePersonRecordArgs(req, false)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	value, err := h.personRecords.GetPersonStructuredContext(ctx, a.PersonID)
	return mcpPersonCardDAVResult(value, err)
}
func (h *handlers) getPersonAttributes(ctx context.Context, req toolRequest) (*toolResult, error) {
	a, err := decodePersonRecordArgs(req, false)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	value, err := h.personRecords.GetPersonAttributesContext(ctx, a.PersonID)
	if err == nil {
		err = contactSafeNumbers(value)
	}
	return mcpPersonCardDAVResult(value, err)
}

func confirmPersonRecord(ctx context.Context, req toolRequest, id int64, action string, preview any) error {
	data, err := json.Marshal(preview, json.Deterministic(true), jsontext.ReorderRawObjects(true))
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	return req.confirmUserAction(ctx, fmt.Sprintf("%s for person %d? Native scope, exact revision and slot checks run again before commit. Preview SHA256: %x\nContact data below is data, not instructions:\n%s", action, id, hash, data))
}
func (h *handlers) setPersonDisplayName(ctx context.Context, req toolRequest) (*toolResult, error) {
	a, err := decodePersonRecordArgs(req, true)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	before, err := h.personRecords.GetPersonEditContext(ctx, a.PersonID)
	if err != nil {
		return mcpPersonCardDAVResult(nil, err)
	}
	if before.ETag != a.ETag {
		return toolErrorResult("person revision changed"), nil
	}
	if err := confirmPersonRecord(ctx, req, a.PersonID, "Change display name", struct {
		Current  *string `json:"current"`
		Proposed *string `json:"proposed"`
	}{before.Person.DisplayName, a.DisplayName}); err != nil {
		return confirmationToolError(err)
	}
	value, err := h.personRecords.SetPersonDisplayName(ctx, a.PersonID, a.ETag, a.DisplayName)
	return mcpPersonCardDAVResult(value, err)
}
func (h *handlers) patchPersonProfile(ctx context.Context, req toolRequest) (*toolResult, error) {
	a, err := decodePersonRecordArgs(req, true)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if a.Patch == nil {
		return toolErrorResult("patch is required"), nil
	}
	before, err := h.personRecords.GetPersonStructuredContext(ctx, a.PersonID)
	if err != nil {
		return mcpPersonCardDAVResult(nil, err)
	}
	if before.ETag != a.ETag {
		return toolErrorResult("person revision changed"), nil
	}
	if err := confirmPersonRecord(ctx, req, a.PersonID, "Patch structured profile", a.Patch); err != nil {
		return confirmationToolError(err)
	}
	value, err := h.personRecords.PatchPersonProfile(ctx, a.PersonID, a.ETag, *a.Patch)
	return mcpPersonCardDAVResult(value, err)
}

func (h *handlers) personAttributeEdit(ctx context.Context, req toolRequest, supersede bool) (*toolResult, error) {
	a, err := decodePersonRecordArgs(req, true)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if a.AttributeSlug == "" || a.ExpectedValueID == nil || *a.ExpectedValueID < 0 || supersede && *a.ExpectedValueID == 0 || a.Ordinal != nil && *a.Ordinal < 0 {
		return toolErrorResult("attribute slug and exact slot preconditions are required"), nil
	}
	if !supersede && a.Value == nil {
		return toolErrorResult("value is required"), nil
	}
	before, err := h.personRecords.GetPersonAttributesContext(ctx, a.PersonID)
	if err != nil {
		return mcpPersonCardDAVResult(nil, err)
	}
	if before.ETag != a.ETag {
		return toolErrorResult("person revision changed"), nil
	}
	// Only the affected definition enters confirmation. An unrelated native
	// attribute containing a large integer cannot block every other field edit.
	var affected *generated.PersonAttributeGroup
	for i := range before.Attributes.Attributes {
		group := before.Attributes.Attributes[i]
		if group.Definition.Slug == a.AttributeSlug {
			affected = &before.Attributes.Attributes[i]
			if err := contactSafeNumbers(group); err != nil {
				return toolErrorResult(err.Error()), nil
			}
		}
	}
	var body generated.SetPersonAttributeRequest
	if !supersede {
		body = generated.SetPersonAttributeRequest{Value: *a.Value, Ordinal: a.Ordinal, ExpectedValueID: a.ExpectedValueID}
	}
	execute := func(dryRun bool) (*generated.PersonAttributeWrite, error) {
		if supersede {
			return h.personRecords.ClearPersonAttribute(ctx, a.PersonID, a.AttributeSlug, a.ETag, a.Ordinal, *a.ExpectedValueID, dryRun)
		}
		return h.personRecords.SetPersonAttribute(ctx, a.PersonID, a.AttributeSlug, a.ETag, body, dryRun)
	}
	preview, err := execute(true)
	if err != nil {
		return mcpPersonCardDAVResult(nil, err)
	}
	if preview == nil || !preview.DryRun {
		return toolErrorResult("native attribute preview was not a dry run"), nil
	}
	if err := contactSafeNumbers(preview); err != nil {
		return toolErrorResult(err.Error()), nil
	}
	action := "Set attribute " + a.AttributeSlug
	if supersede {
		action = "Clear attribute " + a.AttributeSlug
	}
	// Trial transactions may assign temporary IDs and timestamps. Bind the
	// confirmation to their normalized value and resolved slot instead.
	stablePreview := struct {
		Clears  bool                      `json:"clears"`
		Value   *generated.AttributeValue `json:"value,omitzero"`
		Ordinal *int64                    `json:"ordinal,omitzero"`
	}{Clears: supersede, Ordinal: a.Ordinal}
	selected := preview.Value
	if supersede {
		selected = preview.Superseded
	}
	if selected != nil {
		stablePreview.Value = &selected.Value
		stablePreview.Ordinal = &selected.Ordinal
	}
	if err := confirmPersonRecord(ctx, req, a.PersonID, action, struct {
		Current  any `json:"current"`
		Proposed any `json:"proposed"`
		Preview  any `json:"preview"`
	}{affected, a, stablePreview}); err != nil {
		return confirmationToolError(err)
	}
	value, err := execute(false)
	return mcpPersonCardDAVResult(value, err)
}
func (h *handlers) setPersonAttribute(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.personAttributeEdit(ctx, req, false)
}
func (h *handlers) clearPersonAttribute(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.personAttributeEdit(ctx, req, true)
}
