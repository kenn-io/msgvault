package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const (
	// These mirror the store's clamping. Duplicating them is deliberate: the
	// response echoes the effective page size, and a bounded result is an
	// endpoint contract rather than a store implementation detail.
	identityMatchDefaultLimit = 100
	identityMatchMaxLimit     = 500
)

// IdentityMatchStore is the feature-local capability for reviewable identity
// match candidates.
type IdentityMatchStore interface {
	ListIdentityMatchCandidatesContext(
		ctx context.Context, states []store.IdentityMatchState, limit, offset int,
	) ([]store.IdentityMatchCandidate, error)
	GetIdentityMatchCandidateContext(
		ctx context.Context, candidateID int64,
	) (*store.IdentityMatchCandidate, error)
	AcceptIdentityMatchCandidateContext(
		ctx context.Context, candidateID int64, decidedBy string, notes *string,
	) (*store.IdentityMatchCandidate, int64, error)
	DecideIdentityMatchCandidateContext(
		ctx context.Context, candidateID int64, state store.IdentityMatchState,
		decidedBy string, notes *string,
	) (*store.IdentityMatchCandidate, error)
	IdentityRevision() (int64, error)
}

// IdentityMatchCandidatesResponse is a bounded page of candidates with their
// evidence.
type IdentityMatchCandidatesResponse struct {
	Candidates []store.IdentityMatchCandidate `json:"candidates"`
	Limit      int                            `json:"limit"`
	Offset     int                            `json:"offset"`
}

// DecideIdentityMatchRequest is the optional body of an accept or reject.
type DecideIdentityMatchRequest struct {
	Notes *string `json:"notes,omitempty"`
}

// IdentityMatchAcceptResponse reports the decided candidate, identity revision,
// and synchronous analytics-cache state. The link is durable even when the
// cache is stale.
type IdentityMatchAcceptResponse struct {
	Candidate        store.IdentityMatchCandidate `json:"candidate"`
	IdentityRevision int64                        `json:"identity_revision"`
	CacheState       string                       `json:"cache_state" enum:"ready,stale"`
}

// IdentityMatchRejectResponse reports the retained rejected candidate, the
// post-mutation identity revision, and the synchronous cache state. A
// rejection of an earlier system acceptance can remove an owned direct edge;
// in that case the revision and cache state have the same contract as accept.
type IdentityMatchRejectResponse struct {
	Candidate        store.IdentityMatchCandidate `json:"candidate"`
	IdentityRevision int64                        `json:"identity_revision"`
	CacheState       string                       `json:"cache_state" enum:"ready,stale"`
}

func (s *Server) registerIdentityMatchRoutes(api huma.API) {
	list := rawAPIV1Operation("listIdentityMatchCandidates", http.MethodGet,
		"/identity/match-candidates", "List reviewable identity match candidates")
	list.Description = "Candidates are evidence-backed suggestions, never applied links. " +
		"Only a repeated stable provider or Beeper user ID is confirmed automatically; a " +
		"username, phone, email, display name, or shared conversation is evidence and waits " +
		"for an explicit decision."
	list.Responses = jsonResponsesFor[IdentityMatchCandidatesResponse](api)
	addErrorResponses(api, list.Responses, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListIdentityMatchCandidates)

	accept := rawAPIV1Operation("acceptIdentityMatchCandidate", http.MethodPost,
		"/identity/match-candidates/{id}/accept", "Accept an identity match candidate")
	accept.Description = "Accepting is the explicit user confirmation the matching policy " +
		"requires. The participant link is applied through the normal identity link path, so " +
		"a match spanning two curated people is refused rather than merged."
	accept.RequestBody = jsonRequestBodyFor[DecideIdentityMatchRequest](api)
	accept.RequestBody.Required = false
	accept.Responses = jsonResponsesFor[IdentityMatchAcceptResponse](api)
	addErrorResponses(api, accept.Responses, http.StatusConflict, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, accept, s.handleAcceptIdentityMatchCandidate)

	reject := rawAPIV1Operation("rejectIdentityMatchCandidate", http.MethodPost,
		"/identity/match-candidates/{id}/reject", "Reject an identity match candidate")
	reject.Description = "A rejected suggestion is retained rather than deleted, so the same " +
		"low-quality inference is not proposed again on the next import."
	reject.RequestBody = jsonRequestBodyFor[DecideIdentityMatchRequest](api)
	reject.RequestBody.Required = false
	reject.Responses = jsonResponsesFor[IdentityMatchRejectResponse](api)
	addErrorResponses(api, reject.Responses, http.StatusConflict, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, reject, s.handleRejectIdentityMatchCandidate)
}

