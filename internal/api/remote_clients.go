package api

import "net/http"

// Remote client response limits keep one read-only key from pulling unbounded data.
const (
	remoteMessageBytes    int64 = 64 << 20
	remoteAttachmentBytes int64 = 1 << 30
	remoteSearchLimit           = 500
)

// remoteClientOperationAllowed retains the exact remote CLI contract.
func remoteClientOperationAllowed(operationID string, collectionsWrite bool) bool {
	policy := operationPolicy(operationID)
	return policy.RemoteRead || (collectionsWrite && policy.RemoteCollectionsWrite)
}

func (s *Server) remoteClientRequest(r *http.Request) bool {
	return s.requestAuthentication(r).Mode == AuthModeRemoteClient
}
