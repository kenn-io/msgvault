package apiprotocol

import (
	"net/http"
	"slices"
)

// MCPCapabilities describes registered contracts admitted for this caller.
// Source authority and native provider support are checked again per call.
type MCPCapabilities struct {
	Version         int                  `json:"version"`
	Delegated       bool                 `json:"delegated"`
	Routes          []MCPRouteDescriptor `json:"routes"`
	InboxOperations []string             `json:"inbox_operations"`
}

type MCPRouteDescriptor struct {
	OperationID       string   `json:"operation_id"`
	Method            string   `json:"method"`
	Path              string   `json:"path"`
	QueryParameters   []string `json:"query_parameters"`
	RequestProperties []string `json:"request_properties"`
}

// HasInboxContract requires the registered signed control route and fields.
func (c MCPCapabilities) HasInboxContract() bool {
	if c.Version != 1 {
		return false
	}
	for _, route := range c.Routes {
		if route.OperationID != "controlInbox" || route.Method != http.MethodPost || route.Path != "/api/v1/inbox/control" {
			continue
		}
		for _, name := range []string{"operation", "target", "source", "expected", "preview_token", "idempotency_key", "receipt_id", "dry_run", "tags", "destination", "origin_folder"} {
			if !slices.Contains(route.RequestProperties, name) {
				return false
			}
		}
		return true
	}
	return false
}
