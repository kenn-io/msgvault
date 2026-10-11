package apiprotocol

// MCPCapabilities describes implementation contracts, never configured accounts
// or grant contents. Route presence comes from production registration.
type MCPCapabilities struct {
	Version int                  `json:"version"`
	Routes  []MCPRouteDescriptor `json:"routes"`
}

type MCPRouteDescriptor struct {
	OperationID       string   `json:"operation_id"`
	Method            string   `json:"method"`
	Path              string   `json:"path"`
	QueryParameters   []string `json:"query_parameters"`
	RequestProperties []string `json:"request_properties"`
}
