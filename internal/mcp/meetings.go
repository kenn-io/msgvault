package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	meetingDefaultActionLimit = 50
	meetingMaxActionLimit     = 200
	meetingDefaultMaxBytes    = 131072
	meetingMinMaxBytes        = 4096
	meetingMaxMaxBytes        = 1048576
)

// MeetingBackend is the daemon-first boundary shared by the CLI and MCP
// meeting surfaces. Generated request DTOs preserve the public wire contract;
// stable meetingcontent results preserve nil and empty response semantics.
type MeetingBackend interface {
	GetMeetingContext(ctx context.Context, request generated.GetMeetingContextBody) (*meetingcontent.PacketResult, error)
	ListMeetingActionItems(ctx context.Context, request generated.ListMeetingActionItemsBody) (*meetingcontent.ActionsPage, error)
	GetMeetingMetrics(ctx context.Context, request generated.GetMeetingMetricsBody) (*meetingcontent.Metrics, error)
}

func (h *handlers) getMeetingContext(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	messageIDs, err := positiveInt64ArrayArg(args, "message_ids")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if len(messageIDs) == 0 {
		return toolErrorResult("message_ids must contain at least one meeting message ID"), nil
	}
	body := generated.GetMeetingContextBody{MessageIds: &messageIDs}
	if value, ok := args["format"].(string); ok && value != "" {
		format := generated.MeetingContextRequestFormat(value)
		body.Format = &format
	}
	if value, ok := args["include_transcript"].(bool); ok {
		body.IncludeTranscript = &value
	}
	if raw, found := args["max_bytes"]; found {
		value, ok := raw.(float64)
		if !ok || value != math.Trunc(value) || value < meetingMinMaxBytes || value > meetingMaxMaxBytes {
			return toolErrorResult("max_bytes must be an integer between 4096 and 1048576"), nil
		}
		maxBytes := int64(value)
		body.MaxBytes = &maxBytes
	}
	result, err := h.meetings.GetMeetingContext(ctx, body)
	if err != nil {
		return nil, newInternalError("get meeting context", err)
	}
	return jsonResult(result)
}

func (h *handlers) listMeetingActionItems(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	scope, err := meetingScopeFromArgs(args)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	body := generated.ListMeetingActionItemsBody{Scope: scope}
	if value, ok := args["assignee_email"].(string); ok && value != "" {
		body.AssigneeEmail = &value
	}
	if value, ok := args["status"].(string); ok && value != "" {
		status := generated.MeetingActionsRequestStatus(value)
		body.Status = &status
	}
	if value, ok := args[toolArgQuery].(string); ok && value != "" {
		if !validateMeetingTextArg(value, 256) {
			return toolErrorResult("query must be at most 256 characters"), nil
		}
		body.Query = &value
	}
	if raw, found := args[toolArgLimit]; found {
		value, ok := raw.(float64)
		if !ok || value != math.Trunc(value) || value < 1 || value > meetingMaxActionLimit {
			return toolErrorResult("limit must be an integer between 1 and 200"), nil
		}
		limit := int64(value)
		body.Limit = &limit
	}
	if value, ok := args[toolArgCursor].(string); ok && value != "" {
		body.Cursor = &value
	}
	result, err := h.meetings.ListMeetingActionItems(ctx, body)
	if err != nil {
		return nil, newInternalError("list meeting action items", err)
	}
	return jsonResult(result)
}

func (h *handlers) getMeetingMetrics(ctx context.Context, req toolRequest) (*toolResult, error) {
	scope, err := meetingScopeFromArgs(req.GetArguments())
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	result, err := h.meetings.GetMeetingMetrics(ctx, generated.GetMeetingMetricsBody{Scope: scope})
	if err != nil {
		return nil, newInternalError("get meeting metrics", err)
	}
	return jsonResult(result)
}

func meetingScopeFromArgs(args map[string]any) (*generated.MeetingScopeRequest, error) {
	scope := &generated.MeetingScopeRequest{}
	present := false
	if _, found := args["message_ids"]; found {
		ids, err := positiveInt64ArrayArg(args, "message_ids")
		if err != nil {
			return nil, err
		}
		scope.MessageIds = &ids
		present = true
	}
	for key, destination := range map[string]*[]int64{
		"source_ids": &scope.SourceIds, "participant_ids": &scope.ParticipantIds,
	} {
		if _, found := args[key]; !found {
			continue
		}
		ids, err := positiveInt64ArrayArg(args, key)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("%s must contain at least one ID", key)
		}
		*destination = ids
		present = true
	}
	if _, found := args["domains"]; found {
		domains, err := stringArrayArg(args, "domains")
		if err != nil {
			return nil, err
		}
		if len(domains) == 0 {
			return nil, errors.New("domains must contain at least one domain")
		}
		scope.Domains = domains
		present = true
	}
	personID, err := positiveInt64Arg(args, toolArgPersonID)
	if err != nil {
		return nil, err
	}
	participantID, err := positiveInt64Arg(args, toolArgParticipantID)
	if err != nil {
		return nil, err
	}
	if personID != 0 {
		scope.PersonID = &personID
		present = true
	}
	if participantID != 0 {
		scope.ParticipantID = &participantID
		present = true
	}
	if len(scope.ParticipantIds) > 0 && (scope.PersonID != nil || scope.ParticipantID != nil) {
		return nil, errors.New("participant_ids is mutually exclusive with person_id and participant_id")
	}
	if scope.PersonID != nil && scope.ParticipantID != nil {
		return nil, errors.New("person_id and participant_id are mutually exclusive")
	}
	after, err := meetingTimestampArg(args, toolArgAfter)
	if err != nil {
		return nil, err
	}
	before, err := meetingTimestampArg(args, toolArgBefore)
	if err != nil {
		return nil, err
	}
	if after != nil || before != nil {
		if after != nil && before != nil && !after.Before(*before) {
			return nil, errors.New("after must be before before")
		}
		scope.After, scope.Before = after, before
		present = true
	}
	if value, ok := args["deletion"].(string); ok && value != "" {
		deletion := generated.MeetingScopeRequestDeletion(value)
		scope.Deletion = &deletion
		present = true
	}
	if !present {
		return nil, nil //nolint:nilnil // An absent optional scope means the unrestricted meeting population.
	}
	return scope, nil
}

func getMeetingContextDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolGetMeetingContext,
		"Render deterministic, size-bounded context for explicit archived meeting message IDs.",
		closedObject(map[string]*jsonschema.Schema{
			"message_ids":        meetingIDArraySchema("Meeting message IDs", true),
			"format":             stringSchema("Context content format", "json", "markdown"),
			"include_transcript": booleanSchema("Include archived transcript evidence"),
			"max_bytes":          boundedIntegerWithDefault("Maximum UTF-8 content bytes", meetingMinMaxBytes, meetingMaxMaxBytes, meetingDefaultMaxBytes),
		}, "message_ids"),
		outputSchemaFor[meetingcontent.PacketResult](),
		(*handlers).getMeetingContext,
	)
	definition.availability = meetingsAvailable
	return definition
}

func listMeetingActionItemsDefinition(_ *handlers) toolDefinition {
	properties := meetingScopeSchemaProperties()
	properties["assignee_email"] = stringSchema("Exact assignee email")
	properties["status"] = stringSchema("Normalized source status", "pending", "completed", "cancelled", "unknown")
	query := stringSchema("Literal case-insensitive title or description substring")
	maxQuery := 256
	query.MaxLength = &maxQuery
	properties[toolArgQuery] = query
	properties[toolArgLimit] = boundedIntegerWithDefault("Maximum action rows", 1, meetingMaxActionLimit, meetingDefaultActionLimit)
	properties[toolArgCursor] = stringSchema("Opaque continuation cursor")
	definition := readDefinition(
		ToolListMeetingActionItems,
		"List archived source-reported meeting action evidence with provenance and scope coverage.",
		closedObject(properties),
		outputSchemaFor[meetingcontent.ActionsPage](),
		(*handlers).listMeetingActionItems,
	)
	definition.availability = meetingsAvailable
	return definition
}

func getMeetingMetricsDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolGetMeetingMetrics,
		"Get archived meeting duration totals, evidence bases, coverage, and populated month buckets.",
		closedObject(meetingScopeSchemaProperties()),
		outputSchemaFor[meetingcontent.Metrics](),
		(*handlers).getMeetingMetrics,
	)
	definition.availability = meetingsAvailable
	return definition
}

func meetingScopeSchemaProperties() map[string]*jsonschema.Schema {
	return map[string]*jsonschema.Schema{
		"message_ids":     meetingIDArraySchema("Exact meeting message IDs; an explicit empty array matches none", false),
		"source_ids":      meetingNonEmptyIDArraySchema("Exact source IDs"),
		"participant_ids": meetingNonEmptyIDArraySchema("Exact observed participant IDs"),
		toolArgPersonID:   safeIDSchema("Durable person ID; mutually exclusive with participant IDs"),
		toolArgParticipantID: safeIDSchema(
			"Observed participant reference resolved through its durable person when bound; mutually exclusive with participant_ids",
		),
		"domains":     meetingStringArraySchema("Exact case-insensitive participant domains"),
		toolArgAfter:  meetingTimestampSchema("Meetings on or after this RFC3339 timestamp"),
		toolArgBefore: meetingTimestampSchema("Meetings before this RFC3339 timestamp"),
		"deletion":    stringSchema("Archived source deletion state", "any", "active", "deleted"),
	}
}

func meetingIDArraySchema(description string, requireOne bool) *jsonschema.Schema {
	maximum := 100
	schema := &jsonschema.Schema{
		Type: "array", Description: description, Items: safeIDSchema("Meeting message ID"), MaxItems: &maximum,
	}
	if requireOne {
		minimum := 1
		schema.MinItems = &minimum
	}
	return schema
}

func meetingNonEmptyIDArraySchema(description string) *jsonschema.Schema {
	schema := meetingIDArraySchema(description, true)
	schema.Items = safeIDSchema("ID")
	return schema
}

func meetingStringArraySchema(description string) *jsonschema.Schema {
	minimum, maximum := 1, 100
	return &jsonschema.Schema{
		Type: "array", Description: description, Items: stringSchema("Domain"),
		MinItems: &minimum, MaxItems: &maximum,
	}
}

func meetingTimestampSchema(description string) *jsonschema.Schema {
	schema := stringSchema(description)
	schema.Format = "date-time"
	return schema
}

func meetingTimestampArg(args map[string]any, key string) (*time.Time, error) {
	value, ok := args[key].(string)
	if !ok || value == "" {
		return nil, nil //nolint:nilnil // absent optional timestamp is unbounded
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s timestamp %q: expected RFC3339", key, value)
	}
	return &parsed, nil
}

func boundedIntegerWithDefault(description string, minimum, maximum, defaultValue int) *jsonschema.Schema {
	schema := boundedIntegerSchema(description, float64(minimum), float64(maximum))
	schema.Default = json.RawMessage(strconv.Itoa(defaultValue))
	return schema
}

func validateMeetingTextArg(value string, limit int) bool {
	return utf8.RuneCountInString(strings.TrimSpace(value)) <= limit
}
