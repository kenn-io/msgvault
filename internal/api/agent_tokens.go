package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/store"
)

// agentTokensPath is the base path for all agent-token management routes.
// It is used by operationGateExemptPaths to exempt both the collection
// endpoint (POST/GET /api/v1/agent-tokens) and the member endpoint
// (DELETE /api/v1/agent-tokens/{id}) from the generic mutation gate.
// agentgrant.Registry is in-memory and process-scoped; revoke touches no
// archive state, so these routes belong with the session endpoints.
const agentTokensPath = "/api/v1/agent-tokens" //nolint:gosec // endpoint path, not a credential

// agentGrantSourceResolver is the narrow interface on s.store needed by
// handleIssueAgentToken to resolve a source ID to a SourceRef.
type agentGrantSourceResolver interface {
	GetSourceByIDContext(ctx context.Context, id int64) (*store.Source, error)
}

// agentTokenIssueRequest is the body for POST /agent-tokens.
type agentTokenIssueRequest struct {
	Label            string              `json:"label"`
	Permissions      []string            `json:"permissions"`
	SourceIDs        []int64             `json:"source_ids"`
	SenderSelections map[string][]string `json:"sender_selections,omitempty"`
}

// agentTokenIssueResponse is the 201 body returned by issueAgentToken.
// The secret is returned exactly once. Fields are inlined (not embedded) so the
// schema generator exposes every field, including id, to generated clients.
type agentTokenIssueResponse struct {
	ID          string                 `json:"id"`
	Label       string                 `json:"label"`
	Permissions []string               `json:"permissions"`
	Sources     []agentTokenSourceView `json:"sources"`
	CreatedAt   time.Time              `json:"created_at"`
	Secret      string                 `json:"secret"`
	DaemonURL   string                 `json:"daemon_url"`
}

// agentTokenView is the list/revoke-safe view of a grant: no secret or digest.
type agentTokenView struct {
	ID          string                 `json:"id"`
	Label       string                 `json:"label"`
	Permissions []string               `json:"permissions"`
	Sources     []agentTokenSourceView `json:"sources"`
	CreatedAt   time.Time              `json:"created_at"`
}

type agentTokenSourceView struct {
	ID         int64    `json:"id"`
	Type       string   `json:"type"`
	Identifier string   `json:"identifier"`
	SenderKeys []string `json:"sender_keys"`
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
		sources[i] = agentTokenSourceView{
			ID: s.ID, Type: s.Type, Identifier: s.Identifier,
			SenderKeys: append([]string{}, s.SenderKeys...),
		}
	}
	return agentTokenView{
		ID:          g.ID,
		Label:       g.Label,
		Permissions: perms,
		Sources:     sources,
		CreatedAt:   g.CreatedAt,
	}
}

type agentGrantIdentityResolver interface {
	ListAccountIdentitiesContext(ctx context.Context, sourceID int64) ([]store.AccountIdentity, error)
}

func canonicalMailboxIdentity(value string) (string, bool) {
	address, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil || address == nil || address.Address == "" || !strings.Contains(address.Address, "@") {
		return "", false
	}
	return store.NormalizeIdentifierForCompare(address.Address), true
}

