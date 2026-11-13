package cmd

import (
	"net/http"

	"go.kenn.io/msgvault/internal/apiprotocol"
)

// Only advertise collection discovery when the selected daemon reports the
// exact native route. Older daemons do not advertise this capability.
func mcpCollectionScopesAvailable(capabilities *apiprotocol.MCPCapabilities) bool {
	return mcpHasRoute(capabilities, "listCLICollections", http.MethodGet, "/api/v1/cli/collections")
}
