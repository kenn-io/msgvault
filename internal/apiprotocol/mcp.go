package apiprotocol

// MCPCapabilities describes implementation contracts, never configured accounts
// or grant contents. Route and flag presence come from production registration.
type MCPCapabilities struct {
	Version   int                    `json:"version"`
	Delegated bool                   `json:"delegated"`
	Routes    []MCPRouteDescriptor   `json:"routes"`
	Commands  []MCPCommandDescriptor `json:"commands"`
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
