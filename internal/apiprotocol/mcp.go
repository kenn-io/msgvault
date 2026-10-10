package apiprotocol

import (
	"net/http"
	"slices"
)

// MCPCapabilities describes registered implementation contracts admitted for
// this caller. It contains no source identities, credentials or provider data.
type MCPCapabilities struct {
	Version            int                    `json:"version"`
	Delegated          bool                   `json:"delegated"`
	Routes             []MCPRouteDescriptor   `json:"routes"`
	Commands           []MCPCommandDescriptor `json:"commands"`
	InboxOperations    []string               `json:"inbox_operations"`
	IdentityOperations []string               `json:"identity_operations"`
}

type MCPRouteDescriptor struct {
	OperationID       string   `json:"operation_id"`
	Method            string   `json:"method"`
	Path              string   `json:"path"`
	QueryParameters   []string `json:"query_parameters"`
	RequestProperties []string `json:"request_properties"`
}

type MCPCommandDescriptor struct {
	Name      string   `json:"name"`
	Flags     []string `json:"flags"`
	Delegated bool     `json:"delegated"`
}

// HasInboxContract checks the exact route and fields required by signed control.
// Native support and per-item authority are still checked by the daemon.
func (c MCPCapabilities) HasInboxContract() bool {
	if c.Version != 1 {
		return false
	}
	for _, route := range c.Routes {
		if route.OperationID != "controlInbox" || route.Method != http.MethodPost || route.Path != "/api/v1/inbox/control" {
			continue
		}
		for _, field := range []string{"operation", "target", "source", "expected", "preview_token", "idempotency_key", "receipt_id", "dry_run", "tags", "destination", "origin_folder"} {
			if !slices.Contains(route.RequestProperties, field) {
				return false
			}
		}
		return true
	}
	return false
}

// HasInboxCandidatesContract requires the exact metadata route and query fields.
func (c MCPCapabilities) HasInboxCandidatesContract() bool {
	if c.Version != 1 {
		return false
	}
	for _, route := range c.Routes {
		if route.OperationID != "listInboxCandidates" || route.Method != http.MethodGet || route.Path != "/api/v1/inbox/candidates" {
			continue
		}
		for _, field := range []string{"source_id", "source_type", "source_identifier", "account_id", "scope", "limit", "cursor"} {
			if !slices.Contains(route.QueryParameters, field) {
				return false
			}
		}
		return true
	}
	return false
}

// HasInboxContextContract requires the exact scoped content route and fields.
func (c MCPCapabilities) HasInboxContextContract() bool {
	if c.Version != 1 {
		return false
	}
	for _, route := range c.Routes {
		if route.OperationID != "getInboxContext" || route.Method != http.MethodPost || route.Path != "/api/v1/inbox/context" {
			continue
		}
		for _, field := range []string{"target", "message_id", "max_bytes"} {
			if !slices.Contains(route.RequestProperties, field) {
				return false
			}
		}
		return true
	}
	return false
}

// HasIdentityPreviewContract admits only the native scoped preview schema.
func (c MCPCapabilities) HasIdentityPreviewContract() bool {
	return c.hasIdentityContract("previewIdentityOperation", http.MethodPost, "preview", []string{"operation", "target"})
}
func (c MCPCapabilities) HasIdentityApplyContract() bool {
	return c.hasIdentityContract("applyIdentityOperation", http.MethodPost, "apply", []string{"operation", "target", "expected_fingerprint", "preview_token", "idempotency_key"})
}
func (c MCPCapabilities) HasIdentityReceiptContract() bool {
	return c.hasIdentityContract("getIdentityOperationReceipt", http.MethodGet, "receipt", []string{"idempotency_key", "receipt_id", "principal"})
}
func (c MCPCapabilities) hasIdentityContract(operation, method, endpoint string, fields []string) bool {
	if c.Version != 1 {
		return false
	}
	for _, route := range c.Routes {
		if route.OperationID != operation || route.Method != method || route.Path != "/api/v1/identity/operations/"+endpoint {
			continue
		}
		properties := route.RequestProperties
		if method == http.MethodGet {
			properties = route.QueryParameters
		}
		if slices.ContainsFunc(fields, func(field string) bool { return !slices.Contains(properties, field) }) {
			continue
		}
		return true
	}
	return false
}
