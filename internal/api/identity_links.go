package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

// IdentityLinkStore mutates the participant link graph. Implemented by the
// serve daemon's store adapter as a direct pass-through to *store.Store.
type IdentityLinkStore interface {
	LinkParticipants(a, b int64) (int64, error)
	UnlinkParticipants(a, b int64) (int64, error)
}

// ClusterLookupStore resolves a participant's cluster membership and edges,
// read-only. The person-detail handler (see handleGetPerson in people.go)
// uses it to enrich a cluster-aware query.PersonSummary with a PersonCluster
// block; the query layer itself stays free of a store dependency, so this
// capability is composed in at the HTTP handler instead. Implemented by the
// serve daemon's store adapter as a direct pass-through to *store.Store.
type ClusterLookupStore interface {
	ClusterMembers(id int64) ([]int64, error)
	ClusterEdges(id int64) ([]store.LinkEdge, error)
}

// IdentityCacheRefresher re-exports the identity-derived Parquet datasets
// (owner_participants, participant_clusters) after a link/unlink mutation
// commits, so cached analytics reflect the new participant clusters without
// waiting for the next scheduled cache build. Implemented by the serve
// daemon's store adapter, which wraps cacheops.RefreshIdentityDatasets with
// the daemon's analytics directory and the cross-process cache build lock.
type IdentityCacheRefresher interface {
	RefreshIdentityDatasets(ctx context.Context) (int64, error)
}

const (
	identityCacheStateReady = "ready"
	identityCacheStateStale = "stale"
)

// IdentityLinkRequest is the POST /api/v1/identity/links and
// /api/v1/identity/unlinks request body.
type IdentityLinkRequest struct {
	ParticipantA int64 `json:"participant_a"`
	ParticipantB int64 `json:"participant_b"`
}

// IdentityLinkResponse reports the identity revision after the mutation
// committed and whether the synchronous cache refresh that followed it
// succeeded. The mutation is durable regardless of cache_state: "stale"
// only means the Parquet identity datasets have not caught up yet.
type IdentityLinkResponse struct {
	IdentityRevision int64  `json:"identity_revision"`
	CacheState       string `json:"cache_state" enum:"ready,stale"`
}

func (s *Server) registerIdentityLinkRoutes(api huma.API) {
	registerAPIV1RawHumaJSONRouteWithRequest[IdentityLinkRequest, IdentityLinkResponse](
		api, "linkIdentityParticipants", http.MethodPost, "/identity/links",
		"Assert two participants are the same person", s.handleLinkIdentity,
	)
	registerAPIV1RawHumaJSONRouteWithRequest[IdentityLinkRequest, IdentityLinkResponse](
		api, "unlinkIdentityParticipants", http.MethodPost, "/identity/unlinks",
		"Remove a link edge between two participants", s.handleUnlinkIdentity,
	)
}

func (s *Server) handleLinkIdentity(w http.ResponseWriter, r *http.Request) {
	s.handleIdentityLinkMutation(w, r, IdentityLinkStore.LinkParticipants)
}

func (s *Server) handleUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	s.handleIdentityLinkMutation(w, r, IdentityLinkStore.UnlinkParticipants)
}

// handleIdentityLinkMutation is shared by the link and unlink handlers:
// decode and validate the pair, run the requested mutation, then attempt a
// synchronous identity cache refresh and report its outcome. The mutation
// commits before the refresh is attempted, so a refresh failure never rolls
// back or fails the request.
//
// Store errors are mapped by kind, not lumped into one status: ErrAlreadyLinked
// is a 409 (the edge is redundant, not invalid); ErrParticipantNotFound and
// ErrInvalidParticipantID are 400s (the request named a bad participant);
// anything else (lock contention, I/O failure, context cancellation) is an
// internal failure and must not leak driver text to the client as a 400.
func (s *Server) handleIdentityLinkMutation(
	w http.ResponseWriter,
	r *http.Request,
	mutate func(IdentityLinkStore, int64, int64) (int64, error),
) {
	linker, ok := s.store.(IdentityLinkStore)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}

	var req IdentityLinkRequest
	if !decodeIdentityLinkRequest(w, r, &req) {
		return
	}
	if req.ParticipantA <= 0 || req.ParticipantB <= 0 || req.ParticipantA == req.ParticipantB {
		writeError(w, http.StatusBadRequest, "invalid_participant_id",
			"participant_a and participant_b must be distinct positive participant IDs")
		return
	}

	revision, err := mutate(linker, req.ParticipantA, req.ParticipantB)
	switch {
	case errors.Is(err, store.ErrAlreadyLinked):
		writeError(w, http.StatusConflict, "already_linked",
			"these participants are already connected through other links")
		return
	case errors.Is(err, store.ErrParticipantNotFound), errors.Is(err, store.ErrInvalidParticipantID):
		writeError(w, http.StatusBadRequest, "invalid_participant_id", err.Error())
		return
	case err != nil:
		s.logger.Error("identity link mutation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to update participant links")
		return
	}

	writeJSON(w, http.StatusOK, IdentityLinkResponse{
		IdentityRevision: revision,
		CacheState:       s.refreshIdentityCacheState(r.Context()),
	})
}

// decodeIdentityLinkRequest decodes the request body, rejecting unknown
// fields so a typo in participant_a/participant_b is not silently ignored.
func decodeIdentityLinkRequest(w http.ResponseWriter, r *http.Request, req *IdentityLinkRequest) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("invalid JSON request body: %v", err))
		return false
	}
	return true
}

// refreshIdentityCacheState attempts the synchronous identity-dataset cache
// refresh and reports "ready" or "stale". A missing refresher capability or
// a refresh error both degrade to "stale" rather than failing the caller's
// request: the mutation that triggered the refresh already committed.
func (s *Server) refreshIdentityCacheState(ctx context.Context) string {
	refresher, ok := s.store.(IdentityCacheRefresher)
	if !ok {
		return identityCacheStateStale
	}
	if _, err := refresher.RefreshIdentityDatasets(ctx); err != nil {
		s.logger.Error("identity cache refresh failed", "error", err)
		return identityCacheStateStale
	}
	return identityCacheStateReady
}
