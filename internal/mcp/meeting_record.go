package mcp

import (
	"context"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

const (
	ToolGetMeeting          = "get_meeting"
	ToolListPersonMeetings  = "list_person_meetings"
	ToolSuggestMeetingLinks = "suggest_meeting_links"
)

// MeetingRecordsBackend forwards reads with the caller's daemon credential.
type MeetingRecordsBackend interface {
	GetMeeting(ctx context.Context, ref string) (store.MeetingRecord, error)
	ListPersonMeetings(ctx context.Context, ref string, query store.PersonMeetingsQuery) (daemonclient.PersonMeetingsResponse, error)
	SuggestMeetingLinks(ctx context.Context, ref string, limit int) ([]store.MeetingSuggestion, error)
}

type meetingLinkSuggestionsResponse struct {
	Items []store.MeetingSuggestion `json:"items"`
}

func getMeetingDefinition() toolDefinition {
	d := readDefinition(ToolGetMeeting, "Read an archived meeting occurrence with permitted notes, transcript packets, actions, recordings and previous evidence. Requires message.read on each source. Returns at most 10 linked items, with full packets for the first 3. Check packet coverage and content_omitted; use each item's follow_up to read its message.", closedObject(map[string]*jsonschema.Schema{"ref": stringSchema("evt:<id>[@<original-key>] or msg:<id>")}, "ref"), outputSchemaFor[store.MeetingRecord](), (*handlers).getMeeting)
	d.availability = func(c catalogCapabilities) bool { return c.meetingRecords }
	return d
}
func listPersonMeetingsDefinition() toolDefinition {
	d := readDefinition(ToolListPersonMeetings, "List scoped invitation and transcript evidence for a person in chronological order. Requires message.read. Defaults to the past 365 days; positive windows at most 400 days. Continue with the same ref and cursor; omitted dates reuse the first page's window. Unsupported recurrence and query_too_broad return errors rather than partial history.", closedObject(map[string]*jsonschema.Schema{
		"ref":  stringSchema("Numeric person ID, person:<id>, or participant:<id>"),
		"from": meetingTimestampSchema("Inclusive RFC3339 start (optional)"), "to": meetingTimestampSchema("Exclusive RFC3339 end (optional)"),
		"limit": boundedIntegerWithDefault("Maximum meetings", 1, 500, 100), "cursor": stringSchema("Opaque continuation cursor from the same query"),
	}, "ref"), outputSchemaFor[daemonclient.PersonMeetingsResponse](), (*handlers).listPersonMeetings)
	d.availability = func(c catalogCapabilities) bool { return c.meetingRecords }
	return d
}
func suggestMeetingLinksDefinition() toolDefinition {
	d := readDefinition(ToolSuggestMeetingLinks, "Suggest possible archive links for an occurrence or meeting note. Requires message.read on each source. Suggestions do not change links; owners make link decisions through the CLI or HTTP API.", closedObject(map[string]*jsonschema.Schema{"ref": stringSchema("evt:<id>[@<original-key>] or msg:<id>"), "limit": boundedIntegerWithDefault("Maximum suggestions", 1, 100, 10)}, "ref"), outputSchemaFor[meetingLinkSuggestionsResponse](), (*handlers).suggestMeetingLinks)
	d.availability = func(c catalogCapabilities) bool { return c.meetingRecords }
	return d
}
func (h *handlers) getMeeting(ctx context.Context, req toolRequest) (*toolResult, error) {
	ref, _ := req.GetArguments()["ref"].(string)
	if err := store.ValidateMeetingRecordRef(ref); err != nil {
		return toolErrorResult(err.Error()), nil
	}
	record, err := h.meetingRecords.GetMeeting(ctx, ref)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(record)
}
func (h *handlers) listPersonMeetings(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	ref, _ := args["ref"].(string)
	if _, _, err := store.ParsePersonMeetingRef(ref); err != nil {
		return toolErrorResult(err.Error()), nil
	}
	from, err := meetingTimestampArg(args, "from")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	to, err := meetingTimestampArg(args, "to")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	q := store.PersonMeetingsQuery{}
	if from != nil {
		q.From = *from
	}
	if to != nil {
		q.To = *to
	}
	if from != nil && to != nil && (!to.After(*from) || to.Sub(*from) > 400*24*time.Hour) {
		return toolErrorResult("window must be positive and at most 400 days"), nil
	}
	q.Cursor, _ = args["cursor"].(string)
	if _, ok := args["limit"]; ok {
		limit, err := positiveInt64Arg(args, "limit")
		if err != nil {
			return toolErrorResult(err.Error()), nil
		}
		if limit > 500 {
			return toolErrorResult("limit must be between 1 and 500"), nil
		}
		q.Limit = int(limit)
	}
	page, err := h.meetingRecords.ListPersonMeetings(ctx, ref, q)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(page)
}
func (h *handlers) suggestMeetingLinks(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	ref, _ := args["ref"].(string)
	if err := store.ValidateMeetingRecordRef(ref); err != nil {
		return toolErrorResult(err.Error()), nil
	}
	limit := 10
	if _, ok := args["limit"]; ok {
		value, err := positiveInt64Arg(args, "limit")
		if err != nil {
			return toolErrorResult(err.Error()), nil
		}
		if value > 100 {
			return toolErrorResult("limit must be between 1 and 100"), nil
		}
		limit = int(value)
	}
	items, err := h.meetingRecords.SuggestMeetingLinks(ctx, ref, limit)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(meetingLinkSuggestionsResponse{Items: items})
}
