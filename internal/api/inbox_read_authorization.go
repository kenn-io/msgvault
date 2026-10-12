package api

import (
	"net/http"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// Archived Inbox reads bind the source lifetime as well as its identity.
// General archive grants retain their existing cross-reimport matching rules.
func inboxReadAllowed(grant *agentgrant.Grant, permission agentgrant.Permission, source agentgrant.SourceRef) bool {
	if grant == nil || !grant.HasPermission(agentgrant.PermissionInboxRead) || !grant.HasPermission(permission) || (!grant.ExpiresAt.IsZero() && !time.Now().Before(grant.ExpiresAt)) {
		return false
	}
	return slices.ContainsFunc(grant.Sources, func(allowed agentgrant.SourceRef) bool {
		return allowed.ID > 0 && allowed.ID == source.ID && allowed.Type == source.Type && allowed.Identifier == source.Identifier
	})
}

func (s *Server) authorizeInboxRead(w http.ResponseWriter, r *http.Request, source agentgrant.SourceRef, permission agentgrant.Permission) bool {
	auth := s.classifyAPIRequestDirect(r)
	if auth.Mode == AuthModeRequired {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
		return false
	}
	if auth.Mode == AuthModeDelegated && !inboxReadAllowed(auth.Grant, permission, source) {
		writeError(w, http.StatusForbidden, "inbox_denied", inboxcontrol.ErrDenied.Error())
		return false
	}
	return true
}
