package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

const ToolCreatePerson = "create_person"

// PersonCreator is the optional daemon-backed capability for creating people.
type PersonCreator interface {
	CreatePerson(ctx context.Context, input store.PersonCreateInput) (*store.Person, error)
}

func personCreateAvailable(c catalogCapabilities) bool { return c.personCreate }

func createPersonDefinition() toolDefinition {
	// The server sets provenance, so agents never see the source field.
	input := outputSchemaFor[store.PersonCreateInput]()
	delete(input.Properties, "source")
	definition := profileWriteDefinition(ToolCreatePerson,
		"Create a durable person without message participants. Refuses existing emails or "+
			"phones; use the matched person or explicitly promote its participant instead. "+
			"A title requires an org. MCP writes use enrichment provenance, so CardDAV "+
			"publication requires review. Does not publish to CardDAV.",
		input, outputSchemaFor[store.Person](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.createPerson(ctx, req)
		})
	definition.availability = personCreateAvailable
	return definition
}

func (h *handlers) createPerson(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.personCreator == nil {
		return toolErrorResult("Person creation is unavailable"), nil
	}
	data, err := json.Marshal(req.GetArguments())
	if err != nil {
		return nil, newInternalError("decode person creation", err)
	}
	var input store.PersonCreateInput
	if err := json.Unmarshal(data, &input, json.RejectUnknownMembers(true)); err != nil {
		return toolErrorResult(err.Error()), nil
	}
	input.Source = store.ProvenanceEnrichment
	if err := store.ValidatePersonCreateInput(input); err != nil {
		return toolErrorResult(err.Error()), nil
	}
	person, err := h.personCreator.CreatePerson(ctx, input)
	if err != nil {
		if personCreateRefusal(err) {
			return toolErrorResult(err.Error()), nil
		}
		return nil, newInternalError("create person", err)
	}
	return jsonResult(person)
}

// personCreateRefusal reports caller-correctable refusals from the store or
// from the daemon API, which reports them as coded errors.
func personCreateRefusal(err error) bool {
	if errors.Is(err, store.ErrPersonContactExists) || errors.Is(err, store.ErrPersonCreateInvalid) {
		return true
	}
	var coded daemonAPIErrorCoder
	if !errors.As(err, &coded) {
		return false
	}
	code := coded.APIErrorCode()
	return code == "person_contact_exists" || code == "invalid_person"
}
