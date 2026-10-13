package apiprotocol

import (
	"net/http"
	"slices"
)

// MCPCapabilities describes registered contracts admitted for this caller.
// Resource authority and provider availability are checked again per call.
type MCPCapabilities struct {
	Version            int                  `json:"version"`
	Delegated          bool                 `json:"delegated"`
	Routes             []MCPRouteDescriptor `json:"routes"`
	IdentityOperations []string             `json:"identity_operations"`
}

type MCPRouteDescriptor struct {
	OperationID       string   `json:"operation_id"`
	Method            string   `json:"method"`
	Path              string   `json:"path"`
	QueryParameters   []string `json:"query_parameters"`
	RequestProperties []string `json:"request_properties"`
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
		if len(properties) != len(fields) || (method == http.MethodGet && len(route.RequestProperties) != 0) || (method != http.MethodGet && len(route.QueryParameters) != 0) || slices.ContainsFunc(fields, func(field string) bool { return !slices.Contains(properties, field) }) {
			continue
		}
		return true
	}
	return false
}