func (s *Server) handleListIdentityMatchCandidates(w http.ResponseWriter, r *http.Request) {
	matches, ok := s.identityMatchStore(w)
	if !ok {
		return
	}
	states, ok := identityMatchStates(w, r)
	if !ok {
		return
	}
	limit, _, err := queryInt(r, "limit")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	offset, _, err := queryInt(r, "offset")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	limit = clampIdentityMatchLimit(limit)
	if offset < 0 {
		offset = 0
	}

	candidates, err := matches.ListIdentityMatchCandidatesContext(r.Context(), states, limit, offset)
	if err != nil {
		s.writeIdentityMatchError(w, err)
		return
	}
	if candidates == nil {
		candidates = []store.IdentityMatchCandidate{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, IdentityMatchCandidatesResponse{
		Candidates: candidates, Limit: limit, Offset: offset,
	})
}

func (s *Server) handleAcceptIdentityMatchCandidate(w http.ResponseWriter, r *http.Request) {
	matches, ok := s.identityMatchStore(w)
	if !ok {
		return
	}
	id, ok := identityMatchCandidateID(w, r)
	if !ok {
		return
	}
	var request DecideIdentityMatchRequest
	if !decodeIdentityMatchRequest(w, r, &request) {
		return
	}
	// An HTTP accept is always an explicit user decision. The store also
	// refuses a system accept for every basis except a stable provider ID.
	candidate, revision, err := matches.AcceptIdentityMatchCandidateContext(
		r.Context(), id, "user", request.Notes)
	if err != nil {
		s.writeIdentityMatchError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, IdentityMatchAcceptResponse{
		Candidate:        *candidate,
		IdentityRevision: revision,
		CacheState:       s.refreshIdentityCacheState(r.Context()),
	})
}

func (s *Server) handleRejectIdentityMatchCandidate(w http.ResponseWriter, r *http.Request) {
	matches, ok := s.identityMatchStore(w)
	if !ok {
		return
	}
	id, ok := identityMatchCandidateID(w, r)
	if !ok {
		return
	}
	var request DecideIdentityMatchRequest
	if !decodeIdentityMatchRequest(w, r, &request) {
		return
	}
	if _, err := matches.GetIdentityMatchCandidateContext(r.Context(), id); err != nil {
		s.writeIdentityMatchError(w, err)
		return
	}
	beforeRevision, err := matches.IdentityRevision()
	if err != nil {
		s.writeIdentityMatchError(w, err)
		return
	}
	candidate, err := matches.DecideIdentityMatchCandidateContext(
		r.Context(), id, store.IdentityMatchStateRejected, "user", request.Notes)
	if err != nil {
		s.writeIdentityMatchError(w, err)
		return
	}
	afterRevision, err := matches.IdentityRevision()
	if err != nil {
		s.writeIdentityMatchError(w, err)
		return
	}
	cacheState := identityCacheStateReady
	if afterRevision != beforeRevision {
		// A rejection of a previous system acceptance can remove the exact
		// automated edge owned by this candidate. Only that edge mutation
		// changes identity_revision and requires the synchronous cache refresh;
		// a preserved manual edge must not trigger one.
		cacheState = s.refreshIdentityCacheState(r.Context())
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, IdentityMatchRejectResponse{
		Candidate:        *candidate,
		IdentityRevision: afterRevision,
		CacheState:       cacheState,
	})
}

func (s *Server) identityMatchStore(w http.ResponseWriter) (IdentityMatchStore, bool) {
	matches, ok := s.store.(IdentityMatchStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "identity_matches_unavailable",
			"Identity match review is unavailable")
	}
	return matches, ok
}

