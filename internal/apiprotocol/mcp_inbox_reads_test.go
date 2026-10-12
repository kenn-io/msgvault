package apiprotocol

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInboxReadContractsRequireRegisteredRouteAndFields(t *testing.T) {
	for _, tc := range []struct {
		name     string
		route    MCPRouteDescriptor
		admitted func(MCPCapabilities) bool
	}{
		{"candidates", MCPRouteDescriptor{OperationID: "listInboxCandidates", Method: http.MethodGet, Path: "/api/v1/inbox/candidates", QueryParameters: []string{"source_id", "source_type", "source_identifier", "account_id", "scope", "limit", "cursor"}}, MCPCapabilities.HasInboxCandidatesContract},
		{"context", MCPRouteDescriptor{OperationID: "getInboxContext", Method: http.MethodPost, Path: "/api/v1/inbox/context", RequestProperties: []string{"target", "message_id", "max_bytes"}}, MCPCapabilities.HasInboxContextContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			assertions.False(tc.admitted(MCPCapabilities{Version: 1}))
			assertions.True(tc.admitted(MCPCapabilities{Version: 1, Routes: []MCPRouteDescriptor{tc.route}}))
			assertions.False(tc.admitted(MCPCapabilities{Version: 2, Routes: []MCPRouteDescriptor{tc.route}}))
			for _, mutation := range []func(*MCPRouteDescriptor){
				func(r *MCPRouteDescriptor) { r.Path = "/api/v1/unsupported" },
				func(r *MCPRouteDescriptor) { r.Method = http.MethodDelete },
				func(r *MCPRouteDescriptor) { r.OperationID = "unsupported" },
			} {
				route := tc.route
				mutation(&route)
				assertions.False(tc.admitted(MCPCapabilities{Version: 1, Routes: []MCPRouteDescriptor{route}}))
			}
			fields := tc.route.QueryParameters
			if tc.name == "context" {
				fields = tc.route.RequestProperties
			}
			for i := range fields {
				route := tc.route
				remaining := append(append([]string{}, fields[:i]...), fields[i+1:]...)
				if tc.name == "context" {
					route.RequestProperties = remaining
				} else {
					route.QueryParameters = remaining
				}
				assertions.False(tc.admitted(MCPCapabilities{Version: 1, Routes: []MCPRouteDescriptor{route}}), "missing %s", fields[i])
			}
		})
	}
}
