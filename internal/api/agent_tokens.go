package api

import (
	"encoding/json"
	"net/http"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
)

// agentTokenIssueRequest is the body for POST /agent-tokens.
type agentTokenIssueRequest struct {
	Label       string   `json:"label"`
	LifetimeStr string   `json:"lifetime,omitempty"` // e.g. "24h"; omit for default
	Permissions []string `json:"permissions"`
	SourceIDs   []int64  `json:"source_ids"`
}

// agentTokenIssueResponse is the 201 body returned by issueAgentToken.
// The secret is returned exactly once.
type agentTokenIssueResponse struct {
	ID          string                 `json:"id"`
	Secret      string                 `json:"secret"`
	Label       string                 `json:"label"`
	Permissions []string               `json:"permissions"`
	Sources     []agentTokenSourceView `json:"sources"`
	CreatedAt   time.Time              `json:"created_at"`
	ExpiresAt   time.Time              `json:"expires_at"`
	DaemonURL   string                 `json:"daemon_url"`
}

// agentTokenView is the list/revoke-safe view of a grant: no secret or digest.
type agentTokenView struct {
	ID          string                 `json:"id"`
	Label       string                 `json:"label"`
	Permissions []string               `json:"permissions"`
	Sources     []agentTokenSourceView `json:"sources"`
	CreatedAt   time.Time              `json:"created_at"`
	ExpiresAt   time.Time              `json:"expires_at"`
}

type agentTokenSourceView struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
}

type agentTokenListResponse struct {
	Tokens []agentTokenView `json:"tokens"`
}

func grantToView(g agentgrant.Grant) agentTokenView {
	perms := make([]string, len(g.Permissions))
	for i, p := range g.Permissions {
		perms[i] = string(p)
	}
	sources := make([]agentTokenSourceView, len(g.Sources))
	for i, s := range g.Sources {
		sources[i] = agentTokenSourceView{ID: s.ID, Type: s.Type, Identifier: s.Identifier}
	}
	return agentTokenView{
		ID:          g.ID,
		Label:       g.Label,
		Permissions: perms,
		Sources:     sources,
		CreatedAt:   g.CreatedAt,
		ExpiresAt:   g.ExpiresAt,
	}
}

// ownerAPIKeyPresented returns true when the request carries the owner API key
// via X-Api-Key or Authorization Bearer. Session and loopback-only modes are
// not sufficient for agent-token management.
func (s *Server) ownerAPIKeyPresented(r *http.Request) bool {
	if s.cfg.Server.APIKey == "" {
		return false
	}
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		authHeader = r.Header.Get("X-Api-Key")
	}
	if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
		authHeader = authHeader[7:]
	}
	return constantTimeAPIKeyEqual(authHeader, s.cfg.Server.APIKey)
}

// handleIssueAgentToken issues a new restricted agent grant.
// Requires owner API key.
func (s *Server) handleIssueAgentToken(w http.ResponseWriter, r *http.Request) {
	if !s.apiRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or missing API key")
		return
	}
	if !s.ownerAPIKeyPresented(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Agent token management requires the owner API key")
		return
	}
	if s.agentGrants == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_access_disabled", "Agent access is not enabled")
		return
	}

	var req agentTokenIssueRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}

	if req.Label == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "label is required")
		return
	}
	if len(req.Permissions) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "permissions must not be empty")
		return
	}
	if len(req.SourceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "source_ids must not be empty")
		return
	}

	// Validate and resolve permissions
	perms := make([]agentgrant.Permission, 0, len(req.Permissions))
	for _, ps := range req.Permissions {
		p, ok := agentgrant.KnownPermission(ps)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_permission", "unknown permission: "+ps)
			return
		}
		perms = append(perms, p)
	}

	// Resolve source IDs to live SourceRefs
	resolver, ok := s.store.(agentGrantSourceResolver)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "store does not support source resolution")
		return
	}

	sources := make([]agentgrant.SourceRef, 0, len(req.SourceIDs))
	for _, id := range req.SourceIDs {
		src, err := resolver.GetSourceByIDContext(r.Context(), id)
		if err != nil || src == nil {
			writeError(w, http.StatusBadRequest, "invalid_source", "source not found")
			return
		}
		sources = append(sources, agentgrant.SourceRef{
			ID:         src.ID,
			Type:       src.SourceType,
			Identifier: src.Identifier,
		})
	}

	// Parse lifetime
	var lifetime time.Duration
	if req.LifetimeStr != "" {
		var err error
		lifetime, err = time.ParseDuration(req.LifetimeStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_lifetime", "invalid duration: "+req.LifetimeStr)
			return
		}
	}

	_, secret, g, err := s.agentGrants.Issue(req.Label, perms, sources, lifetime)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Build source views
	srcViews := make([]agentTokenSourceView, len(g.Sources))
	for i, src := range g.Sources {
		srcViews[i] = agentTokenSourceView{ID: src.ID, Type: src.Type, Identifier: src.Identifier}
	}

	// Build permission strings
	permStrs := make([]string, len(g.Permissions))
	for i, p := range g.Permissions {
		permStrs[i] = string(p)
	}

	// Derive daemon URL from request Host
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	daemonURL := scheme + "://" + r.Host

	writeJSON(w, http.StatusCreated, agentTokenIssueResponse{
		ID:          g.ID,
		Secret:      secret,
		Label:       g.Label,
		Permissions: permStrs,
		Sources:     srcViews,
		CreatedAt:   g.CreatedAt,
		ExpiresAt:   g.ExpiresAt,
		DaemonURL:   daemonURL,
	})
}

// handleListAgentTokens returns grant metadata; never a secret or digest.
func (s *Server) handleListAgentTokens(w http.ResponseWriter, r *http.Request) {
	if !s.apiRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or missing API key")
		return
	}
	if !s.ownerAPIKeyPresented(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Agent token management requires the owner API key")
		return
	}
	if s.agentGrants == nil {
		writeJSON(w, http.StatusOK, agentTokenListResponse{Tokens: []agentTokenView{}})
		return
	}

	grants := s.agentGrants.List()
	views := make([]agentTokenView, 0, len(grants))
	for _, g := range grants {
		views = append(views, grantToView(g))
	}
	writeJSON(w, http.StatusOK, agentTokenListResponse{Tokens: views})
}

// handleRevokeAgentToken removes a grant by ID.
// Returns 204 regardless of whether the ID existed (to prevent enumeration).
func (s *Server) handleRevokeAgentToken(w http.ResponseWriter, r *http.Request) {
	if !s.apiRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or missing API key")
		return
	}
	if !s.ownerAPIKeyPresented(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Agent token management requires the owner API key")
		return
	}

	id := r.PathValue("id")
	if s.agentGrants != nil && id != "" {
		s.agentGrants.Revoke(id)
	}
	w.WriteHeader(http.StatusNoContent)
}
