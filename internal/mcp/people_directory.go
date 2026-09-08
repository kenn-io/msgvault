package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/store"
)

func listDirectoryPeopleDefinition(_ *handlers) toolDefinition {
	sort := stringSchema(
		"Directory order",
		store.DirectoryPeopleSortName,
		store.DirectoryPeopleSortLastContactDesc,
		store.DirectoryPeopleSortLastContactAsc,
	)
	sort.Default = json.RawMessage(`"last_contact_desc"`)
	definition := readDefinition(
		ToolListDirectoryPeople,
		"List durable people from the Directory, ordered by name or last contact. Search observed contacts separately with search_people.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgQuery:          stringSchema("Optional lexical query over durable person fields"),
			toolArgCursor:         stringSchema("Opaque cursor from the previous list_directory_people response"),
			toolArgLimit:          nonNegativeIntegerSchema("Maximum rows to return (default 50, max 100)", store.DefaultDirectoryPeopleLimit),
			"sort":                sort,
			"last_contact_after":  stringSchema("Inclusive lower bound for last contact: RFC3339 timestamp or YYYY-MM-DD (midnight UTC)"),
			"last_contact_before": stringSchema("Inclusive upper bound for last contact: RFC3339 timestamp or YYYY-MM-DD (midnight UTC)"),
			"contact_state":       stringSchema("Current contact state", "active", "inactive"),
			"category":            stringSchema("Current person category"),
			"organization":        stringSchema("Current organization"),
			"primary_channel":     stringSchema("Curated primary communication channel"),
		}),
		outputSchemaFor[store.DirectoryPeoplePage](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.listDirectoryPeople(ctx, req)
		},
	)
	definition.availability = directoryPeopleAvailable
	return definition
}

func (h *handlers) listDirectoryPeople(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	afterValue, hasAfter, err := parseDirectoryDate(args, "last_contact_after")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	beforeValue, hasBefore, err := parseDirectoryDate(args, "last_contact_before")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	var after, before *time.Time
	if hasAfter {
		after = &afterValue
	}
	if hasBefore {
		before = &beforeValue
	}

	query := store.DirectoryPeopleQuery{
		Query:             stringArgument(args, toolArgQuery),
		Cursor:            stringArgument(args, toolArgCursor),
		Limit:             limitArg(args, toolArgLimit, store.DefaultDirectoryPeopleLimit),
		Sort:              stringArgument(args, "sort"),
		LastContactAfter:  after,
		LastContactBefore: before,
		ContactState:      stringArgument(args, "contact_state"),
		Category:          stringArgument(args, "category"),
		Organization:      stringArgument(args, "organization"),
		PrimaryChannel:    stringArgument(args, "primary_channel"),
	}
	if query.Sort == "" {
		query.Sort = store.DirectoryPeopleSortLastContactDesc
	}

	if h.directoryBackend == nil {
		return nil, newInternalError("list directory people", errors.New("directory listing is unavailable"))
	}
	page, err := h.directoryBackend.ListDirectoryPeople(ctx, query)
	if err != nil {
		if result := directoryDaemonError(err); result != nil {
			return result, nil
		}
		return nil, newInternalError("list directory people", err)
	}
	if page == nil {
		return nil, newInternalError("list directory people", errors.New("empty response"))
	}
	return jsonResult(page)
}

func stringArgument(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func parseDirectoryDate(args map[string]any, key string) (time.Time, bool, error) {
	value := strings.TrimSpace(stringArgument(args, key))
	if value == "" {
		return time.Time{}, false, nil
	}
	layout := time.RFC3339
	if len(value) == len(time.DateOnly) {
		layout = time.DateOnly
	}
	parsed, err := time.Parse(layout, value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s must be RFC3339 or YYYY-MM-DD", key)
	}
	return parsed, true, nil
}

func directoryDaemonError(err error) *toolResult {
	var coded daemonAPIErrorCoder
	if !errors.As(err, &coded) {
		return nil
	}
	switch coded.APIErrorCode() {
	case "invalid_cursor":
		return toolErrorResult("invalid_cursor: directory cursor is invalid; restart the query")
	case "invalid_query":
		return toolErrorResult("invalid_query: directory query is invalid")
	case "directory_projection_stale":
		return toolErrorResult("directory_projection_stale: directory data is refreshing; retry shortly")
	default:
		return nil
	}
}
