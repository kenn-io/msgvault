package apiprotocol

import (
	"net/http"
	"strings"
)

const (
	ClientClassHeader       = "X-Msgvault-Client"
	ClientClassCLI          = "cli"
	DeduplicatePlanProtocol = "explicit-backfill-v1"
	// DaemonRuntimeTokenHeader is an HTTP header name, not a credential.
	// #nosec G101
	DaemonRuntimeTokenHeader = "X-Msgvault-Daemon-Token"
	// AgentTokenHeader carries a restricted agent grant secret. Header name, not a credential.
	// #nosec G101
	AgentTokenHeader = "X-Msgvault-Agent-Token"
	// AllowPendingCountsHeader opts an account list into reusing the last
	// virtual account catalog at once while a refresh of the same catalog runs.
	// The name predates stored message counts, which are never pending.
	AllowPendingCountsHeader = "X-Msgvault-Allow-Pending-Counts"
)

// BearerAuthorization returns a single explicit bearer credential. Duplicate
// Authorization values and empty credentials are rejected.
func BearerAuthorization(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, credential, found := strings.Cut(values[0], " ")
	return credential, found && credential != "" && strings.EqualFold(scheme, "Bearer")
}
