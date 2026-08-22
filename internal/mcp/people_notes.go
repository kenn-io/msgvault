package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type searchPeopleResponse struct {
	Rows          []query.PersonSummary `json:"rows"`
	TotalCount    int64                 `json:"total_count"`
	NextCursor    string                `json:"next_cursor"`
	CacheRevision string                `json:"cache_revision"`
}

type getPersonNotesResponse struct {
	PersonID  int64      `json:"person_id"`
	Text      string     `json:"text"`
	ValueID   int64      `json:"value_id"`
	UpdatedAt *time.Time `json:"updated_at"`
	Exists    bool       `json:"exists"`
}

type updatePersonNotesResponse struct {
	Current    *store.PersonAttributeValue `json:"current"`
	Superseded *store.PersonAttributeValue `json:"superseded"`
}

func searchPeopleDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolSearchPeople,
		"Search observed contacts and durable people profiles without returning private profile attributes.",
		closedObject(map[string]*jsonschema.Schema{
			"query":  stringSchema("Optional identity or display-name query"),
			"limit":  nonNegativeIntegerSchema("Maximum results to return (default 20)", 20),
			"cursor": stringSchema("Opaque cursor from a previous search_people response"),
		}),
		outputSchemaFor[searchPeopleResponse](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.searchPeople(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func getPersonNotesDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolGetPersonNotes,
		"Read the private user-curated Notes value for a durable person profile.",
		closedObject(map[string]*jsonschema.Schema{
			"person_id": safeIDSchema("Durable person profile ID"),
		}, "person_id"),
		outputSchemaFor[getPersonNotesResponse](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.getPersonNotes(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func promotePersonDefinition(_ *handlers) toolDefinition {
	definition := writeDefinition(
		ToolPromotePerson,
		"Explicitly promote an observed participant identity cluster to a durable person profile.",
		closedObject(map[string]*jsonschema.Schema{
			"participant_id": safeIDSchema("Observed participant ID to promote"),
		}, "participant_id"),
		outputSchemaFor[store.Person](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.promotePerson(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func updatePersonNotesDefinition(_ *handlers) toolDefinition {
	mode := stringSchema("Update mode: append atomically or replace with compare-and-swap", "append", "replace")
	mode.Default = []byte(`"append"`)
	definition := writeDefinition(
		ToolUpdatePersonNotes,
		"Update private user-curated Notes for a durable person. Observed contacts must be promoted explicitly first.",
		closedObject(map[string]*jsonschema.Schema{
			"person_id": safeIDSchema("Durable person profile ID"),
			"text":      stringSchema("Non-blank note text; multiline UTF-8 is preserved"),
			"mode":      mode,
			"expected_value_id": safeIDSchema(
				"Required for replace when Notes exist; forbidden for append or first-value creation"),
		}, "person_id", "text"),
		outputSchemaFor[updatePersonNotesResponse](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.updatePersonNotes(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func (h *handlers) searchPeople(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	queryText, _ := args["query"].(string)
	cursor, _ := args["cursor"].(string)
	limit := limitArg(args, "limit", defaultSearchLimit)
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	page, err := h.peopleBackend.Search(ctx, peoplebrowser.SearchRequest{
		Query: queryText, Limit: limit, Cursor: cursor,
	})
	if err != nil {
		return nil, newInternalError("search people", err)
	}
	if page == nil {
		return nil, newInternalError("search people", errors.New("empty response"))
	}
	rows := page.Rows
	if rows == nil {
		rows = []query.PersonSummary{}
	}
	return jsonResult(searchPeopleResponse{
		Rows: rows, TotalCount: page.TotalCount,
		NextCursor: page.NextCursor, CacheRevision: page.CacheRevision,
	})
}

func (h *handlers) getPersonNotes(ctx context.Context, req toolRequest) (*toolResult, error) {
	personID, err := requiredPeopleID(req.GetArguments(), "person_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	attributes, err := h.peopleBackend.ListAttributes(ctx, personID)
	if err != nil {
		if personProfileMissing(err) {
			return toolErrorResult(explicitPromotionMessage), nil
		}
		return nil, newInternalError("get person notes", err)
	}
	response := getPersonNotesResponse{PersonID: personID}
	if current := currentNotesValue(attributes); current != nil {
		if current.Value.Text == nil {
			return nil, newInternalError("get person notes", errors.New("notes value is not text"))
		}
		response.Exists = true
		response.Text = *current.Value.Text
		response.ValueID = current.ID
		updatedAt := current.CreatedAt
		response.UpdatedAt = &updatedAt
	}
	return jsonResult(response)
}

func (h *handlers) promotePerson(ctx context.Context, req toolRequest) (*toolResult, error) {
	participantID, err := requiredPeopleID(req.GetArguments(), "participant_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	person, err := h.peopleBackend.Promote(ctx, participantID)
	if err != nil {
		return nil, newInternalError("promote person", err)
	}
	if person == nil {
		return nil, newInternalError("promote person", errors.New("empty response"))
	}
	return jsonResult(person)
}

func (h *handlers) updatePersonNotes(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	personID, err := requiredPeopleID(args, "person_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return toolErrorResult("text must not be blank"), nil
	}
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "append"
	}
	expectedValueID, hasExpectedValueID, err := optionalPeopleID(args, "expected_value_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}

	var write *store.PersonAttributeWrite
	switch mode {
	case "append":
		if hasExpectedValueID {
			return toolErrorResult("expected_value_id is not allowed with append"), nil
		}
		if _, err = h.peopleBackend.ListAttributes(ctx, personID); err != nil {
			break
		}
		write, err = h.peopleBackend.AppendNote(ctx, peoplebrowser.AppendNoteRequest{
			PersonID: personID, Text: text, Actor: "mcp",
		})
	case "replace":
		attributes, listErr := h.peopleBackend.ListAttributes(ctx, personID)
		if listErr != nil {
			err = listErr
			break
		}
		current := currentNotesValue(attributes)
		switch {
		case current != nil && !hasExpectedValueID:
			return toolErrorResult(
				"expected_value_id is required when replacing existing notes"), nil
		case current == nil && hasExpectedValueID:
			return toolErrorResult(
				"expected_value_id must be omitted when creating the first notes value"), nil
		}
		var expected *int64
		if hasExpectedValueID {
			expected = &expectedValueID
		}
		write, err = h.peopleBackend.SetAttribute(ctx, peoplebrowser.SetAttributeRequest{
			PersonID: personID, Slug: store.AttributeSlugNotes,
			Value:           store.AttributeValue{Type: store.AttributeValueText, Text: &text},
			ExpectedValueID: expected,
		})
	default:
		return toolErrorResult("mode must be append or replace"), nil
	}
	if err != nil {
		if personProfileMissing(err) {
			return toolErrorResult(explicitPromotionMessage), nil
		}
		var stale peoplebrowser.StaleValueError
		if errors.As(err, &stale) {
			return toolErrorResult(fmt.Sprintf(
				"notes changed; reload and retry with expected_value_id %d",
				stale.CurrentValueID,
			)), nil
		}
		return nil, newInternalError("update person notes", err)
	}
	if write == nil {
		return nil, newInternalError("update person notes", errors.New("empty response"))
	}
	return jsonResult(updatePersonNotesResponse{
		Current: write.Value, Superseded: write.Superseded,
	})
}

func requiredPeopleID(args map[string]any, key string) (int64, error) {
	value, err := positiveInt64Arg(args, key)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func optionalPeopleID(args map[string]any, key string) (int64, bool, error) {
	if _, ok := args[key]; !ok {
		return 0, false, nil
	}
	value, err := positiveInt64Arg(args, key)
	return value, true, err
}

func currentNotesValue(attributes *peoplebrowser.Attributes) *store.PersonAttributeValue {
	if attributes == nil {
		return nil
	}
	for i := range attributes.Groups {
		group := &attributes.Groups[i]
		if group.Definition.Slug == store.AttributeSlugNotes && len(group.Current) > 0 {
			return &group.Current[0]
		}
	}
	return nil
}

const explicitPromotionMessage = "person profile not found; invoke promote_person with the observed participant_id before updating notes"

func personProfileMissing(err error) bool {
	var coded daemonAPIErrorCoder
	return errors.As(err, &coded) && coded.APIErrorCode() == "person_profile_not_found"
}
