package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/peoplebrowser"
)

func listRelationshipsDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolListRelationships,
		"List ranked observed relationships with optional durable keep-in-touch cadence context. Unsaved observed contacts remain visible without a cadence filter and are excluded when a cadence filter is requested.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgCursor:     stringSchema("Opaque cursor from the previous list_relationships response"),
			toolArgLimit:      nonNegativeIntegerSchema("Maximum rows to return (default 50, max 500)", 50),
			"show_all":        {Type: "boolean", Description: "Include inbound-only observed relationships"},
			"cadence":         stringSchema("Keep-in-touch cadence filter", "overdue", "due", "no_list", "unknown"),
			"due_within_days": {Type: "integer", Minimum: new(float64(0)), Maximum: new(float64(3660)), Description: "UTC days ahead, only with cadence=due (default 0)"},
		}),
		outputSchemaFor[peoplebrowser.RelationshipListPage](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.listRelationships(ctx, req)
		},
	)
	definition.availability = relationshipPeopleAvailable
	return definition
}

func (h *handlers) listRelationships(ctx context.Context, req toolRequest) (*toolResult, error) {
	lister, ok := h.peopleBackend.(peoplebrowser.RelationshipLister)
	if !ok {
		return toolErrorResult("relationship listing is unavailable"), nil
	}
	args := req.GetArguments()
	cadence := strings.ToLower(strings.TrimSpace(stringArgument(args, "cadence")))
	switch cadence {
	case "", "overdue", "due", "no_list", "unknown":
	default:
		return toolErrorResult("cadence must be overdue, due, no_list, or unknown"), nil
	}
	dueWithinDays, err := relationshipDueWithinDays(args, cadence)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	limit := limitArg(args, toolArgLimit, 50)
	if limit < 1 || limit > 500 {
		return toolErrorResult("limit must be between 1 and 500"), nil
	}
	showAll, _ := args["show_all"].(bool)
	page, err := lister.ListRelationships(ctx, peoplebrowser.RelationshipListRequest{
		Cadence: cadence, DueWithinDays: dueWithinDays, Cursor: stringArgument(args, toolArgCursor),
		Limit: limit, ShowAll: showAll,
	})
	if err != nil {
		if result := directoryDaemonError(err); result != nil {
			return result, nil
		}
		return nil, newInternalError("list relationships", err)
	}
	if page == nil {
		return nil, newInternalError("list relationships", fmt.Errorf("empty response"))
	}
	return jsonResult(page)
}

func relationshipDueWithinDays(args map[string]any, cadence string) (*int, error) {
	if _, present := args["due_within_days"]; !present {
		return nil, nil
	}
	if cadence != "due" {
		return nil, fmt.Errorf("due_within_days requires cadence=due and an integer from 0 to 3660")
	}
	days64, err := nonnegativeInt64Arg(args, "due_within_days")
	if err != nil || days64 > 3660 {
		return nil, fmt.Errorf("due_within_days requires cadence=due and an integer from 0 to 3660")
	}
	days := int(days64)
	return &days, nil
}
