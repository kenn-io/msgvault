package apiprotocol

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentityContractsRequireExactNativeFields(t *testing.T) {
	preview := MCPRouteDescriptor{OperationID: "previewIdentityOperation", Method: http.MethodPost, Path: "/api/v1/identity/operations/preview", RequestProperties: []string{"operation", "target"}}
	apply := MCPRouteDescriptor{OperationID: "applyIdentityOperation", Method: http.MethodPost, Path: "/api/v1/identity/operations/apply", RequestProperties: []string{"operation", "target", "expected_fingerprint", "preview_token", "idempotency_key"}}
	receipt := MCPRouteDescriptor{OperationID: "getIdentityOperationReceipt", Method: http.MethodGet, Path: "/api/v1/identity/operations/receipt", QueryParameters: []string{"idempotency_key", "receipt_id", "principal"}}
	for _, tc := range []struct {
		name  string
		route MCPRouteDescriptor
		check func(MCPCapabilities) bool
	}{
		{"preview", preview, MCPCapabilities.HasIdentityPreviewContract},
		{"apply", apply, MCPCapabilities.HasIdentityApplyContract},
		{"receipt", receipt, MCPCapabilities.HasIdentityReceiptContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			c := MCPCapabilities{Version: 1, Routes: []MCPRouteDescriptor{tc.route}}
			assertions.True(tc.check(c))
			c.Version = 2
			assertions.False(tc.check(c))
			c.Version = 1
			c.Routes[0].Path = "/api/v1/unrelated"
			assertions.False(tc.check(c))
			c.Routes[0] = tc.route
			c.Routes[0].Method = http.MethodDelete
			assertions.False(tc.check(c))
			c.Routes[0] = tc.route
			if len(tc.route.RequestProperties) > 0 {
				c.Routes[0].RequestProperties = tc.route.RequestProperties[1:]
			} else {
				c.Routes[0].QueryParameters = tc.route.QueryParameters[1:]
			}
			assertions.False(tc.check(c))
		})
	}
}
