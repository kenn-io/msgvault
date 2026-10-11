package api

import "net/http"

// Remote client response limits keep one read-only key from pulling unbounded data.
const (
	remoteMessageBytes    int64 = 64 << 20
	remoteAttachmentBytes int64 = 1 << 30
	remoteSearchLimit           = 500
)

// remoteClientOperationAllowed is an exact-match allowlist of operation IDs for read-only remote clients.
func remoteClientOperationAllowed(operationID string, collectionsWrite bool) bool {
	switch operationID {
	case "getHealth", "getCLIStats", "searchCLI", "listCLIAccounts", "getCLICacheStats",
		"getCLIMessage", "getCLIMessageOriginal", "getCLIMessageThread", "getCLIMessageRaw",
		"getCLIAttachment", "listCLICollections", "getCLICollection", "listCLIIdentities":
		return true
	case "createCLICollection", "addCLICollectionSources", "removeCLICollectionSources", "deleteCLICollection":
		return collectionsWrite
	}
	return false
}

func (s *Server) remoteClientRequest(r *http.Request) bool {
	return s.requestAuthentication(r).Mode == AuthModeRemoteClient
}