func senderKeysForSource(
	ctx context.Context,
	resolver agentGrantIdentityResolver,
	source *store.Source,
	selected []string,
	selectedSet bool,
) ([]string, error) {
	if resolver == nil {
		if selectedSet && len(selected) > 0 {
			return nil, errors.New("sender selections are unavailable")
		}
		return nil, nil
	}
	identities, err := resolver.ListAccountIdentitiesContext(ctx, source.ID)
	if err != nil {
		return nil, fmt.Errorf("list identities for source %d: %w", source.ID, err)
	}
	valid := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		if !identity.ConfirmedAt.IsZero() {
			if key, ok := canonicalMailboxIdentity(identity.Address); ok {
				valid[key] = struct{}{}
			}
		}
	}
	if !selectedSet {
		keys := make([]string, 0, len(valid))
		for _, identity := range identities {
			if !identity.ConfirmedAt.IsZero() {
				if key, ok := canonicalMailboxIdentity(identity.Address); ok {
					if _, seen := valid[key]; seen {
						keys = append(keys, key)
						delete(valid, key)
					}
				}
			}
		}
		return keys, nil
	}
	keys := make([]string, 0, len(selected))
	seen := make(map[string]struct{}, len(selected))
	for _, value := range selected {
		key, ok := canonicalMailboxIdentity(value)
		if !ok {
			return nil, fmt.Errorf("sender %q is not a valid mailbox identity", value)
		}
		if _, ok := valid[key]; !ok {
			return nil, fmt.Errorf("sender %q is not a confirmed identity on source %d", value, source.ID)
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("sender %q was selected more than once", value)
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

// ownerAPIKeyPresented returns true when the request carries the owner API key.
// Routes through the classifier cached by requestSecurityMiddleware so header
// normalization is handled in exactly one place.
func (s *Server) ownerAPIKeyPresented(r *http.Request) bool {
	return s.requestAuthentication(r).Mode == AuthModeAPIKey
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
	dec := jsontext.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := json.UnmarshalDecode(dec, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}

	if !requireSingleJSONValue(w, dec, "invalid_request") {
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
	resolvedSources := make([]*store.Source, 0, len(req.SourceIDs))
	for _, id := range req.SourceIDs {
		src, err := resolver.GetSourceByIDContext(r.Context(), id)
		if errors.Is(err, store.ErrSourceNotFound) {
			writeError(w, http.StatusBadRequest, "invalid_source", "source not found")
			return
		}
		if err != nil {
			s.logger.Error("resolve agent token source", "source_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Could not resolve source")
			return
		}
		sources = append(sources, agentgrant.SourceRef{
			ID:         src.ID,
			Type:       src.SourceType,
			Identifier: src.Identifier,
		})
		resolvedSources = append(resolvedSources, src)
	}

	identityResolver, _ := s.store.(agentGrantIdentityResolver)
	validSourceIDs := make(map[int64]struct{}, len(sources))
	for _, source := range sources {
		validSourceIDs[source.ID] = struct{}{}
	}
	for sourceID := range req.SenderSelections {
		id, err := strconv.ParseInt(sourceID, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_sender", "sender selection source ID is invalid")
			return
		}
		if strconv.FormatInt(id, 10) != sourceID {
			writeError(w, http.StatusBadRequest, "invalid_sender", "sender selection source ID must be canonical")
			return
		}
		if _, ok := validSourceIDs[id]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_sender", "sender selection names an unselected source")
			return
		}
	}
	for i := range sources {
		key := strconv.FormatInt(sources[i].ID, 10)
		selected, selectedSet := req.SenderSelections[key]
		senderKeys, err := senderKeysForSource(r.Context(), identityResolver, resolvedSources[i], selected, selectedSet)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sender", err.Error())
			return
		}
		sources[i].SenderKeys = senderKeys
	}

	_, secret, g, err := s.agentGrants.Issue(req.Label, perms, sources)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Derive daemon URL from the validated request origin, including trusted proxy headers.
	security, _ := securityFromRequest(r)
	daemonURL := security.scheme + "://" + security.host

	v := grantToView(g)
	writeJSON(w, http.StatusCreated, agentTokenIssueResponse{
		ID:          v.ID,
		Label:       v.Label,
		Permissions: v.Permissions,
		Sources:     v.Sources,
		CreatedAt:   v.CreatedAt,
		Secret:      secret,
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
		writeError(w, http.StatusServiceUnavailable, "agent_access_disabled", "Agent access is not enabled")
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

	if s.agentGrants == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_access_disabled", "Agent access is not enabled")
		return
	}

	id := r.PathValue("id")
	if id != "" {
		s.agentGrants.Revoke(id)
	}
	w.WriteHeader(http.StatusNoContent)
}
