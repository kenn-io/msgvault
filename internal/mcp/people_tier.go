package mcp

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PersonTierBackend reads the evidence behind an archived interaction tier.
type PersonTierBackend interface {
	GetPersonTier(ctx context.Context, personID int64) (generated.PersonTierExplanation, error)
}

func getPersonTierDefinition() toolDefinition {
	definition := readDefinition(
		ToolGetPersonTier,
		"Explain the tier inferred from archived interaction patterns for one durable person. The tier is not a judgement about the person. Returns signals, thresholds, manual grants, opt-outs and enrichment eligibility.",
		closedObject(map[string]*jsonschema.Schema{toolArgPersonID: safeIDSchema("Durable person ID")}, toolArgPersonID),
		outputSchemaFor[generated.PersonTierExplanation](),
		(*handlers).getPersonTier,
	)
	definition.availability = personTierAvailable
	return definition
}

func personTierAvailable(capabilities catalogCapabilities) bool { return capabilities.personTier }

func (h *handlers) getPersonTier(ctx context.Context, request toolRequest) (*toolResult, error) {
	personID, err := requiredPeopleID(request.GetArguments(), toolArgPersonID)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	explanation, err := h.personTierBackend.GetPersonTier(ctx, personID)
	if err != nil {
		return nil, newInternalError("get person tier", err)
	}
	return jsonResult(explanation)
}
