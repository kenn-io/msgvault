package cmd

import (
	"net/http"

	"go.kenn.io/msgvault/internal/apiprotocol"
)

// Only advertise collection discovery when the selected daemon reports the
// exact native route. Older daemons do not advertise this capability.
func mcpCollectionScopesAvailable(capabilities *apiprotocol.MCPCapabilities) bool {
	if capabilities == nil {
		return false
	}
	for _, route := range capabilities.Routes {
		if route.OperationID == "listCLICollections" && route.Method == http.MethodGet && route.Path == "/api/v1/cli/collections" {
			return true
		}
	}
	return false
}
