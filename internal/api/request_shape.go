package api

import (
	"net/http"
	"net/url"
	"slices"

	"go.kenn.io/msgvault/internal/search"
)

type requestQueryShapeKey struct{}

func queryShapeForRequest(r *http.Request) []string {
	if shape, ok := r.Context().Value(requestQueryShapeKey{}).([]string); ok {
		return shape
	}
	return requestQueryShape(r.URL.Query())
}

// requestQueryShape projects parsed criteria onto a fixed vocabulary. Never
// copy query tokens, unknown operator names, parameter values or parse errors
// here: each can contain private message content or addresses.
func requestQueryShape(params url.Values) []string {
	operators := make(map[string]bool)
	for _, key := range []string{"q", "search_query", "query"} {
		for _, text := range params[key] {
			q := search.Parse(text)
			for name, present := range map[string]bool{
				"text": len(q.TextTerms) > 0,
				"from": len(q.FromAddrs) > 0, "to": len(q.ToAddrs) > 0, "cc": len(q.CcAddrs) > 0, "bcc": len(q.BccAddrs) > 0,
				"subject": len(q.SubjectTerms) > 0, "label": len(q.Labels) > 0, "list": len(q.ListIDs) > 0,
				"account": len(q.AccountAddrs) > 0, "received": len(q.ReceivedAddrs) > 0,
				"has": q.HasAttachment != nil, "before": q.BeforeDate != nil, "after": q.AfterDate != nil,
				"larger": q.LargerThan != nil, "smaller": q.SmallerThan != nil, "in": len(q.AccountIDs) > 0,
				"conversation_id": len(q.ConversationIDs) > 0, "message_type": len(q.MessageTypes) > 0,
			} {
				if present {
					operators[name] = true
				}
			}
		}
	}
	for _, key := range []string{
		"sender", "sender_name", "recipient", "recipient_name", "domain", "label", "list_id",
		"source_id", "source_ids", "account", "collection", "conversation_id", "message_type",
		"after", "before", "time_period", "time_granularity", "attachments_only", "hide_deleted",
		"empty_targets", "view_type", "offset", "limit", "sort", "direction", "mode", "scope",
	} {
		if params.Has(key) {
			operators[key] = true
		}
	}
	shape := make([]string, 0, len(operators))
	for operator := range operators {
		shape = append(shape, operator)
	}
	slices.Sort(shape)
	return shape
}