func clampIdentityMatchLimit(limit int) int {
	switch {
	case limit <= 0:
		return identityMatchDefaultLimit
	case limit > identityMatchMaxLimit:
		return identityMatchMaxLimit
	default:
		return limit
	}
}

// identityMatchStates parses the repeatable, optionally comma-separated state
// filter. An empty filter means every state.
func identityMatchStates(
	w http.ResponseWriter, r *http.Request,
) ([]store.IdentityMatchState, bool) {
	allowed := []string{
		string(store.IdentityMatchStateCandidate),
		string(store.IdentityMatchStateAccepted),
		string(store.IdentityMatchStateRejected),
		string(store.IdentityMatchStateConflict),
	}
	var states []store.IdentityMatchState
	for _, raw := range r.URL.Query()["state"] {
		for value := range strings.SplitSeq(raw, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if !slices.Contains(allowed, value) {
				writeAPIHTTPError(w, apiHTTPErrorFromParam(
					enumParamError("state", value, allowed)))
				return nil, false
			}
			states = append(states, store.IdentityMatchState(value))
		}
	}
	return states, true
}

func identityMatchCandidateID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_candidate_id",
			"Identity match candidate ID must be a positive integer")
		return 0, false
	}
	return id, true
}

// decodeIdentityMatchRequest decodes an optional decision body. Unknown fields
// are rejected so a typo in notes is never silently discarded.
func decodeIdentityMatchRequest(
	w http.ResponseWriter, r *http.Request, request *DecideIdentityMatchRequest,
) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"Invalid JSON request body: "+err.Error())
		return false
	}
	return requireSingleJSONValue(w, decoder, "invalid_request")
}

func (s *Server) writeIdentityMatchError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrIdentityMatchNotFound):
		writeError(w, http.StatusNotFound, "identity_match_not_found",
			"Identity match candidate not found")
	case errors.Is(err, store.ErrIdentityMatchNotAcceptable):
		writeError(w, http.StatusConflict, "identity_match_not_acceptable",
			"This match needs stable provider corroboration or an explicit user decision")
	case errors.Is(err, store.ErrIdentityMatchAlreadyAccepted):
		writeError(w, http.StatusConflict, "identity_match_already_accepted",
			"An accepted identity match cannot be rejected because its participant link is retained")
	case errors.Is(err, store.ErrIdentityMatchAlreadyApplied):
		writeError(w, http.StatusConflict, "identity_match_already_applied",
			"An applied identity match must remain accepted while its participant link is retained")
	case errors.Is(err, store.ErrIdentityMatchNotAccepted):
		writeError(w, http.StatusConflict, "identity_match_state_changed",
			"The identity match changed before its participant link could be applied")
	case errors.Is(err, store.ErrIdentityMatchEndpointUnsupported):
		writeError(w, http.StatusConflict, "identity_match_endpoint_unsupported",
			"Only participant-to-participant matches can be applied")
	case errors.Is(err, store.ErrPersonBindingConflict):
		writeError(w, http.StatusConflict, "person_binding_conflict",
			"The identity clusters belong to different person profiles")
	case errors.Is(err, store.ErrParticipantNotFound), errors.Is(err, store.ErrInvalidParticipantID):
		writeError(w, http.StatusBadRequest, "invalid_participant_id", err.Error())
	case errors.Is(err, store.ErrInvalidIdentityMatchState):
		writeError(w, http.StatusBadRequest, "invalid_identity_match_state", err.Error())
	default:
		s.logger.Error("identity match operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "identity_match_failed",
			"Identity match operation failed")
	}
}
