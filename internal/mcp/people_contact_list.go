package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/contactcadence"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

func setPersonContactListDefinition(_ *handlers) toolDefinition {
	d := profileWriteDefinition(ToolSetPersonContactList, "Set a durable person's Sivers-style contact list using the existing audited contact_frequency attribute. Requires confirmation; never assigns a list from signal tiers.", closedObject(map[string]*jsonschema.Schema{
		toolArgPersonID: safeIDSchema("Durable person ID"), "list": stringSchema("Contact list preset", "A", "B", "C", "D"), "expected_value_id": safeIDSchema("Required when replacing a current frequency; omit for first creation"),
	}, toolArgPersonID, "list"), outputSchemaFor[store.PersonAttributeWrite](), func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
		return h.setPersonContactList(ctx, req)
	})
	d.availability = peopleAvailable
	return d
}
func (h *handlers) setPersonContactList(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	id, err := requiredPeopleID(args, toolArgPersonID)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	days, ok := contactcadence.DaysForList(stringArgument(args, "list"))
	if !ok {
		return toolErrorResult("list must be A, B, C, or D"), nil
	}
	expected, hasExpected, err := optionalPeopleID(args, "expected_value_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	attributes, err := h.peopleBackend.ListAttributes(ctx, id)
	if err != nil {
		return nil, newInternalError("read contact list", err)
	}
	var group *peoplebrowser.AttributeGroup
	if attributes != nil {
		for i := range attributes.Groups {
			if attributes.Groups[i].Definition.UniversalID == store.AttributeUniversalIDContactFrequency {
				group = &attributes.Groups[i]
				break
			}
		}
	}
	if group == nil {
		return nil, newInternalError("set contact list", errors.New("contact frequency definition is unavailable"))
	}
	if len(group.Current) > 1 {
		return toolErrorResult("contact frequency has conflicting current values; resolve them before setting a list"), nil
	}
	if len(group.Current) == 1 && !hasExpected {
		return toolErrorResult("expected_value_id is required when replacing a current frequency"), nil
	}
	if len(group.Current) == 0 && hasExpected {
		return toolErrorResult("expected_value_id must be omitted for first creation"), nil
	}
	if err := req.confirmUserAction(ctx, fmt.Sprintf("Set person %d contact list to %s (%d days)", id, contactcadence.ListForDays(days), days)); err != nil {
		return confirmationToolError(err)
	}
	// A confirmation may take time: preserve the observed absence atomically.
	zero := int64(0)
	expectedID := &zero
	if hasExpected {
		expectedID = &expected
	}
	result, err := h.peopleBackend.SetAttribute(ctx, peoplebrowser.SetAttributeRequest{PersonID: id, Slug: group.Definition.Slug, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, ExpectedValueID: expectedID, Source: store.ProvenanceEnrichment, Actor: "mcp"})
	if err != nil {
		if _, ok := errors.AsType[peoplebrowser.StaleValueError](err); ok {
			return toolErrorResult("contact frequency changed; reload before retrying"), nil
		}
		return nil, newInternalError("set contact list", err)
	}
	if result == nil {
		return nil, newInternalError("set contact list", errors.New("empty response"))
	}
	return jsonResult(result)
}
