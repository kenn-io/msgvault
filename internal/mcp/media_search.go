package mcp

import (
	"context"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const ToolSearchMedia = "search_media"

type MediaSearcher interface {
	SearchMedia(ctx context.Context, opts daemonclient.MediaSearchOptions) (generated.MediaSearchResponse, error)
}

func searchMediaDefinition() toolDefinition {
	definition := readDefinition(ToolSearchMedia, "Find spoken words in recording transcripts as visible messages. Lexical search preserves every live occurrence, supplied or generated origin, optional timing, and partial coverage.", closedObject(map[string]*jsonschema.Schema{
		toolArgQuery:    stringSchema("Spoken words to find"),
		toolArgMode:     stringSchema("Search mode", "lexical"),
		toolArgPersonID: safeIDSchema("Optional durable person ID"),
		"directions":    {Type: schemaTypeArray, Items: stringSchema("Person relation", "from_person", "to_person", "group")},
		toolArgLimit:    boundedIntegerSchema("Maximum occurrences, default 20", 1, 100),
	}, toolArgQuery), outputSchemaFor[generated.MediaSearchResponse](), (*handlers).searchMedia)
	definition.availability = func(capabilities catalogCapabilities) bool { return capabilities.mediaSearch }
	return definition
}

func (h *handlers) searchMedia(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	query, _ := args[toolArgQuery].(string)
	if strings.TrimSpace(query) == "" {
		return toolErrorResult("query parameter is required"), nil
	}
	if h.mediaSearcher == nil {
		return toolErrorResult("media_search_unavailable: media search requires the daemon"), nil
	}
	personID, err := positiveInt64Arg(args, toolArgPersonID)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	directions, err := stringArrayArg(args, "directions")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	limit := int64(20)
	if _, exists := args[toolArgLimit]; exists {
		limit, err = positiveInt64Arg(args, toolArgLimit)
		if err != nil {
			return toolErrorResult(err.Error()), nil
		}
	}
	if int64(int(limit)) != limit {
		return toolErrorResult("limit is out of range"), nil
	}
	request := daemonclient.MediaSearchOptions{Query: query, PersonID: personID, Limit: int(limit), Mode: "lexical"}
	if mode, exists := args[toolArgMode].(string); exists {
		request.Mode = mode
	}
	for _, direction := range directions {
		request.Directions = append(request.Directions, personscope.Direction(direction))
	}
	response, err := h.mediaSearcher.SearchMedia(ctx, request)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(response)
}
